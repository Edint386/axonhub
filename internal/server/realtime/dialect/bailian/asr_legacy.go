package bailian

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// asrLegacyDialectName is reported by Name() for beta-protocol clients relayed to Qwen-ASR.
const asrLegacyDialectName = "bailian_asr_legacy"

// ASRPassthrough relays a client that already speaks Bailian's own protocol (the OpenAI
// Realtime beta shape: OpenAI-Beta: realtime=v1, flat session fields,
// conversation.item.input_audio_transcription.text) to Qwen-ASR realtime unchanged. It only
// echoes the requested model name back and meters usage: Bailian reports running session
// totals on every transcription.completed, so each utterance is billed the difference.
type ASRPassthrough struct {
	mu sync.Mutex

	opts  ASROptions
	meter usageMeter
	// clientFinished is set once the client sent session.finish itself; the gateway then sends
	// no second one (Bailian's reaction to it is unknown) but still waits for session.finished.
	clientFinished bool
	itemSeq        int
}

var _ dialect.Dialect = (*ASRPassthrough)(nil)

// NewASRPassthrough returns the beta-protocol passthrough dialect for a Qwen-ASR model.
func NewASRPassthrough(opts ASROptions) *ASRPassthrough {
	return &ASRPassthrough{opts: opts}
}

// Name implements dialect.Dialect.
func (d *ASRPassthrough) Name() string { return asrLegacyDialectName }

// Start implements dialect.Dialect.
func (d *ASRPassthrough) Start() ([][]byte, bool) { return nil, false }

// FinishDrain tells the session to wait for session.finished after CloseUpstream.
func (d *ASRPassthrough) FinishDrain() bool { return true }

// CloseGrace bounds the close handshake; Bailian echoes a client close frame promptly.
func (d *ASRPassthrough) CloseGrace() time.Duration { return 2 * time.Second }

// CloseUpstream implements dialect.Dialect: the gateway finishes the session unless the client
// already did, so the utterance in progress is transcribed and billed. Either way the session
// waits for session.finished (FinishDrain).
func (d *ASRPassthrough) CloseUpstream() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.clientFinished {
		return nil
	}

	return [][]byte{[]byte(`{"type":"session.finish"}`)}
}

// ClientFrame implements dialect.Dialect: every client frame is forwarded unchanged.
func (d *ASRPassthrough) ClientFrame(frame []byte) (dialect.Output, error) {
	if dialect.EventType(frame) == "session.finish" {
		d.mu.Lock()
		d.clientFinished = true
		d.mu.Unlock()
	}

	return dialect.Output{ToUpstream: [][]byte{frame}}, nil
}

// UpstreamFrame implements dialect.Dialect: frames are forwarded unchanged apart from the
// model name in session events; completions yield per-utterance usage records.
func (d *ASRPassthrough) UpstreamFrame(frame []byte) (dialect.Output, error) {
	switch dialect.EventType(frame) {
	case "session.created", "session.updated":
		if d.opts.RequestedModel != "" && d.opts.RequestedModel != d.opts.ActualModel && gjson.GetBytes(frame, "session.model").Exists() {
			if out, err := sjson.SetBytes(frame, "session.model", d.opts.RequestedModel); err == nil {
				frame = out
			}
		}

		return dialect.Output{ToClient: [][]byte{frame}}, nil
	case "session.finished":
		return dialect.Output{ToClient: [][]byte{frame}, UpstreamFinished: true}, nil
	case "conversation.item.input_audio_transcription.completed":
		return d.completed(frame), nil
	default:
		return dialect.Output{ToClient: [][]byte{frame}}, nil
	}
}

func (d *ASRPassthrough) completed(frame []byte) dialect.Output {
	d.mu.Lock()
	defer d.mu.Unlock()

	ev := gjson.ParseBytes(frame)

	itemID := ev.Get("item_id").String()
	if itemID == "" {
		// Manual-mode completions carry no item id.
		d.itemSeq++
		itemID = "item_axh_" + itoa(d.itemSeq)
	}

	usage := d.meter.next(parseASRUsage(ev.Get("usage")))

	summary, _ := json.Marshal(map[string]any{
		"kind":       "transcription",
		"item_id":    itemID,
		"transcript": ev.Get("transcript").String(),
		"language":   ev.Get("language").String(),
		"usage":      usage.gaTokens(),
		"seconds":    usage.Duration,
	})

	return dialect.Output{
		ToClient: [][]byte{frame},
		Usage:    []dialect.UsageRecord{usage.record(itemID, d.opts.ActualModel, summary)},
	}
}

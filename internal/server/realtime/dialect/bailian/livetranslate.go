package bailian

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// liveTranslateFinishDrain bounds the wait for session.finished after session.finish: the
// upstream first translates the speech still in flight (live probe: the last sentence finished
// ~1.5 s after its audio, session.finished ~0.4 s after that).
const liveTranslateFinishDrain = 15 * time.Second

// LiveTranslateOptions configures an OpenAI realtime translation session relayed to a
// Qwen-LiveTranslate realtime model.
type LiveTranslateOptions struct {
	// RequestedModel is the model the client selected; it is echoed in the session object.
	RequestedModel string
	// ActualModel is the upstream model; usage is billed under it.
	ActualModel string
	// IDs generates ids for synthesised server events; a nil value gets a fresh generator.
	IDs *dialect.EventIDs
	// Voice is the upstream voice of spoken translations; empty means DefaultLiveTranslateVoice.
	Voice string
	// SessionDuration is the session's lifetime limit, reported as session.expires_at.
	SessionDuration time.Duration
}

// LiveTranslateDialect relays the OpenAI realtime translation protocol
// (GET /v1/realtime/translations) to Qwen-LiveTranslate realtime.
//
// The upstream accepts exactly one session.update per session (a second one closes the socket
// with 1007, live probe 2026-11-05), so client session.update events are answered locally and
// one merged update is sent right before the first audio; afterwards the target language is
// fixed. Client audio (24 kHz) is resampled to 16 kHz; the 24 kHz translated speech passes
// through. The upstream's per-sentence response cycles are flattened into the protocol's three
// delta streams and each response.done is billed as one usage record.
type LiveTranslateDialect struct {
	mu sync.Mutex

	opts LiveTranslateOptions
	ids  *dialect.EventIDs
	ctx  context.Context

	cfg       translateConfig
	sessionID string
	expiresAt int64
	created   bool

	initSent bool
	// awaitingAck is set from the upstream update until its session.updated; the session gate
	// holds client frames meanwhile and pending holds the audio that triggered the update.
	awaitingAck  bool
	pending      [][]byte
	lastUpdateID string

	// closing is set once the session is ending (client session.close, upstream finish or
	// error); closed once session.closed was sent; failed after an upstream error.
	// clientClosing marks a session.close from the client, finishSent the session.finish
	// teardown sent upstream, and finished the upstream's session.finished.
	closing       bool
	closed        bool
	failed        bool
	clientClosing bool
	finishSent    bool
	finished      bool

	resampler *audio.Resampler
	// inputBytes counts client PCM (24 kHz) for elapsed_ms.
	inputBytes int64

	source transcriptStitcher
	target transcriptStitcher
}

var _ dialect.Dialect = (*LiveTranslateDialect)(nil)

// NewLiveTranslate returns the translation dialect for a Qwen-LiveTranslate realtime model.
func NewLiveTranslate(opts LiveTranslateOptions) (*LiveTranslateDialect, error) {
	if !SupportedLiveTranslate(opts.ActualModel) {
		return nil, fmt.Errorf("bailian: model %q is not a Qwen-LiveTranslate realtime model", opts.ActualModel)
	}

	if opts.RequestedModel == "" {
		opts.RequestedModel = opts.ActualModel
	}

	if strings.TrimSpace(opts.Voice) == "" {
		opts.Voice = DefaultLiveTranslateVoice
	}

	ids := opts.IDs
	if ids == nil {
		ids = &dialect.EventIDs{}
	}

	r, err := audio.NewResampler(clientRate, liveTranslateInputRate)
	if err != nil {
		return nil, fmt.Errorf("bailian livetranslate: resampler: %w", err)
	}

	d := &LiveTranslateDialect{
		opts:      opts,
		ids:       ids,
		ctx:       context.Background(),
		cfg:       defaultTranslateConfig(),
		resampler: r,
	}

	if opts.SessionDuration > 0 {
		d.expiresAt = time.Now().Add(opts.SessionDuration).Unix()
	}

	return d, nil
}

// Name implements dialect.Dialect.
func (d *LiveTranslateDialect) Name() string { return LiveTranslateDialectName }

// Start implements dialect.Dialect: the gate stays closed until the upstream session.created,
// so the OpenAI session.created always precedes the locally answered session.updated events.
func (d *LiveTranslateDialect) Start() ([][]byte, bool) { return nil, true }

// GateTimeout bounds the wait for session.created and for the acknowledgement of the update.
func (d *LiveTranslateDialect) GateTimeout() time.Duration { return 5 * time.Second }

// CloseGrace bounds the close handshake.
func (d *LiveTranslateDialect) CloseGrace() time.Duration { return 2 * time.Second }

// CloseUpstream implements dialect.Dialect: session.finish makes the upstream translate the
// speech still in flight, bill it and answer session.finished. Nothing is sent when no audio
// went upstream: none was appended, or the only audio is still held for the acknowledgement of
// the update (it is dropped once the session ends). Nothing is sent to a failed upstream either.
func (d *LiveTranslateDialect) CloseUpstream() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.initSent || d.awaitingAck || d.failed {
		return nil
	}

	d.finishSent = true

	return [][]byte{[]byte(`{"event_id":"axh_lt_finish","type":"session.finish"}`)}
}

// FinishDrain tells the session to wait for session.finished once CloseUpstream sent
// session.finish, so the last translation reaches the client and its usage is recorded. It
// follows what CloseUpstream sent, so an acknowledgement arriving in between cannot make the
// session wait for a session.finished nobody asked for.
func (d *LiveTranslateDialect) FinishDrain() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.finishSent && !d.failed
}

// PendingGoodbye implements the session's pendingGoodbyer: a client session.close is always
// answered with session.closed, also when the upstream errored, closed or timed out before its
// session.finished (otherwise the answer). It is sent once.
func (d *LiveTranslateDialect) PendingGoodbye() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.clientClosing {
		return nil
	}

	return d.sessionClosed()
}

// GoodbyeFailed implements the session's goodbyeReporter: a client session.close whose flush
// the upstream did not complete (it errored, or never answered session.finish with
// session.finished) is not a completed session.
func (d *LiveTranslateDialect) GoodbyeFailed() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch {
	case !d.clientClosing:
		return nil
	case d.failed:
		return errors.New("upstream error while finishing the translation session")
	case d.finishSent && !d.finished:
		return errors.New("upstream did not finish the translation session after session.close")
	default:
		return nil
	}
}

// FinishDrainTimeout extends the finish drain beyond the session's default response drain.
func (d *LiveTranslateDialect) FinishDrainTimeout() time.Duration { return liveTranslateFinishDrain }

// ClientFrame implements dialect.Dialect for frames received from the OpenAI client.
func (d *LiveTranslateDialect) ClientFrame(frame []byte) (dialect.Output, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ev := gjson.ParseBytes(frame)
	clientEventID := ev.Get("event_id").String()
	typ := ev.Get("type").String()

	if d.closing && (typ == "session.update" || typ == "session.input_audio_buffer.append") {
		return d.clientError("session_closing", "The session is closing.", "", clientEventID), nil
	}

	switch typ {
	case "session.update":
		return d.clientSessionUpdate(ev, clientEventID), nil
	case "session.input_audio_buffer.append":
		return d.clientAppend(ev, clientEventID), nil
	case "session.close":
		return d.clientClose(), nil
	default:
		return d.clientError("invalid_value", fmt.Sprintf("Unsupported event type %q; a translation session accepts "+
			"session.update, session.input_audio_buffer.append and session.close.", typ), "type", clientEventID), nil
	}
}

func (d *LiveTranslateDialect) clientError(code, message, param, clientEventID string) dialect.Output {
	return dialect.Output{ToClient: [][]byte{dialect.ErrorEvent(d.ids, "invalid_request_error", code, message, param, clientEventID)}}
}

// clientSessionUpdate merges an update into the effective config and answers it locally. Once
// audio went upstream the target language can no longer change.
func (d *LiveTranslateDialect) clientSessionUpdate(ev gjson.Result, clientEventID string) dialect.Output {
	cfg, rej := mergeTranslateUpdate(d.cfg, ev.Get("session"), d.opts.RequestedModel, d.opts.ActualModel)
	if rej != nil {
		return d.clientError(rej.code, rej.message, rej.param, clientEventID)
	}

	if d.initSent && cfg.language != d.cfg.language {
		return d.clientError("unsupported_on_upstream",
			"The output language is fixed once audio has been sent; reconnect to translate into another language.",
			"session.audio.output.language", clientEventID)
	}

	d.cfg = cfg
	d.lastUpdateID = clientEventID

	return dialect.Output{ToClient: [][]byte{d.sessionEvent("session.updated")}}
}

// clientAppend resamples one append to 16 kHz upstream appends of at most 100 ms. The first
// audio is preceded by the single upstream session.update and held until it is acknowledged.
func (d *LiveTranslateDialect) clientAppend(ev gjson.Result, clientEventID string) dialect.Output {
	pcm, err := base64.StdEncoding.DecodeString(ev.Get("audio").String())
	if err != nil || len(pcm) == 0 {
		return d.clientError("invalid_value", "Invalid base64 audio.", "audio", clientEventID)
	}

	d.inputBytes += int64(len(pcm))
	frames := d.upstreamAppends(d.resampler.Process(pcm), clientEventID)

	if d.initSent {
		return dialect.Output{ToUpstream: frames}
	}

	d.initSent, d.awaitingAck = true, true
	d.pending = append(d.pending, frames...)

	return dialect.Output{ToUpstream: [][]byte{d.cfg.initUpdate(d.opts.Voice)}, CloseGate: true}
}

func (d *LiveTranslateDialect) upstreamAppends(pcm []byte, clientEventID string) [][]byte {
	chunks := audio.SplitPCM(pcm, liveTranslateInputRate, upstreamChunk)
	frames := make([][]byte, 0, len(chunks))

	for i, chunk := range chunks {
		event := map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(chunk)}
		if i == 0 && clientEventID != "" {
			event["event_id"] = clientEventID
		}

		b, _ := json.Marshal(event)
		frames = append(frames, b)
	}

	return frames
}

// clientClose ends the session with 1000. When audio went upstream, teardown sends
// session.finish (CloseUpstream) and relays the remaining translation until session.finished,
// which becomes session.closed; otherwise there is nothing to flush.
func (d *LiveTranslateDialect) clientClose() dialect.Output {
	if d.closing {
		return dialect.Output{}
	}

	d.closing, d.clientClosing = true, true
	out := dialect.Output{Close: &dialect.CloseRequest{Code: 1000, Reason: "session closed by client"}}

	if !d.initSent {
		out.ToClient = d.sessionClosed()
	}

	return out
}

func (d *LiveTranslateDialect) sessionClosed() [][]byte {
	if d.closed {
		return nil
	}

	d.closed = true

	return [][]byte{d.synth(map[string]any{"type": "session.closed"})}
}

func (d *LiveTranslateDialect) sessionEvent(typ string) []byte {
	return d.synth(map[string]any{
		"type":    typ,
		"session": d.cfg.renderSession(d.sessionID, d.opts.RequestedModel, d.expiresAt),
	})
}

func (d *LiveTranslateDialect) synth(event map[string]any) []byte {
	event["event_id"] = d.ids.Next()

	b, err := json.Marshal(event)
	if err != nil {
		log.Warn(d.ctx, "bailian livetranslate: marshal synthesized event", log.Cause(err))
		return nil
	}

	return b
}

// elapsedMs approximates the protocol's elapsed_ms: the client audio received so far, in the
// 200 ms steps the OpenAI engine reports.
func (d *LiveTranslateDialect) elapsedMs() int64 {
	ms := d.inputBytes / 2 * 1000 / clientRate
	return ms / 200 * 200
}

// UpstreamFrame implements dialect.Dialect for frames received from Qwen-LiveTranslate. Events
// without an OpenAI counterpart are dropped (never returned as an error, which would make the
// session forward the raw frame).
func (d *LiveTranslateDialect) UpstreamFrame(frame []byte) (dialect.Output, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ev := gjson.ParseBytes(frame)

	switch ev.Get("type").String() {
	case "session.created":
		return d.upstreamSessionCreated(ev), nil
	case "session.updated":
		return d.upstreamSessionUpdated(), nil
	case "session.finished":
		return d.upstreamFinished(), nil
	case "error":
		return d.upstreamError(ev), nil
	case "response.audio_transcript.delta", "response.text.delta":
		return d.transcript(&d.target, "session.output_transcript.delta", targetKey(ev), ev.Get("delta").String(), false), nil
	case "response.audio_transcript.done":
		return d.transcript(&d.target, "session.output_transcript.delta", targetKey(ev), ev.Get("transcript").String(), true), nil
	case "response.text.done":
		return d.transcript(&d.target, "session.output_transcript.delta", targetKey(ev), ev.Get("text").String(), true), nil
	case "response.audio.delta":
		return d.outputAudio(ev), nil
	case "conversation.item.input_audio_transcription.delta":
		return d.sourceTranscript(ev.Get("item_id").String(), ev.Get("delta").String(), false), nil
	case "conversation.item.input_audio_transcription.completed":
		return d.sourceTranscript(ev.Get("item_id").String(), ev.Get("transcript").String(), true), nil
	case "conversation.item.input_audio_transcription.failed":
		return d.sourceFailed(ev), nil
	case "response.done":
		return d.responseDone(ev), nil
	default:
		return dialect.Output{}, nil
	}
}

// upstreamSessionCreated answers the client's session.created once and opens the gate.
func (d *LiveTranslateDialect) upstreamSessionCreated(ev gjson.Result) dialect.Output {
	if d.created {
		return dialect.Output{}
	}

	d.created = true

	d.sessionID = ev.Get("session.id").String()
	if d.sessionID == "" {
		d.sessionID = "sess_" + d.ids.Next()
	}

	return dialect.Output{ToClient: [][]byte{d.sessionEvent("session.created")}, OpenGate: true}
}

// upstreamSessionUpdated acknowledges the adapter's single update: the held audio is released
// together with the gate. The client already got its own session.updated events.
func (d *LiveTranslateDialect) upstreamSessionUpdated() dialect.Output {
	if !d.awaitingAck {
		return dialect.Output{}
	}

	d.awaitingAck = false
	out := dialect.Output{ToUpstream: d.pending, OpenGate: true}
	d.pending = nil

	return out
}

// upstreamFinished turns session.finished into session.closed and ends the session.
func (d *LiveTranslateDialect) upstreamFinished() dialect.Output {
	d.finished = true
	out := dialect.Output{ToClient: d.sessionClosed(), UpstreamFinished: true}

	if !d.closing {
		d.closing = true
		out.Close = &dialect.CloseRequest{Code: 1000, Reason: "session finished"}
	}

	return out
}

// upstreamError relays an upstream error and ends the session: the upstream closes the socket
// after its errors (1007 on a rejected update and on response failures, live probe).
func (d *LiveTranslateDialect) upstreamError(ev gjson.Result) dialect.Output {
	e := ev.Get("error")

	errType := e.Get("type").String()
	if errType == "" {
		errType = "server_error"
	}

	code := e.Get("code").String()

	message := e.Get("message").String()
	if message == "" {
		message = "upstream error"
	}

	param := ""
	if strings.HasPrefix(e.Get("param").String(), "session.translation.language") {
		param = "session.audio.output.language"
	}

	eventID := e.Get("event_id").String()
	if d.awaitingAck || eventID == liveTranslateInitEventID {
		// The rejected update merges the client's updates; point at the last one.
		errType, code = "server_error", "upstream_config_rejected"
		message = "The upstream rejected the translation session configuration: " + message
		eventID = d.lastUpdateID
	}

	log.Warn(d.ctx, "bailian livetranslate: upstream error; closing session",
		log.String("model", d.opts.ActualModel), log.String("code", e.Get("code").String()), log.String("message", e.Get("message").String()))

	d.closing, d.failed = true, true

	return dialect.Output{
		ToClient: [][]byte{dialect.ErrorEvent(d.ids, errType, code, message, param, eventID)},
		Close:    &dialect.CloseRequest{Code: 1011, Reason: "upstream error"},
	}
}

// targetKey identifies the translated segment of a response event.
func targetKey(ev gjson.Result) string {
	if id := ev.Get("item_id").String(); id != "" {
		return id
	}

	return ev.Get("response_id").String()
}

// transcript emits one stitched transcript delta of type typ; final marks the segment's final
// text (only its missing suffix is emitted).
func (d *LiveTranslateDialect) transcript(s *transcriptStitcher, typ, key, text string, final bool) dialect.Output {
	var out string
	if final {
		out = s.done(key, text)
	} else {
		out = s.delta(key, text)
	}

	if out == "" {
		return dialect.Output{}
	}

	return dialect.Output{ToClient: [][]byte{d.synth(map[string]any{
		"type":       typ,
		"delta":      out,
		"elapsed_ms": d.elapsedMs(),
	})}}
}

// sourceTranscript relays the upstream's (always on) source transcription when the client
// enabled audio.input.transcription.
func (d *LiveTranslateDialect) sourceTranscript(itemID, text string, final bool) dialect.Output {
	if d.cfg.transcription == nil {
		return dialect.Output{}
	}

	return d.transcript(&d.source, "session.input_transcript.delta", itemID, text, final)
}

func (d *LiveTranslateDialect) sourceFailed(ev gjson.Result) dialect.Output {
	if d.cfg.transcription == nil {
		return dialect.Output{}
	}

	code := ev.Get("error.code").String()
	if code == "" {
		code = "input_transcription_failed"
	}

	message := ev.Get("error.message").String()
	if message == "" {
		message = "Input transcription failed."
	}

	return dialect.Output{ToClient: [][]byte{dialect.ErrorEvent(d.ids, "server_error", code, message, "", "")}}
}

// outputAudio relays translated speech; it is already 24 kHz PCM16.
func (d *LiveTranslateDialect) outputAudio(ev gjson.Result) dialect.Output {
	delta := ev.Get("delta").String()
	if delta == "" {
		return dialect.Output{}
	}

	return dialect.Output{ToClient: [][]byte{d.synth(map[string]any{
		"type":        "session.output_audio.delta",
		"delta":       delta,
		"format":      "pcm16",
		"sample_rate": liveTranslateOutputRate,
		"channels":    1,
		"elapsed_ms":  d.elapsedMs(),
	})}}
}

// responseDone bills one translated segment. Usage is per response, not cumulative (live
// probe: 154 then 269 total tokens for two sentences). The response state is deliberately not
// reported: teardown must not send response.cancel, which this upstream does not know.
func (d *LiveTranslateDialect) responseDone(ev gjson.Result) dialect.Output {
	resp := ev.Get("response")
	responseID := resp.Get("id").String()
	status := resp.Get("status").String()

	summary, _ := json.Marshal(map[string]any{
		"kind":        "translation",
		"response_id": responseID,
		"status":      status,
		"language":    d.cfg.language,
		"transcript":  responseTranscript(resp),
		"usage":       rawOrNull(rawCopy(resp.Get("usage"))),
	})

	out := dialect.Output{Usage: []dialect.UsageRecord{{
		Kind:       "response",
		ResponseID: responseID,
		Usage:      dialect.GAUsage(resp.Get("usage")),
		Summary:    summary,
	}}}

	if status == "failed" || status == "incomplete" {
		message := resp.Get("status_details.error.message").String()
		if message == "" {
			message = "The translation of a segment ended with status " + status + "."
		}

		out.ToClient = [][]byte{dialect.ErrorEvent(d.ids, "server_error", "response_"+status, message, "", "")}
	}

	return out
}

// responseTranscript returns the translated text of a response object.
func responseTranscript(resp gjson.Result) string {
	for _, item := range resp.Get("output").Array() {
		for _, part := range item.Get("content").Array() {
			if t := part.Get("transcript").String(); t != "" {
				return t
			}

			if t := part.Get("text").String(); t != "" {
				return t
			}
		}
	}

	return ""
}

// Package bailian implements the realtime relay dialect for Alibaba Bailian's
// Qwen-Omni-Realtime models: GA OpenAI Realtime clients on one side, the beta-shaped
// Bailian WebSocket protocol on the other.
//
// The adapter owns the complete upstream session configuration (every client
// session.update is merged into it and sent whole), owns the audio formats (clients always
// speak 24 kHz PCM; the adapter resamples to the model's input rate when it is lower), owns
// the voice (only voices the model family can actually speak become effective), caches
// conversation items to answer conversation.item.retrieve locally, diffs Bailian's
// per-sentence transcription deltas into GA deltas and synthesises the usage GA requires.
//
// Everything in this package is custom (fork) code.
package bailian

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

const (
	// dialectName is reported by Name() and used by the session for duration defaults.
	dialectName = "bailian"

	// initEventID1 and initEventID2 tag the adapter's own init session.update and its retry.
	initEventID1 = "axh_init_1"
	initEventID2 = "axh_init_2"

	// upstreamChunk bounds the duration of one upstream input_audio_buffer.append.
	upstreamChunk = 100 * time.Millisecond

	// imageSilence is appended before the first image when no audio was sent yet, because
	// Bailian requires at least one audio append before input_image_buffer.append.
	imageSilence = 100 * time.Millisecond
)

// Options configures a Bailian dialect instance.
type Options struct {
	// RequestedModel is the model name echoed back to clients.
	RequestedModel string
	// ActualModel is the upstream model; it also selects the capability profile.
	ActualModel string
	// IDs generates ids for synthesised server events; a nil value gets a fresh generator.
	IDs *dialect.EventIDs
	// WantTranscription is whether the client's first session.update (or query) asked for
	// input transcription; later session.update frames may change it.
	WantTranscription bool
}

// initState tracks the adapter's initialisation handshake with the upstream.
type initState int

const (
	// initWaitCreated means the upstream session.created has not been read yet.
	initWaitCreated initState = iota
	// initPending means an init session.update is in flight.
	initPending
	// initDone means the init was acknowledged and the gate is open.
	initDone
	// initFailed means both init attempts were rejected and the session is closing.
	initFailed
)

// pendingUpdate is a session.update sent upstream that has not been acknowledged yet.
type pendingUpdate struct {
	upstreamEventID string
	clientEventID   string
	cfg             sessionConfig
	init            bool
}

// pendingDelete is a conversation.item.delete forwarded upstream.
type pendingDelete struct {
	itemID        string
	clientEventID string
}

// Dialect is the Bailian implementation of dialect.Dialect. All state is guarded by mu
// because ClientFrame and UpstreamFrame run on different goroutines.
type Dialect struct {
	mu sync.Mutex

	opts Options
	caps Capabilities
	ids  *dialect.EventIDs
	ctx  context.Context

	initState   initState
	initRetried bool
	committed   sessionConfig
	pending     []pendingUpdate
	// held buffers client-bound frames produced before the GA session.created is rendered.
	held [][]byte

	resampler *audio.Resampler
	// audioStarted is set once an append reached the upstream (formats are frozen after it).
	audioStarted bool
	// bufferHasAudio is set while the upstream input buffer holds audio since the last
	// commit/clear; images need a non-empty buffer to be committed with.
	bufferHasAudio bool
	appendedBytes  int64
	itemBytes      map[string]int64

	responseItems map[string]bool
	doneEmitted   map[string]bool
	items         *itemCache
	transcripts   map[string]*transcriptTracker

	pendingMetadata json.RawMessage
	activeMetadata  json.RawMessage
	pendingDeletes  map[string]pendingDelete

	seq uint64
}

// New returns a dialect for opts.ActualModel, or an error when the model is not a
// Qwen-Omni-Realtime model.
func New(opts Options) (dialect.Dialect, error) {
	if !Supported(opts.ActualModel) {
		return nil, fmt.Errorf("bailian: model %q is not a Qwen-Omni-Realtime model", opts.ActualModel)
	}

	if opts.RequestedModel == "" {
		opts.RequestedModel = opts.ActualModel
	}

	if opts.IDs == nil {
		opts.IDs = &dialect.EventIDs{}
	}

	d := &Dialect{
		opts:           opts,
		caps:           CapabilitiesFor(opts.ActualModel),
		ids:            opts.IDs,
		ctx:            context.Background(),
		itemBytes:      map[string]int64{},
		responseItems:  map[string]bool{},
		doneEmitted:    map[string]bool{},
		items:          newItemCache(),
		transcripts:    map[string]*transcriptTracker{},
		pendingDeletes: map[string]pendingDelete{},
	}

	if d.caps.Resample {
		r, err := audio.NewResampler(clientRate, d.caps.InputRate)
		if err != nil {
			return nil, fmt.Errorf("bailian: resampler for %s: %w", opts.ActualModel, err)
		}

		d.resampler = r
	}

	return d, nil
}

// Name implements dialect.Dialect.
func (d *Dialect) Name() string {
	return dialectName
}

// Start implements dialect.Dialect. The init session.update is only sent once the upstream
// session.created has been read, so nothing is sent here and the gate stays closed.
func (d *Dialect) Start() ([][]byte, bool) {
	return nil, true
}

// CloseUpstream implements dialect.Dialect: Bailian sessions end with session.finish, except
// on families that reject the event (qwen3.8), which only get the close frame.
func (d *Dialect) CloseUpstream() [][]byte {
	if !d.caps.SessionFinish {
		return nil
	}

	return [][]byte{[]byte(`{"event_id":"axh_finish","type":"session.finish"}`)}
}

// Capabilities returns the capability profile in use.
func (d *Dialect) Capabilities() Capabilities {
	return d.caps
}

// GateTimeout bounds the init handshake (session.created → init update → ack, including the
// retry); the session closes with 1011 when it expires.
func (d *Dialect) GateTimeout() time.Duration {
	return 5 * time.Second
}

// CloseGrace is how long the session waits for Bailian's close handshake after the goodbye
// before hard-closing; Bailian can take several seconds, which would hold the channel slot.
func (d *Dialect) CloseGrace() time.Duration {
	return 2 * time.Second
}

// latestConfig returns the config the next client update must be merged into: the tail of
// the pending queue, or the committed state.
func (d *Dialect) latestConfig() sessionConfig {
	if n := len(d.pending); n > 0 {
		return d.pending[n-1].cfg
	}

	return d.committed
}

// nextSeq returns a monotonically increasing counter for adapter-generated ids.
func (d *Dialect) nextSeq() uint64 {
	d.seq++

	return d.seq
}

// upstreamID builds an upstream event id with the given prefix.
func (d *Dialect) upstreamID(prefix string) string {
	return prefix + strconv.FormatUint(d.nextSeq(), 10)
}

// synth marshals a synthesised server event with a fresh event_id.
func (d *Dialect) synth(event map[string]any) []byte {
	event["event_id"] = d.ids.Next()

	b, err := json.Marshal(event)
	if err != nil {
		log.Warn(d.ctx, "bailian: marshal synthesized event", log.Cause(err))

		return nil
	}

	return b
}

// clientError builds a GA invalid_request_error for the client.
func (d *Dialect) clientError(code, message, param, clientEventID string) []byte {
	return dialect.ErrorEvent(d.ids, "invalid_request_error", code, message, param, clientEventID)
}

// rejectionOutput turns a rejection into the client-bound error (and optional close).
func (d *Dialect) rejectionOutput(rej *rejection, clientEventID string) dialect.Output {
	out := dialect.Output{
		ToClient: [][]byte{dialect.ErrorEvent(d.ids, rej.errType, rej.code, rej.message, rej.param, clientEventID)},
	}

	if rej.closeCode != 0 {
		out.Close = &dialect.CloseRequest{Code: rej.closeCode, Reason: rej.closeReason}
	}

	return out
}

// initUpdate builds the adapter's init session.update. Both variants carry the effective
// voice (seeded from session.created and already mapped to a usable family voice), because
// the upstream default is not necessarily usable. The minimal variant (retry) drops every
// audio format the upstream already defaults to; the input format stays whenever client
// audio is forwarded unresampled, since the upstream default is 16 kHz.
func (d *Dialect) initUpdate(eventID string, minimal bool) []byte {
	s := map[string]any{
		"modalities": []string{"text", "audio"},
		"voice":      d.committed.voice,
	}

	if d.committed.transcription {
		s["input_audio_transcription"] = map[string]any{"model": d.caps.TranscriptionModel}
	} else {
		s["input_audio_transcription"] = nil
	}

	switch {
	case d.caps.NestedAudio:
		output := map[string]any{"voice": d.committed.voice}
		audioObj := map[string]any{"output": output}

		switch {
		case !minimal:
			audioObj["input"] = map[string]any{"format": pcmFormat(d.caps.InputRate)}

			if d.caps.OutputFormatSettable {
				output["format"] = pcmFormat(clientRate)
			}
		case d.caps.InputRate != 16000:
			audioObj["input"] = map[string]any{"format": pcmFormat(d.caps.InputRate)}
		}

		s["audio"] = audioObj
	case !minimal:
		s["input_audio_format"] = "pcm"
		s["output_audio_format"] = "pcm"
	}

	b, _ := json.Marshal(map[string]any{
		"event_id": eventID,
		"type":     "session.update",
		"session":  s,
	})

	return b
}

// resetAudio clears the resampler and the per-buffer byte counter.
func (d *Dialect) resetAudio() {
	if d.resampler != nil {
		d.resampler.Reset()
	}

	d.appendedBytes = 0
	d.bufferHasAudio = false
}

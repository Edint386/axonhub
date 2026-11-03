package bailian

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

const (
	// asrDialectName is reported by Name() for GA transcription sessions on Qwen-ASR.
	asrDialectName = "bailian_asr"

	// asrInputRate is the PCM rate sent upstream. Bailian ASR also accepts 24 kHz, but the
	// only 24 kHz live run stalled the paced send; 16 kHz is the documented and proven rate.
	asrInputRate = 16000

	// asrInitEventID tags the one session.update the adapter sends upstream.
	asrInitEventID = "axh_asr_init"

	// asrSilenceMin and asrSilenceMax bound turn_detection.silence_duration_ms on Bailian.
	asrSilenceMin = 200
	asrSilenceMax = 6000
)

// ASROptions configures a GA transcription session relayed to a Qwen-ASR realtime model.
type ASROptions struct {
	// RequestedModel is the model the client selected; it is echoed as the transcription model
	// until the client names one in session.update.
	RequestedModel string
	// ActualModel is the upstream ASR model; transcription usage is billed under it.
	ActualModel string
	// IDs generates ids for synthesised server events; a nil value gets a fresh generator.
	IDs *dialect.EventIDs
}

// asrVAD is the server_vad configuration; nil fields keep the upstream default.
type asrVAD struct {
	threshold         *float64
	prefixPaddingMs   *int64
	silenceDurationMs *int64
}

// asrConfig is the effective transcription configuration the client sees.
type asrConfig struct {
	model    string
	language string
	manual   bool
	vad      asrVAD
}

// ASRDialect relays a GA transcription session (?intent=transcription, session.type
// "transcription") to Qwen-ASR realtime.
//
// Bailian accepts exactly one session.update per session: a second one, even before any
// audio, closes the socket with 1007 (live probe 2026-11-03). The adapter therefore answers
// client session.update events itself and sends a single merged update upstream right before
// the first audio; once that update went out, configuration changes are refused with an error
// event and the session stays open. Client audio (24 kHz) is resampled to 16 kHz.
type ASRDialect struct {
	mu sync.Mutex

	opts ASROptions
	ids  *dialect.EventIDs
	ctx  context.Context

	cfg            asrConfig
	upstreamDefVAD asrVAD
	sessionID      string
	initSent       bool
	lastUpdateID   string
	// awaitingAck is set from the upstream update until its session.updated; the session gate
	// holds client frames meanwhile and pending holds the frame that triggered the update.
	awaitingAck bool
	pending     [][]byte

	resampler *audio.Resampler
	meter     usageMeter

	itemSeq       int
	current       string
	lastCommitted string
	// speaking is the item of the utterance server_vad is currently in (speech_started until
	// its committed), or "".
	speaking string
	itemPrev map[string]string
	trackers map[string]*hypothesisTracker
}

var _ dialect.Dialect = (*ASRDialect)(nil)

// NewASR returns the GA transcription dialect for a Qwen-ASR realtime model.
func NewASR(opts ASROptions) (*ASRDialect, error) {
	ids := opts.IDs
	if ids == nil {
		ids = &dialect.EventIDs{}
	}

	r, err := audio.NewResampler(clientRate, asrInputRate)
	if err != nil {
		return nil, fmt.Errorf("bailian asr: resampler: %w", err)
	}

	return &ASRDialect{
		opts:      opts,
		ids:       ids,
		ctx:       context.Background(),
		cfg:       asrConfig{model: opts.RequestedModel},
		resampler: r,
		itemPrev:  map[string]string{},
		trackers:  map[string]*hypothesisTracker{},
	}, nil
}

// Name implements dialect.Dialect.
func (d *ASRDialect) Name() string { return asrDialectName }

// Start implements dialect.Dialect: nothing is sent until the first audio, so no gate.
func (d *ASRDialect) Start() ([][]byte, bool) { return nil, false }

// CloseUpstream implements dialect.Dialect: session.finish makes Bailian flush the utterance in
// progress (its transcription.completed carries the usage) and answer session.finished.
func (d *ASRDialect) CloseUpstream() [][]byte {
	return [][]byte{[]byte(`{"type":"session.finish"}`)}
}

// FinishDrain tells the session to wait for session.finished after CloseUpstream, so the last
// utterance's usage is recorded.
func (d *ASRDialect) FinishDrain() bool { return true }

// CloseGrace bounds the close handshake; Bailian echoes a client close frame promptly.
func (d *ASRDialect) CloseGrace() time.Duration { return 2 * time.Second }

// GateTimeout bounds the wait for the acknowledgement of the upstream session.update.
func (d *ASRDialect) GateTimeout() time.Duration { return 5 * time.Second }

func (d *ASRDialect) synth(event map[string]any) []byte {
	event["event_id"] = d.ids.Next()

	b, err := json.Marshal(event)
	if err != nil {
		log.Warn(d.ctx, "bailian asr: marshal synthesized event", log.Cause(err))
		return nil
	}

	return b
}

func (d *ASRDialect) clientError(code, message, param, clientEventID string) []byte {
	return dialect.ErrorEvent(d.ids, "invalid_request_error", code, message, param, clientEventID)
}

// ClientFrame implements dialect.Dialect for frames received from the GA client.
func (d *ASRDialect) ClientFrame(frame []byte) (dialect.Output, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ev := gjson.ParseBytes(frame)
	clientEventID := ev.Get("event_id").String()

	switch typ := ev.Get("type").String(); typ {
	case "session.update":
		return d.clientSessionUpdate(ev, clientEventID), nil
	case "input_audio_buffer.append":
		return d.clientAppend(ev, clientEventID), nil
	case "input_audio_buffer.commit":
		if !d.cfg.manual && d.speaking == "" {
			// server_vad already committed everything it heard (or heard nothing yet): a
			// forwarded commit would have nothing to cut. GA answers an empty commit with an
			// error.
			return dialect.Output{ToClient: [][]byte{d.clientError("input_audio_buffer_commit_empty",
				"No speech is in progress; server_vad commits utterances by itself.", "", clientEventID)}}, nil
		}

		// Manual mode, or server_vad mid-utterance: Bailian ends the utterance now (a client
		// streaming a file needs this for a last sentence without trailing silence). The
		// committed Bailian repeats for that item when its speech later stops is dropped.
		return d.withInit(frame), nil
	case "input_audio_buffer.clear":
		// Bailian rejects input_audio_buffer.clear (invalid_value) and keeps the audio.
		return dialect.Output{ToClient: [][]byte{d.clientError("unsupported_on_upstream",
			"input_audio_buffer.clear is not supported by this transcription upstream; the buffered audio is still transcribed.",
			"type", clientEventID)}}, nil
	case "conversation.item.create", "conversation.item.delete", "conversation.item.retrieve", "conversation.item.truncate",
		"response.create", "response.cancel", "output_audio_buffer.clear":
		return dialect.Output{ToClient: [][]byte{d.clientError("unsupported_in_transcription_session",
			fmt.Sprintf("%s is not supported in a transcription session.", typ), "type", clientEventID)}}, nil
	default:
		return dialect.Output{ToUpstream: [][]byte{frame}}, nil
	}
}

// withInit forwards frame, preceded by the single upstream session.update when it has not been
// sent yet. In that case frame is held back and the session gate closed until the update is
// acknowledged: whether Bailian accepts audio right behind its configuration is unverified,
// and a configuration error after audio started would end the session.
func (d *ASRDialect) withInit(frame []byte) dialect.Output {
	init := d.initFrames()
	if len(init) == 0 {
		if frame == nil {
			return dialect.Output{}
		}

		return dialect.Output{ToUpstream: [][]byte{frame}}
	}

	d.awaitingAck = true
	if frame != nil {
		d.pending = append(d.pending, frame)
	}

	return dialect.Output{ToUpstream: init, CloseGate: true}
}

// initFrames returns the single upstream session.update when it has not been sent yet.
func (d *ASRDialect) initFrames() [][]byte {
	if d.initSent {
		return nil
	}

	d.initSent = true

	s := map[string]any{
		"input_audio_format": "pcm",
		"sample_rate":        asrInputRate,
	}

	if d.cfg.manual {
		s["turn_detection"] = nil
	} else {
		td := map[string]any{"type": "server_vad"}
		if d.cfg.vad.threshold != nil {
			td["threshold"] = *d.cfg.vad.threshold
		}

		if d.cfg.vad.prefixPaddingMs != nil {
			td["prefix_padding_ms"] = *d.cfg.vad.prefixPaddingMs
		}

		if d.cfg.vad.silenceDurationMs != nil {
			td["silence_duration_ms"] = *d.cfg.vad.silenceDurationMs
		}

		s["turn_detection"] = td
	}

	if d.cfg.language != "" {
		s["input_audio_transcription"] = map[string]any{"language": d.cfg.language}
	}

	b, _ := json.Marshal(map[string]any{"event_id": asrInitEventID, "type": "session.update", "session": s})

	return [][]byte{b}
}

// clientAppend resamples one append to the upstream rate, sending the upstream session
// configuration first when this is the first audio.
func (d *ASRDialect) clientAppend(ev gjson.Result, clientEventID string) dialect.Output {
	// A bad or empty payload is answered with an error event; the upstream is not touched.
	pcm, err := base64.StdEncoding.DecodeString(ev.Get("audio").String())
	if err != nil || len(pcm) == 0 {
		return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", "Invalid base64 audio.", "audio", clientEventID)}}
	}

	var b []byte

	if resampled := d.resampler.Process(pcm); len(resampled) > 0 {
		event := map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(resampled)}
		if clientEventID != "" {
			event["event_id"] = clientEventID
		}

		b, _ = json.Marshal(event)
	}

	return d.withInit(b)
}

// clientSessionUpdate merges a GA transcription session.update into the effective config and
// answers it locally.
func (d *ASRDialect) clientSessionUpdate(ev gjson.Result, clientEventID string) dialect.Output {
	if d.initSent {
		return dialect.Output{ToClient: [][]byte{d.clientError("unsupported_on_upstream",
			"This transcription upstream accepts its configuration only before the first audio; reconnect to change it.",
			"session", clientEventID)}}
	}

	cfg, rej := mergeASRUpdate(d.cfg, ev.Get("session"))
	if rej != nil {
		out := dialect.Output{ToClient: [][]byte{dialect.ErrorEvent(d.ids, rej.errType, rej.code, rej.message, rej.param, clientEventID)}}
		if rej.closeCode != 0 {
			out.Close = &dialect.CloseRequest{Code: rej.closeCode, Reason: rej.closeReason}
		}

		return out
	}

	d.cfg = cfg
	d.lastUpdateID = clientEventID

	return dialect.Output{ToClient: [][]byte{d.renderSessionEvent("session.updated")}}
}

// mergeASRUpdate applies the GA transcription fields of session to cfg. Fields Bailian has no
// counterpart for (noise_reduction, include, prompt, keywords, delay, idle_timeout_ms,
// create_response, interrupt_response) are dropped.
func mergeASRUpdate(cfg asrConfig, session gjson.Result) (asrConfig, *rejection) {
	if t := session.Get("type"); t.Exists() && t.String() != "transcription" {
		return cfg, &rejection{
			errType: "invalid_request_error", code: "unsupported_session_type", param: "session.type",
			message: fmt.Sprintf("This model only supports transcription sessions; session.type %q is not available.", t.String()),
		}
	}

	in := session.Get("audio.input")

	if format := in.Get("format"); format.Exists() && format.Type != gjson.Null {
		switch typ := format.Get("type").String(); typ {
		case "", "audio/pcm":
			if rate := format.Get("rate"); rate.Exists() && rate.Int() != clientRate {
				return cfg, &rejection{
					errType: "invalid_request_error", code: "invalid_value", param: "session.audio.input.format.rate",
					message: "Only 24000 Hz PCM input is supported.",
				}
			}
		case "audio/pcmu", "audio/pcma":
			return cfg, &rejection{
				errType: "invalid_request_error", code: "unsupported_on_upstream", param: "session.audio.input.format.type",
				message:   fmt.Sprintf("%s audio is not supported by this transcription upstream; send 24 kHz audio/pcm.", typ),
				closeCode: 1003, closeReason: "G.711 audio is not supported",
			}
		default:
			return cfg, &rejection{
				errType: "invalid_request_error", code: "invalid_value", param: "session.audio.input.format.type",
				message: fmt.Sprintf("Unsupported input audio format %q.", typ),
			}
		}
	}

	if tr := in.Get("transcription"); tr.Exists() && tr.Type != gjson.Null {
		if m := tr.Get("model"); m.Type == gjson.String && m.String() != "" {
			cfg.model = m.String()
		}

		if lang := tr.Get("language"); lang.Exists() {
			cfg.language = asrLanguage(lang.String())
		}

		if langs := tr.Get("languages"); langs.IsArray() {
			if list := langs.Array(); len(list) == 1 {
				cfg.language = asrLanguage(list[0].String())
			} else {
				cfg.language = ""
			}
		}
	}

	if td := in.Get("turn_detection"); td.Exists() {
		next, rej := mergeASRTurnDetection(td)
		if rej != nil {
			return cfg, rej
		}

		cfg.manual = td.Type == gjson.Null
		cfg.vad = next
	}

	return cfg, nil
}

func mergeASRTurnDetection(td gjson.Result) (asrVAD, *rejection) {
	var vad asrVAD

	if td.Type == gjson.Null {
		return vad, nil
	}

	switch typ := td.Get("type").String(); typ {
	case "", "server_vad":
		if v := td.Get("threshold"); v.Type == gjson.Number {
			t := min(max(v.Float(), -1), 1)
			vad.threshold = &t
		}

		if v := td.Get("prefix_padding_ms"); v.Type == gjson.Number {
			p := max(v.Int(), 0)
			vad.prefixPaddingMs = &p
		}

		if v := td.Get("silence_duration_ms"); v.Type == gjson.Number {
			s := min(max(v.Int(), asrSilenceMin), asrSilenceMax)
			vad.silenceDurationMs = &s
		}
	case "semantic_vad":
		// Bailian has no semantic VAD; eagerness becomes a silence window instead.
		var silence int64

		switch td.Get("eagerness").String() {
		case "high":
			silence = 400
		case "low":
			silence = 1200
		default:
			silence = 800
		}

		vad.silenceDurationMs = &silence
	default:
		return vad, &rejection{
			errType: "invalid_request_error", code: "invalid_value", param: "session.audio.input.turn_detection.type",
			message: fmt.Sprintf("Unsupported turn_detection type %q.", typ),
		}
	}

	return vad, nil
}

// renderSessionEvent renders the effective config as a GA transcription session event.
func (d *ASRDialect) renderSessionEvent(typ string) []byte {
	var turnDetection any

	if !d.cfg.manual {
		td := map[string]any{"type": "server_vad"}

		if t := firstFloat(d.cfg.vad.threshold, d.upstreamDefVAD.threshold); t != nil {
			td["threshold"] = *t
		}

		if p := firstInt(d.cfg.vad.prefixPaddingMs, d.upstreamDefVAD.prefixPaddingMs); p != nil {
			td["prefix_padding_ms"] = *p
		}

		if s := firstInt(d.cfg.vad.silenceDurationMs, d.upstreamDefVAD.silenceDurationMs); s != nil {
			td["silence_duration_ms"] = *s
		}

		turnDetection = td
	}

	var language any
	if d.cfg.language != "" {
		language = d.cfg.language
	}

	return d.synth(map[string]any{
		"type": typ,
		"session": map[string]any{
			"type":    "transcription",
			"id":      d.sessionID,
			"object":  "realtime.transcription_session",
			"include": nil,
			"audio": map[string]any{
				"input": map[string]any{
					"format":          map[string]any{"type": "audio/pcm", "rate": clientRate},
					"transcription":   map[string]any{"model": d.cfg.model, "language": language},
					"noise_reduction": nil,
					"turn_detection":  turnDetection,
				},
			},
		},
	})
}

func firstFloat(vals ...*float64) *float64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}

	return nil
}

func firstInt(vals ...*int64) *int64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}

	return nil
}

// UpstreamFrame implements dialect.Dialect for frames received from Bailian ASR.
func (d *ASRDialect) UpstreamFrame(frame []byte) (dialect.Output, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ev := gjson.ParseBytes(frame)

	switch ev.Get("type").String() {
	case "session.created":
		d.upstreamSessionCreated(ev)
		return dialect.Output{ToClient: [][]byte{d.renderSessionEvent("session.created")}}, nil
	case "session.updated":
		// The acknowledgement of the adapter's single update (the client already got its own
		// session.updated for every update it sent): release the held frame and the gate.
		if !d.awaitingAck {
			return dialect.Output{}, nil
		}

		d.awaitingAck = false
		out := dialect.Output{ToUpstream: d.pending, OpenGate: true}
		d.pending = nil

		return out, nil
	case "session.finished":
		return dialect.Output{UpstreamFinished: true}, nil
	case "error":
		return dialect.Output{ToClient: [][]byte{d.upstreamError(frame, ev)}}, nil
	case "input_audio_buffer.speech_started":
		d.speaking = d.itemID(ev, "item_id")
		return dialect.Output{ToClient: [][]byte{frame}}, nil
	case "conversation.item.created":
		return dialect.Output{ToClient: [][]byte{d.itemAdded(ev)}}, nil
	case "conversation.item.input_audio_transcription.text", "conversation.item.input_audio_transcription.delta":
		return d.hypothesis(ev), nil
	case "input_audio_buffer.committed":
		return d.committed(ev), nil
	case "conversation.item.input_audio_transcription.completed":
		return d.completed(ev)
	case "conversation.item.input_audio_transcription.failed":
		return d.failed(frame, ev), nil
	default:
		return dialect.Output{ToClient: [][]byte{frame}}, nil
	}
}

// upstreamSessionCreated seeds the session id and Bailian's VAD defaults (shown to the client
// while the client has not configured its own) from the upstream session.created.
func (d *ASRDialect) upstreamSessionCreated(ev gjson.Result) {
	s := ev.Get("session")
	d.sessionID = s.Get("id").String()

	td := s.Get("turn_detection")
	if !td.Exists() || td.Type == gjson.Null {
		if !d.initSent {
			d.cfg.manual = true
		}

		return
	}

	if v := td.Get("threshold"); v.Type == gjson.Number {
		t := v.Float()
		d.upstreamDefVAD.threshold = &t
	}

	if v := td.Get("prefix_padding_ms"); v.Type == gjson.Number {
		p := v.Int()
		d.upstreamDefVAD.prefixPaddingMs = &p
	}

	if v := td.Get("silence_duration_ms"); v.Type == gjson.Number {
		s := v.Int()
		d.upstreamDefVAD.silenceDurationMs = &s
	}
}

// upstreamError adds the GA error type Bailian omits and points errors caused by the adapter's
// merged session.update at the client's last session.update.
func (d *ASRDialect) upstreamError(frame []byte, ev gjson.Result) []byte {
	out := gaError(frame, "")

	if ev.Get("error.event_id").String() == asrInitEventID && d.lastUpdateID != "" {
		if rewritten, err := sjson.SetBytes(out, "error.event_id", d.lastUpdateID); err == nil {
			out = rewritten
		}
	}

	return out
}

// itemID returns the event's item id, or the current item's id for the id-less events of
// manual mode, creating one when none exists.
func (d *ASRDialect) itemID(ev gjson.Result, path string) string {
	if id := ev.Get(path).String(); id != "" {
		return id
	}

	if d.current == "" {
		d.itemSeq++
		d.current = fmt.Sprintf("item_axh_%d", d.itemSeq)
	}

	return d.current
}

// itemAdded renders Bailian's conversation.item.created (role "assistant", no ids in manual
// mode) as the GA conversation.item.added of the user audio item.
func (d *ASRDialect) itemAdded(ev gjson.Result) []byte {
	if ev.Get("item.id").String() == "" {
		// A new manual-mode utterance gets a fresh id.
		d.current = ""
	}

	id := d.itemID(ev, "item.id")
	d.current = id

	prev := d.lastCommitted
	d.itemPrev[id] = prev

	return d.synth(map[string]any{
		"type":             "conversation.item.added",
		"previous_item_id": nullable(prev),
		"item":             userAudioItem(id, "in_progress", nil),
	})
}

func userAudioItem(id, status string, transcript any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "realtime.item",
		"type":    "message",
		"status":  status,
		"role":    "user",
		"content": []map[string]any{{"type": "input_audio", "transcript": transcript}},
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}

	return s
}

// hypothesis turns a Bailian transcription.text event into a GA delta.
func (d *ASRDialect) hypothesis(ev gjson.Result) dialect.Output {
	id := d.itemID(ev, "item_id")

	tr := d.trackers[id]
	if tr == nil {
		tr = &hypothesisTracker{}
		d.trackers[id] = tr
	}

	delta := tr.next(ev.Get("text").String() + ev.Get("stash").String())
	if delta == "" {
		return dialect.Output{}
	}

	return dialect.Output{ToClient: [][]byte{d.synth(map[string]any{
		"type":          "conversation.item.input_audio_transcription.delta",
		"item_id":       id,
		"content_index": ev.Get("content_index").Int(),
		"delta":         delta,
	})}}
}

// committed adds the ids Bailian leaves out and drops a repeated commit of the same item.
func (d *ASRDialect) committed(ev gjson.Result) dialect.Output {
	id := d.itemID(ev, "item_id")
	if d.speaking == id {
		d.speaking = ""
	}

	if id == d.lastCommitted {
		return dialect.Output{}
	}

	prev := d.lastCommitted
	d.lastCommitted = id

	return dialect.Output{ToClient: [][]byte{d.synth(map[string]any{
		"type":             "input_audio_buffer.committed",
		"previous_item_id": nullable(prev),
		"item_id":          id,
	})}}
}

// completed emits the catch-up delta, the GA completion with per-utterance usage and the
// finished item, and records the utterance's usage.
func (d *ASRDialect) completed(ev gjson.Result) (dialect.Output, error) {
	id := d.itemID(ev, "item_id")
	transcript := ev.Get("transcript").String()
	contentIndex := ev.Get("content_index").Int()

	var toClient [][]byte

	tr := d.trackers[id]
	if tr == nil {
		tr = &hypothesisTracker{}
	}

	if catch := tr.complete(transcript); catch != "" {
		toClient = append(toClient, d.synth(map[string]any{
			"type":          "conversation.item.input_audio_transcription.delta",
			"item_id":       id,
			"content_index": contentIndex,
			"delta":         catch,
		}))
	}

	delete(d.trackers, id)

	usage := d.meter.next(parseASRUsage(ev.Get("usage")))

	done := map[string]any{
		"type":          "conversation.item.input_audio_transcription.completed",
		"item_id":       id,
		"content_index": contentIndex,
		"transcript":    transcript,
		"usage":         usage.gaTokens(),
	}

	if lang := ev.Get("language").String(); lang != "" {
		done["languages"] = []map[string]any{{"code": lang}}
	}

	completedFrame := d.synth(done)
	toClient = append(toClient, completedFrame, d.synth(map[string]any{
		"type":             "conversation.item.done",
		"previous_item_id": nullable(d.itemPrev[id]),
		"item":             userAudioItem(id, "completed", transcript),
	}))

	delete(d.itemPrev, id)

	if d.current == id {
		d.current = ""
	}

	return dialect.Output{
		ToClient: toClient,
		Usage:    []dialect.UsageRecord{usage.record(id, d.opts.ActualModel, completedFrame)},
	}, nil
}

// failed stamps the item id and the GA error type on a transcription failure.
func (d *ASRDialect) failed(frame []byte, ev gjson.Result) dialect.Output {
	out := frame

	if ev.Get("item_id").String() == "" {
		if stamped, err := sjson.SetBytes(out, "item_id", d.itemID(ev, "item_id")); err == nil {
			out = stamped
		}
	}

	if ev.Get("error.type").String() == "" {
		if typed, err := sjson.SetBytes(out, "error.type", "transcription_error"); err == nil {
			out = typed
		}
	}

	return dialect.Output{ToClient: [][]byte{out}}
}

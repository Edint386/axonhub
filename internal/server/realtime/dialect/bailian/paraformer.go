package bailian

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

const (
	paraformerDialectName = "bailian_paraformer"

	paraformerRateV2 = 16000
	paraformerRate8k = 8000

	paraformerSilenceMin = 200
	paraformerSilenceMax = 6000
	paraformerSilenceDef = 1300
)

// paraformerAdmission matches the realtime Paraformer models in scope.
var paraformerAdmission = regexp.MustCompile(`^paraformer-realtime-(8k-)?v2`)

// SupportedParaformer reports whether model is a Paraformer realtime model this dialect relays.
func SupportedParaformer(model string) bool {
	return paraformerAdmission.MatchString(strings.ToLower(strings.TrimSpace(model)))
}

// SupportedBailianTranscription reports whether model is any Bailian transcription realtime
// model (Qwen-ASR or Paraformer). Legacy beta passthrough remains Qwen-ASR only.
func SupportedBailianTranscription(model string) bool {
	return SupportedASR(model) || SupportedParaformer(model)
}

// ParaformerWSURL rewrites a Bailian realtime WebSocket URL to the inference endpoint Paraformer
// speaks: path /realtime → /inference, and the model query is dropped (model goes in run-task).
func ParaformerWSURL(wsURL string) string {
	u := wsURL
	if i := strings.Index(u, "?"); i >= 0 {
		base, query := u[:i], u[i+1:]
		u = base
		kept := make([]string, 0)
		for part := range strings.SplitSeq(query, "&") {
			if part == "" || strings.HasPrefix(part, "model=") {
				continue
			}
			kept = append(kept, part)
		}
		if len(kept) > 0 {
			u = u + "?" + strings.Join(kept, "&")
		}
	}
	switch {
	case strings.HasSuffix(u, "/realtime"):
		return strings.TrimSuffix(u, "/realtime") + "/inference"
	case strings.Contains(u, "/realtime?"):
		return strings.Replace(u, "/realtime?", "/inference?", 1)
	default:
		return u
	}
}

// ParaformerOptions configures a GA transcription session relayed to Paraformer realtime.
type ParaformerOptions struct {
	RequestedModel string
	ActualModel    string
	IDs            *dialect.EventIDs
}

type paraformerInputFormat int

const (
	paraformerPCM24 paraformerInputFormat = iota
	paraformerMuLaw
	paraformerALaw
)

type paraformerConfig struct {
	model             string
	language          string
	manual            bool
	semanticVAD       bool
	eagerness         string // GA semantic_vad eagerness; empty unless semanticVAD
	silenceDurationMs int64
	inputFormat       paraformerInputFormat
}

// ParaformerDialect relays a GA transcription session to Bailian Paraformer realtime
// (run-task / binary PCM / finish-task on /api-ws/v1/inference).
type ParaformerDialect struct {
	mu sync.Mutex

	opts ParaformerOptions
	ids  *dialect.EventIDs
	ctx  context.Context

	cfg       paraformerConfig
	sessionID string
	taskID    string
	inputRate int

	resampler *audio.Resampler
	g711Up    *audio.Resampler // 8 kHz PCM → model rate when needed

	started  bool // run-task sent
	awaiting bool // waiting for task-started
	pending  [][]byte
	finished bool

	lastDuration int64 // cumulative usage.duration from last final

	itemSeq       int
	current       string
	lastCommitted string
	itemPrev      map[string]string
	trackers      map[string]*hypothesisTracker
	sentenceOpen  bool
}

var _ dialect.Dialect = (*ParaformerDialect)(nil)

// NewParaformer returns the GA transcription dialect for a Paraformer realtime model.
func NewParaformer(opts ParaformerOptions) (*ParaformerDialect, error) {
	ids := opts.IDs
	if ids == nil {
		ids = &dialect.EventIDs{}
	}

	rate := paraformerRateV2
	if strings.Contains(strings.ToLower(opts.ActualModel), "8k") {
		rate = paraformerRate8k
	}

	rs, err := audio.NewResampler(clientRate, rate)
	if err != nil {
		return nil, fmt.Errorf("bailian paraformer: resampler: %w", err)
	}

	var g711Up *audio.Resampler
	if rate != audio.G711Rate {
		g711Up, err = audio.NewResampler(audio.G711Rate, rate)
		if err != nil {
			return nil, fmt.Errorf("bailian paraformer: g711 resampler: %w", err)
		}
	}

	return &ParaformerDialect{
		opts:      opts,
		ids:       ids,
		ctx:       context.Background(),
		cfg:       paraformerConfig{model: opts.RequestedModel, silenceDurationMs: paraformerSilenceDef},
		sessionID: "sess_axh_" + uuid.NewString(),
		taskID:    uuid.NewString(),
		inputRate: rate,
		resampler: rs,
		g711Up:    g711Up,
		itemPrev:  map[string]string{},
		trackers:  map[string]*hypothesisTracker{},
	}, nil
}

func (d *ParaformerDialect) Name() string { return paraformerDialectName }

// Greeting synthesises the GA session.created Paraformer never sends on connect.
func (d *ParaformerDialect) Greeting() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return [][]byte{d.renderSessionEvent("session.created")}
}

func (d *ParaformerDialect) Start() ([][]byte, bool) { return nil, false }

func (d *ParaformerDialect) CloseUpstream() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started || d.finished {
		return nil
	}
	return [][]byte{d.finishTaskFrame()}
}

func (d *ParaformerDialect) FinishDrain() bool { return true }

func (d *ParaformerDialect) CloseGrace() time.Duration { return 2 * time.Second }

func (d *ParaformerDialect) GateTimeout() time.Duration { return 5 * time.Second }

func (d *ParaformerDialect) synth(event map[string]any) []byte {
	event["event_id"] = d.ids.Next()
	b, err := json.Marshal(event)
	if err != nil {
		log.Warn(d.ctx, "bailian paraformer: marshal synthesized event", log.Cause(err))
		return nil
	}
	return b
}

func (d *ParaformerDialect) clientError(code, message, param, clientEventID string) []byte {
	return dialect.ErrorEvent(d.ids, "invalid_request_error", code, message, param, clientEventID)
}

func (d *ParaformerDialect) ClientFrame(frame []byte) (dialect.Output, error) {
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
		// Paraformer has no commit; VAD / finish-task ends utterances. Empty commits in
		// server_vad with nothing open match GA's empty-commit error.
		if !d.cfg.manual && !d.sentenceOpen {
			return dialect.Output{ToClient: [][]byte{d.clientError("input_audio_buffer_commit_empty",
				"No speech is in progress; server_vad commits utterances by itself.", "", clientEventID)}}, nil
		}
		return dialect.Output{}, nil
	case "input_audio_buffer.clear":
		return dialect.Output{ToClient: [][]byte{d.clientError("unsupported_on_upstream",
			"input_audio_buffer.clear is not supported by this transcription upstream; the buffered audio is still transcribed.",
			"type", clientEventID)}}, nil
	case "conversation.item.create", "conversation.item.delete", "conversation.item.retrieve", "conversation.item.truncate",
		"response.create", "response.cancel", "output_audio_buffer.clear":
		return dialect.Output{ToClient: [][]byte{d.clientError("unsupported_in_transcription_session",
			fmt.Sprintf("%s is not supported in a transcription session.", typ), "type", clientEventID)}}, nil
	default:
		return dialect.Output{}, nil
	}
}

func (d *ParaformerDialect) clientSessionUpdate(ev gjson.Result, clientEventID string) dialect.Output {
	if d.started {
		return dialect.Output{ToClient: [][]byte{d.clientError("unsupported_on_upstream",
			"This transcription upstream accepts its configuration only before the first audio; reconnect to change it.",
			"session", clientEventID)}}
	}

	cfg, rej := mergeParaformerUpdate(d.cfg, ev.Get("session"), d.inputRate)
	if rej != nil {
		out := dialect.Output{ToClient: [][]byte{dialect.ErrorEvent(d.ids, rej.errType, rej.code, rej.message, rej.param, clientEventID)}}
		if rej.closeCode != 0 {
			out.Close = &dialect.CloseRequest{Code: rej.closeCode, Reason: rej.closeReason}
		}
		return out
	}
	d.cfg = cfg
	return dialect.Output{ToClient: [][]byte{d.renderSessionEvent("session.updated")}}
}

func mergeParaformerUpdate(cfg paraformerConfig, session gjson.Result, modelRate int) (paraformerConfig, *rejection) {
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
					message: "Only 24000 Hz PCM input is supported (G.711 may use audio/pcmu or audio/pcma).",
				}
			}
			cfg.inputFormat = paraformerPCM24
		case "audio/pcmu":
			cfg.inputFormat = paraformerMuLaw
		case "audio/pcma":
			cfg.inputFormat = paraformerALaw
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
			cfg.language = paraformerLanguage(lang.String())
		}
		if langs := tr.Get("languages"); langs.IsArray() {
			if list := langs.Array(); len(list) == 1 {
				cfg.language = paraformerLanguage(list[0].String())
			} else {
				cfg.language = ""
			}
		}
	}

	if td := in.Get("turn_detection"); td.Exists() {
		if td.Type == gjson.Null {
			cfg.manual = true
			cfg.semanticVAD = false
			cfg.eagerness = ""
		} else {
			switch typ := td.Get("type").String(); typ {
			case "", "server_vad":
				cfg.manual = false
				cfg.semanticVAD = false
				cfg.eagerness = ""
				if v := td.Get("silence_duration_ms"); v.Type == gjson.Number {
					cfg.silenceDurationMs = min(max(v.Int(), paraformerSilenceMin), paraformerSilenceMax)
				}
			case "semantic_vad":
				// Enable Bailian semantic punctuation and fold eagerness into max_sentence_silence
				// the same way Qwen-ASR does (high=400, medium/default=800, low=1200).
				cfg.manual = false
				cfg.semanticVAD = true
				eagerness := td.Get("eagerness").String()
				if eagerness == "" {
					eagerness = "medium"
				}
				cfg.eagerness = eagerness
				var silence int64
				switch eagerness {
				case "high":
					silence = 400
				case "low":
					silence = 1200
				default:
					silence = 800
				}
				cfg.silenceDurationMs = silence
			default:
				return cfg, &rejection{
					errType: "invalid_request_error", code: "invalid_value", param: "session.audio.input.turn_detection.type",
					message: fmt.Sprintf("Unsupported turn_detection.type %q.", typ),
				}
			}
		}
	}

	_ = modelRate
	return cfg, nil
}

func paraformerLanguage(code string) string {
	// Paraformer accepts a subset of the Qwen-ASR list.
	switch c := asrLanguage(code); c {
	case "zh", "en", "ja", "yue", "ko", "de", "fr", "ru":
		return c
	default:
		return ""
	}
}

func (d *ParaformerDialect) clientAppend(ev gjson.Result, clientEventID string) dialect.Output {
	raw, err := base64.StdEncoding.DecodeString(ev.Get("audio").String())
	if err != nil || len(raw) == 0 {
		return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", "Invalid base64 audio.", "audio", clientEventID)}}
	}

	pcm := d.decodeClientAudio(raw)
	if len(pcm) == 0 {
		return dialect.Output{}
	}

	if !d.started {
		d.started = true
		d.awaiting = true
		d.pending = append(d.pending, pcm)
		return dialect.Output{ToUpstream: [][]byte{d.runTaskFrame()}, CloseGate: true}
	}

	if d.awaiting {
		d.pending = append(d.pending, pcm)
		return dialect.Output{}
	}

	return dialect.Output{ToUpstream: [][]byte{pcm}, BinaryUpstream: true}
}

func (d *ParaformerDialect) decodeClientAudio(raw []byte) []byte {
	switch d.cfg.inputFormat {
	case paraformerMuLaw:
		pcm := audio.DecodeMuLaw(raw)
		if d.g711Up != nil {
			return d.g711Up.Process(pcm)
		}
		return pcm
	case paraformerALaw:
		pcm := audio.DecodeALaw(raw)
		if d.g711Up != nil {
			return d.g711Up.Process(pcm)
		}
		return pcm
	default:
		return d.resampler.Process(raw)
	}
}

func (d *ParaformerDialect) runTaskFrame() []byte {
	params := map[string]any{
		"format":      "pcm",
		"sample_rate": d.inputRate,
	}
	if d.cfg.language != "" {
		params["language_hints"] = []string{d.cfg.language}
	}
	if !d.cfg.manual {
		params["max_sentence_silence"] = d.cfg.silenceDurationMs
		params["semantic_punctuation_enabled"] = d.cfg.semanticVAD
	} else {
		// Manual clients still need segmentation somewhere; keep VAD with a long silence so
		// finish-task is what usually ends the utterance.
		params["max_sentence_silence"] = paraformerSilenceMax
		params["semantic_punctuation_enabled"] = false
	}

	b, _ := json.Marshal(map[string]any{
		"header": map[string]any{
			"action":    "run-task",
			"task_id":   d.taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{
			"task_group": "audio",
			"task":       "asr",
			"function":   "recognition",
			"model":      d.opts.ActualModel,
			"parameters": params,
			"input":      map[string]any{},
		},
	})
	return b
}

func (d *ParaformerDialect) finishTaskFrame() []byte {
	b, _ := json.Marshal(map[string]any{
		"header": map[string]any{
			"action":    "finish-task",
			"task_id":   d.taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{"input": map[string]any{}},
	})
	return b
}

func (d *ParaformerDialect) UpstreamFrame(frame []byte) (dialect.Output, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	ev := gjson.ParseBytes(frame)
	switch ev.Get("header.event").String() {
	case "task-started":
		if !d.awaiting {
			return dialect.Output{}, nil
		}
		d.awaiting = false
		out := dialect.Output{ToUpstream: d.pending, BinaryUpstream: true, OpenGate: true}
		d.pending = nil
		return out, nil
	case "result-generated":
		return d.resultGenerated(ev), nil
	case "task-finished":
		d.finished = true
		return dialect.Output{UpstreamFinished: true}, nil
	case "task-failed":
		return d.taskFailed(ev), nil
	default:
		return dialect.Output{}, nil
	}
}

func (d *ParaformerDialect) resultGenerated(ev gjson.Result) dialect.Output {
	sent := ev.Get("payload.output.sentence")
	if !sent.Exists() || sent.Get("heartbeat").Bool() {
		return dialect.Output{}
	}

	text := sent.Get("text").String()
	final := sent.Get("sentence_end").Bool()
	// Live Paraformer often emits sentence_begin with empty text before the first hypothesis.
	if text == "" && !final {
		return dialect.Output{}
	}

	var out dialect.Output
	if !d.sentenceOpen {
		d.sentenceOpen = true
		d.itemSeq++
		d.current = fmt.Sprintf("item_axh_%d", d.itemSeq)
		prev := d.lastCommitted
		d.itemPrev[d.current] = prev
		d.trackers[d.current] = &hypothesisTracker{}
		out.ToClient = append(out.ToClient, d.synth(map[string]any{
			"type":           "input_audio_buffer.speech_started",
			"item_id":        d.current,
			"audio_start_ms": sent.Get("begin_time").Int(),
		}))
		out.ToClient = append(out.ToClient, d.synth(map[string]any{
			"type":             "conversation.item.added",
			"previous_item_id": nullable(prev),
			"item":             userAudioItem(d.current, "in_progress", nil),
		}))
	}

	id := d.current
	tr := d.trackers[id]
	if tr == nil {
		tr = &hypothesisTracker{}
		d.trackers[id] = tr
	}

	if !final {
		if delta := tr.next(text); delta != "" {
			out.ToClient = append(out.ToClient, d.synth(map[string]any{
				"type":          "conversation.item.input_audio_transcription.delta",
				"item_id":       id,
				"content_index": 0,
				"delta":         delta,
			}))
		}
		return out
	}

	if catchup := tr.complete(text); catchup != "" {
		out.ToClient = append(out.ToClient, d.synth(map[string]any{
			"type":          "conversation.item.input_audio_transcription.delta",
			"item_id":       id,
			"content_index": 0,
			"delta":         catchup,
		}))
	}

	endMs := sent.Get("end_time").Int()
	out.ToClient = append(out.ToClient, d.synth(map[string]any{
		"type":         "input_audio_buffer.speech_stopped",
		"item_id":      id,
		"audio_end_ms": endMs,
	}))
	out.ToClient = append(out.ToClient, d.synth(map[string]any{
		"type":             "input_audio_buffer.committed",
		"item_id":          id,
		"previous_item_id": nullable(d.itemPrev[id]),
	}))

	usageRaw := ev.Get("payload.usage")
	var usageRec []dialect.UsageRecord
	duration := usageRaw.Get("duration").Int()
	deltaSec := duration - d.lastDuration
	if deltaSec < 0 {
		deltaSec = duration
	}
	d.lastDuration = duration
	if deltaSec > 0 || duration > 0 {
		summary, _ := json.Marshal(map[string]any{
			"type":             "paraformer_sentence",
			"item_id":          id,
			"transcript":       text,
			"duration_seconds": deltaSec,
			"begin_time_ms":    sent.Get("begin_time").Int(),
			"end_time_ms":      endMs,
		})
		usageRec = append(usageRec, dialect.UsageRecord{
			Kind:            "transcription",
			ItemID:          id,
			Model:           d.opts.ActualModel,
			DurationSeconds: float64(deltaSec),
			Summary:         summary,
		})
	}

	completed := d.synth(map[string]any{
		"type":          "conversation.item.input_audio_transcription.completed",
		"item_id":       id,
		"content_index": 0,
		"transcript":    text,
		"usage": map[string]any{
			"type":    "duration",
			"seconds": deltaSec,
		},
	})
	out.ToClient = append(out.ToClient, completed)
	out.ToClient = append(out.ToClient, d.synth(map[string]any{
		"type":             "conversation.item.done",
		"previous_item_id": nullable(d.itemPrev[id]),
		"item":             userAudioItem(id, "completed", text),
	}))

	out.Usage = usageRec
	d.lastCommitted = id
	d.current = ""
	d.sentenceOpen = false
	delete(d.trackers, id)
	return out
}

func (d *ParaformerDialect) taskFailed(ev gjson.Result) dialect.Output {
	code := ev.Get("header.error_code").String()
	msg := ev.Get("header.error_message").String()
	if msg == "" {
		msg = "Paraformer task failed"
	}

	reason := msg
	if code != "" {
		reason = code + ": " + msg
	}

	out := dialect.Output{
		ToClient: [][]byte{dialect.ErrorEvent(d.ids, "server_error", code, msg, "", "")},
		Close:    &dialect.CloseRequest{Code: 1011, Reason: reason},
	}

	// task-failed before task-started leaves CloseGate stuck; open the gate and drop
	// pending audio so the session ends on Close instead of waiting ~5s for gate timeout.
	if d.awaiting {
		d.awaiting = false
		d.pending = nil
		out.OpenGate = true
	}

	return out
}

func (d *ParaformerDialect) renderSessionEvent(typ string) []byte {
	var language any
	if d.cfg.language != "" {
		language = d.cfg.language
	}

	format := map[string]any{"type": "audio/pcm", "rate": clientRate}
	switch d.cfg.inputFormat {
	case paraformerMuLaw:
		format = map[string]any{"type": "audio/pcmu", "rate": audio.G711Rate}
	case paraformerALaw:
		format = map[string]any{"type": "audio/pcma", "rate": audio.G711Rate}
	}

	var turnDetection any
	if d.cfg.manual {
		turnDetection = nil
	} else if d.cfg.semanticVAD {
		td := map[string]any{"type": "semantic_vad"}
		if d.cfg.eagerness != "" {
			td["eagerness"] = d.cfg.eagerness
		}
		turnDetection = td
	} else {
		turnDetection = map[string]any{
			"type":                "server_vad",
			"silence_duration_ms": d.cfg.silenceDurationMs,
		}
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
					"format":          format,
					"transcription":   map[string]any{"model": d.cfg.model, "language": language},
					"noise_reduction": nil,
					"turn_detection":  turnDetection,
				},
			},
		},
	})
}

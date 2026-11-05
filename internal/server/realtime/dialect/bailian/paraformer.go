package bailian

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
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

	// paraformerCommitMinMs is OpenAI's minimum for input_audio_buffer.commit.
	paraformerCommitMinMs = 100

	// paraformerTicksPerMs: client audio is counted in 24 kHz sample ticks, exact for PCM and G.711.
	paraformerTicksPerMs = clientRate / 1000
)

// SupportedParaformer reports whether model speaks Bailian's inference WebSocket protocol and
// this dialect relays it: Paraformer realtime, or Qwen-Audio ASR -streaming / -message (see
// inferenceProfiles).
func SupportedParaformer(model string) bool {
	_, ok := inferenceProfileFor(model)
	return ok
}

// SupportedBailianTranscription reports whether model is any Bailian transcription realtime
// model (Qwen-ASR, Paraformer or Qwen-Audio ASR). Legacy beta passthrough remains Qwen-ASR only.
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
	noiseReduction    string // GA noise_reduction type; only for profiles with vadModel
}

// ParaformerDialect relays a GA transcription session to a model on Bailian's inference
// WebSocket (run-task / binary PCM / finish-task on /api-ws/v1/inference): Paraformer realtime
// and Qwen-Audio ASR -streaming / -message, whose differences live in prof.
type ParaformerDialect struct {
	mu sync.Mutex

	opts ParaformerOptions
	prof inferenceProfile
	ids  *dialect.EventIDs
	ctx  context.Context

	cfg       paraformerConfig
	sessionID string
	taskID    string
	inputRate int

	resampler *audio.Resampler
	g711Up    *audio.Resampler // 8 kHz PCM → model rate when needed

	started  bool // run-task sent for the current task
	awaiting bool // waiting for task-started
	pending  [][]byte
	finished bool
	// forwardedTicks is the client audio of the session handed to the session for the upstream
	// and not handed back (Output.Discarded): what usageAudioSeconds bills at most.
	// pendingTicks are the ticks of each pending frame, and heldTicks those of client audio the
	// decoder has not produced output for yet (it comes out with the next frame).
	forwardedTicks int64
	pendingTicks   []int64
	heldTicks      int64
	// Client audio in ticks. Paraformer's timeline restarts at zero with every task: taskTicks is
	// the audio sent in the current task, committedTicks where its last commit point falls (a
	// final moves it the way OpenAI's server VAD commits), so the GA input audio buffer a commit
	// must not find under 100 ms is the difference. offsetTicks is the audio of the earlier tasks,
	// keeping audio_start_ms / audio_end_ms on the session timeline.
	taskTicks      int64
	committedTicks int64
	offsetTicks    int64
	// committing is set while a client commit's finish-task waits for task-finished; the next
	// audio then starts a new task on the same connection.
	committing bool
	// commitAnswered is set once a final answered the commit in flight; otherwise task-finished
	// synthesises the reply, since OpenAI answers every commit.
	commitAnswered bool
	// closing is set once teardown asked for the goodbye; drain reports whether a finish-task is
	// still outstanding for the session's last task.
	closing bool
	drain   bool

	lastDuration int64 // cumulative usage.duration from last final (Paraformer)
	// meter diffs the cumulative per-task token usage (usageReportedTokens); billedSeconds is
	// the session audio already billed in whole seconds (usageAudioSeconds).
	meter         usageMeter
	billedSeconds int64

	itemSeq       int
	current       string
	lastCommitted string
	itemPrev      map[string]string
	trackers      map[string]*hypothesisTracker
	sentenceOpen  bool
}

var _ dialect.Dialect = (*ParaformerDialect)(nil)

// NewParaformer returns the GA transcription dialect for a model SupportedParaformer admits.
func NewParaformer(opts ParaformerOptions) (*ParaformerDialect, error) {
	ids := opts.IDs
	if ids == nil {
		ids = &dialect.EventIDs{}
	}

	prof, ok := inferenceProfileFor(opts.ActualModel)
	if !ok {
		// Callers admit with SupportedParaformer; anything else (live probes) is spoken to
		// as Paraformer.
		prof = paraformerProfile
	}

	rate := prof.rate(opts.ActualModel)

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
		prof:      prof,
		ids:       ids,
		ctx:       context.Background(),
		cfg:       paraformerConfig{model: opts.RequestedModel, silenceDurationMs: prof.silenceDefault},
		sessionID: "sess_axh_" + uuid.NewString(),
		taskID:    prof.newTaskID(),
		inputRate: rate,
		resampler: rs,
		g711Up:    g711Up,
		itemPrev:  map[string]string{},
		trackers:  map[string]*hypothesisTracker{},
	}, nil
}

func (d *ParaformerDialect) Name() string { return d.prof.name }

// Greeting synthesises the GA session.created Paraformer never sends on connect.
func (d *ParaformerDialect) Greeting() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return [][]byte{d.renderSessionEvent("session.created")}
}

func (d *ParaformerDialect) Start() ([][]byte, bool) { return nil, false }

// CloseUpstream sends finish-task for a running task so Paraformer finalises (and bills) the
// sentence in progress. A commit's finish-task already in flight is waited for instead of
// being sent twice; with no task (none started yet, or the last one ended by a commit) there
// is nothing to finish.
func (d *ParaformerDialect) CloseUpstream() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.closing = true

	switch {
	case d.committing:
		d.drain = true
		return nil
	case !d.started || d.finished:
		d.drain = false
		return nil
	default:
		d.drain = true
		return [][]byte{d.finishTaskFrame()}
	}
}

// FinishDrain reports whether teardown must wait for task-finished: only while a finish-task
// is outstanding, so an idle session closes at once.
func (d *ParaformerDialect) FinishDrain() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.drain
}

func (d *ParaformerDialect) CloseGrace() time.Duration { return 2 * time.Second }

// GateTimeout bounds the waits for task-started and for a commit's task-finished; the
// inference endpoint may be in Beijing, so cross-border round trips get the default bound.
func (d *ParaformerDialect) GateTimeout() time.Duration { return 10 * time.Second }

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
		if d.closing {
			// Teardown already sent the goodbye: nothing reaches the upstream any more.
			return dialect.Output{}, nil
		}

		// Paraformer has no commit: finish-task makes it finalise the sentence in progress now,
		// without waiting for max_sentence_silence of trailing silence. Client frames are held
		// until task-finished; the next audio then starts a new task on the same connection.
		// Like OpenAI, a buffer under 100 ms is an error; anything else gets a committed item.
		buffered := d.taskTicks - d.committedTicks
		if d.committing {
			buffered = 0
		}

		running := d.started && !d.awaiting && !d.committing
		if !running || (!d.sentenceOpen && buffered < paraformerCommitMinMs*paraformerTicksPerMs) {
			return dialect.Output{ToClient: [][]byte{d.clientError("input_audio_buffer_commit_empty",
				fmt.Sprintf("Error committing input audio buffer: buffer too small. Expected at least %dms of audio, but buffer only has %.2fms of audio.",
					paraformerCommitMinMs, float64(buffered)/paraformerTicksPerMs), "", clientEventID)}}, nil
		}

		d.committing = true
		d.commitAnswered = false

		return dialect.Output{ToUpstream: [][]byte{d.finishTaskFrame()}, CloseGate: true}, nil
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

	cfg, rej := mergeParaformerUpdate(d.cfg, ev.Get("session"), d.prof)
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

func mergeParaformerUpdate(cfg paraformerConfig, session gjson.Result, prof inferenceProfile) (paraformerConfig, *rejection) {
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
			cfg.language = prof.language(lang.String())
		}
		if langs := tr.Get("languages"); langs.IsArray() {
			if list := langs.Array(); len(list) == 1 {
				cfg.language = prof.language(list[0].String())
			} else {
				cfg.language = ""
			}
		}
	}

	if nr := in.Get("noise_reduction"); prof.vadModel && nr.Exists() {
		typ := nr.Get("type").String()
		if _, ok := vadModels[typ]; nr.Type != gjson.Null && !ok {
			return cfg, &rejection{
				errType: "invalid_request_error", code: "invalid_value", param: "session.audio.input.noise_reduction.type",
				message: fmt.Sprintf("Unsupported noise_reduction.type %q; use near_field or far_field.", typ),
			}
		}
		cfg.noiseReduction = typ
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
	if d.closing {
		// Teardown already sent the goodbye and the session forwards nothing any more: the
		// audio is neither buffered nor billed.
		return dialect.Output{}
	}

	raw, err := base64.StdEncoding.DecodeString(ev.Get("audio").String())
	if err != nil || len(raw) == 0 {
		return dialect.Output{ToClient: [][]byte{d.clientError("invalid_value", "Invalid base64 audio.", "audio", clientEventID)}}
	}

	ticks := d.clientTicks(len(raw))
	d.taskTicks += ticks
	d.heldTicks += ticks

	pcm := d.decodeClientAudio(raw)
	if len(pcm) == 0 {
		return dialect.Output{}
	}

	ticks, d.heldTicks = d.heldTicks, 0

	if !d.started {
		d.started = true
		d.awaiting = true
		d.pending = append(d.pending, pcm)
		d.pendingTicks = append(d.pendingTicks, ticks)
		return dialect.Output{ToUpstream: [][]byte{d.runTaskFrame()}, CloseGate: true}
	}

	if d.awaiting {
		d.pending = append(d.pending, pcm)
		d.pendingTicks = append(d.pendingTicks, ticks)
		return dialect.Output{}
	}

	return d.forward([][]byte{pcm}, []int64{ticks})
}

// forward hands PCM frames carrying ticks of client audio each to the session for the upstream
// and counts them as forwarded; the session hands back the frames it then drops (the session
// was ending or the upstream writer failed), so only audio that reached the upstream writer is
// billed.
func (d *ParaformerDialect) forward(frames [][]byte, ticks []int64) dialect.Output {
	if len(frames) == 0 {
		return dialect.Output{}
	}

	for _, t := range ticks {
		d.forwardedTicks += t
	}

	return dialect.Output{
		ToUpstream:     frames,
		BinaryUpstream: true,
		Discarded: func(sent int) {
			d.mu.Lock()
			defer d.mu.Unlock()

			for _, t := range ticks[min(max(sent, 0), len(ticks)):] {
				d.forwardedTicks -= t
			}
		},
	}
}

// clientTicks is the duration of n bytes of client audio in the session's input format.
func (d *ParaformerDialect) clientTicks(n int) int64 {
	switch d.cfg.inputFormat {
	case paraformerMuLaw, paraformerALaw:
		return int64(n) * (clientRate / audio.G711Rate) // one byte per 8 kHz sample
	default:
		return int64(n) / 2 // pcm16 at 24 kHz
	}
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
	maps.Copy(params, d.prof.fixedParams)
	if d.cfg.language != "" {
		params["language_hints"] = []string{d.cfg.language}
	}
	if !d.cfg.manual {
		params["max_sentence_silence"] = d.cfg.silenceDurationMs
	} else {
		// Manual clients still need segmentation somewhere; keep VAD with a long silence so
		// finish-task is what usually ends the utterance.
		params["max_sentence_silence"] = paraformerSilenceMax
	}
	if d.prof.semanticPunctuation {
		params["semantic_punctuation_enabled"] = !d.cfg.manual && d.cfg.semanticVAD
	}
	if vad := vadModels[d.cfg.noiseReduction]; vad != "" {
		params["vad_model"] = vad
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
		pending, ticks := d.pending, d.pendingTicks
		d.pending, d.pendingTicks = nil, nil

		if d.closing {
			// The goodbye is already on its way and the session forwards nothing any more:
			// the held audio is never sent, so it is not billed either.
			return dialect.Output{OpenGate: true}, nil
		}

		out := d.forward(pending, ticks)
		out.OpenGate = true
		return out, nil
	case "result-generated":
		return d.resultGenerated(ev), nil
	case "task-finished":
		tail := d.taskTail(ev)

		if !d.committing {
			d.finished = true
			return dialect.Output{UpstreamFinished: true, Usage: d.tailRecords(tail)}, nil
		}

		// A client commit's finish-task: answer the commit, then either start over for the next
		// audio or, when teardown is already waiting for this task, end there.
		out := dialect.Output{ToClient: d.commitReply(tail)}
		out.Usage = d.tailRecords(tail)
		d.committing = false

		if d.closing {
			d.finished = true
			out.UpstreamFinished = true

			return out, nil
		}

		d.nextTask()
		out.OpenGate = true

		return out, nil
	case "task-failed":
		return d.taskFailed(ev), nil
	default:
		return dialect.Output{}, nil
	}
}

// nextTask resets for the task the next audio starts after a commit: new task_id, usage counted
// from zero, and a timeline that continues where this task's audio ended.
func (d *ParaformerDialect) nextTask() {
	d.started = false
	d.taskID = d.prof.newTaskID()
	d.lastDuration = 0
	d.meter = usageMeter{}
	d.offsetTicks += d.taskTicks
	d.taskTicks = 0
	d.committedTicks = 0
}

// commitReply answers a client commit once its finish-task is done, unless a Paraformer final
// already did: OpenAI answers every commit with a committed item. An item synthesised here
// carries the task's tail usage.
func (d *ParaformerDialect) commitReply(tail asrUsage) [][]byte {
	answered := d.commitAnswered
	d.commitAnswered = false

	var out [][]byte

	switch {
	case d.sentenceOpen:
		// Paraformer finalises the sentence on finish-task; should it not have, close the item
		// with the last hypothesis so the client is not left with an open turn.
		id := d.current

		text := ""
		if tr := d.trackers[id]; tr != nil {
			text = tr.prev
		}

		out = append(out,
			d.synth(map[string]any{"type": "input_audio_buffer.committed", "item_id": id, "previous_item_id": nullable(d.itemPrev[id])}),
			d.synth(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": id, "content_index": 0, "transcript": text, "usage": d.gaUsage(tail)}),
			d.synth(map[string]any{"type": "conversation.item.done", "previous_item_id": nullable(d.itemPrev[id]), "item": userAudioItem(id, "completed", text)}),
		)

		d.lastCommitted = id
		d.current = ""
		d.sentenceOpen = false
		delete(d.trackers, id)
	case !answered:
		// No speech in the committed audio (Paraformer ends a silent task with task-finished and
		// no result): answer the way OpenAI answers a silent buffer, with an empty user item.
		d.itemSeq++
		id := fmt.Sprintf("item_axh_%d", d.itemSeq)
		prev := nullable(d.lastCommitted)

		out = append(out,
			d.synth(map[string]any{"type": "input_audio_buffer.committed", "item_id": id, "previous_item_id": prev}),
			d.synth(map[string]any{"type": "conversation.item.added", "previous_item_id": prev, "item": userAudioItem(id, "in_progress", nil)}),
			d.synth(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": id, "content_index": 0, "transcript": "", "usage": d.gaUsage(tail)}),
			d.synth(map[string]any{"type": "conversation.item.done", "previous_item_id": prev, "item": userAudioItem(id, "completed", "")}),
		)

		d.lastCommitted = id
	}

	return out
}

func (d *ParaformerDialect) resultGenerated(ev gjson.Result) dialect.Output {
	sent := ev.Get("payload.output.sentence")
	if !sent.Exists() || sent.Get("heartbeat").Bool() {
		// A heartbeat result (heartbeat:true in run-task) only keeps a silent task alive: no
		// event, no usage, no commit point.
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
			"audio_start_ms": d.sessionMs(sent.Get("begin_time").Int()),
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

	endMs := sent.Get("end_time").Int() // on this task's timeline
	out.ToClient = append(out.ToClient, d.synth(map[string]any{
		"type":         "input_audio_buffer.speech_stopped",
		"item_id":      id,
		"audio_end_ms": d.sessionMs(endMs),
	}))
	out.ToClient = append(out.ToClient, d.synth(map[string]any{
		"type":             "input_audio_buffer.committed",
		"item_id":          id,
		"previous_item_id": nullable(d.itemPrev[id]),
	}))

	var (
		usage    map[string]any
		usageRec []dialect.UsageRecord
	)
	if d.prof.usage == usageReportedSeconds {
		usage, usageRec = d.paraformerFinalUsage(ev, sent, id, text, endMs)
	} else {
		usage, usageRec = d.qwenAudioFinalUsage(ev, sent, id, text, endMs)
	}

	completed := d.synth(map[string]any{
		"type":          "conversation.item.input_audio_transcription.completed",
		"item_id":       id,
		"content_index": 0,
		"transcript":    text,
		"usage":         usage,
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

	// A final commits the buffer the way OpenAI's server VAD does: at the end of the speech plus
	// the silence that ended it, on Paraformer's timeline rather than when the final arrives, so
	// audio already sent past that point stays in the buffer. Semantic VAD can end a sentence
	// sooner, so only the speech end counts there; OpenAI never commits on its own in manual mode.
	if !d.cfg.manual {
		point := endMs
		if !d.cfg.semanticVAD {
			point += d.cfg.silenceDurationMs
		}

		d.committedTicks = min(d.taskTicks, max(d.committedTicks, point*paraformerTicksPerMs))
	}

	if d.committing {
		d.commitAnswered = true
	}

	return out
}

// paraformerFinalUsage bills a Paraformer final by the difference of its cumulative
// usage.duration.
func (d *ParaformerDialect) paraformerFinalUsage(ev, sent gjson.Result, id, text string, endMs int64) (map[string]any, []dialect.UsageRecord) {
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
			"type":             d.prof.summaryType,
			"item_id":          id,
			"transcript":       text,
			"duration_seconds": deltaSec,
			"begin_time_ms":    d.sessionMs(sent.Get("begin_time").Int()),
			"end_time_ms":      d.sessionMs(endMs),
		})
		usageRec = append(usageRec, dialect.UsageRecord{
			Kind:            "transcription",
			ItemID:          id,
			Model:           d.opts.ActualModel,
			DurationSeconds: float64(deltaSec),
			Summary:         summary,
		})
	}

	return map[string]any{"type": "duration", "seconds": deltaSec}, usageRec
}

// sessionMs moves a time on the current task's timeline onto the session timeline.
func (d *ParaformerDialect) sessionMs(taskMs int64) int64 {
	return taskMs + d.offsetTicks/paraformerTicksPerMs
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
		d.pending, d.pendingTicks = nil, nil
		out.OpenGate = true
	}

	// The -streaming audio forwarded since the last final is billed once the session ended
	// (FinalUsage), since no task-finished follows a failed task.
	return out
}

// FinalUsage implements the session's finalUsager: once the session ended, -streaming bills
// the forwarded audio no final or task tail billed yet. That is the remainder of a task that
// ended without task-finished (task-failed, a lost upstream, a commit or goodbye whose
// task-finished never came); billAudioTo never bills a second twice, so after a task tail it
// is nothing. -message and Paraformer bill what the upstream reports, so they have no
// remainder of their own.
func (d *ParaformerDialect) FinalUsage() []dialect.UsageRecord {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.prof.usage != usageAudioSeconds {
		return nil
	}

	return d.tailRecords(asrUsage{Duration: d.billAudioTo(d.forwardedTicks)})
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

	var noiseReduction any
	if d.cfg.noiseReduction != "" {
		noiseReduction = map[string]any{"type": d.cfg.noiseReduction}
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
					"noise_reduction": noiseReduction,
					"turn_detection":  turnDetection,
				},
			},
		},
	})
}

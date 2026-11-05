package bailianasr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
)

// inferencePath is the DashScope duplex inference WebSocket (run-task / binary audio /
// finish-task), the only interface of qwen-audio-3.1-asr-flash-message.
const inferencePath = "/api-ws/v1/inference"

type runTaskFrame struct {
	Header  runTaskHeader  `json:"header"`
	Payload runTaskPayload `json:"payload"`
}

type runTaskHeader struct {
	Action    string `json:"action"`
	TaskID    string `json:"task_id"`
	Streaming string `json:"streaming"`
}

type runTaskPayload struct {
	TaskGroup  string         `json:"task_group"`
	Task       string         `json:"task"`
	Function   string         `json:"function"`
	Model      string         `json:"model"`
	Parameters map[string]any `json:"parameters"`
	Input      map[string]any `json:"input"`
}

// buildMessageRequest builds the run-task frame of one -message task. The request body is that
// frame without a task_id (the executor sets a fresh one per attempt), so the logged and
// persisted body never carries audio; the audio stays in the job.
func (o *Outbound) buildMessageRequest(
	ctx context.Context,
	req *llm.Request,
	opts renderOptions,
	src AudioSource,
	head []byte,
) (*httpclient.Request, error) {
	if _, ok := src.(RemoteAudio); ok {
		return nil, fmt.Errorf("%w: %s needs the audio file itself; url input is not supported for this model",
			transformer.ErrInvalidRequest, req.Model)
	}

	if opts.Format == formatDiarizedJSON {
		return nil, fmt.Errorf("%w: %s does not support speaker diarization (response_format diarized_json)",
			transformer.ErrInvalidRequest, req.Model)
	}

	in, err := detectMessageAudio(src, head, req.Transcription.Extra, o.cfg.messageMaxDuration())
	if err != nil {
		return nil, err
	}

	// Only parameters the 2026-11-05 live probe accepted, plus the documented context input.
	params := map[string]any{"format": in.Format, "sample_rate": messageSampleRate, "heartbeat": false}
	if hints := languageHints(ctx, familyMessage, opts.Language); hints != nil {
		params["language_hints"] = hints
	}

	input := map[string]any{}
	if prompt := strings.TrimSpace(req.Transcription.Prompt); prompt != "" {
		input["context"] = []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": prompt}},
		}}
	}

	raw, err := json.Marshal(runTaskFrame{
		Header: runTaskHeader{Action: "run-task", Streaming: "duplex"},
		Payload: runTaskPayload{
			TaskGroup:  "audio",
			Task:       "asr",
			Function:   "recognition",
			Model:      req.Model,
			Parameters: params,
			Input:      input,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal qwen-audio-asr-flash-message task: %w", err)
	}

	// The exact length of WAV and raw PCM input; DashScope reports whole billed seconds.
	opts.AudioSeconds = in.Duration

	j := &job{family: familyMessage, native: o.native, audio: src, opts: opts, message: &in}

	return o.providerRequest(ctx, kindMessage, o.native+inferencePath, http.Header{}, raw, raw, opts, j)
}

// messageFolder reads the inference frames of one -message task. Each sentence yields one empty
// partial and then one final (sentence_end) carrying that sentence's text only; usage on the
// finals is cumulative for the task (live probe 2026-11-05), so the last report wins.
type messageFolder struct {
	text     strings.Builder
	segments []segment
	res      streamResult
}

func (f *messageFolder) fold(event *httpclient.StreamEvent) ([]string, error) {
	if event == nil || len(event.Data) == 0 {
		return nil, nil
	}

	root := gjson.ParseBytes(event.Data)
	if id := root.Get("header.task_id").String(); id != "" {
		f.res.id = id
	}

	switch root.Get("header.event").String() {
	case "result-generated":
		f.takeUsage(root.Get("payload.usage"))

		sentence := root.Get("payload.output.sentence")
		if !sentence.Get("sentence_end").Bool() || sentence.Get("heartbeat").Bool() {
			return nil, nil
		}

		seg := parseSentence(sentence, 0)
		if seg.Text == "" {
			return nil, nil
		}

		// Without a long enough pause one final covers several sentences (live run 2026-11-06: the
		// whole two-sentence clip came back as one final); split it at sentence-final punctuation
		// so srt / vtt / verbose_json get one segment per sentence.
		if parts := splitSentence(sentence); len(parts) > 1 {
			f.segments = append(f.segments, parts...)
		} else {
			f.segments = append(f.segments, seg)
		}
		delta := sentenceDelta(f.text.String(), seg.Text)
		f.text.WriteString(delta)

		return []string{delta}, nil
	case "task-finished":
		f.takeUsage(root.Get("payload.usage"))
		f.res.final = true
	}

	return nil, nil
}

func (f *messageFolder) takeUsage(v gjson.Result) {
	if !v.Exists() {
		return
	}

	if usage := parseUsage(v); usage.Duration > 0 || usage.hasTokens() {
		f.res.usage = usage
	}
}

func (f *messageFolder) finish() []string { return nil }

func (f *messageFolder) result() streamResult {
	res := f.res
	res.text = f.text.String()

	res.duration = res.usage.Duration
	if end := lastEnd(f.segments); end > res.duration {
		res.duration = end
	}

	return res
}

func (f *messageFolder) transcript() *transcript {
	res := f.result()

	return &transcript{ID: res.id, Text: res.text, Duration: res.duration, Segments: f.segments, Usage: res.usage}
}

// sentenceDelta is the text one more sentence adds: a space separates two sentences unless one
// side is Chinese or Japanese text, which is written without spaces (Korean separates words and
// sentences with spaces like Latin scripts).
func sentenceDelta(prev, next string) string {
	if prev == "" {
		return next
	}

	last, _ := utf8.DecodeLastRuneInString(prev)
	first, _ := utf8.DecodeRuneInString(next)

	if unicode.IsSpace(last) || unicode.IsSpace(first) || isCJK(last) || isCJK(first) {
		return next
	}

	return " " + next
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

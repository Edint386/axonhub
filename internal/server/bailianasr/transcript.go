package bailianasr

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
)

// transcript is the normalized recognition result shared by the flash and async paths. Its
// JSON form is also the body of the executor's synthetic response for async jobs.
type transcript struct {
	ID       string    `json:"id,omitempty"`
	Text     string    `json:"text"`
	Duration float64   `json:"duration,omitempty"`
	Segments []segment `json:"segments,omitempty"`
	Usage    dsUsage   `json:"usage"`
}

type segment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Channel int     `json:"channel,omitempty"`
	Speaker *int    `json:"speaker,omitempty"`
	Words   []word  `json:"words,omitempty"`
}

type word struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// dsUsage is DashScope's usage object. Duration is in seconds; the token counts are only
// reported by the qwen-audio models.
type dsUsage struct {
	Duration     float64 `json:"duration,omitempty"`
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	TotalTokens  int64   `json:"total_tokens,omitempty"`
}

func (u dsUsage) hasTokens() bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.TotalTokens > 0
}

// llmUsage is the gateway usage record. DurationSeconds is always set (billed seconds, else the
// audio duration) so a duration_seconds price applies to every model.
func (u dsUsage) llmUsage(audioSeconds float64) *llm.Usage {
	usage := &llm.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		DurationSeconds:  u.Duration,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}

	if usage.DurationSeconds <= 0 {
		usage.DurationSeconds = audioSeconds
	}

	return usage
}

// withoutWords returns a copy of t without word timestamps (they are only rendered on request).
func (t *transcript) withoutWords() *transcript {
	out := *t
	out.Segments = make([]segment, len(t.Segments))

	for i, seg := range t.Segments {
		seg.Words = nil
		out.Segments[i] = seg
	}

	return &out
}

func parseUsage(v gjson.Result) dsUsage {
	return dsUsage{
		Duration:     v.Get("duration").Float(),
		InputTokens:  v.Get("input_tokens").Int(),
		OutputTokens: v.Get("output_tokens").Int(),
		TotalTokens:  v.Get("total_tokens").Int(),
	}
}

func seconds(ms gjson.Result) float64 {
	return ms.Float() / 1000
}

func speakerOf(v gjson.Result) *int {
	speaker := v.Get("speaker_id")
	if speaker.Type != gjson.Number {
		return nil
	}

	id := int(speaker.Int())

	return &id
}

func firstString(values ...gjson.Result) string {
	for _, v := range values {
		if s := v.String(); s != "" {
			return s
		}
	}

	return ""
}

func parseWord(w gjson.Result) word {
	return word{
		Text:  strings.TrimSpace(w.Get("text").String()),
		Start: seconds(w.Get("begin_time")),
		End:   seconds(w.Get("end_time")),
	}
}

func parseSentence(s gjson.Result, channel int) segment {
	seg := segment{
		Start:   seconds(s.Get("begin_time")),
		End:     seconds(s.Get("end_time")),
		Text:    strings.TrimSpace(s.Get("text").String()),
		Channel: channel,
		Speaker: speakerOf(s),
	}

	for _, w := range s.Get("words").Array() {
		seg.Words = append(seg.Words, parseWord(w))
	}

	return seg
}

// errDashScopeBody marks a 2xx body that carries a DashScope error instead of a result.
var errDashScopeBody = errors.New("dashscope error body")

// parseFlashBody reads a qwen-audio-3.1-asr-flash generation response (or one SSE event).
// Without diarization the model returns ONE aggregated output.sentence for the whole clip
// (live probe 2026-11-05), so segments are split from its words.
func parseFlashBody(body []byte) (*transcript, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("invalid qwen-audio-asr-flash response: not a JSON object")
	}

	if code := root.Get("code").String(); code != "" && !root.Get("output").Exists() {
		return nil, fmt.Errorf("%w: %s", errDashScopeBody, code)
	}

	output := root.Get("output")
	t := &transcript{
		ID:    root.Get("request_id").String(),
		Text:  strings.TrimSpace(firstString(output.Get("text"), output.Get("sentence.text"))),
		Usage: parseUsage(root.Get("usage")),
	}

	if sentences := output.Get("sentences").Array(); len(sentences) > 0 {
		for _, s := range sentences {
			t.Segments = append(t.Segments, parseSentence(s, 0))
		}
	} else if sentence := output.Get("sentence"); sentence.Exists() {
		t.Segments = splitSentence(sentence)
	}

	t.Duration = t.Usage.Duration
	if t.Duration <= 0 {
		t.Duration = lastEnd(t.Segments)
	}

	return t, nil
}

// splitSentence cuts one DashScope sentence into segments after every word whose punctuation
// ends a sentence (。！？.!?).
func splitSentence(sentence gjson.Result) []segment {
	words := sentence.Get("words").Array()
	if len(words) == 0 {
		seg := parseSentence(sentence, 0)
		if seg.Text == "" {
			return nil
		}

		return []segment{seg}
	}

	speaker := speakerOf(sentence)

	var (
		segments []segment
		current  segment
		text     strings.Builder
	)

	flush := func() {
		current.Text = strings.TrimSpace(text.String())
		current.Speaker = speaker
		if current.Text != "" || len(current.Words) > 0 {
			segments = append(segments, current)
		}

		current = segment{}
		text.Reset()
	}

	for _, w := range words {
		piece := w.Get("text").String() + w.Get("punctuation").String()
		parsed := parseWord(w)

		if len(current.Words) == 0 {
			current.Start = parsed.Start
		}

		current.End = parsed.End
		current.Words = append(current.Words, parsed)
		text.WriteString(piece)

		if endsSentence(piece) {
			flush()
		}
	}

	if len(current.Words) > 0 {
		flush()
	}

	return segments
}

func endsSentence(piece string) bool {
	r, _ := utf8.DecodeLastRuneInString(strings.TrimSpace(piece))
	return strings.ContainsRune("。！？.!?", r)
}

// parseFileResult reads the transcription_url JSON of an async job. The docs' field tables and
// examples disagree on a few names (original_duration vs original_duration_in_milliseconds,
// transcript vs transcripts); both spellings are accepted.
func parseFileResult(body []byte) (*transcript, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("invalid transcription result: not a JSON object")
	}

	transcripts := root.Get("transcripts")
	if !transcripts.IsArray() {
		transcripts = root.Get("transcript")
	}

	t := &transcript{}

	var texts []string

	for _, tr := range transcripts.Array() {
		channel := int(tr.Get("channel_id").Int())
		if text := strings.TrimSpace(firstString(tr.Get("text"), tr.Get("transcript"))); text != "" {
			texts = append(texts, text)
		}

		for _, s := range tr.Get("sentences").Array() {
			t.Segments = append(t.Segments, parseSentence(s, channel))
		}
	}

	sort.SliceStable(t.Segments, func(i, j int) bool {
		if t.Segments[i].Start != t.Segments[j].Start {
			return t.Segments[i].Start < t.Segments[j].Start
		}

		return t.Segments[i].Channel < t.Segments[j].Channel
	})

	t.Text = strings.Join(texts, "\n")
	if t.Text == "" {
		t.Text = joinSegmentTexts(t.Segments)
	}

	durationMS := root.Get("properties.original_duration_in_milliseconds")
	if !durationMS.Exists() {
		durationMS = root.Get("properties.original_duration")
	}

	t.Duration = seconds(durationMS)
	if t.Duration <= 0 {
		t.Duration = lastEnd(t.Segments)
	}

	return t, nil
}

func joinSegmentTexts(segments []segment) string {
	var b strings.Builder
	for _, seg := range segments {
		b.WriteString(seg.Text)
	}

	return b.String()
}

func lastEnd(segments []segment) float64 {
	var end float64
	for _, seg := range segments {
		end = max(end, seg.End)
	}

	return end
}

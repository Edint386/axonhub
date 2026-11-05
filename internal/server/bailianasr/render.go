package bailianasr

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
)

// The response shapes follow the OpenAI transcription reference:
// https://developers.openai.com/api/reference/resources/audio/subresources/transcriptions/methods/create
const (
	formatJSON         = "json"
	formatText         = "text"
	formatSRT          = "srt"
	formatVTT          = "vtt"
	formatVerboseJSON  = "verbose_json"
	formatDiarizedJSON = "diarized_json"

	contentTypeJSON = "application/json"
	// contentTypeText is used for text, srt and vtt alike, as OpenAI does.
	contentTypeText = "text/plain; charset=utf-8"
)

var responseFormats = map[string]bool{
	formatJSON: true, formatText: true, formatSRT: true, formatVTT: true,
	formatVerboseJSON: true, formatDiarizedJSON: true,
}

// renderOptions are the client-side choices that shape the response. They travel on the
// provider request as Metadata so every response path (non-stream, stream, aggregation) sees them.
type renderOptions struct {
	Model    string `json:"model"`
	Format   string `json:"format"`
	Words    bool   `json:"words,omitempty"`
	Segments bool   `json:"segments,omitempty"`
	Language string `json:"language,omitempty"`
	// AudioSeconds is the exact audio length when the gateway knows it (WAV and raw PCM input of
	// flash and -message); it is the reported duration in place of DashScope's whole seconds.
	AudioSeconds float64 `json:"audio_seconds,omitempty"`
}

type openAIUsage struct {
	Type         string   `json:"type"`
	InputTokens  *int64   `json:"input_tokens,omitempty"`
	OutputTokens *int64   `json:"output_tokens,omitempty"`
	TotalTokens  *int64   `json:"total_tokens,omitempty"`
	Seconds      *float64 `json:"seconds,omitempty"`
}

func toOpenAIUsage(u dsUsage, audioSeconds float64) openAIUsage {
	if u.hasTokens() {
		total := u.TotalTokens
		if total == 0 {
			total = u.InputTokens + u.OutputTokens
		}

		return openAIUsage{Type: "tokens", InputTokens: lo.ToPtr(u.InputTokens), OutputTokens: lo.ToPtr(u.OutputTokens), TotalTokens: &total}
	}

	billed := u.Duration
	if billed <= 0 {
		billed = math.Ceil(audioSeconds)
	}

	return openAIUsage{Type: "duration", Seconds: &billed}
}

type jsonBody struct {
	Text  string      `json:"text"`
	Usage openAIUsage `json:"usage"`
}

type verboseBody struct {
	Task     string            `json:"task"`
	Language string            `json:"language"`
	Duration float64           `json:"duration"`
	Text     string            `json:"text"`
	Segments *[]verboseSegment `json:"segments,omitempty"`
	Words    *[]verboseWord    `json:"words,omitempty"`
	Usage    openAIUsage       `json:"usage"`
}

type verboseSegment struct {
	ID               int     `json:"id"`
	Seek             int     `json:"seek"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Text             string  `json:"text"`
	Tokens           []int   `json:"tokens"`
	Temperature      float64 `json:"temperature"`
	AvgLogprob       float64 `json:"avg_logprob"`
	CompressionRatio float64 `json:"compression_ratio"`
	NoSpeechProb     float64 `json:"no_speech_prob"`
}

type verboseWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type diarizedBody struct {
	Task     string            `json:"task"`
	Duration float64           `json:"duration"`
	Text     string            `json:"text"`
	Segments []diarizedSegment `json:"segments"`
	Usage    openAIUsage       `json:"usage"`
}

type diarizedSegment struct {
	Type    string  `json:"type"`
	ID      string  `json:"id"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker string  `json:"speaker"`
}

// render builds the client body and its Content-Type for opts.Format.
func render(t *transcript, opts renderOptions) ([]byte, string, error) {
	usage := toOpenAIUsage(t.Usage, t.Duration)

	switch opts.Format {
	case formatText:
		return []byte(t.Text + "\n"), contentTypeText, nil
	case formatSRT:
		// An SRT file without cues is empty; a lone newline keeps the body non-empty.
		return []byte(lo.CoalesceOrEmpty(renderCues(t, srtTimestamp, true), "\n")), contentTypeText, nil
	case formatVTT:
		return []byte("WEBVTT\n\n" + renderCues(t, vttTimestamp, false)), contentTypeText, nil
	case formatVerboseJSON:
		return marshalBody(verboseFrom(t, opts, usage))
	case formatDiarizedJSON:
		return marshalBody(diarizedFrom(t, usage))
	default:
		return marshalBody(jsonBody{Text: t.Text, Usage: usage})
	}
}

func marshalBody(v any) ([]byte, string, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal transcription body: %w", err)
	}

	return body, contentTypeJSON, nil
}

func verboseFrom(t *transcript, opts renderOptions, usage openAIUsage) verboseBody {
	body := verboseBody{
		Task:     "transcribe",
		Language: languageName(opts.Language),
		Duration: t.Duration,
		Text:     t.Text,
		Usage:    usage,
	}

	if opts.Segments {
		segments := make([]verboseSegment, 0, len(t.Segments))
		for i, seg := range t.Segments {
			segments = append(segments, verboseSegment{ID: i, Start: seg.Start, End: seg.End, Text: seg.Text, Tokens: []int{}})
		}

		body.Segments = &segments
	}

	if opts.Words {
		words := make([]verboseWord, 0)
		for _, seg := range t.Segments {
			for _, w := range seg.Words {
				if w.Text != "" {
					words = append(words, verboseWord{Word: w.Text, Start: w.Start, End: w.End})
				}
			}
		}

		body.Words = &words
	}

	return body
}

func diarizedFrom(t *transcript, usage openAIUsage) diarizedBody {
	labels := speakerLabels{}
	segments := make([]diarizedSegment, 0, len(t.Segments))

	for i, seg := range t.Segments {
		segments = append(segments, diarizedSegment{
			Type:    "transcript.text.segment",
			ID:      fmt.Sprintf("seg_%03d", i+1),
			Start:   seg.Start,
			End:     seg.End,
			Text:    seg.Text,
			Speaker: labels.label(seg.Channel, seg.Speaker),
		})
	}

	return diarizedBody{Task: "transcribe", Duration: t.Duration, Text: t.Text, Segments: segments, Usage: usage}
}

// speakerLabels names speakers "A", "B", … in order of first appearance, as OpenAI does.
// A segment without a speaker id (single speaker or diarization unsupported) is one speaker
// per channel.
type speakerLabels map[[2]int]string

func (l speakerLabels) label(channel int, speaker *int) string {
	key := [2]int{channel, -1}
	if speaker != nil {
		key[1] = *speaker
	}

	if label, ok := l[key]; ok {
		return label
	}

	label := alphabeticLabel(len(l))
	l[key] = label

	return label
}

// alphabeticLabel maps 0, 1, …, 25, 26 to "A", "B", …, "Z", "AA".
func alphabeticLabel(n int) string {
	label := ""
	for n >= 0 {
		label = string(rune('A'+n%26)) + label
		n = n/26 - 1
	}

	return label
}

// renderCues writes SRT (numbered) or VTT cues; a transcript with text but no segments becomes
// one cue spanning the whole audio.
func renderCues(t *transcript, stamp func(float64) string, numbered bool) string {
	segments := t.Segments
	if len(segments) == 0 && t.Text != "" {
		segments = []segment{{Start: 0, End: t.Duration, Text: t.Text}}
	}

	var b strings.Builder
	for i, seg := range segments {
		if numbered {
			fmt.Fprintf(&b, "%d\n", i+1)
		}

		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", stamp(seg.Start), stamp(seg.End), seg.Text)
	}

	return b.String()
}

func srtTimestamp(sec float64) string {
	h, m, s, ms := clockParts(sec)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

func vttTimestamp(sec float64) string {
	h, m, s, ms := clockParts(sec)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

func clockParts(sec float64) (int64, int64, int64, int64) {
	total := max(int64(math.Round(sec*1000)), 0)
	return total / 3_600_000, total / 60_000 % 60, total / 1000 % 60, total % 1000
}

// toLLMResponse wraps a rendered body as the unified transcription response. Usage carries
// the billing data; the OpenAI inbound returns Raw to the client untouched.
func toLLMResponse(t *transcript, opts renderOptions) (*llm.Response, error) {
	body, contentType, err := render(t, opts)
	if err != nil {
		return nil, err
	}

	return &llm.Response{
		ID:          t.ID,
		Model:       opts.Model,
		RequestType: llm.RequestTypeTranscription,
		APIFormat:   llm.APIFormatOpenAITranscription,
		Usage:       t.Usage.llmUsage(t.Duration),
		Transcription: &llm.TranscriptionResponse{
			Text:           t.Text,
			Language:       languageName(opts.Language),
			Duration:       lo.ToPtr(t.Duration),
			Raw:            body,
			RawContentType: contentType,
		},
	}, nil
}

package bailian

import (
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// inferenceUsage is how a model on Bailian's inference WebSocket reports what it bills.
type inferenceUsage int

const (
	// usageReportedSeconds: finals carry usage.duration, cumulative per task (Paraformer).
	usageReportedSeconds inferenceUsage = iota
	// usageReportedTokens: finals carry {duration, input_tokens, output_tokens, total_tokens},
	// cumulative per task (qwen-audio -message, live probe 2026-11-05).
	usageReportedTokens
	// usageAudioSeconds: no usage anywhere, not even on task-finished (qwen-audio -streaming,
	// live probe 2026-11-05); the gateway bills the seconds of audio it sent.
	usageAudioSeconds
)

// inferenceProfile is how one model family on Bailian's inference WebSocket (run-task, binary
// audio, finish-task; result-generated with payload.output.sentence) differs from the others.
type inferenceProfile struct {
	admission *regexp.Regexp
	name      string // dialect name for logs and metadata

	sampleRate int
	// rate8k feeds 8 kHz audio to a model whose name contains "8k".
	rate8k         bool
	silenceDefault int64
	// semanticPunctuation sends semantic_punctuation_enabled (true under semantic_vad).
	semanticPunctuation bool
	// vadModel maps the GA noise_reduction type to the upstream vad_model and echoes it.
	vadModel bool
	// compactTaskID uses 32 hex characters without dashes, the task id form probed live.
	compactTaskID bool
	// fixedParams are added to every run-task's parameters.
	fixedParams map[string]any

	language    func(string) string
	usage       inferenceUsage
	summaryType string
}

var paraformerProfile = inferenceProfile{
	admission:           regexp.MustCompile(`^paraformer-realtime-(8k-)?v2`),
	name:                paraformerDialectName,
	sampleRate:          paraformerRateV2,
	rate8k:              true,
	silenceDefault:      paraformerSilenceDef,
	semanticPunctuation: true,
	language:            paraformerLanguage,
	usage:               usageReportedSeconds,
	summaryType:         "paraformer_sentence",
}

// qwenAudioProfile describes qwen-audio-3.1-asr-flash-<variant> and its dated snapshots. Only
// parameters the live probe accepted are sent: format, sample_rate, language_hints,
// max_sentence_silence, vad_model and heartbeat. heartbeat is true: with the default false the
// upstream times the task out after a stretch of continuous silence (documented for both
// variants), which is exactly what an open-mic GA transcription client streams between
// utterances. The heartbeat results it then sends (sentence.heartbeat) are dropped by
// resultGenerated and never billed.
func qwenAudioProfile(variant string, usage inferenceUsage) inferenceProfile {
	return inferenceProfile{
		admission:      regexp.MustCompile(`^qwen-audio-3\.1-asr-flash-` + variant + `(-[0-9][0-9-]*)?$`),
		name:           qwenAudioDialectName,
		sampleRate:     qwenAudioRate,
		silenceDefault: qwenAudioSilenceDef,
		vadModel:       true,
		compactTaskID:  true,
		fixedParams:    map[string]any{"heartbeat": true},
		language:       qwenAudioLanguage,
		usage:          usage,
		summaryType:    "qwen_audio_sentence",
	}
}

const (
	qwenAudioDialectName = "bailian_qwen_audio"
	qwenAudioRate        = 16000 // the vad_model choices are 16 kHz models
	qwenAudioSilenceDef  = 800   // Bailian's documented max_sentence_silence default
)

// inferenceProfiles is the ordered admission table of the inference-protocol dialect.
var inferenceProfiles = []inferenceProfile{
	paraformerProfile,
	qwenAudioProfile("streaming", usageAudioSeconds),
	qwenAudioProfile("message", usageReportedTokens),
}

// inferenceProfileFor returns the profile of the first family model belongs to.
func inferenceProfileFor(model string) (inferenceProfile, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, p := range inferenceProfiles {
		if p.admission.MatchString(m) {
			return p, true
		}
	}

	return inferenceProfile{}, false
}

// rate is the PCM sample rate sent upstream for model.
func (p inferenceProfile) rate(model string) int {
	if p.rate8k && strings.Contains(strings.ToLower(model), "8k") {
		return paraformerRate8k
	}

	return p.sampleRate
}

func (p inferenceProfile) newTaskID() string {
	id := uuid.NewString()
	if p.compactTaskID {
		return strings.ReplaceAll(id, "-", "")
	}

	return id
}

// vadModels maps GA noise_reduction types to the qwen-audio vad_model.
var vadModels = map[string]string{
	"near_field": "near_meeting_16k",
	"far_field":  "far_field_meeting_16k",
}

// qwenAudioLanguages are the language_hints codes qwen-audio-3.1 ASR accepts: no yue, and
// Filipino is tl.
var qwenAudioLanguages = map[string]bool{
	"zh": true, "en": true, "ja": true, "ko": true, "vi": true, "th": true, "id": true, "ms": true, "tl": true, "hi": true,
	"ar": true, "fr": true, "de": true, "es": true, "pt": true, "ru": true, "it": true, "nl": true, "sv": true, "da": true,
	"fi": true, "no": true, "el": true, "pl": true, "cs": true, "hu": true, "ro": true, "bg": true, "hr": true, "sk": true,
}

// qwenAudioLanguageAliases covers the spellings asrLanguageAliases does not, and overrides its
// Filipino mapping.
var qwenAudioLanguageAliases = map[string]string{
	"fil": "tl", "tgl": "tl", "tl": "tl",
	"nld": "nl", "dut": "nl", "ell": "el", "gre": "el", "hun": "hu", "ron": "ro", "rum": "ro",
	"bul": "bg", "hrv": "hr", "slk": "sk", "slo": "sk",
}

// qwenAudioLanguage normalises a GA language code to a qwen-audio language hint, or "" for
// automatic detection when the model does not know the language.
func qwenAudioLanguage(code string) string {
	c := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(code, "_", "-")))
	if c == "" {
		return ""
	}

	if lang, known := qwenAudioResolve(c); known {
		return lang
	}

	if base, _, found := strings.Cut(c, "-"); found {
		lang, _ := qwenAudioResolve(base)
		return lang
	}

	return ""
}

// qwenAudioResolve maps one code. known reports whether the code names a language at all, so
// a regional code of an unsupported language (zh-HK is Cantonese) is not widened to its base.
func qwenAudioResolve(c string) (string, bool) {
	if alias, ok := qwenAudioLanguageAliases[c]; ok {
		c = alias
	} else if alias, ok := asrLanguageAliases[c]; ok {
		c = alias
	}

	if qwenAudioLanguages[c] {
		return c, true
	}

	return "", asrLanguages[c]
}

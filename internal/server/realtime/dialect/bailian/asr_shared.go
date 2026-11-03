package bailian

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// asrAdmission is the model-name pattern of the Qwen-ASR realtime models
// (qwen3-asr-flash-realtime and its dated snapshots). They speak the same beta-shaped
// WebSocket protocol as the omni models, restricted to transcription.
var asrAdmission = regexp.MustCompile(`^qwen[\d.]*-asr-.*realtime`)

// SupportedASR reports whether model is a Qwen-ASR realtime model the ASR dialects can relay.
func SupportedASR(model string) bool {
	return asrAdmission.MatchString(strings.ToLower(strings.TrimSpace(model)))
}

// asrUsage is one Bailian ASR usage object. Bailian reports usage on every
// transcription.completed as running totals for the whole session (live probe 2026-11-03:
// total_tokens 58, 98, 136, ... over consecutive utterances; duration is the rounded session
// audio position), so a single utterance's usage is the difference to the previous report.
type asrUsage struct {
	Duration   int64
	Total      int64
	Input      int64
	Output     int64
	InputText  int64
	InputAudio int64
	OutputText int64
	hasDetails bool
}

func parseASRUsage(u gjson.Result) asrUsage {
	in := u.Get("input_tokens_details")
	if !in.Exists() {
		in = u.Get("input_token_details")
	}

	out := u.Get("output_tokens_details")
	if !out.Exists() {
		out = u.Get("output_token_details")
	}

	return asrUsage{
		Duration:   u.Get("duration").Int(),
		Total:      u.Get("total_tokens").Int(),
		Input:      u.Get("input_tokens").Int(),
		Output:     u.Get("output_tokens").Int(),
		InputText:  in.Get("text_tokens").Int(),
		InputAudio: in.Get("audio_tokens").Int(),
		OutputText: out.Get("text_tokens").Int(),
		hasDetails: in.Exists(),
	}
}

// usageMeter turns Bailian's cumulative session usage into per-utterance usage.
type usageMeter struct {
	last asrUsage
}

// next returns the usage of the utterance whose completion reported cumulative cur. A total
// that went down means the upstream restarted its counters; cur is then taken as is. A
// completion without usage bills nothing and keeps the running baseline, so the next report
// does not bill earlier utterances again.
func (m *usageMeter) next(cur asrUsage) asrUsage {
	if cur.Total == 0 && cur.Input == 0 && cur.Output == 0 && cur.Duration == 0 {
		return asrUsage{}
	}

	prev := m.last
	m.last = cur

	if cur.Total < prev.Total || cur.Input < prev.Input || cur.Output < prev.Output {
		return cur
	}

	sub := func(a, b int64) int64 { return max(a-b, 0) }

	return asrUsage{
		Duration:   sub(cur.Duration, prev.Duration),
		Total:      sub(cur.Total, prev.Total),
		Input:      sub(cur.Input, prev.Input),
		Output:     sub(cur.Output, prev.Output),
		InputText:  sub(cur.InputText, prev.InputText),
		InputAudio: sub(cur.InputAudio, prev.InputAudio),
		OutputText: sub(cur.OutputText, prev.OutputText),
		hasDetails: cur.hasDetails,
	}
}

// gaTokens renders the usage as the GA transcription usage object (type "tokens").
func (u asrUsage) gaTokens() map[string]any {
	out := map[string]any{
		"type":          "tokens",
		"total_tokens":  u.Total,
		"input_tokens":  u.Input,
		"output_tokens": u.Output,
	}

	if u.hasDetails {
		out["input_token_details"] = map[string]any{"text_tokens": u.InputText, "audio_tokens": u.InputAudio}
	}

	return out
}

// record builds the usage record of one utterance billed under model.
func (u asrUsage) record(itemID, model string, summary []byte) dialect.UsageRecord {
	ga, _ := json.Marshal(u.gaTokens())

	return dialect.UsageRecord{
		Kind:            "transcription",
		ItemID:          itemID,
		Model:           model,
		Usage:           dialect.GAUsage(gjson.ParseBytes(ga)),
		DurationSeconds: float64(u.Duration),
		Summary:         append(json.RawMessage(nil), summary...),
	}
}

// hypothesisTracker turns Bailian ASR hypotheses into GA append-only deltas.
//
// Each Bailian transcription.text event carries the whole current hypothesis of the utterance
// (text + stash; on the probed model text was always empty) and later hypotheses revise
// earlier ones in place, mostly trailing punctuation ("今天" -> "今天。" -> "今天天"). GA deltas
// can only append, so the tracker only emits text two consecutive hypotheses agree on, minus
// trailing punctuation; the final transcript then supplies the rest as a catch-up delta.
type hypothesisTracker struct {
	prev    string
	emitted string
}

// next returns the delta to emit for hypothesis h; it may be empty.
func (t *hypothesisTracker) next(h string) string {
	stable := strings.TrimRightFunc(commonPrefix(t.prev, h), func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSpace(r)
	})
	t.prev = h

	if len(stable) <= len(t.emitted) || !strings.HasPrefix(stable, t.emitted) {
		return ""
	}

	d := stable[len(t.emitted):]
	t.emitted = stable

	return d
}

// complete returns the catch-up delta for the final transcript. When the transcript does not
// extend what was emitted (an earlier agreed character was revised) there is no append-only
// fix; GA clients must treat the completed transcript as authoritative, as OpenAI advises.
func (t *hypothesisTracker) complete(transcript string) string {
	if len(transcript) > len(t.emitted) && strings.HasPrefix(transcript, t.emitted) {
		return transcript[len(t.emitted):]
	}

	return ""
}

// commonPrefix returns the longest common prefix of a and b on a rune boundary.
func commonPrefix(a, b string) string {
	n := min(len(a), len(b))

	i := 0
	for i < n && a[i] == b[i] {
		i++
	}

	for i > 0 && i < len(a) && !utf8.RuneStart(a[i]) {
		i--
	}

	return a[:i]
}

// asrLanguages are the language codes Qwen-ASR realtime accepts in
// session.input_audio_transcription.language.
var asrLanguages = map[string]bool{
	"zh": true, "yue": true, "en": true, "ja": true, "de": true, "ko": true, "ru": true, "fr": true, "pt": true,
	"ar": true, "it": true, "es": true, "hi": true, "id": true, "th": true, "tr": true, "uk": true, "vi": true,
	"cs": true, "da": true, "fil": true, "fi": true, "is": true, "ms": true, "no": true, "pl": true, "sv": true,
}

// asrLanguageAliases maps the GA spellings (ISO 639-3 and regional zh codes) to Bailian codes.
var asrLanguageAliases = map[string]string{
	"cmn": "zh", "zh-cn": "zh", "zh-tw": "zh", "zh-sg": "zh", "zh-hans": "zh", "zh-hant": "zh",
	"zh-hk": "yue", "zh-mo": "yue",
	"eng": "en", "spa": "es", "fra": "fr", "fre": "fr", "deu": "de", "ger": "de", "jpn": "ja", "kor": "ko",
	"rus": "ru", "por": "pt", "ara": "ar", "ita": "it", "hin": "hi", "ind": "id", "tha": "th", "tur": "tr",
	"ukr": "uk", "vie": "vi", "ces": "cs", "cze": "cs", "dan": "da", "tgl": "fil", "tl": "fil", "fin": "fi",
	"isl": "is", "ice": "is", "msa": "ms", "may": "ms", "zsm": "ms", "nor": "no", "nob": "no", "nb": "no",
	"nno": "no", "nn": "no", "pol": "pl", "swe": "sv",
}

// asrLanguage normalises a GA language code to a Bailian one. It returns "" (automatic
// language detection) for codes Bailian does not know, rather than letting the upstream
// reject the whole session configuration.
func asrLanguage(code string) string {
	c := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(code, "_", "-")))
	if c == "" {
		return ""
	}

	if alias, ok := asrLanguageAliases[c]; ok {
		return alias
	}

	if asrLanguages[c] {
		return c
	}

	if base, _, found := strings.Cut(c, "-"); found {
		if alias, ok := asrLanguageAliases[base]; ok {
			return alias
		}

		if asrLanguages[base] {
			return base
		}
	}

	return ""
}

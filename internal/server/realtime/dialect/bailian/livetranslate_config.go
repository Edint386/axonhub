package bailian

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	// LiveTranslateDialectName is reported by Name() for OpenAI translation sessions relayed to
	// Qwen-LiveTranslate. The "bailian" prefix gives it the Bailian session-duration limit.
	LiveTranslateDialectName = "bailian_livetranslate"

	// DefaultLiveTranslateVoice is the upstream voice used for spoken translations. The upstream
	// announces "Chelsie" in session.created and then fails the first response with "Voice
	// 'Chelsie' is not supported" when no voice is set (live probe 2026-11-05); "Tina" works.
	DefaultLiveTranslateVoice = "Tina"

	// liveTranslateInitEventID tags the one session.update the adapter sends upstream.
	liveTranslateInitEventID = "axh_lt_init"

	// liveTranslateInputRate is the PCM rate sent upstream (the documented 3.8 input rate).
	liveTranslateInputRate = 16000

	// liveTranslateOutputRate is the PCM rate of the upstream's translated speech; it equals the
	// OpenAI translation protocol's rate, so output audio passes through unchanged.
	liveTranslateOutputRate = 24000

	// defaultTranslationLanguage is the upstream's own default target language.
	defaultTranslationLanguage = "en"
)

// liveTranslateAdmission is the model-name pattern of the Qwen-LiveTranslate realtime models.
// They speak their own protocol and are only served on the translation endpoint. Only the 3.8
// wire format is implemented: qwen3.5 / qwen3 livetranslate use different field names
// (modalities, top-level sample_rate, text+stash events), so they stay rejected.
var liveTranslateAdmission = regexp.MustCompile(`^qwen3\.8-livetranslate-flash-realtime`)

// SupportedLiveTranslate reports whether model is a Qwen-LiveTranslate realtime model the
// translation dialect can relay.
func SupportedLiveTranslate(model string) bool {
	return liveTranslateAdmission.MatchString(strings.ToLower(strings.TrimSpace(model)))
}

// Target languages of qwen3.8-livetranslate-flash-realtime (zh and en guides, 2026-11-05).
// Audio languages are translated to text and speech; text languages to text only.
var (
	liveTranslateAudioLanguages = stringSet(
		"zh", "en", "ar", "de", "fr", "es", "pt", "id", "it", "ko", "ru", "th", "vi", "ja", "tr",
		"hi", "ms", "nl", "ur", "nb", "sv", "da", "he", "fi", "pl", "is", "cs", "fil", "fa",
	)
	liveTranslateTextLanguages = stringSet(
		"yue", "el", "af", "ast", "be", "bg", "bn", "bs", "ca", "ceb", "et", "gl", "gu", "hr", "hu",
		"jv", "kk", "kn", "ky", "lv", "mk", "ml", "mr", "pa", "ro", "sk", "sl", "sw", "tg", "az", "uk",
	)

	// liveTranslateLanguageAliases maps BCP-47 spellings and legacy ISO codes to Bailian codes.
	// Tags are looked up without their script subtag, so zh-Hant-HK is zh-HK; other region
	// subtags fall back to their primary language (pt-BR -> pt). Hong Kong and Macau Chinese
	// are Cantonese, in every spelling, which the upstream translates to text only.
	liveTranslateLanguageAliases = map[string]string{
		"cmn": "zh", "zh-cn": "zh", "zh-sg": "zh", "zh-tw": "zh", "zh-hans": "zh", "zh-hant": "zh",
		"zh-hk": "yue", "zh-mo": "yue", "zh-yue": "yue",
		"no": "nb", "nob": "nb", "tl": "fil", "iw": "he", "in": "id",
	}
)

func stringSet(values ...string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}

	return set
}

// translationLanguage normalises an OpenAI/BCP-47 language code to a Bailian target code. ok
// is false for languages the upstream cannot translate into. Every spelling of one locale
// resolves the same way: the script subtag is ignored (zh-Hant-HK is zh-HK), then the tag is
// shortened one subtag at a time until a code or alias matches (zh-yue-HK is zh-yue).
func translationLanguage(code string) (string, bool) {
	known := func(c string) (string, bool) {
		if alias, ok := liveTranslateLanguageAliases[c]; ok {
			c = alias
		}

		return c, liveTranslateAudioLanguages[c] || liveTranslateTextLanguages[c]
	}

	subtags := languageSubtags(code)
	for n := len(subtags); n > 0; n-- {
		if lang, ok := known(strings.Join(subtags[:n], "-")); ok {
			return lang, true
		}
	}

	return "", false
}

// languageSubtags splits a language tag into its lower-case subtags without the script subtag
// (four letters, e.g. Hant or Latn), which never decides the target language.
func languageSubtags(code string) []string {
	c := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(code, "_", "-")))
	if c == "" {
		return nil
	}

	parts := strings.Split(c, "-")
	subtags := []string{parts[0]}

	for _, p := range parts[1:] {
		if len(p) == 4 && strings.Trim(p, "abcdefghijklmnopqrstuvwxyz") == "" {
			continue
		}

		subtags = append(subtags, p)
	}

	return subtags
}

// translateConfig is the effective translation configuration the client sees.
type translateConfig struct {
	// language is the Bailian target code; clientLanguage the spelling the client chose, echoed
	// back to it ("" until it chooses one).
	language       string
	clientLanguage string
	// transcription and noiseReduction are the client's objects, echoed back; nil is null. The
	// upstream always transcribes the source speech (for free), so transcription only decides
	// whether the client receives it; noise reduction has no upstream counterpart.
	transcription  json.RawMessage
	noiseReduction json.RawMessage
}

func defaultTranslateConfig() translateConfig {
	return translateConfig{language: defaultTranslationLanguage}
}

// spoken reports whether the target language is translated to speech as well as text.
func (c translateConfig) spoken() bool {
	return liveTranslateAudioLanguages[c.language]
}

func (c translateConfig) outputLanguage() string {
	if c.clientLanguage != "" {
		return c.clientLanguage
	}

	return c.language
}

// mergeTranslateUpdate applies an OpenAI translation session.update (session.audio.output.
// language, session.audio.input.transcription, session.audio.input.noise_reduction) to cfg.
// The update is validated as a whole: on a rejection nothing is applied.
func mergeTranslateUpdate(cfg translateConfig, session gjson.Result, models ...string) (translateConfig, *rejection) {
	if !session.Exists() || session.Type == gjson.Null {
		return cfg, nil
	}

	if !session.IsObject() {
		return cfg, invalidValue("session", "session must be an object.")
	}

	if t := session.Get("type"); t.Exists() && t.String() != "translation" {
		return cfg, invalidValue("session.type", fmt.Sprintf("This is a translation session; session.type %q is not available.", t.String()))
	}

	if m := session.Get("model"); m.Exists() && !containsString(models, m.String()) {
		return cfg, invalidValue("session.model", "The model of a translation session cannot be changed; reconnect with another model.")
	}

	if lang := session.Get("audio.output.language"); lang.Exists() {
		switch {
		case lang.Type == gjson.Null:
			cfg.language, cfg.clientLanguage = defaultTranslationLanguage, ""
		case lang.Type != gjson.String:
			return cfg, invalidValue("session.audio.output.language", "audio.output.language must be a language code string.")
		default:
			code, ok := translationLanguage(lang.String())
			if !ok {
				return cfg, invalidValue("session.audio.output.language", fmt.Sprintf("Unsupported translation output language %q.", lang.String()))
			}

			cfg.language, cfg.clientLanguage = code, lang.String()
		}
	}

	for _, field := range []struct {
		path string
		dst  *json.RawMessage
	}{
		{"audio.input.transcription", &cfg.transcription},
		{"audio.input.noise_reduction", &cfg.noiseReduction},
	} {
		v := session.Get(field.path)

		switch {
		case !v.Exists():
		case v.Type == gjson.Null:
			*field.dst = nil
		case v.IsObject():
			*field.dst = rawCopy(v)
		default:
			return cfg, invalidValue("session."+field.path, field.path+" must be an object or null.")
		}
	}

	return cfg, nil
}

func invalidValue(param, message string) *rejection {
	return &rejection{errType: "invalid_request_error", code: "invalid_value", param: param, message: message}
}

func containsString(values []string, v string) bool {
	for _, candidate := range values {
		if candidate != "" && candidate == v {
			return true
		}
	}

	return false
}

// renderSession renders cfg as the OpenAI RealtimeTranslationSession object.
func (c translateConfig) renderSession(id, model string, expiresAt int64) map[string]any {
	var expires any
	if expiresAt > 0 {
		expires = expiresAt
	}

	return map[string]any{
		"id":         id,
		"type":       "translation",
		"model":      model,
		"expires_at": expires,
		"audio": map[string]any{
			"input": map[string]any{
				"transcription":   rawOrNull(c.transcription),
				"noise_reduction": rawOrNull(c.noiseReduction),
			},
			"output": map[string]any{"language": c.outputLanguage()},
		},
	}
}

func rawOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}

	return raw
}

// initUpdate builds the single upstream session.update (qwen3.8 field names; the upstream
// rejects a second update with 1007). Spoken target languages need an explicit voice; text-only
// languages are requested as text only.
func (c translateConfig) initUpdate(voice string) []byte {
	modalities := []string{"text"}
	if c.spoken() {
		modalities = append(modalities, "audio")
	}

	b, _ := json.Marshal(map[string]any{
		"event_id": liveTranslateInitEventID,
		"type":     "session.update",
		"session": map[string]any{
			"output_modalities": modalities,
			"voice":             voice,
			"audio":             map[string]any{"input": map[string]any{"format": pcmFormat(liveTranslateInputRate)}},
			"translation":       map[string]any{"language": c.language},
		},
	})

	return b
}

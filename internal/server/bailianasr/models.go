package bailianasr

import "strings"

// family groups the allowlisted models by how DashScope serves them.
type family int

const (
	familyFlash family = iota + 1
	familyQwenFiletrans
	familyParaformer
	familyParaformer8k
	// familyMessage is qwen-audio-3.1-asr-flash-message: one task on the inference WebSocket
	// (run-task, binary audio, finish-task), run inside the executor.
	familyMessage
)

var modelFamilies = map[string]family{
	"qwen-audio-3.1-asr-flash":           familyFlash,
	"qwen-audio-3.1-asr-flash-filetrans": familyQwenFiletrans,
	"paraformer-v2":                      familyParaformer,
	"paraformer-8k-v2":                   familyParaformer8k,
	"qwen-audio-3.1-asr-flash-message":   familyMessage,
}

// SupportsModel reports whether model (the upstream model name after channel model mapping)
// is transcribed by this package instead of the wrapped outbound.
func SupportsModel(model string) bool {
	_, ok := modelFamilies[model]
	return ok
}

// SupportsLargeUpload reports whether model runs as an async file job, which reads its audio from
// an AudioSource without holding it in memory. Only these models may receive audio above the
// upstream multipart caps (see internal/server/api/bailian_transcription_upload.go).
func SupportsLargeUpload(model string) bool {
	f, ok := modelFamilies[model]
	return ok && f.async()
}

// SupportsURLInput reports whether model can transcribe a public http(s) URL that DashScope
// downloads itself (async jobs take it as input.file_urls, the flash model as input_audio.data).
// The -message model needs the audio bytes on its WebSocket, so it takes no URL.
func SupportsURLInput(model string) bool {
	f, ok := modelFamilies[model]
	return ok && f != familyMessage
}

// StreamsWithKeepAlive reports whether a stream=true request for model commits its SSE response
// before the transcript is ready (async jobs right after submit, -message right after the task
// started), so keep-alive comments can cover the wait.
func StreamsWithKeepAlive(model string) bool {
	f, ok := modelFamilies[model]
	return ok && (f.async() || f == familyMessage)
}

func familyOf(model string) (family, bool) {
	f, ok := modelFamilies[model]
	return f, ok
}

func (f family) async() bool {
	return f == familyQwenFiletrans || f.paraformer()
}

func (f family) paraformer() bool {
	return f == familyParaformer || f == familyParaformer8k
}

// diarizationParam is the DashScope parameter that turns on speaker labels.
func (f family) diarizationParam() string {
	if f.paraformer() {
		return "diarization_enabled"
	}

	return "speaker_diarization_enabled"
}

// paraformerV2Languages is the language_hints set paraformer-v2 accepts.
var paraformerV2Languages = languageSet("zh en ja yue ko de fr ru")

// qwenAudioLanguages are the 30 base language_hints codes of qwen-audio-3.1 (filetrans and
// -message): no yue, and Filipino is tl.
var qwenAudioLanguages = languageSet("zh en ja ko vi th id ms tl hi ar fr de es pt ru it nl sv da fi no el pl cs hu ro bg hr sk")

// qwenAudioFlashLanguages adds the codes only qwen-audio-3.1-asr-flash takes.
var qwenAudioFlashLanguages = languageSet("zh en ja ko vi th id ms tl hi ar fr de es pt ru it nl sv da fi no el pl cs hu ro bg hr sk " +
	"tr fa mr ml ne ky az ka hy sl sq et lv lt ca eu gl or dv as ku tig fy ba be tt ia mhr tok")

// languageAliases maps other spellings of a language (ISO 639-2/3 codes, Norwegian Bokmål and
// Nynorsk, Filipino, the old Indonesian code) to the code DashScope uses.
var languageAliases = map[string]string{
	"nb": "no", "nn": "no", "nob": "no", "nno": "no", "nor": "no",
	"fil": "tl", "tgl": "tl", "in": "id",
	"cmn": "zh", "zho": "zh", "chi": "zh", "eng": "en", "jpn": "ja", "kor": "ko", "vie": "vi", "tha": "th",
	"ind": "id", "msa": "ms", "may": "ms", "zsm": "ms", "hin": "hi", "ara": "ar", "fra": "fr", "fre": "fr",
	"deu": "de", "ger": "de", "spa": "es", "por": "pt", "rus": "ru", "ita": "it", "nld": "nl", "dut": "nl",
	"swe": "sv", "dan": "da", "fin": "fi", "ell": "el", "gre": "el", "pol": "pl", "ces": "cs", "cze": "cs",
	"hun": "hu", "ron": "ro", "rum": "ro", "bul": "bg", "hrv": "hr", "slk": "sk", "slo": "sk", "tur": "tr",
}

func languageSet(codes string) map[string]bool {
	set := map[string]bool{}
	for _, code := range strings.Fields(codes) {
		set[code] = true
	}

	return set
}

// languages is the language_hints set of the family (nil: the model takes no hint).
func (f family) languages() map[string]bool {
	switch f {
	case familyParaformer:
		return paraformerV2Languages
	case familyFlash:
		return qwenAudioFlashLanguages
	case familyQwenFiletrans, familyMessage:
		return qwenAudioLanguages
	default:
		return nil
	}
}

// languageHints returns the language_hints value for a normalized language code, or nil when
// the model takes none for it: paraformer-8k-v2 never does, and every other model only knows
// its own code list. An unknown code means automatic detection rather than a request DashScope
// would reject.
func (f family) languageHints(lang string) []string {
	if alias, ok := languageAliases[lang]; ok {
		lang = alias
	}

	if lang == "" || !f.languages()[lang] {
		return nil
	}

	return []string{lang}
}

// normalizeLanguage turns an ISO-639-1 or BCP-47 tag ("zh-CN") into the bare code DashScope uses.
func normalizeLanguage(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}

	return lang
}

var languageNames = map[string]string{
	"ar": "arabic", "bg": "bulgarian", "cs": "czech", "da": "danish", "de": "german",
	"el": "greek", "en": "english", "es": "spanish", "fi": "finnish", "fr": "french",
	"hi": "hindi", "hr": "croatian", "hu": "hungarian", "id": "indonesian", "it": "italian",
	"ja": "japanese", "ko": "korean", "ms": "malay", "nl": "dutch", "no": "norwegian",
	"pl": "polish", "pt": "portuguese", "ro": "romanian", "ru": "russian", "sk": "slovak",
	"sv": "swedish", "th": "thai", "tl": "tagalog", "tr": "turkish", "vi": "vietnamese",
	"yue": "cantonese", "zh": "chinese",
}

// languageName is the lower-case English language name OpenAI's verbose_json reports. DashScope
// detects no language, so it comes from the client's hint; unknown codes are returned as is.
func languageName(code string) string {
	if name, ok := languageNames[code]; ok {
		return name
	}

	return code
}

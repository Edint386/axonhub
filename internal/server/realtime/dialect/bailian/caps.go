package bailian

import (
	"regexp"
	"strings"
)

// admission is the model-name pattern the dialect accepts. Live-translate, qwen-audio and
// ASR models speak different protocols and are rejected before the upgrade.
var admission = regexp.MustCompile(`^qwen[\d.]*-omni-.*realtime`)

// Supported reports whether model is a Qwen-Omni-Realtime model this dialect can relay.
func Supported(model string) bool {
	return admission.MatchString(strings.ToLower(strings.TrimSpace(model)))
}

// Capabilities describes how one Qwen-Omni-Realtime model family differs on the wire.
type Capabilities struct {
	// NestedAudio means the model accepts the nested audio.input/output.format objects;
	// legacy models only take the flat input_audio_format/output_audio_format strings.
	NestedAudio bool
	// InputRate is the PCM sample rate sent upstream.
	InputRate int
	// Resample means client audio (always 24 kHz) is resampled to InputRate.
	Resample bool
	// SemanticVAD means turn_detection.type semantic_vad is honoured; otherwise it is
	// rewritten to server_vad.
	SemanticVAD bool
	// IdleTimeout means turn_detection.idle_timeout_ms is honoured for server_vad.
	IdleTimeout bool
	// TranscriptionModel is the fixed input transcription model of the family.
	TranscriptionModel string
	// OutputFormatSettable means audio.output.format may be sent in the init update.
	OutputFormatSettable bool
	// VoiceFamily selects the voice allow-list ("3.8", "3.5" or "legacy"); a voice outside
	// the family's list would make Bailian reject the whole session.update.
	VoiceFamily string
	// DefaultVoice is the family voice used when no usable voice is known: the upstream's
	// own default is not necessarily usable (qwen3.8 defaults to Chelsie and then rejects
	// every response with "Voice 'Chelsie' is not supported").
	DefaultVoice string
	// SessionFinish means the upstream accepts session.finish as the goodbye frame; qwen3.8
	// rejects it ("Invalid value: 'session.finish'") and only the close frame is sent.
	SessionFinish bool
}

const (
	// transcriptionModel is the only documented input transcription model.
	transcriptionModel = "qwen3-asr-flash-realtime"

	// clientRate is the only PCM rate the GA protocol allows in either direction.
	clientRate = 24000
)

// CapabilitiesFor returns the capability profile of a model family.
func CapabilitiesFor(model string) Capabilities {
	m := strings.ToLower(strings.TrimSpace(model))

	switch {
	case strings.HasPrefix(m, "qwen3.5-omni-"):
		return Capabilities{
			NestedAudio:          true,
			InputRate:            24000,
			Resample:             false,
			SemanticVAD:          true,
			IdleTimeout:          true,
			TranscriptionModel:   transcriptionModel,
			OutputFormatSettable: true,
			VoiceFamily:          "3.5",
			DefaultVoice:         "Tina",
			SessionFinish:        true,
		}
	case strings.HasPrefix(m, "qwen3.8-omni-"):
		// Live probe of qwen3.8-omni-flash-realtime (2026-10-22): 24 kHz input and output
		// formats are accepted and honoured, idle_timeout_ms is echoed, session.finish is
		// rejected and the default voice Chelsie fails at response time.
		return Capabilities{
			NestedAudio:          true,
			InputRate:            24000,
			Resample:             false,
			SemanticVAD:          true,
			IdleTimeout:          true,
			TranscriptionModel:   transcriptionModel,
			OutputFormatSettable: true,
			VoiceFamily:          "3.8",
			DefaultVoice:         "Tina",
			SessionFinish:        false,
		}
	default:
		// qwen3-omni-flash-realtime*, qwen-omni-turbo-realtime* and anything older speak the
		// legacy flat protocol at 16 kHz.
		return Capabilities{
			NestedAudio:          false,
			InputRate:            16000,
			Resample:             true,
			SemanticVAD:          false,
			IdleTimeout:          false,
			TranscriptionModel:   transcriptionModel,
			OutputFormatSettable: false,
			VoiceFamily:          "legacy",
			DefaultVoice:         "Cherry",
			SessionFinish:        true,
		}
	}
}

// Voice allow-lists (omni-voice-list digest), keyed by lower-case name per family. Unknown
// GA voices (alloy, ash, marin, …) and voices of another family are dropped so the whole
// update is not rejected by Bailian.
var (
	// sharedVoices are available on both 3.8 and 3.5.
	sharedVoices = []string{
		"Tina", "Cindy", "Liora", "Mira", "Liora Mira", "Raymond", "Katerina", "Ryan", "Mia",
		"Theo", "Calm", "Theo Calm", "Serena", "Maia", "Evan", "Qiao", "Momo", "Wil", "Angel",
		"Li", "Cassian", "Li Cassian", "Joyner", "Gold", "Jennifer", "Aiden", "Mione", "Sunny", "Dylan",
		"Eric", "Peter", "Joseph", "Joseph Chen", "Marcus", "Rocky", "Kiki", "Sohee", "Eliška", "Eliska",
		"Alek", "Arda", "Dolce", "Lenn", "Ono", "Anna", "Ono Anna", "Sonrisa", "Bodega", "Andre",
		"Radio", "Gol", "Radio Gol", "Rizky", "Roya", "Hana", "Jakub", "Griet", "Marina", "Siiri",
		"Ingrid", "Sigga", "Bea", "Chloe", "Emilien",
	}
	// voices38Only exist on qwen3.8 only.
	voices38Only = []string{"Zane", "Cici", "longanlingxin"}
	// voices35Only exist on qwen3.5 only.
	voices35Only = []string{"Sunnybobi", "Harvey", "Ethan"}
	// legacyVoices are the documented voices of qwen3-omni-flash-realtime and older.
	legacyVoices = []string{"Cherry", "Chelsie", "Serena", "Ethan"}

	voicesByFamily = map[string]map[string]string{
		"3.8":    voiceSet(sharedVoices, voices38Only),
		"3.5":    voiceSet(sharedVoices, voices35Only),
		"legacy": voiceSet(legacyVoices),
	}
)

func voiceSet(lists ...[]string) map[string]string {
	m := map[string]string{}

	for _, names := range lists {
		for _, n := range names {
			m[strings.ToLower(n)] = n
		}
	}

	return m
}

// canonicalVoice returns the allow-listed spelling of a voice for the family and whether it
// is known there.
func canonicalVoice(v string, caps Capabilities) (string, bool) {
	set, ok := voicesByFamily[caps.VoiceFamily]
	if !ok {
		set = voicesByFamily["3.8"]
	}

	c, ok := set[strings.ToLower(strings.TrimSpace(v))]

	return c, ok
}

// effectiveVoice returns the allow-listed spelling of v, or the family's DefaultVoice when v
// is empty or unknown there. Bailian accepts an unknown voice (its own default Chelsie on
// qwen3.8, GA voices such as alloy) in session.update and only fails when a response starts,
// so the adapter never lets one become the effective voice.
func effectiveVoice(v string, caps Capabilities) string {
	if c, ok := canonicalVoice(v, caps); ok {
		return c
	}

	return caps.DefaultVoice
}

package bailian

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// transcriptStitcher joins the upstream's per-segment transcript deltas (one item per spoken
// segment) into the single continuous, append-only transcript stream of the OpenAI translation
// protocol. Within a segment deltas pass unchanged; a new segment gets one separating space when
// the text on both sides of the boundary is written with spaces, and a segment's final text
// contributes whatever its deltas left out (the upstream ends a sentence's final text with the
// space its deltas omit: "... park." vs "... park. ").
type transcriptStitcher struct {
	// current is the key of the open segment and text what was emitted for it.
	current string
	open    bool
	text    string
	// tail is the last rune emitted on the stream (0 before the first delta).
	tail rune
	// started holds segments with deltas whose final text has not arrived yet.
	started map[string]bool
}

// delta returns the text to emit for a delta of segment key; it may be empty.
func (s *transcriptStitcher) delta(key, delta string) string {
	if delta == "" {
		return ""
	}

	prefix := ""

	if !s.open || key != s.current {
		s.current, s.open, s.text = key, true, ""

		if s.started == nil {
			s.started = map[string]bool{}
		}

		s.started[key] = true

		if needsSeparator(s.tail, delta) {
			prefix = " "
		}
	}

	s.text += delta

	return s.emit(prefix + delta)
}

// done closes segment key with its final text and returns the text still to emit: the missing
// suffix when the final text extends the emitted deltas, the whole text for a segment that had
// no deltas, and nothing when the deltas diverged (an append-only stream cannot retract).
func (s *transcriptStitcher) done(key, final string) string {
	switch {
	case s.open && key == s.current:
		s.open = false
		delete(s.started, key)

		if len(final) > len(s.text) && strings.HasPrefix(final, s.text) {
			return s.emit(final[len(s.text):])
		}

		return ""
	case s.started[key]:
		// An older segment finishing after a newer one started: its suffix cannot be appended.
		delete(s.started, key)
		return ""
	case final == "":
		return ""
	default:
		out := s.delta(key, final)
		s.open = false
		delete(s.started, key)

		return out
	}
}

func (s *transcriptStitcher) emit(text string) string {
	if r, _ := utf8.DecodeLastRuneInString(text); r != utf8.RuneError {
		s.tail = r
	}

	return text
}

// needsSeparator reports whether a space must separate the stream's last rune from a new
// segment starting with next.
func needsSeparator(tail rune, next string) bool {
	if tail == 0 {
		return false
	}

	first, _ := utf8.DecodeRuneInString(next)

	switch {
	case unicode.IsSpace(tail) || unicode.IsSpace(first):
		return false
	case unspacedScript(tail) || unspacedScript(first):
		return false
	case unicode.In(first, unicode.Pe, unicode.Pf) || strings.ContainsRune(",.;:!?%", first):
		return false
	}

	return true
}

// unspacedScript reports runes of scripts written without spaces between sentences (Chinese,
// Japanese, Thai, Lao, Myanmar, Khmer) and full-width punctuation.
func unspacedScript(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai, unicode.Lao, unicode.Myanmar, unicode.Khmer) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

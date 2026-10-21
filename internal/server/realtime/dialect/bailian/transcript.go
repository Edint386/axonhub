package bailian

import "strings"

// transcriptTracker turns Bailian's per-sentence confirmed-prefix transcription deltas into
// GA incremental deltas.
//
// Bailian's delta carries "text", the confirmed prefix of the current sentence, and "stash",
// a provisional suffix. Confirmed text never changes, so a text that no longer extends the
// last confirmed text starts a new sentence. The tracker keeps base (text emitted for
// finished sentences) and last (the current sentence's confirmed text).
type transcriptTracker struct {
	base string
	last string
}

// delta returns the GA delta to emit for a new confirmed text; it may be empty.
func (t *transcriptTracker) delta(text string) string {
	if strings.HasPrefix(text, t.last) {
		d := text[len(t.last):]
		t.last = text

		return d
	}

	// Confirmed text never changes, so anything that does not extend it is a new sentence.
	t.base += t.last
	t.last = text

	return text
}

// complete returns the catch-up delta for the final transcript, if the transcript extends
// what was already emitted.
func (t *transcriptTracker) complete(transcript string) string {
	emitted := t.base + t.last
	if len(transcript) > len(emitted) && strings.HasPrefix(transcript, emitted) {
		return transcript[len(emitted):]
	}

	return ""
}

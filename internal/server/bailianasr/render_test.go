package bailianasr

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "rewrite the render golden files")

func fileTranscript(t *testing.T, result string, usage dsUsage) *transcript {
	t.Helper()

	tr, err := parseFileResult([]byte(result))
	require.NoError(t, err)

	tr.ID = fakeTaskID
	tr.Usage = usage

	return tr
}

func flashTranscript(t *testing.T) *transcript {
	t.Helper()

	tr, err := parseFlashBody([]byte(flashBody))
	require.NoError(t, err)

	return tr
}

// TestRender_Golden pins every response format against the OpenAI shapes
// (https://developers.openai.com/api/reference/resources/audio/subresources/transcriptions/methods/create).
// Run with -update to rewrite testdata/golden after an intended change.
func TestRender_Golden(t *testing.T) {
	paraformer := fileTranscript(t, paraformerResult(""), dsUsage{Duration: 8})
	paraformerDiarized := fileTranscript(t, paraformerResult(`,"speaker_id":0`), dsUsage{Duration: 8})
	qwen := fileTranscript(t, qwenFiletransResult, dsUsage{Duration: 6, InputTokens: 100, OutputTokens: 18, TotalTokens: 118})
	flash := flashTranscript(t)
	empty := &transcript{ID: fakeTaskID, Duration: 3}

	tests := []struct {
		name string
		tr   *transcript
		opts renderOptions
		ct   string
	}{
		{"paraformer.json", paraformer, renderOptions{Format: formatJSON}, contentTypeJSON},
		{"paraformer.text", paraformer, renderOptions{Format: formatText}, contentTypeText},
		{"paraformer.srt", paraformer, renderOptions{Format: formatSRT}, contentTypeText},
		{"paraformer.vtt", paraformer, renderOptions{Format: formatVTT}, contentTypeText},
		{"paraformer.verbose_json", paraformer, renderOptions{Format: formatVerboseJSON, Segments: true, Words: true, Language: "zh"}, contentTypeJSON},
		{"paraformer.diarized_json", paraformerDiarized, renderOptions{Format: formatDiarizedJSON}, contentTypeJSON},
		{"qwen-filetrans.json", qwen, renderOptions{Format: formatJSON}, contentTypeJSON},
		{"qwen-filetrans.srt", qwen, renderOptions{Format: formatSRT}, contentTypeText},
		{"qwen-filetrans.vtt", qwen, renderOptions{Format: formatVTT}, contentTypeText},
		{"qwen-filetrans.diarized_json", qwen, renderOptions{Format: formatDiarizedJSON}, contentTypeJSON},
		{"qwen-filetrans.verbose_json-words", qwen, renderOptions{Format: formatVerboseJSON, Words: true, Language: "en"}, contentTypeJSON},
		{"flash.json", flash, renderOptions{Format: formatJSON}, contentTypeJSON},
		{"flash.verbose_json", flash, renderOptions{Format: formatVerboseJSON, Segments: true}, contentTypeJSON},
		{"flash.srt", flash, renderOptions{Format: formatSRT}, contentTypeText},
		{"empty.text", empty, renderOptions{Format: formatText}, contentTypeText},
		{"empty.srt", empty, renderOptions{Format: formatSRT}, contentTypeText},
		{"empty.vtt", empty, renderOptions{Format: formatVTT}, contentTypeText},
		{"empty.verbose_json", empty, renderOptions{Format: formatVerboseJSON, Segments: true}, contentTypeJSON},
		{"empty.diarized_json", empty, renderOptions{Format: formatDiarizedJSON}, contentTypeJSON},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType, err := render(tt.tr, tt.opts)
			require.NoError(t, err)
			require.Equal(t, tt.ct, contentType)
			require.NotEmpty(t, body, "an empty body would trip the pipeline's empty-response retry")

			path := filepath.Join("testdata", "golden", tt.name)
			if *updateGolden {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, body, 0o600))
			}

			want, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, string(want), string(body))
		})
	}
}

func TestSpeakerLabels(t *testing.T) {
	labels := speakerLabels{}
	one, two := 1, 0

	require.Equal(t, "A", labels.label(0, &one))
	require.Equal(t, "B", labels.label(0, &two))
	require.Equal(t, "A", labels.label(0, &one))
	require.Equal(t, "C", labels.label(1, nil))
	require.Equal(t, "AA", alphabeticLabel(26))
	require.Equal(t, "AB", alphabeticLabel(27))
}

func TestSegmentDeltasAlwaysRebuildTheText(t *testing.T) {
	tr := fileTranscript(t, qwenFiletransResult, dsUsage{})
	tr.Text = "前言 " + tr.Text + " 结束"

	deltas := segmentDeltas(tr)
	require.Len(t, deltas, 2)

	joined := ""
	for _, delta := range deltas {
		joined += delta
	}

	require.Equal(t, tr.Text, joined)
}

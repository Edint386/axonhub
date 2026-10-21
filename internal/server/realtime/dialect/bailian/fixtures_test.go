package bailian

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// fixture is one golden scenario under testdata/: a model, then a sequence of frames from
// either side with the exact output each must produce.
type fixture struct {
	Model             string `json:"model"`
	RequestedModel    string `json:"requested_model"`
	WantTranscription bool   `json:"want_transcription"`
	// Init runs the standard init handshake before the steps so fixtures that are not about
	// initialisation stay short.
	Init  bool   `json:"init"`
	Steps []step `json:"steps"`
}

type step struct {
	Name      string          `json:"name"`
	Client    json.RawMessage `json:"client"`
	ClientRaw string          `json:"client_raw"`
	Upstream  json.RawMessage `json:"upstream"`
	Expect    expect          `json:"expect"`
}

type expect struct {
	ToUpstream      []json.RawMessage `json:"to_upstream"`
	ToClient        []json.RawMessage `json:"to_client"`
	ToClientTypes   []string          `json:"to_client_types"`
	ToUpstreamTypes []string          `json:"to_upstream_types"`
	OpenGate        bool              `json:"open_gate"`
	Close           *closeExpect      `json:"close"`
	InProgress      *bool             `json:"response_in_progress"`
	Usage           []usageExpect     `json:"usage"`
	Err             bool              `json:"error"`
}

type closeExpect struct {
	Code int `json:"code"`
}

type usageExpect struct {
	Kind            string          `json:"kind"`
	ResponseID      string          `json:"response_id"`
	ItemID          string          `json:"item_id"`
	Model           string          `json:"model"`
	DurationSeconds float64         `json:"duration_seconds"`
	PromptTokens    int64           `json:"prompt_tokens"`
	CompletionTok   int64           `json:"completion_tokens"`
	TotalTokens     int64           `json:"total_tokens"`
	PromptAudio     int64           `json:"prompt_audio_tokens"`
	CompletionAudio int64           `json:"completion_audio_tokens"`
	NilUsage        bool            `json:"nil_usage"`
	Summary         json.RawMessage `json:"summary"`
}

// initFrames is the standard happy-path init exchange used when a fixture sets "init".
var initFrames = struct {
	created, updated string
}{
	created: `{"event_id":"event_up_1","type":"session.created","session":{"id":"sess_1","object":"realtime.session","model":"MODEL","modalities":["text","audio"],"voice":"Tina","input_audio_format":"pcm","output_audio_format":"pcm","input_audio_transcription":{"model":"qwen3-asr-flash-realtime"},"turn_detection":{"type":"server_vad","threshold":0.5,"prefix_padding_ms":300,"silence_duration_ms":800,"create_response":true,"interrupt_response":true}}}`,
	updated: `{"event_id":"event_up_2","type":"session.updated","session":{}}`,
}

// newTestDialect builds a dialect with a fresh id generator.
func newTestDialect(t *testing.T, model, requested string, wantTranscription bool) *Dialect {
	t.Helper()

	d, err := New(Options{
		RequestedModel:    requested,
		ActualModel:       model,
		IDs:               &dialect.EventIDs{},
		WantTranscription: wantTranscription,
	})
	require.NoError(t, err)

	bd, ok := d.(*Dialect)
	require.True(t, ok)

	return bd
}

// runInit drives the happy-path init and asserts the gate opened.
func runInit(t *testing.T, d *Dialect, model string) {
	t.Helper()

	toUpstream, gated := d.Start()
	require.Nil(t, toUpstream)
	require.True(t, gated)

	out, err := d.UpstreamFrame([]byte(strings.ReplaceAll(initFrames.created, "MODEL", model)))
	require.NoError(t, err)
	require.Len(t, out.ToUpstream, 1)
	require.Equal(t, "axh_init_1", gjson.GetBytes(out.ToUpstream[0], "event_id").String())
	require.False(t, out.OpenGate)

	out, err = d.UpstreamFrame([]byte(initFrames.updated))
	require.NoError(t, err)
	require.True(t, out.OpenGate)
	require.Len(t, out.ToClient, 1)
	require.Equal(t, "session.created", dialect.EventType(out.ToClient[0]))
}

func TestFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)

			var fx fixture
			require.NoError(t, json.Unmarshal(raw, &fx), "fixture must be valid JSON")

			d := newTestDialect(t, fx.Model, fx.RequestedModel, fx.WantTranscription)

			if fx.Init {
				runInit(t, d, fx.Model)
			}

			for i, st := range fx.Steps {
				name := st.Name
				if name == "" {
					name = "step " + itoa(i)
				}

				var (
					out dialect.Output
					err error
				)

				switch {
				case st.ClientRaw != "":
					out, err = d.ClientFrame([]byte(st.ClientRaw))
				case st.Client != nil:
					out, err = d.ClientFrame(st.Client)
				case st.Upstream != nil:
					out, err = d.UpstreamFrame(st.Upstream)
				default:
					t.Fatalf("%s: step has neither client nor upstream frame", name)
				}

				if st.Expect.Err {
					require.Error(t, err, name)

					continue
				}

				require.NoError(t, err, name)
				checkFrames(t, name+" to_upstream", st.Expect.ToUpstream, st.Expect.ToUpstreamTypes, out.ToUpstream)
				checkFrames(t, name+" to_client", st.Expect.ToClient, st.Expect.ToClientTypes, out.ToClient)
				require.Equal(t, st.Expect.OpenGate, out.OpenGate, "%s: open_gate", name)

				if st.Expect.Close == nil {
					require.Nil(t, out.Close, "%s: unexpected close", name)
				} else {
					require.NotNil(t, out.Close, "%s: expected close", name)
					require.Equal(t, st.Expect.Close.Code, out.Close.Code, "%s: close code", name)
				}

				if st.Expect.InProgress == nil {
					require.Nil(t, out.ResponseInProgress, "%s: response_in_progress", name)
				} else {
					require.NotNil(t, out.ResponseInProgress, "%s: response_in_progress", name)
					require.Equal(t, *st.Expect.InProgress, *out.ResponseInProgress, "%s: response_in_progress", name)
				}

				checkUsage(t, name, st.Expect.Usage, out.Usage)
			}
		})
	}
}

// checkFrames compares produced frames with the expectation: exact JSON, or types only.
func checkFrames(t *testing.T, name string, want []json.RawMessage, wantTypes []string, got [][]byte) {
	t.Helper()

	switch {
	case want != nil:
		gotStrs := make([]string, 0, len(got))
		for _, g := range got {
			gotStrs = append(gotStrs, string(g))
		}

		require.Len(t, got, len(want), "%s: frame count; got %s", name, strings.Join(gotStrs, "\n"))

		for i := range want {
			require.JSONEq(t, string(want[i]), string(got[i]), "%s[%d]", name, i)
		}
	case wantTypes != nil:
		types := make([]string, 0, len(got))
		for _, g := range got {
			types = append(types, dialect.EventType(g))
		}

		require.Equal(t, wantTypes, types, name)
	default:
		require.Empty(t, got, "%s: expected no frames, got %d", name, len(got))
	}
}

// checkUsage compares usage records; Summary is compared only when the fixture gives one,
// and must never carry audio bytes.
func checkUsage(t *testing.T, name string, want []usageExpect, got []dialect.UsageRecord) {
	t.Helper()

	require.Len(t, got, len(want), "%s: usage count", name)

	for i, w := range want {
		g := got[i]
		require.Equal(t, w.Kind, g.Kind, "%s: usage kind", name)
		require.Equal(t, w.ResponseID, g.ResponseID, "%s: usage response id", name)
		require.Equal(t, w.ItemID, g.ItemID, "%s: usage item id", name)
		require.Equal(t, w.Model, g.Model, "%s: usage model", name)
		require.InDelta(t, w.DurationSeconds, g.DurationSeconds, 1e-9, "%s: duration", name)

		if w.NilUsage {
			require.Nil(t, g.Usage, "%s: usage tokens", name)
		} else {
			require.NotNil(t, g.Usage, "%s: usage tokens", name)
			require.Equal(t, w.PromptTokens, g.Usage.PromptTokens, "%s: prompt tokens", name)
			require.Equal(t, w.CompletionTok, g.Usage.CompletionTokens, "%s: completion tokens", name)
			require.Equal(t, w.TotalTokens, g.Usage.TotalTokens, "%s: total tokens", name)

			if w.PromptAudio != 0 {
				require.NotNil(t, g.Usage.PromptTokensDetails)
				require.Equal(t, w.PromptAudio, g.Usage.PromptTokensDetails.AudioTokens, "%s: prompt audio", name)
			}

			if w.CompletionAudio != 0 {
				require.NotNil(t, g.Usage.CompletionTokensDetails)
				require.Equal(t, w.CompletionAudio, g.Usage.CompletionTokensDetails.AudioTokens, "%s: completion audio", name)
			}
		}

		require.True(t, gjson.ValidBytes(g.Summary), "%s: summary must be JSON", name)
		require.False(t, hasAudioPayload(gjson.ParseBytes(g.Summary)), "%s: summary carries audio bytes", name)

		if w.Summary != nil {
			require.JSONEq(t, string(w.Summary), string(g.Summary), "%s: summary", name)
		}
	}
}

// hasAudioPayload walks a JSON value looking for a string-valued "audio" or "delta" key.
func hasAudioPayload(v gjson.Result) bool {
	found := false

	v.ForEach(func(key, value gjson.Result) bool {
		if (key.String() == "audio" || key.String() == "delta") && value.Type == gjson.String {
			found = true

			return false
		}

		if value.IsObject() || value.IsArray() {
			if hasAudioPayload(value) {
				found = true

				return false
			}
		}

		return true
	})

	return found
}

package dialect

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
)

const (
	gaTestRequestedModel = "my-realtime-alias"
	gaTestActualModel    = "gpt-realtime-2.1"
)

// Fixtures are plain GA wire frames. Audio payloads are short base64 strings.
const (
	gaFixtureClientAudioAppend = `{"type":"input_audio_buffer.append","event_id":"evt_c1","audio":"AAECAwQFBgcICQ=="}`

	gaFixtureUpstreamAudioDelta = `{"type":"response.output_audio.delta","event_id":"evt_u1","response_id":"resp_1","item_id":"item_1","output_index":0,"content_index":0,"delta":"AAECAwQFBgcICQ=="}`

	gaFixtureSessionUpdateWithModel = `{"type":"session.update","event_id":"evt_c2","session":{"type":"realtime","model":"my-realtime-alias","instructions":"Be brief.","audio":{"input":{"transcription":{"model":"gpt-4o-transcribe"}}}}}`

	gaFixtureSessionUpdateNoModel = `{"type":"session.update","event_id":"evt_c3","session":{"type":"realtime","instructions":"Be brief."}}`

	gaFixtureSessionUpdateTranscription = `{"type":"session.update","event_id":"evt_c4","session":{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"transcription":{"model":"gpt-4o-mini-transcribe","language":"en"}}}}}`

	gaFixtureSessionCreatedWithModel = `{"type":"session.created","event_id":"evt_u2","session":{"id":"sess_1","object":"realtime.session","type":"realtime","model":"gpt-realtime-2.1","output_modalities":["audio"]}}`

	gaFixtureSessionUpdatedWithModel = `{"type":"session.updated","event_id":"evt_u3","session":{"id":"sess_1","object":"realtime.session","type":"realtime","model":"gpt-realtime-2.1","audio":{"input":{"transcription":{"model":"gpt-realtime-whisper"}}}}}`

	gaFixtureSessionCreatedNoModel = `{"type":"session.created","event_id":"evt_u4","session":{"id":"sess_2","object":"realtime.transcription_session","type":"transcription","audio":{"input":{"transcription":{"model":"gpt-4o-transcribe"}}}}}`

	gaFixtureResponseCreated = `{"type":"response.created","event_id":"evt_u5","response":{"id":"resp_1","object":"realtime.response","status":"in_progress","output":[]}}`

	gaFixtureResponseDoneSingular = `{"type":"response.done","event_id":"evt_u6","response":{"id":"resp_1","object":"realtime.response","status":"completed","conversation_id":"conv_1","output":[{"id":"item_1","object":"realtime.item","type":"message","role":"assistant","status":"completed","content":[{"type":"output_audio","audio":"QUJDREVG","transcript":"Hello there."}]}],"usage":{"input_tokens":120,"output_tokens":40,"total_tokens":160,"input_token_details":{"cached_tokens":10,"text_tokens":20,"audio_tokens":100,"image_tokens":0,"cached_tokens_details":{"text_tokens":5,"audio_tokens":5,"image_tokens":0}},"output_token_details":{"text_tokens":10,"audio_tokens":30}}}}`

	gaFixtureResponseDonePlural = `{"type":"response.done","event_id":"evt_u7","response":{"id":"resp_2","object":"realtime.response","status":"completed","output":[],"usage":{"input_tokens":30,"output_tokens":12,"input_tokens_details":{"cached_tokens":4,"text_tokens":6,"audio_tokens":24},"output_tokens_details":{"text_tokens":2,"audio_tokens":10}}}}`

	gaFixtureResponseDoneNoUsage = `{"type":"response.done","event_id":"evt_u8","response":{"id":"resp_3","object":"realtime.response","status":"cancelled","output":[]}}`

	gaFixtureResponseDoneMultiAudio = `{"type":"response.done","event_id":"evt_u9","response":{"id":"resp_4","object":"realtime.response","status":"completed","output":[{"id":"item_1","type":"message","role":"assistant","content":[{"type":"output_audio","audio":"QUJD","transcript":"first"},{"type":"output_text","text":"plain"},{"type":"output_audio","audio":"REVG","transcript":"second"}]},{"id":"item_2","type":"function_call","name":"lookup","arguments":"{}","call_id":"call_1"},{"id":"item_3","type":"message","role":"assistant","content":[{"type":"output_audio","audio":"R0hJ","transcript":"third"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`

	gaFixtureTranscriptionTokens = `{"type":"conversation.item.input_audio_transcription.completed","event_id":"evt_u10","item_id":"item_9","content_index":0,"transcript":"hello world","usage":{"type":"tokens","input_tokens":50,"output_tokens":5,"total_tokens":55,"input_token_details":{"text_tokens":2,"audio_tokens":48}}}`

	gaFixtureTranscriptionDuration = `{"type":"conversation.item.input_audio_transcription.completed","event_id":"evt_u11","item_id":"item_10","content_index":0,"transcript":"hello again","usage":{"type":"duration","seconds":12.5}}`

	gaFixtureTranscriptionNoUsage = `{"type":"conversation.item.input_audio_transcription.completed","event_id":"evt_u12","item_id":"item_11","content_index":0,"transcript":"no usage"}`
)

func newTestGA(rewrite bool) Dialect {
	return NewGA(GAOptions{
		RequestedModel: gaTestRequestedModel,
		ActualModel:    gaTestActualModel,
		RewriteModel:   rewrite,
		IDs:            &EventIDs{},
	})
}

// requireUntouched asserts that got is the very same slice as frame (no copy was made) and
// that frame still equals its original bytes.
func requireUntouched(t *testing.T, original, frame, got []byte) {
	t.Helper()

	require.Equal(t, original, frame, "input frame must not be mutated")
	require.Equal(t, original, got, "output frame must be byte-identical")
	require.Same(t, &frame[0], &got[0], "output must reuse the input slice")
}

func TestGA_Basics(t *testing.T) {
	d := newTestGA(true)

	require.Equal(t, "ga", d.Name())

	toUpstream, gated := d.Start()
	require.Nil(t, toUpstream)
	require.False(t, gated)

	require.Nil(t, d.CloseUpstream())
}

func TestGA_AudioPassthrough(t *testing.T) {
	tests := []struct {
		name     string
		frame    string
		client   bool
		upstream bool
	}{
		{name: "client audio append", frame: gaFixtureClientAudioAppend, client: true},
		{name: "upstream audio delta", frame: gaFixtureUpstreamAudioDelta, upstream: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestGA(true)
			frame := []byte(tt.frame)
			original := bytes.Clone(frame)

			if tt.client {
				out, err := d.ClientFrame(frame)
				require.NoError(t, err)
				require.Len(t, out.ToUpstream, 1)
				requireUntouched(t, original, frame, out.ToUpstream[0])
				require.Empty(t, out.ToClient)
				require.Empty(t, out.Usage)
				require.Nil(t, out.ResponseInProgress)
				require.Nil(t, out.Close)
				require.False(t, out.OpenGate)
			}

			if tt.upstream {
				out, err := d.UpstreamFrame(frame)
				require.NoError(t, err)
				require.Len(t, out.ToClient, 1)
				requireUntouched(t, original, frame, out.ToClient[0])
				require.Empty(t, out.ToUpstream)
				require.Empty(t, out.Usage)
				require.Nil(t, out.ResponseInProgress)
				require.Nil(t, out.Close)
				require.False(t, out.OpenGate)
			}
		})
	}
}

func TestGA_ClientModelRewrite(t *testing.T) {
	tests := []struct {
		name      string
		rewrite   bool
		frame     string
		wantModel string
		untouched bool
	}{
		{
			name:      "session.update with model is rewritten to the actual model",
			rewrite:   true,
			frame:     gaFixtureSessionUpdateWithModel,
			wantModel: gaTestActualModel,
		},
		{
			name:      "session.update without model is untouched",
			rewrite:   true,
			frame:     gaFixtureSessionUpdateNoModel,
			untouched: true,
		},
		{
			name:      "transcription session.update has no model and is untouched",
			rewrite:   true,
			frame:     gaFixtureSessionUpdateTranscription,
			untouched: true,
		},
		{
			name:      "RewriteModel=false leaves the model alone",
			rewrite:   false,
			frame:     gaFixtureSessionUpdateWithModel,
			untouched: true,
		},
		{
			name:      "RewriteModel=false leaves a frame without model alone",
			rewrite:   false,
			frame:     gaFixtureSessionUpdateNoModel,
			untouched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestGA(tt.rewrite)
			frame := []byte(tt.frame)
			original := bytes.Clone(frame)

			out, err := d.ClientFrame(frame)
			require.NoError(t, err)
			require.Len(t, out.ToUpstream, 1)
			require.Empty(t, out.ToClient)
			require.Empty(t, out.Usage)
			require.Nil(t, out.ResponseInProgress)

			got := out.ToUpstream[0]

			if tt.untouched {
				requireUntouched(t, original, frame, got)

				return
			}

			require.Equal(t, original, frame, "input frame must not be mutated")
			require.Equal(t, tt.wantModel, gjson.GetBytes(got, "session.model").String())
			require.False(t, gjson.GetBytes(got, "model").Exists(), "no top-level model must appear")

			// Everything apart from the model must survive the rewrite.
			require.Equal(t, gjson.GetBytes(original, "event_id").String(), gjson.GetBytes(got, "event_id").String())
			require.Equal(t, gjson.GetBytes(original, "session.instructions").String(), gjson.GetBytes(got, "session.instructions").String())
			require.Equal(t, gjson.GetBytes(original, "session.audio").Raw, gjson.GetBytes(got, "session.audio").Raw)
		})
	}
}

func TestGA_UpstreamSessionEcho(t *testing.T) {
	tests := []struct {
		name      string
		rewrite   bool
		frame     string
		untouched bool
	}{
		{
			name:    "session.created echo is rewritten to the requested model",
			rewrite: true,
			frame:   gaFixtureSessionCreatedWithModel,
		},
		{
			name:    "session.updated echo is rewritten to the requested model",
			rewrite: true,
			frame:   gaFixtureSessionUpdatedWithModel,
		},
		{
			name:      "session.created without model is untouched",
			rewrite:   true,
			frame:     gaFixtureSessionCreatedNoModel,
			untouched: true,
		},
		{
			name:      "RewriteModel=false leaves session.created alone",
			rewrite:   false,
			frame:     gaFixtureSessionCreatedWithModel,
			untouched: true,
		},
		{
			name:      "RewriteModel=false leaves session.updated alone",
			rewrite:   false,
			frame:     gaFixtureSessionUpdatedWithModel,
			untouched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestGA(tt.rewrite)
			frame := []byte(tt.frame)
			original := bytes.Clone(frame)

			out, err := d.UpstreamFrame(frame)
			require.NoError(t, err)
			require.Len(t, out.ToClient, 1)
			require.Empty(t, out.ToUpstream)
			require.Empty(t, out.Usage)
			require.Nil(t, out.ResponseInProgress)

			got := out.ToClient[0]

			if tt.untouched {
				requireUntouched(t, original, frame, got)

				return
			}

			require.Equal(t, original, frame, "input frame must not be mutated")
			require.Equal(t, gaTestRequestedModel, gjson.GetBytes(got, "session.model").String())
			require.Equal(t, gjson.GetBytes(original, "event_id").String(), gjson.GetBytes(got, "event_id").String())
			require.Equal(t, gjson.GetBytes(original, "session.id").String(), gjson.GetBytes(got, "session.id").String())
			require.Equal(t, gjson.GetBytes(original, "session.type").String(), gjson.GetBytes(got, "session.type").String())
		})
	}
}

func TestGA_ResponseDoneUsage(t *testing.T) {
	tests := []struct {
		name       string
		frame      string
		wantRecord bool
		wantID     string
		wantUsage  *llm.Usage
	}{
		{
			name:       "GA singular token details",
			frame:      gaFixtureResponseDoneSingular,
			wantRecord: true,
			wantID:     "resp_1",
			wantUsage: &llm.Usage{
				PromptTokens:            120,
				CompletionTokens:        40,
				TotalTokens:             160,
				PromptTokensDetails:     &llm.PromptTokensDetails{AudioTokens: 100, CachedTokens: 10},
				CompletionTokensDetails: &llm.CompletionTokensDetails{AudioTokens: 30},
			},
		},
		{
			name:       "plural token details with total derived",
			frame:      gaFixtureResponseDonePlural,
			wantRecord: true,
			wantID:     "resp_2",
			wantUsage: &llm.Usage{
				PromptTokens:            30,
				CompletionTokens:        12,
				TotalTokens:             42,
				PromptTokensDetails:     &llm.PromptTokensDetails{AudioTokens: 24, CachedTokens: 4},
				CompletionTokensDetails: &llm.CompletionTokensDetails{AudioTokens: 10},
			},
		},
		{
			name:  "response.done without usage yields no record",
			frame: gaFixtureResponseDoneNoUsage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestGA(true)
			frame := []byte(tt.frame)
			original := bytes.Clone(frame)

			out, err := d.UpstreamFrame(frame)
			require.NoError(t, err)
			require.Len(t, out.ToClient, 1)
			requireUntouched(t, original, frame, out.ToClient[0])
			require.NotNil(t, out.ResponseInProgress)
			require.False(t, *out.ResponseInProgress)

			if !tt.wantRecord {
				require.Empty(t, out.Usage)

				return
			}

			require.Len(t, out.Usage, 1)

			record := out.Usage[0]
			require.Equal(t, "response", record.Kind)
			require.Equal(t, tt.wantID, record.ResponseID)
			require.Empty(t, record.ItemID)
			require.Empty(t, record.Model)
			require.Zero(t, record.DurationSeconds)
			require.Equal(t, tt.wantUsage, record.Usage)

			require.True(t, gjson.ValidBytes(record.Summary), "summary must be valid JSON")
			require.Equal(t, tt.wantID, gjson.GetBytes(record.Summary, "id").String())
			require.Equal(t, gjson.GetBytes(original, "response.usage").Raw, gjson.GetBytes(record.Summary, "usage").Raw)
			require.False(t, gjson.GetBytes(record.Summary, "type").Exists(), "summary is the response object, not the event")
		})
	}
}

func TestGA_ResponseSummaryStripsAudio(t *testing.T) {
	d := newTestGA(true)
	frame := []byte(gaFixtureResponseDoneMultiAudio)
	original := bytes.Clone(frame)

	out, err := d.UpstreamFrame(frame)
	require.NoError(t, err)
	require.Len(t, out.Usage, 1)

	// The frame forwarded to the client keeps its audio.
	require.Len(t, out.ToClient, 1)
	requireUntouched(t, original, frame, out.ToClient[0])
	require.True(t, gjson.GetBytes(out.ToClient[0], "response.output.0.content.0.audio").Exists())

	summary := out.Usage[0].Summary
	require.True(t, gjson.ValidBytes(summary))
	require.NotContains(t, string(summary), `"audio"`, "no audio field may survive in the summary")

	// Audio parts lose only their audio; every other field survives.
	require.False(t, gjson.GetBytes(summary, "output.0.content.0.audio").Exists())
	require.Equal(t, "output_audio", gjson.GetBytes(summary, "output.0.content.0.type").String())
	require.Equal(t, "first", gjson.GetBytes(summary, "output.0.content.0.transcript").String())
	require.Equal(t, `{"type":"output_text","text":"plain"}`, gjson.GetBytes(summary, "output.0.content.1").Raw)
	require.False(t, gjson.GetBytes(summary, "output.0.content.2.audio").Exists())
	require.Equal(t, "second", gjson.GetBytes(summary, "output.0.content.2.transcript").String())

	// Items without content are left exactly as they were.
	require.Equal(t, gjson.GetBytes(original, "response.output.1").Raw, gjson.GetBytes(summary, "output.1").Raw)

	require.False(t, gjson.GetBytes(summary, "output.2.content.0.audio").Exists())
	require.Equal(t, "third", gjson.GetBytes(summary, "output.2.content.0.transcript").String())

	require.Equal(t, int64(3), gjson.GetBytes(summary, "output.#").Int())
	require.Equal(t, gjson.GetBytes(original, "response.usage").Raw, gjson.GetBytes(summary, "usage").Raw)
	require.Equal(t, "resp_4", out.Usage[0].ResponseID)
}

func TestGA_TranscriptionUsage(t *testing.T) {
	tests := []struct {
		name         string
		frame        string
		wantRecord   bool
		wantItemID   string
		wantUsage    *llm.Usage
		wantDuration float64
	}{
		{
			name:       "tokens usage is mapped like a response usage",
			frame:      gaFixtureTranscriptionTokens,
			wantRecord: true,
			wantItemID: "item_9",
			wantUsage: &llm.Usage{
				PromptTokens:        50,
				CompletionTokens:    5,
				TotalTokens:         55,
				PromptTokensDetails: &llm.PromptTokensDetails{AudioTokens: 48},
			},
		},
		{
			name:         "duration usage carries seconds and no tokens",
			frame:        gaFixtureTranscriptionDuration,
			wantRecord:   true,
			wantItemID:   "item_10",
			wantDuration: 12.5,
		},
		{
			name:  "completion without usage yields no record",
			frame: gaFixtureTranscriptionNoUsage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestGA(true)
			frame := []byte(tt.frame)
			original := bytes.Clone(frame)

			out, err := d.UpstreamFrame(frame)
			require.NoError(t, err)
			require.Len(t, out.ToClient, 1)
			requireUntouched(t, original, frame, out.ToClient[0])
			require.Nil(t, out.ResponseInProgress)

			if !tt.wantRecord {
				require.Empty(t, out.Usage)

				return
			}

			require.Len(t, out.Usage, 1)

			record := out.Usage[0]
			require.Equal(t, "transcription", record.Kind)
			require.Equal(t, tt.wantItemID, record.ItemID)
			require.Empty(t, record.ResponseID)
			require.Empty(t, record.Model, "no session frame was seen, so the session decides the model")
			require.Equal(t, tt.wantUsage, record.Usage)
			require.InDelta(t, tt.wantDuration, record.DurationSeconds, 0)
			require.JSONEq(t, tt.frame, string(record.Summary), "summary is the whole event")
			require.NotSame(t, &frame[0], &record.Summary[0], "summary must not alias the frame")
		})
	}
}

func TestGA_TranscriptionModelMemory(t *testing.T) {
	d := newTestGA(true)

	transcribe := func(t *testing.T) string {
		t.Helper()

		out, err := d.UpstreamFrame([]byte(gaFixtureTranscriptionTokens))
		require.NoError(t, err)
		require.Len(t, out.Usage, 1)

		return out.Usage[0].Model
	}

	require.Empty(t, transcribe(t), "nothing seen yet")

	_, err := d.ClientFrame([]byte(gaFixtureSessionUpdateWithModel))
	require.NoError(t, err)
	require.Equal(t, "gpt-4o-transcribe", transcribe(t), "client session.update sets the model")

	_, err = d.UpstreamFrame([]byte(gaFixtureSessionUpdatedWithModel))
	require.NoError(t, err)
	require.Equal(t, "gpt-realtime-whisper", transcribe(t), "upstream session.updated overrides it")

	_, err = d.ClientFrame([]byte(gaFixtureSessionUpdateNoModel))
	require.NoError(t, err)
	require.Equal(t, "gpt-realtime-whisper", transcribe(t), "a session.update without transcription keeps it")

	_, err = d.ClientFrame([]byte(gaFixtureSessionUpdateTranscription))
	require.NoError(t, err)
	require.Equal(t, "gpt-4o-mini-transcribe", transcribe(t), "a transcription session.update sets it")

	_, err = d.UpstreamFrame([]byte(gaFixtureSessionCreatedNoModel))
	require.NoError(t, err)
	require.Equal(t, "gpt-4o-transcribe", transcribe(t), "session.created without session.model still carries a transcription model")

	// RewriteModel=false still tracks the transcription model.
	passthrough := NewGA(GAOptions{RequestedModel: gaTestRequestedModel, ActualModel: gaTestActualModel})

	_, err = passthrough.ClientFrame([]byte(gaFixtureSessionUpdateTranscription))
	require.NoError(t, err)

	out, err := passthrough.UpstreamFrame([]byte(gaFixtureTranscriptionDuration))
	require.NoError(t, err)
	require.Len(t, out.Usage, 1)
	require.Equal(t, "gpt-4o-mini-transcribe", out.Usage[0].Model)
}

func TestGA_ResponseInProgress(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  *bool
	}{
		{name: "response.created starts a response", frame: gaFixtureResponseCreated, want: lo.ToPtr(true)},
		{name: "response.done ends a response", frame: gaFixtureResponseDoneNoUsage, want: lo.ToPtr(false)},
		{name: "response.done with usage ends a response", frame: gaFixtureResponseDoneSingular, want: lo.ToPtr(false)},
		{name: "audio delta reports nothing", frame: gaFixtureUpstreamAudioDelta},
		{name: "session.created reports nothing", frame: gaFixtureSessionCreatedWithModel},
		{name: "transcription completion reports nothing", frame: gaFixtureTranscriptionTokens},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestGA(true)

			out, err := d.UpstreamFrame([]byte(tt.frame))
			require.NoError(t, err)
			require.Equal(t, tt.want, out.ResponseInProgress)
		})
	}

	t.Run("sequence created then done", func(t *testing.T) {
		d := newTestGA(true)

		out, err := d.UpstreamFrame([]byte(gaFixtureResponseCreated))
		require.NoError(t, err)
		require.NotNil(t, out.ResponseInProgress)
		require.True(t, *out.ResponseInProgress)

		out, err = d.UpstreamFrame([]byte(gaFixtureUpstreamAudioDelta))
		require.NoError(t, err)
		require.Nil(t, out.ResponseInProgress)

		out, err = d.UpstreamFrame([]byte(gaFixtureResponseDoneSingular))
		require.NoError(t, err)
		require.NotNil(t, out.ResponseInProgress)
		require.False(t, *out.ResponseInProgress)
	})
}

func TestGA_ClientFrameOnlyReadsTypeForNonSession(t *testing.T) {
	// Client frames other than session.update are never inspected: an invalid JSON body with
	// a readable type and a frame whose "session.model" would be rewritable both pass through.
	tests := []struct {
		name  string
		frame string
	}{
		{name: "response.create with a model field is not a session.update", frame: `{"type":"response.create","response":{"instructions":"hi"},"session":{"model":"x"}}`},
		{name: "conversation.item.create passes through", frame: `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`},
		{name: "unknown event passes through", frame: `{"type":"custom.event","session":{"model":"x"}}`},
		{name: "malformed frame passes through", frame: `{"type":"input_audio_buffer.append","audio":`},
		{name: "empty frame passes through", frame: `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestGA(true)
			frame := []byte(tt.frame)
			original := bytes.Clone(frame)

			out, err := d.ClientFrame(frame)
			require.NoError(t, err)
			require.Len(t, out.ToUpstream, 1)
			requireUntouched(t, original, frame, out.ToUpstream[0])
		})
	}
}

func TestTranscriptionModel(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  string
	}{
		{name: "realtime session.update", frame: gaFixtureSessionUpdateWithModel, want: "gpt-4o-transcribe"},
		{name: "transcription session.update", frame: gaFixtureSessionUpdateTranscription, want: "gpt-4o-mini-transcribe"},
		{name: "session.updated echo", frame: gaFixtureSessionUpdatedWithModel, want: "gpt-realtime-whisper"},
		{name: "session.created transcription session", frame: gaFixtureSessionCreatedNoModel, want: "gpt-4o-transcribe"},
		{name: "no audio block", frame: gaFixtureSessionUpdateNoModel, want: ""},
		{name: "transcription cleared with null", frame: `{"type":"session.update","session":{"audio":{"input":{"transcription":null}}}}`, want: ""},
		{name: "not a session frame", frame: gaFixtureClientAudioAppend, want: ""},
		{name: "invalid JSON", frame: `{"type":"session.update","session":`, want: ""},
		{name: "empty", frame: ``, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, TranscriptionModel([]byte(tt.frame)))
		})
	}
}

func TestGA_Concurrent(t *testing.T) {
	d := newTestGA(true)

	const (
		workers    = 16
		iterations = 200
	)

	clientFrames := [][]byte{
		[]byte(gaFixtureSessionUpdateWithModel),
		[]byte(gaFixtureClientAudioAppend),
		[]byte(gaFixtureSessionUpdateTranscription),
		[]byte(gaFixtureSessionUpdateNoModel),
	}

	upstreamFrames := [][]byte{
		[]byte(gaFixtureSessionCreatedWithModel),
		[]byte(gaFixtureUpstreamAudioDelta),
		[]byte(gaFixtureResponseCreated),
		[]byte(gaFixtureTranscriptionTokens),
		[]byte(gaFixtureResponseDoneSingular),
		[]byte(gaFixtureSessionUpdatedWithModel),
		[]byte(gaFixtureTranscriptionDuration),
	}

	knownModels := map[string]bool{
		"":                       true,
		"gpt-4o-transcribe":      true,
		"gpt-4o-mini-transcribe": true,
		"gpt-realtime-whisper":   true,
	}

	var (
		wg   sync.WaitGroup
		errs = make(chan error, 2*workers)
	)

	for range workers {
		wg.Add(2)

		go func() {
			defer wg.Done()

			for i := range iterations {
				frame := clientFrames[i%len(clientFrames)]

				out, err := d.ClientFrame(frame)
				if err != nil {
					errs <- err

					return
				}

				if len(out.ToUpstream) != 1 {
					errs <- errUnexpectedOutput

					return
				}
			}
		}()

		go func() {
			defer wg.Done()

			for i := range iterations {
				frame := upstreamFrames[i%len(upstreamFrames)]

				out, err := d.UpstreamFrame(frame)
				if err != nil {
					errs <- err

					return
				}

				if len(out.ToClient) != 1 {
					errs <- errUnexpectedOutput

					return
				}

				for _, record := range out.Usage {
					if !knownModels[record.Model] {
						errs <- errUnexpectedOutput

						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	// The shared fixtures were never mutated by any goroutine.
	require.Equal(t, gaFixtureSessionUpdateWithModel, string(clientFrames[0]))
	require.Equal(t, gaFixtureSessionCreatedWithModel, string(upstreamFrames[0]))
	require.Equal(t, gaFixtureResponseDoneSingular, string(upstreamFrames[4]))

	// After the storm the remembered model is one of the values seen.
	out, err := d.UpstreamFrame([]byte(gaFixtureTranscriptionTokens))
	require.NoError(t, err)
	require.Len(t, out.Usage, 1)
	require.True(t, knownModels[out.Usage[0].Model])
	require.True(t, strings.HasPrefix(out.Usage[0].Model, "gpt-"), "a model was remembered")
}

var errUnexpectedOutput = errors.New("unexpected dialect output")

func TestGA_TranscriptionModelAllowed(t *testing.T) {
	onlyMini := func(model string) bool { return model == "gpt-4o-mini-transcribe" }

	t.Run("a rejected model is answered with an error event and never forwarded", func(t *testing.T) {
		d := NewGA(GAOptions{
			RequestedModel:            gaTestRequestedModel,
			ActualModel:               gaTestActualModel,
			RewriteModel:              true,
			TranscriptionModelAllowed: onlyMini,
		})

		out, err := d.ClientFrame([]byte(gaFixtureSessionUpdateWithModel))
		require.NoError(t, err)
		require.Empty(t, out.ToUpstream, "the rejected session.update must not reach the upstream")
		require.Len(t, out.ToClient, 1)

		event := string(out.ToClient[0])
		require.Equal(t, "error", gjson.Get(event, "type").String())
		require.True(t, strings.HasPrefix(gjson.Get(event, "event_id").String(), "event_axh_"), "a nil IDs option gets a private generator")
		require.Equal(t, "invalid_request_error", gjson.Get(event, "error.type").String())
		require.Equal(t, "model_not_allowed", gjson.Get(event, "error.code").String())
		require.Equal(t, gaPathTranscriptionModel, gjson.Get(event, "error.param").String())
		require.Equal(t, "evt_c2", gjson.Get(event, "error.event_id").String())
		require.Contains(t, gjson.Get(event, "error.message").String(), "gpt-4o-transcribe")

		// The rejected model is not remembered for billing either.
		usage, err := d.UpstreamFrame([]byte(gaFixtureTranscriptionTokens))
		require.NoError(t, err)
		require.Len(t, usage.Usage, 1)
		require.Empty(t, usage.Usage[0].Model)
	})

	t.Run("an allowed model is forwarded and remembered", func(t *testing.T) {
		d := NewGA(GAOptions{
			RequestedModel:            gaTestRequestedModel,
			ActualModel:               gaTestActualModel,
			IDs:                       &EventIDs{},
			TranscriptionModelAllowed: onlyMini,
		})

		out, err := d.ClientFrame([]byte(gaFixtureSessionUpdateTranscription))
		require.NoError(t, err)
		require.Len(t, out.ToUpstream, 1)
		require.Empty(t, out.ToClient)

		usage, err := d.UpstreamFrame([]byte(gaFixtureTranscriptionTokens))
		require.NoError(t, err)
		require.Len(t, usage.Usage, 1)
		require.Equal(t, "gpt-4o-mini-transcribe", usage.Usage[0].Model)
	})

	t.Run("frames without a transcription model are not checked", func(t *testing.T) {
		d := NewGA(GAOptions{
			RequestedModel:            gaTestRequestedModel,
			ActualModel:               gaTestActualModel,
			IDs:                       &EventIDs{},
			TranscriptionModelAllowed: func(string) bool { return false },
		})

		out, err := d.ClientFrame([]byte(gaFixtureSessionUpdateNoModel))
		require.NoError(t, err)
		require.Len(t, out.ToUpstream, 1)
		require.Empty(t, out.ToClient)
	})

	t.Run("nil allows every model", func(t *testing.T) {
		d := newTestGA(true)

		out, err := d.ClientFrame([]byte(gaFixtureSessionUpdateWithModel))
		require.NoError(t, err)
		require.Len(t, out.ToUpstream, 1)
		require.Empty(t, out.ToClient)
	})
}

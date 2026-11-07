package api

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// editingAttemptHooks stands in for a plugin whose onAttempt is scoped to
// openai/audio_transcriptions and edits the body. Like upstream's attempt hook it
// replaces the redacted log copy (JSONBody) with its output.
type editingAttemptHooks struct {
	calls int
}

func (h *editingAttemptHooks) Middleware(context.Context) (orchestrator.RequestHooks, error) {
	return orchestrator.RequestHooks{
		Attempt: pipeline.OnRawRequest("test-attempt-hooks", func(_ context.Context, request *httpclient.Request) (*httpclient.Request, error) {
			h.calls++

			edited := *request
			edited.Body = append(bytes.TrimSuffix(bytes.Clone(request.Body), []byte("}")), []byte(`,"tag":"plugin"}`)...)
			if len(edited.JSONBody) > 0 {
				edited.JSONBody = bytes.Clone(edited.Body)
			}

			return &edited, nil
		}),
	}, nil
}

func (*editingAttemptHooks) PassThroughApplied(context.Context, int) {}

// fork: Bailian ASR sends DashScope-native JSON under the openai/audio_transcriptions
// label, so plugin attempt hooks must skip it (see orchestrator/plugin_hooks_bailian_asr.go):
// a hook edit would otherwise store the audio in the request execution.
func TestCreateTranscription_BailianSkipsPluginAttemptHooks(t *testing.T) {
	h := newBailianASRHarness(t)
	hooks := &editingAttemptHooks{}
	h.handlers.TranscriptionHandlers.ChatCompletionOrchestrator.Plugins = orchestrator.PluginIntegration{Hooks: hooks}

	w := h.transcribe(t, map[string]string{"model": "qwen-audio-3.1-asr-flash", "stream": "true"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	require.Zero(t, hooks.calls, "attempt hooks must not see a Bailian ASR provider request")

	executions, err := h.client.RequestExecution.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, executions, 1)
	stored := executions[0].RequestBody
	require.Contains(t, gjson.GetBytes(stored, "input.messages.0.content.0.input_audio.data").String(), "<audio bytes:",
		"the stored body keeps the audio placeholder")
	require.NotContains(t, string(stored), "base64,")
	require.False(t, gjson.GetBytes(stored, "tag").Exists(), "no hook edit reached the stored body")
}

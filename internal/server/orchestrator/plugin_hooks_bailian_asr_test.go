package orchestrator

import (
	"context"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/bailianasr"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// countingAttemptHook records the attempt-stage callbacks it receives.
type countingAttemptHook struct {
	pipeline.DummyMiddleware

	rawRequests int
	processEnds int
}

func (h *countingAttemptHook) OnOutboundRawRequest(_ context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	h.rawRequests++
	return request, nil
}

func (h *countingAttemptHook) OnProcessEnd() { h.processEnds++ }

func TestSkipAttemptHooksForBailianASR(t *testing.T) {
	hook := &countingAttemptHook{}
	stage := skipAttemptHooksForBailianASR([]pipeline.Middleware{hook})
	require.Len(t, stage, 1)

	asr := bailianasr.Wrap(nil, bailianasr.Config{
		BaseURL:        "https://dashscope.aliyuncs.com/compatible-mode/v1",
		APIKeyProvider: auth.NewStaticKeyProvider("sk-test"),
	})

	for _, model := range []string{"paraformer-v2", "qwen-audio-3.1-asr-flash"} {
		providerReq, err := asr.TransformRequest(context.Background(), &llm.Request{
			Model:         model,
			Stream:        lo.ToPtr(false),
			RequestType:   llm.RequestTypeTranscription,
			APIFormat:     llm.APIFormatOpenAITranscription,
			Transcription: &llm.TranscriptionRequest{File: wavForPerformanceTest(), FileName: "a.wav"},
		})
		require.NoError(t, err, model)

		got, err := stage[0].OnOutboundRawRequest(context.Background(), providerReq)
		require.NoError(t, err)
		require.Same(t, providerReq, got, "%s passes through untouched", model)
	}

	require.Zero(t, hook.rawRequests, "attempt hooks never see a Bailian ASR provider request")

	chat := &httpclient.Request{
		Method:      "POST",
		URL:         "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
		ContentType: "application/json",
		Body:        []byte(`{"model":"qwen-plus"}`),
		APIFormat:   string(llm.APIFormatOpenAIChatCompletion),
	}
	_, err := stage[0].OnOutboundRawRequest(context.Background(), chat)
	require.NoError(t, err)
	require.Equal(t, 1, hook.rawRequests, "other requests still reach the attempt hooks")

	stage[0].OnProcessEnd()
	require.Equal(t, 1, hook.processEnds, "the wrapper forwards the cleanup callback")
}

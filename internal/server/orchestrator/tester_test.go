package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

// TestBuildTestRequestUsesConfiguredPrompts verifies ordinary channel tests retain their configured prompts.
func TestBuildTestRequestUsesConfiguredPrompts(t *testing.T) {
	req := buildChannelTestRequest("test-model", true, "system prompt", "user prompt", false)

	require.Equal(t, "test-model", req.Model)
	require.Len(t, req.Messages, 2)
	require.Equal(t, "system", req.Messages[0].Role)
	require.Equal(t, "system prompt", *req.Messages[0].Content.Content)
	require.Equal(t, "user", req.Messages[1].Role)
	require.Equal(t, "user prompt", *req.Messages[1].Content.Content)
	require.Equal(t, int64(256), *req.MaxCompletionTokens)
	require.True(t, *req.Stream)
}

// TestBuildTestRequestUsesPingForResponsesWebSocket verifies WebSocket tests use a minimal compatible payload.
func TestBuildTestRequestUsesPingForResponsesWebSocket(t *testing.T) {
	req := buildChannelTestRequest("test-model", false, "system prompt", "user prompt", true)

	require.Equal(t, "test-model", req.Model)
	require.Len(t, req.Messages, 1)
	require.Equal(t, "user", req.Messages[0].Role)
	require.Equal(t, responsesWebSocketTestPrompt, *req.Messages[0].Content.Content)
	require.Nil(t, req.MaxCompletionTokens)
	require.True(t, *req.Stream)
}

// TestUsesResponsesWebSocket verifies explicit and URL-inferred WebSocket transports.
func TestUsesResponsesWebSocket(t *testing.T) {
	t.Run("inferred from channel base URL", func(t *testing.T) {
		ch := &biz.Channel{Channel: &ent.Channel{
			Type:    channel.TypeOpenaiResponses,
			BaseURL: "wss://api.openai.com/v1",
		}}

		require.True(t, usesResponsesWebSocket(ch))
	})

	t.Run("explicit transport", func(t *testing.T) {
		ch := &biz.Channel{Channel: &ent.Channel{
			Type:    channel.TypeOpenai,
			BaseURL: "https://api.example.com/v1",
			Endpoints: []objects.ChannelEndpoint{{
				APIFormat: llm.APIFormatOpenAIResponse.String(),
				Transport: objects.ChannelEndpointTransportWebSocket,
			}},
		}}

		require.True(t, usesResponsesWebSocket(ch))
	})

	t.Run("HTTP transport", func(t *testing.T) {
		ch := &biz.Channel{Channel: &ent.Channel{
			Type:    channel.TypeOpenaiResponses,
			BaseURL: "https://api.openai.com/v1",
		}}

		require.False(t, usesResponsesWebSocket(ch))
	})
}

func TestHandleStreamResponseIgnoresUsageOnlyChunksForTTFT(t *testing.T) {
	usageOnly, err := json.Marshal(map[string]any{
		"choices": []any{},
		"usage":   map[string]any{"completion_tokens": 1},
	})
	require.NoError(t, err)

	streamResult, err := (&TestChannelOrchestrator{}).handleStreamResponse(
		context.Background(),
		streams.SliceStream([]*httpclient.StreamEvent{
			{Data: usageOnly},
			{Data: []byte(`[DONE]`)},
		}),
		time.Now(),
	)
	require.NoError(t, err)
	require.False(t, streamResult.Success)
	require.True(t, streamResult.Stream)
	require.NotNil(t, streamResult.TTFBMs)
	require.Nil(t, streamResult.TTFTMs, "usage-only chunks must not count as TTFT")
	require.GreaterOrEqual(t, streamResult.TotalMs, 0.0)
}

func TestHandleStreamResponseRecordsFirstUsableOutput(t *testing.T) {
	emptyDelta, err := json.Marshal(map[string]any{
		"choices": []any{
			map[string]any{"delta": map[string]any{"role": "assistant", "content": ""}},
		},
	})
	require.NoError(t, err)
	contentDelta, err := json.Marshal(map[string]any{
		"choices": []any{
			map[string]any{"delta": map[string]any{"content": "hello"}},
		},
	})
	require.NoError(t, err)

	streamResult, err := (&TestChannelOrchestrator{}).handleStreamResponse(
		context.Background(),
		streams.SliceStream([]*httpclient.StreamEvent{
			{Data: emptyDelta},
			{Data: contentDelta},
			{Data: []byte(`[DONE]`)},
		}),
		time.Now(),
	)
	require.NoError(t, err)
	require.True(t, streamResult.Success)
	require.Equal(t, "hello", *streamResult.Message)
	require.NotNil(t, streamResult.TTFBMs)
	require.NotNil(t, streamResult.TTFTMs)
	require.GreaterOrEqual(t, *streamResult.TTFTMs, *streamResult.TTFBMs)
	require.GreaterOrEqual(t, streamResult.TotalMs, *streamResult.TTFTMs)
}

func TestHandleNonStreamingResponseUsesTotalAsTTFBFallback(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"content": "hello"}},
		},
	})
	require.NoError(t, err)

	result := (&TestChannelOrchestrator{}).handleNonStreamingResponse(
		ChatCompletionResult{ChatCompletion: &httpclient.Response{Body: body}},
		time.Now(),
		false,
	)
	require.True(t, result.Success)
	require.False(t, result.Stream)
	require.Nil(t, result.TTFTMs)
	require.NotNil(t, result.TTFBMs)
	require.Equal(t, result.TotalMs, *result.TTFBMs)
}

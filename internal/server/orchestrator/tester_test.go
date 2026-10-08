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
	req := buildChannelTestRequest("test-model", true, "system prompt", "user prompt", false, llm.APIFormatOpenAIChatCompletion)

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
	req := buildChannelTestRequest("test-model", false, "system prompt", "user prompt", true, llm.APIFormatOpenAIChatCompletion)

	require.Equal(t, "test-model", req.Model)
	require.Len(t, req.Messages, 1)
	require.Equal(t, "user", req.Messages[0].Role)
	require.Equal(t, responsesWebSocketTestPrompt, *req.Messages[0].Content.Content)
	require.Nil(t, req.MaxCompletionTokens)
	require.True(t, *req.Stream)
}

func TestBuildTestRequestUsesDecisionsFormat(t *testing.T) {
	req := buildChannelTestRequest("gpt-6-luna", true, "ignored", "classify this", false, llm.APIFormatOpenAIDecisions)

	require.Equal(t, llm.RequestTypeDecisions, req.RequestType)
	require.Equal(t, llm.APIFormatOpenAIDecisions, req.APIFormat)
	require.NotNil(t, req.Decisions)
	require.False(t, *req.Stream)

	var body struct {
		Model     string            `json:"model"`
		Input     string            `json:"input"`
		Questions []json.RawMessage `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(req.Decisions.Body, &body))
	require.Equal(t, "gpt-6-luna", body.Model)
	require.Equal(t, "classify this", body.Input)
	require.Len(t, body.Questions, 1)
}

func TestChannelTestAPIFormatPrefersDecisionsEndpoint(t *testing.T) {
	ch := &biz.Channel{Channel: &ent.Channel{
		Endpoints: []objects.ChannelEndpoint{
			{APIFormat: llm.APIFormatOpenAIChatCompletion.String()},
			{APIFormat: llm.APIFormatOpenAIDecisions.String()},
		},
	}}

	require.Equal(t, llm.APIFormatOpenAIDecisions, channelTestAPIFormat(ch, "gpt-6-luna"))
}

func TestParseDecisionsTestResponseRequiresAnswers(t *testing.T) {
	message, err := parseDecisionsTestResponse([]byte(`{"model":"gpt-6-luna","answers":[{"name":"answer","probability":1}]}`))
	require.NoError(t, err)
	require.Contains(t, message, `"answers"`)

	_, err = parseDecisionsTestResponse([]byte(`{"model":"gpt-6-luna","answers":[]}`))
	require.EqualError(t, err, "no answers in Decisions response")
}

func TestChannelTestAPIFormatUsesModelProtocolPriority(t *testing.T) {
	for _, tt := range []struct {
		name    string
		formats []string
		want    llm.APIFormat
	}{
		{"chat first", []string{llm.APIFormatOpenAIChatCompletion.String(), llm.APIFormatTypeSafeSystemOne.String()}, llm.APIFormatOpenAIChatCompletion},
		{"systemone first", []string{llm.APIFormatTypeSafeSystemOne.String(), llm.APIFormatOpenAIChatCompletion.String()}, llm.APIFormatTypeSafeSystemOne},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ch := &biz.Channel{Channel: &ent.Channel{
				Type:            channel.TypeOpenai,
				SupportedModels: []string{"test-model"},
				Endpoints:       []objects.ChannelEndpoint{{APIFormat: llm.APIFormatTypeSafeSystemOne.String(), Path: "/systemone"}},
				Settings: &objects.ChannelSettings{
					ExtraModelPrefix: "alias",
					ModelProtocols:   []objects.ModelProtocol{{Model: "test-model", APIFormats: tt.formats}},
				},
			}}
			require.Equal(t, tt.want, channelTestAPIFormat(ch, "test-model"))
			require.Equal(t, tt.want, channelTestAPIFormat(ch, "alias/test-model"))
		})
	}
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
		llm.APIFormatOpenAIChatCompletion,
	)
	require.True(t, result.Success)
	require.False(t, result.Stream)
	require.Nil(t, result.TTFTMs)
	require.NotNil(t, result.TTFBMs)
	require.Equal(t, result.TotalMs, *result.TTFBMs)
}

package biz

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/bailianasr"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer/bailian"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func bailianASRTestChannel(id int, channelType channel.Type, baseURL string, endpoints ...objects.ChannelEndpoint) *ent.Channel {
	return &ent.Channel{
		ID:              id,
		Name:            "bailian-asr-fork",
		Type:            channelType,
		BaseURL:         baseURL,
		Credentials:     objects.ChannelCredentials{APIKey: "sk-dashscope-test"},
		SupportedModels: []string{"qwen3.7-max", "paraformer-v2"},
		Endpoints:       endpoints,
	}
}

func paraformerTranscription() *llm.Request {
	return &llm.Request{
		Model:         "paraformer-v2",
		Stream:        lo.ToPtr(false),
		RequestType:   llm.RequestTypeTranscription,
		APIFormat:     llm.APIFormatOpenAITranscription,
		Transcription: &llm.TranscriptionRequest{File: []byte("RIFF....WAVE"), FileName: "a.wav"},
	}
}

// TestBailianASR_ForkHookWrapsDefaultOutbounds guards the fork line in channel_llm.go
// buildChannelWithOutbounds:
//
//	wrapBailianASROutbounds(c, ch, outbounds) // fork: see channel_bailian_asr.go
//
// Without it a TypeBailian channel sends POST /v1/audio/transcriptions for the Bailian
// file-transcription models to compatible-mode /audio/transcriptions, which returns 404. A
// merge that drops the line keeps compiling, so this test is what notices. It is listed in
// CUSTOM_BRANCH_RULES.md section 5.
func TestBailianASR_ForkHookWrapsDefaultOutbounds(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_bailian_asr_fork?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	ch, err := svc.buildChannelWithOutbounds(bailianASRTestChannel(7, channel.TypeBailian, "https://dashscope.aliyuncs.com/compatible-mode/v1"))
	require.NoError(t, err)

	// The primary outbound stays the upstream Bailian transformer (TestBailianChannel_ExistingProtocolsUnchanged).
	require.IsType(t, &bailian.OutboundTransformer{}, ch.Outbound)

	out, err := BuildOutboundByAPIFormat(ch, llm.APIFormatOpenAIChatCompletion.String())
	require.NoError(t, err)

	asr, ok := out.(*bailianasr.Outbound)
	require.True(t, ok, "the default chat endpoint of a Bailian channel must route through bailianasr, got %T", out)
	require.Same(t, ch.Outbound, asr.Inner())
	require.Equal(t, "https://dashscope.aliyuncs.com", asr.NativeBaseURL())

	ctx := t.Context()

	transcription, err := out.TransformRequest(ctx, paraformerTranscription())
	require.NoError(t, err)
	require.Equal(t, "https://dashscope.aliyuncs.com/api/v1/services/audio/asr/transcription", transcription.URL)

	chat := &llm.Request{
		Model:    "qwen3.7-max",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("hello")}}},
		Stream:   lo.ToPtr(false),
	}
	viaWrapper, err := out.TransformRequest(ctx, chat)
	require.NoError(t, err)
	direct, err := ch.Outbound.TransformRequest(ctx, chat)
	require.NoError(t, err)
	require.Equal(t, direct.URL, viaWrapper.URL)
	require.JSONEq(t, string(direct.Body), string(viaWrapper.Body))

	// Other channel types keep their outbounds untouched.
	for _, channelType := range []channel.Type{channel.TypeBailianResponses, channel.TypeOpenai} {
		other, err := svc.buildChannelWithOutbounds(bailianASRTestChannel(8, channelType, "https://dashscope.aliyuncs.com/compatible-mode/v1"))
		require.NoError(t, err)

		for format, out := range other.Outbounds {
			_, wrapped := out.(*bailianasr.Outbound)
			require.False(t, wrapped, "%s %s must not be wrapped", channelType, format)
		}
	}
}

// TestBailianASR_ForkHookInBuildNonDefaultEndpointOutbound guards the fork lines in
// channel_llm.go buildNonDefaultEndpointOutbound, right before the api_format switch:
//
//	if out, ok, err := bailianASREndpointOutbound(c, ch, ep, baseURL); ok { // fork: see channel_bailian_asr.go
//		return out, err
//	}
//
// Without them an explicit openai/audio_transcriptions endpoint on a Bailian channel gets the
// generic OpenAI outbound, which 404s on DashScope. It is listed in CUSTOM_BRANCH_RULES.md
// section 5.
func TestBailianASR_ForkHookInBuildNonDefaultEndpointOutbound(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_bailian_asr_endpoint_fork?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	transcriptionFormat := llm.APIFormatOpenAITranscription.String()

	tests := []struct {
		name        string
		channelType channel.Type
		baseURL     string
		endpoint    objects.ChannelEndpoint
		wantNative  string
	}{
		{
			name:        "chat channel, endpoint inherits the channel base",
			channelType: channel.TypeBailian,
			baseURL:     "https://dashscope.aliyuncs.com/compatible-mode/v1",
			endpoint:    objects.ChannelEndpoint{APIFormat: transcriptionFormat},
			wantNative:  "https://dashscope.aliyuncs.com",
		},
		{
			name:        "endpoint base points at another region",
			channelType: channel.TypeBailian,
			baseURL:     "https://dashscope.aliyuncs.com/compatible-mode/v1",
			endpoint:    objects.ChannelEndpoint{APIFormat: transcriptionFormat, BaseURL: "https://dashscope-intl.aliyuncs.com/api/v1"},
			wantNative:  "https://dashscope-intl.aliyuncs.com",
		},
		{
			name:        "anthropic-type Bailian channel",
			channelType: channel.TypeBailianAnthropic,
			baseURL:     "https://dashscope.aliyuncs.com/apps/anthropic",
			endpoint:    objects.ChannelEndpoint{APIFormat: transcriptionFormat},
			wantNative:  "https://dashscope.aliyuncs.com",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, ValidateEndpoints([]objects.ChannelEndpoint{tt.endpoint}, tt.baseURL))

			ch, err := svc.buildChannelWithOutbounds(bailianASRTestChannel(20+i, tt.channelType, tt.baseURL, tt.endpoint))
			require.NoError(t, err)

			out, err := BuildOutboundByAPIFormat(ch, transcriptionFormat)
			require.NoError(t, err)

			asr, ok := out.(*bailianasr.Outbound)
			require.True(t, ok, "the explicit transcription endpoint must route through bailianasr, got %T", out)
			require.IsType(t, &openai.OutboundTransformer{}, asr.Inner(), "other models keep the upstream OpenAI outbound")
			require.Equal(t, tt.wantNative, asr.NativeBaseURL())

			httpReq, err := out.TransformRequest(t.Context(), paraformerTranscription())
			require.NoError(t, err)
			require.Equal(t, tt.wantNative+"/api/v1/services/audio/asr/transcription", httpReq.URL)
		})
	}

	// Non-Bailian channels keep the upstream OpenAI outbound for the same endpoint.
	endpoint := objects.ChannelEndpoint{APIFormat: transcriptionFormat}
	openaiChannel, err := svc.buildChannelWithOutbounds(bailianASRTestChannel(30, channel.TypeOpenai, "https://api.openai.com/v1", endpoint))
	require.NoError(t, err)
	require.IsType(t, &openai.OutboundTransformer{}, openaiChannel.Outbounds[transcriptionFormat])
}

// TestBailianASR_UserChatEndpointIsWrapped covers a TypeBailian channel whose chat endpoint the
// user configured (for example another region): a transcription request falls back to that
// endpoint, so it must route through bailianasr too, with the endpoint's own native host.
func TestBailianASR_UserChatEndpointIsWrapped(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_bailian_asr_user_chat?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	chatFormat := llm.APIFormatOpenAIChatCompletion.String()
	endpoint := objects.ChannelEndpoint{APIFormat: chatFormat, BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"}

	ch, err := svc.buildChannelWithOutbounds(bailianASRTestChannel(40, channel.TypeBailian, "https://dashscope.aliyuncs.com/compatible-mode/v1", endpoint))
	require.NoError(t, err)

	out, err := BuildOutboundByAPIFormat(ch, chatFormat)
	require.NoError(t, err)

	asr, ok := out.(*bailianasr.Outbound)
	require.True(t, ok, "the user chat endpoint of a Bailian channel must route through bailianasr, got %T", out)
	require.NotSame(t, ch.Outbound, asr.Inner(), "the user endpoint keeps its own outbound behind the wrapper")
	require.Equal(t, "https://dashscope-intl.aliyuncs.com", asr.NativeBaseURL())

	httpReq, err := out.TransformRequest(t.Context(), paraformerTranscription())
	require.NoError(t, err)
	require.Equal(t, "https://dashscope-intl.aliyuncs.com/api/v1/services/audio/asr/transcription", httpReq.URL)

	chat := &llm.Request{
		Model:    "qwen3.7-max",
		Messages: []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("hello")}}},
		Stream:   lo.ToPtr(false),
	}
	viaWrapper, err := out.TransformRequest(t.Context(), chat)
	require.NoError(t, err)
	direct, err := asr.Inner().TransformRequest(t.Context(), chat)
	require.NoError(t, err)
	require.Equal(t, direct.URL, viaWrapper.URL, "chat on the user endpoint is unchanged")
}

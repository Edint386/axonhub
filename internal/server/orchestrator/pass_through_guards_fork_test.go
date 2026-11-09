package orchestrator

import (
	"context"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// fork: guards for custom pass-through hooks inside upstream files
// (CUSTOM_BRANCH_RULES.md section 5). Each test fails if its hook is dropped.

// The multipart guard in isPassThroughEnabled is the only thing that keeps
// response and stream pass-through off for multipart requests (for example
// Bailian transcription, whose provider format equals the inbound format).
func TestIsPassThroughEnabled_SkipsMultipartRawRequest(t *testing.T) {
	newOutbound := func(contentType string) *PersistentOutboundTransformer {
		channel := &biz.Channel{Channel: &ent.Channel{
			ID:       1,
			Name:     "multipart",
			Settings: &objects.ChannelSettings{PassThroughBody: lo.ToPtr(true)},
		}}

		return &PersistentOutboundTransformer{state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
			LlmRequest: &llm.Request{
				APIFormat: llm.APIFormatOpenAIImageEdit,
				RawRequest: &httpclient.Request{
					APIFormat: string(llm.APIFormatOpenAIImageEdit),
					Headers:   http.Header{"Content-Type": []string{contentType}},
				},
			},
			RawProviderRequest: &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIImageEdit)},
		}}
	}

	require.True(t, newOutbound("application/json").isPassThroughEnabled(), "control: a JSON request passes through")
	require.False(t, newOutbound("multipart/form-data; boundary=x").isPassThroughEnabled())
	require.False(t, newOutbound(" Multipart/Form-Data; boundary=x").isPassThroughEnabled())
}

type rewritingLlmRequestMiddleware struct {
	pipeline.DummyMiddleware

	rewrite bool
}

func (m *rewritingLlmRequestMiddleware) Name() string { return "test-rewrite" }

func (m *rewritingLlmRequestMiddleware) OnOutboundLlmRequest(_ context.Context, req *llm.Request, _ llm.APIFormat) (*llm.Request, error) {
	if !m.rewrite {
		return req, nil
	}

	cloned := *req
	cloned.Temperature = lo.ToPtr(0.1)

	return &cloned, nil
}

// TransformRequest must mark the attempt when a channel setting or an outbound
// middleware rewrote the request, so pass-through cannot put the raw client
// body back over the rewrite (isPassThroughEnabled and
// responsesTransportAdaptationEnabled both honour PassThroughBodyBlocked).
func TestTransformRequest_BlocksPassThroughAfterSemanticRewrite(t *testing.T) {
	cases := []struct {
		name        string
		effort      string
		middlewares []pipeline.OutboundLlmRequestMiddleware
		want        bool
	}{
		{name: "control: nothing rewritten", effort: "low", want: false},
		{name: "reasoning effort mapping", effort: "xhigh", want: true},
		{name: "outbound middleware returns a new request", effort: "low", middlewares: []pipeline.OutboundLlmRequestMiddleware{&rewritingLlmRequestMiddleware{rewrite: true}}, want: true},
		{name: "control: outbound middleware keeps the request", effort: "low", middlewares: []pipeline.OutboundLlmRequestMiddleware{&rewritingLlmRequestMiddleware{}}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outbound := &mockTransformer{
				apiFormat:        llm.APIFormatOpenAIChatCompletion,
				requestAPIFormat: llm.APIFormatOpenAIChatCompletion,
			}
			channel := &biz.Channel{
				Channel: &ent.Channel{
					ID:   1,
					Name: "pass-through",
					Settings: &objects.ChannelSettings{
						PassThroughBody: lo.ToPtr(true),
						TransformOptions: objects.TransformOptions{
							ReasoningEffortMapping: []llm.ReasoningEffortMapping{{From: "xhigh", To: "max"}},
						},
					},
				},
				Outbound: outbound,
			}
			processor := &PersistentOutboundTransformer{
				wrapped:                       outbound,
				outboundLlmRequestMiddlewares: tc.middlewares,
				state: &PersistenceState{
					ChannelModelsCandidates: []*ChannelModelsCandidate{{
						Channel:   channel,
						Models:    []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
						APIFormat: llm.APIFormatOpenAIChatCompletion.String(),
					}},
				},
			}
			request := &llm.Request{
				Model:           "gpt-4",
				ReasoningEffort: tc.effort,
				Messages:        []llm.Message{{Role: "user", Content: llm.MessageContent{Content: lo.ToPtr("hello")}}},
			}

			_, err := processor.TransformRequest(context.Background(), request)
			require.NoError(t, err)
			require.Equal(t, tc.want, processor.state.PassThroughBodyBlocked)
		})
	}
}

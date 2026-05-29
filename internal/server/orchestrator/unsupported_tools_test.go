package orchestrator

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestMergeUnsupportedToolsIntoBodyRestoresOriginalOrder(t *testing.T) {
	body := []byte(`{"model":"gpt-5","tools":[{"type":"function","name":"known_a"},{"type":"function","name":"known_b"}]}`)
	unsupportedTools := []llm.UnsupportedTool{
		{
			Index: 1,
			Type:  "namespace",
			Raw:   json.RawMessage(`{"type":"namespace","name":"mcp__node_repl__"}`),
		},
		{
			Index: 3,
			Type:  "tool_search",
			Raw:   json.RawMessage(`{"type":"tool_search","description":"search tools"}`),
		},
	}

	result, err := mergeUnsupportedToolsIntoBody(body, unsupportedTools)
	require.NoError(t, err)

	tools := gjson.GetBytes(result, "tools").Array()
	require.Len(t, tools, 4)
	require.Equal(t, "known_a", tools[0].Get("name").String())
	require.Equal(t, "namespace", tools[1].Get("type").String())
	require.Equal(t, "known_b", tools[2].Get("name").String())
	require.Equal(t, "tool_search", tools[3].Get("type").String())
}

func TestMergeUnsupportedToolsIntoBodyRestoresConsecutiveUnsupportedTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5","tools":[{"type":"function","name":"known"}]}`)
	unsupportedTools := []llm.UnsupportedTool{
		{
			Index: 0,
			Type:  "namespace",
			Raw:   json.RawMessage(`{"type":"namespace","name":"mcp__node_repl__"}`),
		},
		{
			Index: 1,
			Type:  "tool_search",
			Raw:   json.RawMessage(`{"type":"tool_search","description":"search tools"}`),
		},
	}

	result, err := mergeUnsupportedToolsIntoBody(body, unsupportedTools)
	require.NoError(t, err)

	tools := gjson.GetBytes(result, "tools").Array()
	require.Len(t, tools, 3)
	require.Equal(t, "namespace", tools[0].Get("type").String())
	require.Equal(t, "tool_search", tools[1].Get("type").String())
	require.Equal(t, "known", tools[2].Get("name").String())
}

func TestMergeUnsupportedToolsIntoBodySkipsMissingToolsArray(t *testing.T) {
	body := []byte(`{"model":"gpt-5"}`)

	result, err := mergeUnsupportedToolsIntoBody(body, []llm.UnsupportedTool{
		{Index: 0, Raw: json.RawMessage(`{"type":"namespace"}`)},
	})
	require.NoError(t, err)
	tools := gjson.GetBytes(result, "tools").Array()
	require.Len(t, tools, 1)
	require.Equal(t, "namespace", tools[0].Get("type").String())
}

func TestApplyUnsupportedToolsPolicySameFormatChannelEnabled(t *testing.T) {
	ctx := context.Background()
	outbound := &PersistentOutboundTransformer{
		state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{
				Channel: &biz.Channel{
					Channel: &ent.Channel{
						ID:   1,
						Name: "test",
						Settings: &objects.ChannelSettings{
							PreserveUnsupportedTools: lo.ToPtr(true),
						},
					},
				},
			},
			LlmRequest: &llm.Request{
				APIFormat: llm.APIFormatOpenAIResponse,
				UnsupportedTools: []llm.UnsupportedTool{
					{Index: 1, Raw: json.RawMessage(`{"type":"namespace","name":"mcp__node_repl__"}`)},
				},
			},
		},
	}
	request := &httpclient.Request{
		APIFormat: string(llm.APIFormatOpenAIResponse),
		Body:      []byte(`{"tools":[{"type":"function","name":"known"}]}`),
	}

	result, err := applyUnsupportedToolsPolicy(outbound, nil).OnOutboundRawRequest(ctx, request)
	require.NoError(t, err)
	tools := gjson.GetBytes(result.Body, "tools").Array()
	require.Len(t, tools, 2)
	require.Equal(t, "known", tools[0].Get("name").String())
	require.Equal(t, "namespace", tools[1].Get("type").String())
}

func TestApplyUnsupportedToolsPolicyDifferentFormatRequiresForwardFlag(t *testing.T) {
	ctx := context.Background()
	outbound := &PersistentOutboundTransformer{
		state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{
				Channel: &biz.Channel{
					Channel: &ent.Channel{
						ID:   1,
						Name: "test",
						Settings: &objects.ChannelSettings{
							PreserveUnsupportedTools: lo.ToPtr(true),
						},
					},
				},
			},
			LlmRequest: &llm.Request{
				APIFormat: llm.APIFormatOpenAIResponse,
				UnsupportedTools: []llm.UnsupportedTool{
					{Index: 1, Raw: json.RawMessage(`{"type":"namespace"}`)},
				},
			},
		},
	}
	request := &httpclient.Request{
		APIFormat: string(llm.APIFormatAnthropicMessage),
		Body:      []byte(`{"tools":[{"name":"known"}]}`),
	}

	result, err := applyUnsupportedToolsPolicy(outbound, nil).OnOutboundRawRequest(ctx, request)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(result.Body, "tools").Array(), 1)

	outbound.state.CurrentCandidate.Channel.Settings.ForwardUnsupportedToolsAcrossFormats = lo.ToPtr(true)
	result, err = applyUnsupportedToolsPolicy(outbound, nil).OnOutboundRawRequest(ctx, request)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(result.Body, "tools").Array(), 2)
}

func TestApplyUnsupportedToolsPolicySkipsPassThroughBody(t *testing.T) {
	ctx := context.Background()
	outbound := &PersistentOutboundTransformer{
		state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{
				Channel: &biz.Channel{
					Channel: &ent.Channel{
						ID:   1,
						Name: "test",
						Settings: &objects.ChannelSettings{
							PassThroughBody:          lo.ToPtr(true),
							PreserveUnsupportedTools: lo.ToPtr(true),
						},
					},
				},
			},
			LlmRequest: &llm.Request{
				APIFormat: llm.APIFormatOpenAIResponse,
				UnsupportedTools: []llm.UnsupportedTool{
					{Index: 1, Raw: json.RawMessage(`{"type":"namespace"}`)},
				},
			},
			RawProviderRequest: &httpclient.Request{
				APIFormat: string(llm.APIFormatOpenAIResponse),
			},
		},
	}
	request := &httpclient.Request{
		APIFormat: string(llm.APIFormatOpenAIResponse),
		Body:      []byte(`{"tools":[{"type":"function","name":"known"},{"type":"namespace"}]}`),
	}

	result, err := applyUnsupportedToolsPolicy(outbound, nil).OnOutboundRawRequest(ctx, request)
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(result.Body, "tools").Array(), 2)
}

package responses

import (
	"testing"

	"github.com/looplj/axonhub/llm"
	"github.com/stretchr/testify/require"
)

func TestAttachOpenAIResponsesRequestExtensionsPreservesReasoningContextAndNamespaceTools(t *testing.T) {
	chatReq := &llm.Request{}
	req := &Request{
		Reasoning: &Reasoning{Context: "all_turns"},
		Tools: []Tool{{
			Type: "namespace",
			Name: "mcp__node_repl",
		}},
	}

	attachOpenAIResponsesRequestExtensions(chatReq, req, []byte(`{
		"tools": [{
			"type": "namespace",
			"name": "mcp__node_repl",
			"tools": [{"type": "function", "name": "js"}]
		}]
	}`))

	requestExt := chatReq.ProviderExtensions.OpenAIResponses.Request
	require.Equal(t, "all_turns", requestExt.ReasoningContext)
	require.Equal(t, []llm.OpenAIResponsesNamespaceTool{{
		Namespace: "mcp__node_repl",
		Name:      "js",
	}}, requestExt.NamespaceTools)
}

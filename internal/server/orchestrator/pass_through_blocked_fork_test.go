package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// TestResponsesTransportAdaptationHonoursPassThroughBodyBlocked covers the custom
// PassThroughBodyBlocked guard on upstream's Responses HTTP<->WebSocket transport
// adaptation: once channel transforms changed the request, the raw body must not
// replace the transformed one.
func TestResponsesTransportAdaptationHonoursPassThroughBodyBlocked(t *testing.T) {
	newOutbound := func(blocked bool) *PersistentOutboundTransformer {
		return &PersistentOutboundTransformer{state: &PersistenceState{
			LlmRequest:             &llm.Request{APIFormat: llm.APIFormatOpenAIResponseWebSocket},
			RawProviderRequest:     &httpclient.Request{APIFormat: llm.APIFormatOpenAIResponse.String()},
			PassThroughBodyBlocked: blocked,
		}}
	}

	require.True(t, responsesTransportAdaptationEnabled(t.Context(), newOutbound(false), nil))
	require.False(t, responsesTransportAdaptationEnabled(t.Context(), newOutbound(true), nil))
}

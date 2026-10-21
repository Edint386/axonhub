//nolint:exhaustruct_v5 // Test fixtures intentionally set only fields relevant to each scenario.
package biz

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
)

// TestRealtimeEndpoint_ForkHookInBuildNonDefaultEndpointOutbound guards the two fork lines in
// channel_llm.go buildNonDefaultEndpointOutbound:
//
//	case llm.APIFormatOpenAIRealtime.String(): // fork: see channel_endpoint_realtime.go
//		return newRealtimeEndpointOutbound(), nil
//
// Without them the switch falls into the upstream default branch ("unsupported api_format"),
// buildChannelWithOutbounds fails and ChannelService silently drops the whole channel from the
// enabled-channel cache. An upstream merge that loses the lines keeps compiling, so this test
// is the only thing that notices. It is listed in CUSTOM_BRANCH_RULES.md section 5.
func TestRealtimeEndpoint_ForkHookInBuildNonDefaultEndpointOutbound(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_realtime_fork?mode=memory&_fk=0")
	t.Cleanup(func() { client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	endpoints := []objects.ChannelEndpoint{{
		APIFormat: llm.APIFormatOpenAIRealtime.String(),
		BaseURL:   "https://x.example.com/api-ws/v1",
		Path:      "/realtime",
	}}

	// ValidateEndpoints accepts the format because init() registers it in SupportedAPIFormats.
	require.NoError(t, ValidateEndpoints(endpoints, "https://api.openai.com/v1"))

	c := &ent.Channel{
		ID:          1,
		Name:        "realtime-fork",
		Type:        channel.TypeOpenai,
		BaseURL:     "https://api.openai.com/v1",
		Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
		Endpoints:   endpoints,
	}

	ch, err := svc.buildChannelWithOutbounds(c)
	require.NoError(t, err, "the fork case in buildNonDefaultEndpointOutbound must accept openai/realtime")
	require.NotNil(t, ch)

	out, ok := ch.Outbounds[llm.APIFormatOpenAIRealtime.String()]
	require.True(t, ok, "the realtime endpoint must be registered in Outbounds")
	require.Equal(t, llm.APIFormatOpenAIRealtime, out.APIFormat())

	// The primary chat outbound is untouched by the extra endpoint.
	require.NotNil(t, ch.Outbounds[llm.APIFormatOpenAIChatCompletion.String()])

	// The relay reads the endpoint through RealtimeTarget, not through the stub.
	target, err := ch.RealtimeTarget(RealtimeModeWebSocket)
	require.NoError(t, err)
	require.True(t, target.Explicit)
	require.Equal(t, "wss://x.example.com/api-ws/v1/realtime", target.WSURL)
}

// TestRealtimeEndpoint_RejectsWebSocketBaseURL documents that realtime endpoints must store
// https base URLs: the upstream websocket-transport rejection that runs before the fork hook
// (and in ValidateEndpoints) still applies to openai/realtime.
func TestRealtimeEndpoint_RejectsWebSocketBaseURL(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_realtime_fork_ws?mode=memory&_fk=0")
	t.Cleanup(func() { client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	endpoints := []objects.ChannelEndpoint{{
		APIFormat: llm.APIFormatOpenAIRealtime.String(),
		BaseURL:   "wss://x.example.com/api-ws/v1",
		Path:      "/realtime",
	}}

	err := ValidateEndpoints(endpoints, "https://api.openai.com/v1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "websocket transport only supports")

	_, err = svc.buildChannelWithOutbounds(&ent.Channel{
		ID:          2,
		Name:        "realtime-wss",
		Type:        channel.TypeOpenai,
		BaseURL:     "https://api.openai.com/v1",
		Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
		Endpoints:   endpoints,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "websocket transport only supports")
}

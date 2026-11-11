package biz

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
)

// TestBailianASR_ChannelsShareTheJobCache: a client that resends a request whose Bailian job is
// still running (or done) must resume it on whichever Bailian channel the load balancer picks.
// With a per-channel cache scope the resend reached another channel's scope, found nothing and
// paid for the audio again.
func TestBailianASR_ChannelsShareTheJobCache(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_bailian_asr_cache?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	const baseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"

	first := bailianASRTestChannel(7, channel.TypeBailian, baseURL)
	second := bailianASRTestChannel(8, channel.TypeBailian, baseURL)
	second.Credentials.APIKey = "sk-another-account"

	firstChannel, err := svc.buildChannelWithOutbounds(first)
	require.NoError(t, err)
	secondChannel, err := svc.buildChannelWithOutbounds(second)
	require.NoError(t, err)

	firstConfig, err := bailianASRConfig(first, firstChannel, baseURL)
	require.NoError(t, err)
	secondConfig, err := bailianASRConfig(second, secondChannel, baseURL)
	require.NoError(t, err)

	require.Empty(t, firstConfig.CacheScope)
	require.Equal(t, firstConfig.CacheScope, secondConfig.CacheScope)
}

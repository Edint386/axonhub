package biz

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

// Custom guard: custom moved the delete-time eviction into
// forgetLimitersAndReloadAfterCommit, so upstream's per-channel API key
// selection state (#2548) is now cleared there. Without it a recreated channel
// id would inherit stale round-robin / fixed cursors.
func TestChannelDelete_ForgetsAPIKeySelectionState(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := channelAutoPriceTestContext(client)

	create := func(name string) *ent.Channel {
		ch, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
			Type:            channel.TypeOpenai,
			Name:            name,
			BaseURL:         lo.ToPtr("https://api.openai.com/v1"),
			Credentials:     objects.ChannelCredentials{APIKeys: []string{"k1", "k2"}},
			SupportedModels: []string{"gpt-test"},
		})
		require.NoError(t, err)

		return ch
	}
	hasState := func(id int) bool {
		svc.apiKeyStatesLock.Lock()
		defer svc.apiKeyStatesLock.Unlock()
		_, ok := svc.apiKeyStates[id]

		return ok
	}
	seed := func(id int) {
		svc.apiKeyStatesLock.Lock()
		defer svc.apiKeyStatesLock.Unlock()
		if svc.apiKeyStates == nil {
			svc.apiKeyStates = map[int]*apiKeySelectionState{}
		}
		svc.apiKeyStates[id] = &apiKeySelectionState{}
	}

	single := create("delete-one")
	seed(single.ID)
	require.NoError(t, svc.DeleteChannel(ctx, single.ID))
	require.False(t, hasState(single.ID), "DeleteChannel must forget the key selection state")

	a, b := create("bulk-a"), create("bulk-b")
	seed(a.ID)
	seed(b.ID)
	require.NoError(t, svc.BulkDeleteChannels(ctx, []int{a.ID, b.ID}))
	require.False(t, hasState(a.ID))
	require.False(t, hasState(b.ID))
}

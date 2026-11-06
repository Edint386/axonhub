package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

// Since upstream #2917 a plugin runs under its own principal and generic system bypass fails
// closed for it, while settings reads are host reads. The custom health-probe policy reads the
// setting directly, so a plugin granted read_channels can read it. Its policy write stays a system
// bypass (it must keep the caller's transaction), so a plugin's update is refused and nothing is
// stored. See CUSTOM_BRANCH_RULES.md section 3 (semantic review after a merge).
func TestChannelHealthProbePolicy_PluginPrincipal(t *testing.T) {
	systemService, client := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer client.Close()

	channelSvc := newTestChannelService(client)
	defer channelSvc.Stop()

	svc := &ChannelHealthProbeService{
		AbstractService: &AbstractService{db: client},
		channelService:  channelSvc,
		systemService:   systemService,
	}

	owner := &ent.User{ID: 1, IsOwner: true}
	parent := contexts.WithUser(authz.NewUserContext(ent.NewContext(context.Background(), client), owner.ID), owner)

	granted, err := authz.DelegatePlugin(parent, 7, []string{"read_channels", "write_channels"})
	require.NoError(t, err)

	_, err = svc.Policy(granted)
	require.NoError(t, err)
	_, err = svc.Overview(granted)
	require.NoError(t, err)

	_, err = svc.UpdatePolicy(granted, UpdateChannelHealthProbePolicyInput{
		Enabled: true, AcceptableLatencyMs: 60000, ExtraChannels: 1, P95LookbackHours: 24,
	})
	require.ErrorIs(t, err, authz.ErrPluginSystemBypass)

	stored, err := svc.ScanPolicy(parent)
	require.NoError(t, err)
	require.False(t, stored.Enabled, "a refused plugin update must not be stored")

	policy, err := svc.UpdatePolicy(parent, UpdateChannelHealthProbePolicyInput{
		Enabled: true, AcceptableLatencyMs: 60000, ExtraChannels: 1, P95LookbackHours: 24,
	})
	require.NoError(t, err, "a direct user with the channel scopes still updates the policy")
	require.True(t, policy.Enabled)

	ungranted, err := authz.DelegatePlugin(parent, 8, nil)
	require.NoError(t, err)
	_, err = svc.Policy(ungranted)
	require.Error(t, err, "a plugin without read_channels must not read the policy")
	_, err = svc.UpdatePolicy(ungranted, UpdateChannelHealthProbePolicyInput{Enabled: false, AcceptableLatencyMs: 60000, ExtraChannels: 1, P95LookbackHours: 24})
	require.Error(t, err, "a plugin without write_channels must not update the policy")
}

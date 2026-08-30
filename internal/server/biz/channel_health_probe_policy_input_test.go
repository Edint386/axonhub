package biz

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

// gateWindowMinutes arrived after this mutation shipped, and every other field on the
// input is required -- so a non-null addition rejected any caller that had no way to
// know about it. Omitting the field must keep the STORED window, not reset it to the
// default and not fail validation.
func TestChannelHealthProbeService_UpdatePolicyKeepsTheStoredGateWindowWhenOmitted(t *testing.T) {
	systemService, client := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), client))
	svc := &ChannelHealthProbeService{
		AbstractService: &AbstractService{db: client},
		systemService:   systemService,
	}

	baseInput := func() UpdateChannelHealthProbePolicyInput {
		return UpdateChannelHealthProbePolicyInput{
			Enabled:             true,
			IntervalMinutes:     5,
			AcceptableLatencyMs: 60_000,
			ExtraChannels:       1,
			P95LookbackHours:    24,
		}
	}

	// An omitted window on a fresh install keeps the default.
	updated, err := svc.UpdatePolicy(ctx, baseInput())
	require.NoError(t, err)
	require.Equal(t, defaultActiveHealthProbeScanSetting.GateWindowMinutes, updated.GateWindowMinutes)

	// An explicit window is applied.
	explicit := baseInput()
	window := 45
	explicit.GateWindowMinutes = &window
	updated, err = svc.UpdatePolicy(ctx, explicit)
	require.NoError(t, err)
	require.Equal(t, 45, updated.GateWindowMinutes)

	// Omitting it again keeps 45 -- the fallback reads the STORED value, not the
	// default, so an older caller cannot silently reset an operator's setting.
	updated, err = svc.UpdatePolicy(ctx, baseInput())
	require.NoError(t, err)
	require.Equal(t, 45, updated.GateWindowMinutes)
}

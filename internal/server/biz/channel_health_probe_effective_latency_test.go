package biz

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

type effectiveLatencyAPIKeySpec struct {
	status  apikey.Status
	latency *int64
}

func setupEffectiveAcceptableLatencyService(
	t *testing.T,
	globalAcceptableLatencyMs int,
	keys []effectiveLatencyAPIKeySpec,
) (*ChannelHealthProbeService, func()) {
	t.Helper()

	systemService, client := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), client))

	require.NoError(t, systemService.UpdateChannelSetting(ctx, UpdateSystemChannelSettings{
		ActiveHealthProbeScan: &ActiveHealthProbeScanSetting{
			Enabled:             true,
			AcceptableLatencyMs: globalAcceptableLatencyMs,
		},
	}))

	projectEntity := client.Project.Create().
		SetName("effective-acceptable-latency").
		SetStatus(project.StatusActive).
		SaveX(ctx)

	for index, spec := range keys {
		create := client.APIKey.Create().
			SetName(fmt.Sprintf("effective-acceptable-latency-%d", index)).
			SetKey(fmt.Sprintf("ah-effective-acceptable-latency-%d", index)).
			SetProjectID(projectEntity.ID).
			SetStatus(spec.status)

		profile := objects.APIKeyProfile{Name: "default"}
		if spec.latency != nil {
			latency := *spec.latency
			profile.MaxFirstTokenLatencyMs = &latency
		}

		create.SetProfiles(&objects.APIKeyProfiles{
			ActiveProfile: "default",
			Profiles:      []objects.APIKeyProfile{profile},
		}).ExecX(ctx)
	}

	svc := &ChannelHealthProbeService{
		AbstractService: &AbstractService{db: client},
		systemService:   systemService,
	}

	return svc, func() { client.Close() }
}

func effectiveAcceptableLatencyMs(t *testing.T, svc *ChannelHealthProbeService) int {
	t.Helper()

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), svc.db))
	value, err := svc.EffectiveAcceptableLatencyMs(ctx)
	require.NoError(t, err)

	return value
}

func ptrInt64(value int64) *int64 { return &value }

func TestChannelHealthProbeService_EffectiveAcceptableLatencyMsTakesTheStrictestKey(t *testing.T) {
	// The chain must stop on the strictest ceiling any ENABLED key can impose, not on
	// the operator's global 600s -- otherwise every answering channel looks acceptable
	// and the scan never walks down to the channels a strict key would exclude.
	svc, cleanup := setupEffectiveAcceptableLatencyService(t, 600_000, []effectiveLatencyAPIKeySpec{
		{status: apikey.StatusEnabled, latency: ptrInt64(30_000)},
		{status: apikey.StatusEnabled, latency: ptrInt64(20_000)},
	})
	defer cleanup()

	require.Equal(t, 20_000, effectiveAcceptableLatencyMs(t, svc))
}

func TestChannelHealthProbeService_EffectiveAcceptableLatencyMsKeepsGlobalWhenStricter(t *testing.T) {
	// min() in the other direction: a key ceiling looser than the global setting must
	// not relax the scan.
	svc, cleanup := setupEffectiveAcceptableLatencyService(t, 5_000, []effectiveLatencyAPIKeySpec{
		{status: apikey.StatusEnabled, latency: ptrInt64(20_000)},
	})
	defer cleanup()

	require.Equal(t, 5_000, effectiveAcceptableLatencyMs(t, svc))
}

func TestChannelHealthProbeService_EffectiveAcceptableLatencyMsFallsBackWhenNoKeySetsOne(t *testing.T) {
	// The global value is the documented fallback: with no ceiling anywhere it is what
	// remains in force.
	svc, cleanup := setupEffectiveAcceptableLatencyService(t, 600_000, []effectiveLatencyAPIKeySpec{
		{status: apikey.StatusEnabled},
		{status: apikey.StatusEnabled, latency: ptrInt64(0)},
	})
	defer cleanup()

	require.Equal(t, 600_000, effectiveAcceptableLatencyMs(t, svc))
}

func TestChannelHealthProbeService_EffectiveAcceptableLatencyMsIgnoresDisabledKeys(t *testing.T) {
	// A disabled key cannot route anything, so its ceiling must stop constraining the
	// scan the moment it is turned off.
	svc, cleanup := setupEffectiveAcceptableLatencyService(t, 600_000, []effectiveLatencyAPIKeySpec{
		{status: apikey.StatusDisabled, latency: ptrInt64(1_000)},
		{status: apikey.StatusEnabled, latency: ptrInt64(20_000)},
	})
	defer cleanup()

	require.Equal(t, 20_000, effectiveAcceptableLatencyMs(t, svc))
}

func TestChannelHealthProbeService_EffectiveAcceptableLatencyMsFallsBackWhenStrictKeyIsDeleted(t *testing.T) {
	svc, cleanup := setupEffectiveAcceptableLatencyService(t, 600_000, []effectiveLatencyAPIKeySpec{
		{status: apikey.StatusEnabled, latency: ptrInt64(20_000)},
	})
	defer cleanup()

	require.Equal(t, 20_000, effectiveAcceptableLatencyMs(t, svc))

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), svc.db))
	_, err := svc.db.APIKey.Delete().Exec(ctx)
	require.NoError(t, err)

	require.Equal(t, 600_000, effectiveAcceptableLatencyMs(t, svc))
}

func TestChannelHealthProbeService_PolicyReportsTheSameStrictestCeiling(t *testing.T) {
	// Policy and the scan must read one implementation, so the number the operator is
	// shown is the number the chain actually stops on.
	svc, cleanup := setupEffectiveAcceptableLatencyService(t, 600_000, []effectiveLatencyAPIKeySpec{
		{status: apikey.StatusEnabled, latency: ptrInt64(30_000)},
		{status: apikey.StatusEnabled, latency: ptrInt64(20_000)},
		{status: apikey.StatusDisabled, latency: ptrInt64(10_000)},
	})
	defer cleanup()

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), svc.db))
	policy, err := svc.Policy(ctx)
	require.NoError(t, err)
	require.NotNil(t, policy.APIKeyMaxFirstTokenLatencyMs)
	require.Equal(t, 20_000.0, *policy.APIKeyMaxFirstTokenLatencyMs)
	require.Equal(t, 600_000, policy.AcceptableLatencyMs)
	require.Equal(t, 20_000, effectiveAcceptableLatencyMs(t, svc))
}

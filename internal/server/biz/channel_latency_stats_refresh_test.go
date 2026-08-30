package biz

import (
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/pkg/xtime"
	"github.com/looplj/axonhub/internal/server/gql/qb"
)

// dropRequestsTable breaks the aggregation query without breaking the channel query
// that runs before it, so a refresh fails at SCOPE level -- the failure mode the
// all-or-nothing early return handled by publishing nothing at all.
func dropRequestsTable(t *testing.T, svc *ChannelService) {
	t.Helper()

	driver, ok := svc.db.Driver().(*entsql.Driver)
	require.True(t, ok)

	_, err := driver.DB().Exec("DROP TABLE requests")
	require.NoError(t, err)
}

// A failing refresh must not leave the routing ceiling reading a window that never
// expires. Returning early published nothing, so the previous snapshot stayed
// authoritative for as long as the error lasted; now the error is reported while the
// still-overlapping previous values are carried forward explicitly.
func TestChannelLatencyStatsRefreshReportsScopeFailuresAndKeepsOverlappingValues(t *testing.T) {
	svc, client, ctx := newLatencyStatsTestService(t, "refresh-scope-failure")

	ch := newLatencyStatsChannel(t, ctx, client, "probe-channel", channel.StatusEnabled)
	now := xtime.UTCNow()
	for _, latencyMs := range []int64{1000, 1200, 1400} {
		seedLatencySample(t, ctx, client, ch.ID, streamingSample(request.SourceTest, latencyMs, now.Add(-time.Minute)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	before, ok := svc.ChannelLatencyStats(ch.ID, "gpt-4", false)
	require.True(t, ok)
	require.True(t, before.UsableForGate())

	dropRequestsTable(t, svc)

	err := svc.RefreshChannelLatencyStats(ctx)
	require.Error(t, err, "a scope query failure must be reported, not swallowed")

	// The snapshot was republished (computedAt advanced) and the previous values were
	// carried forward, because the window they describe still overlaps the current one.
	after, ok := svc.ChannelLatencyStats(ch.ID, "gpt-4", false)
	require.True(t, ok)
	require.InEpsilon(t, before.AvgMs, after.AvgMs, 0.0001)
	require.Equal(t, before.SampleCount, after.SampleCount)
	require.Equal(t, int64(3), after.SampleCount)

	computedAt, _ := svc.ChannelLatencyStatsComputedAt()
	require.False(t, computedAt.IsZero())
}

// The carry-forward is bounded: once the previous snapshot is older than one window it
// describes a period that has entirely passed, so it is dropped and the pair reads
// UNKNOWN -- an admitted absence rather than a stale number presented as current.
func TestChannelLatencyStatsRefreshDropsValuesOlderThanTheWindow(t *testing.T) {
	svc, client, ctx := newLatencyStatsTestService(t, "refresh-stale-drop")

	ch := newLatencyStatsChannel(t, ctx, client, "probe-channel", channel.StatusEnabled)
	now := xtime.UTCNow()
	for _, latencyMs := range []int64{1000, 1200, 1400} {
		seedLatencySample(t, ctx, client, ch.ID, streamingSample(request.SourceTest, latencyMs, now.Add(-time.Minute)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	_, ok := svc.ChannelLatencyStats(ch.ID, "gpt-4", false)
	require.True(t, ok)

	// Age the published snapshot past the lookback window.
	current := svc.channelLatencyStats.Load()
	require.NotNil(t, current)
	svc.channelLatencyStats.Store(&channelLatencyStatsSnapshot{
		computedAt: current.computedAt.Add(-2 * channelLatencyStatsDefaultLookback),
		lookback:   current.lookback,
		samples:    current.samples,
	})

	dropRequestsTable(t, svc)

	require.Error(t, svc.RefreshChannelLatencyStats(ctx))

	_, ok = svc.ChannelLatencyStats(ch.ID, "gpt-4", false)
	require.False(t, ok, "a window that has entirely passed must read UNKNOWN, not stale")
}

// One scope failing must not discard the other scope's fresh rows, which is what the
// early return did.
func TestCarryForwardLatencyScopeTouchesOnlyItsOwnScope(t *testing.T) {
	svc, _, _ := newLatencyStatsTestService(t, "carry-forward-scope")

	now := xtime.UTCNow()
	probeKey := channelLatencyStatsKey{channelID: 1, modelID: "gpt-4", includeRealTraffic: false}
	allKey := channelLatencyStatsKey{channelID: 1, modelID: "gpt-4", includeRealTraffic: true}

	svc.channelLatencyStats.Store(&channelLatencyStatsSnapshot{
		computedAt: now.Add(-time.Minute),
		lookback:   channelLatencyStatsDefaultLookback,
		samples: map[channelLatencyStatsKey]ChannelLatencySample{
			probeKey: {AvgMs: 111, SampleCount: 5},
			allKey:   {AvgMs: 222, SampleCount: 7},
		},
	})

	// The probe scope succeeded this round and already wrote a fresh row; only the
	// all-sources scope failed and needs its previous value.
	samples := map[channelLatencyStatsKey]ChannelLatencySample{
		probeKey: {AvgMs: 999, SampleCount: 9},
	}
	svc.carryForwardLatencyScope(samples, true, now, channelLatencyStatsDefaultLookback)

	require.InEpsilon(t, 999.0, samples[probeKey].AvgMs, 0.0001, "the fresh scope must not be overwritten")
	require.InEpsilon(t, 222.0, samples[allKey].AvgMs, 0.0001, "the failed scope must keep its previous value")
}

func TestCarryForwardLatencyScopeHandlesAnEmptyHistory(t *testing.T) {
	svc, _, _ := newLatencyStatsTestService(t, "carry-forward-empty")

	samples := map[channelLatencyStatsKey]ChannelLatencySample{}
	svc.carryForwardLatencyScope(samples, false, xtime.UTCNow(), channelLatencyStatsDefaultLookback)

	require.Empty(t, samples)
}

// Guards the scope constant pairing the refresh relies on: the `all` scope is the one
// that admits real traffic.
func TestChannelLatencyStatsScopeMapsToIncludeRealTraffic(t *testing.T) {
	require.Equal(t, qb.ChannelLatencyStatsScope("all"), qb.ChannelLatencyStatsScopeAll)
	require.Equal(t, qb.ChannelLatencyStatsScope("probe"), qb.ChannelLatencyStatsScopeProbe)
}

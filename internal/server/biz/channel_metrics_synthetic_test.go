package biz

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	entrequest "github.com/looplj/axonhub/internal/ent/request"
)

// A synthetic probe runs the full request pipeline, so RecordPerformance is reached
// with a probe record. None of the real-traffic signals may move, and the request
// count must not drift: IncrementChannelSelection already counted the request at
// selection time and the sliding-window cleanup only subtracts SLOT counts, so a
// probe that skips the slot has to undo the selection increment itself.
func TestChannelService_RecordPerformanceIgnoresSyntheticProbes(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:synthetic-perf?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)

	const channelID = 7

	// Seed real traffic so every field under test starts non-zero and a regression
	// would show up as a changed value rather than an absent one.
	svc.IncrementChannelSelection(channelID)
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID:        channelID,
		Source:           entrequest.SourceAPI,
		StartTime:        time.Now().Add(-time.Second),
		EndTime:          time.Now(),
		Stream:           false,
		Success:          true,
		RequestCompleted: true,
	})
	svc.IncrementChannelSelection(channelID)
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID:          channelID,
		Source:             entrequest.SourceAPI,
		StartTime:          time.Now().Add(-time.Second),
		EndTime:            time.Now(),
		Success:            false,
		RequestCompleted:   true,
		ResponseStatusCode: 500,
	})

	firstTokenAt := time.Now()
	streamPerf := &PerformanceRecord{
		ChannelID:        channelID,
		Source:           entrequest.SourceAPI,
		StartTime:        firstTokenAt.Add(-2 * time.Second),
		FirstTokenTime:   &firstTokenAt,
		EndTime:          firstTokenAt.Add(time.Second),
		Stream:           true,
		Success:          true,
		RequestCompleted: true,
	}
	svc.IncrementChannelSelection(channelID)
	svc.RecordPerformance(ctx, streamPerf)

	before, err := svc.GetChannelMetrics(ctx, channelID)
	require.NoError(t, err)
	require.Positive(t, before.SuccessCount)
	require.Positive(t, before.FailureCount)
	require.Positive(t, before.StreamingSampleCount)
	require.Positive(t, before.NonStreamingSampleCount)
	require.Positive(t, before.StreamingFirstTokenLatencyEWMA)
	require.Positive(t, before.NonStreamingLatencyEWMA)
	require.NotNil(t, before.LastSelectedAt)

	// Selection happens for a probe too -- it goes through the same load balancer --
	// so the increment that RecordPerformance has to undo is present here.
	svc.IncrementChannelSelection(channelID)

	afterSelection, err := svc.GetChannelMetrics(ctx, channelID)
	require.NoError(t, err)
	require.Equal(t, before.RequestCount+1, afterSelection.RequestCount)

	lastSelectedAtBeforeProbe := *afterSelection.LastSelectedAt

	probeFirstTokenAt := time.Now().Add(time.Minute)
	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID:        channelID,
		Source:           entrequest.SourceTest,
		StartTime:        probeFirstTokenAt.Add(-30 * time.Second),
		FirstTokenTime:   &probeFirstTokenAt,
		EndTime:          probeFirstTokenAt.Add(30 * time.Second),
		Stream:           true,
		Success:          true,
		RequestCompleted: true,
	})

	after, err := svc.GetChannelMetrics(ctx, channelID)
	require.NoError(t, err)

	require.Equal(t, before.SuccessCount, after.SuccessCount)
	require.Equal(t, before.FailureCount, after.FailureCount)
	require.Equal(t, before.ConsecutiveFailures, after.ConsecutiveFailures)
	require.Equal(t, before.StreamingFirstTokenLatencyEWMA, after.StreamingFirstTokenLatencyEWMA)
	require.Equal(t, before.StreamingTokensPerSecondEWMA, after.StreamingTokensPerSecondEWMA)
	require.Equal(t, before.StreamingSampleCount, after.StreamingSampleCount)
	require.Equal(t, before.NonStreamingLatencyEWMA, after.NonStreamingLatencyEWMA)
	require.Equal(t, before.NonStreamingSampleCount, after.NonStreamingSampleCount)
	require.NotNil(t, after.LastSelectedAt)
	require.True(t, after.LastSelectedAt.Equal(lastSelectedAtBeforeProbe),
		"a probe must not advance LastSelectedAt past the last selection")

	// The whole point of the decrement: the aggregate returns to its pre-selection
	// value instead of keeping the probe's selection increment forever.
	require.Equal(t, before.RequestCount, after.RequestCount)
}

// A failing probe must not push a channel toward auto-disable, which is exactly what
// consecutive-failure state drives.
func TestChannelService_RecordPerformanceIgnoresFailedSyntheticProbes(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:synthetic-perf-failure?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	svc := newTestChannelService(client)

	const channelID = 9

	svc.IncrementChannelSelection(channelID)

	beforeCount, err := svc.GetChannelMetrics(ctx, channelID)
	require.NoError(t, err)
	require.Equal(t, int64(1), beforeCount.RequestCount)

	svc.RecordPerformance(ctx, &PerformanceRecord{
		ChannelID:          channelID,
		Source:             entrequest.SourceTest,
		StartTime:          time.Now().Add(-time.Second),
		EndTime:            time.Now(),
		Success:            false,
		RequestCompleted:   true,
		ResponseStatusCode: 500,
	})

	after, err := svc.GetChannelMetrics(ctx, channelID)
	require.NoError(t, err)
	require.Equal(t, int64(0), after.FailureCount)
	require.Equal(t, int64(0), after.ConsecutiveFailures)
	require.Equal(t, int64(0), after.RequestCount)
	require.Nil(t, after.LastFailureAt)
}

func TestPerformanceRecord_IsSynthetic(t *testing.T) {
	// An absent source must never be mistaken for a probe.
	require.False(t, (&PerformanceRecord{}).IsSynthetic())
	require.False(t, (&PerformanceRecord{Source: entrequest.SourceAPI}).IsSynthetic())
	require.False(t, (&PerformanceRecord{Source: entrequest.SourcePlayground}).IsSynthetic())
	require.True(t, (&PerformanceRecord{Source: entrequest.SourceTest}).IsSynthetic())
}

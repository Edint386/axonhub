package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/biz"
)

func TestRunPriorityProbeTargetsStopsAfterConfiguredFallbacks(t *testing.T) {
	targets := []biz.ChannelHealthProbeTarget{
		{ChannelID: 1},
		{ChannelID: 2},
		{ChannelID: 3},
		{ChannelID: 4},
		{ChannelID: 5},
	}
	executed := make([]int, 0)
	skipped := runPriorityProbeTargets(t.Context(), targets, 100, 1, func(
		_ context.Context,
		target biz.ChannelHealthProbeTarget,
	) (*biz.ChannelHealthProbeRunRecord, bool) {
		executed = append(executed, target.ChannelID)
		if target.ChannelID == 2 {
			latency := 80.0
			return &biz.ChannelHealthProbeRunRecord{Status: "healthy", Stream: true, TTFTMs: &latency}, true
		}
		return &biz.ChannelHealthProbeRunRecord{Status: "unhealthy"}, true
	})

	require.Equal(t, []int{1, 2, 3}, executed)
	require.Equal(t, []biz.ChannelHealthProbeTarget{targets[3], targets[4]}, skipped)
}

func TestRunPriorityProbeTargetsStopsWhenAnotherInstanceOwnsGroup(t *testing.T) {
	targets := []biz.ChannelHealthProbeTarget{{ChannelID: 1}, {ChannelID: 2}}
	executed := 0
	skipped := runPriorityProbeTargets(t.Context(), targets, 100, 0, func(
		_ context.Context,
		_ biz.ChannelHealthProbeTarget,
	) (*biz.ChannelHealthProbeRunRecord, bool) {
		executed++
		return nil, false
	})

	require.Equal(t, 1, executed)
	require.Empty(t, skipped)
}

func TestChannelHealthProbeRunIsAcceptableUsesModeSpecificFirstTokenMetric(t *testing.T) {
	ttfb := 25.0
	ttft := 125.0
	require.False(t, channelHealthProbeRunIsAcceptable(&biz.ChannelHealthProbeRunRecord{
		Status:  "healthy",
		Stream:  true,
		TTFBMs:  &ttfb,
		TTFTMs:  &ttft,
		TotalMs: 150,
	}, 100))
	require.True(t, channelHealthProbeRunIsAcceptable(&biz.ChannelHealthProbeRunRecord{
		Status:  "healthy",
		Stream:  false,
		TTFBMs:  &ttfb,
		TTFTMs:  &ttft,
		TotalMs: 150,
	}, 100))
}

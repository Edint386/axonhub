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

func TestRunPriorityProbeTargetsStopsWhenTheTopChannelIsAcceptable(t *testing.T) {
	// The reported symptom: with one spare configured and a threshold loose enough that
	// the highest-priority channel always passes, only two channels may be probed and
	// every remaining one must be reported as skipped.
	targets := []biz.ChannelHealthProbeTarget{
		{ChannelID: 1},
		{ChannelID: 2},
		{ChannelID: 3},
		{ChannelID: 4},
		{ChannelID: 5},
		{ChannelID: 6},
	}
	executed := make([]int, 0)
	skipped := runPriorityProbeTargets(t.Context(), targets, 600_000, 1, func(
		_ context.Context,
		target biz.ChannelHealthProbeTarget,
	) (*biz.ChannelHealthProbeRunRecord, bool) {
		executed = append(executed, target.ChannelID)
		latency := 5_000.0

		return &biz.ChannelHealthProbeRunRecord{Status: "healthy", Stream: true, TTFTMs: &latency}, true
	})

	require.Equal(t, []int{1, 2}, executed)
	require.Equal(t, targets[2:], skipped)
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

// The reaper's patience must exceed a probe's whole budget, and nothing in the type
// system says so: the threshold lives in biz and the budget lives here.
//
// Break the inequality and the sweep starts closing runs that are still executing. The
// probe then finishes and CompleteRun's `Where(status == pending)` matches zero rows, so
// a genuine result is discarded behind a generic persist error -- a silent loss that
// looks like a write failure. This assertion is the only thing tying the two packages.
func TestChannelHealthProbeStaleThresholdExceedsTheProbeBudget(t *testing.T) {
	budget := channelHealthProbeTimeout + channelHealthProbePersistTimeout

	require.Greater(t, biz.ChannelHealthProbeStaleAfter, budget,
		"a probe can legitimately hold a pending row for %s; reaping earlier than that discards real results", budget)
}

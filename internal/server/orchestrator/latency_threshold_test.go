package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

// latencyStatsKey mirrors the identity of one windowed statistic: a channel, a
// streaming mode, and whether real traffic was admitted alongside probes.
type latencyStatsKey struct {
	channelID          int
	streaming          bool
	includeRealTraffic bool
}

type latencyThresholdStatsProvider struct {
	samples map[latencyStatsKey]biz.ChannelLatencySample
}

func (p *latencyThresholdStatsProvider) ChannelLatencyStats(
	channelID int,
	streaming bool,
	includeRealTraffic bool,
) (biz.ChannelLatencySample, bool) {
	sample, ok := p.samples[latencyStatsKey{
		channelID:          channelID,
		streaming:          streaming,
		includeRealTraffic: includeRealTraffic,
	}]

	return sample, ok
}

// probeScope and allScope name the two source scopes at the call site, so a fixture
// reads as "this channel's probe-only window says X".
func probeScope(channelID int, streaming bool) latencyStatsKey {
	return latencyStatsKey{channelID: channelID, streaming: streaming}
}

func allScope(channelID int, streaming bool) latencyStatsKey {
	return latencyStatsKey{channelID: channelID, streaming: streaming, includeRealTraffic: true}
}

// window builds a statistic whose MEAN is meanMs -- the value the ceiling reads.
//
// The percentile is deliberately set an order of magnitude above the mean. A fixture
// where both numbers agreed could not tell the two apart, so every test built here
// also fails if the ceiling goes back to reading the tail.
func window(meanMs float64, sampleCount int64) biz.ChannelLatencySample {
	return biz.ChannelLatencySample{
		AvgMs:       meanMs,
		P95Ms:       meanMs * 10,
		SampleCount: sampleCount,
	}
}

func latencyStats(samples map[latencyStatsKey]biz.ChannelLatencySample) *latencyThresholdStatsProvider {
	return &latencyThresholdStatsProvider{samples: samples}
}

type staticCandidateSelector struct {
	candidates []*ChannelModelsCandidate
}

func (s *staticCandidateSelector) Select(_ context.Context, _ *llm.Request) ([]*ChannelModelsCandidate, error) {
	return s.candidates, nil
}

func latencyCandidate(id int) *ChannelModelsCandidate {
	return &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{ID: id}}}
}

func streamRequest(stream bool) *llm.Request {
	return &llm.Request{Stream: &stream}
}

func TestFirstTokenLatencySelectorFiltersKnownSlowCandidates(t *testing.T) {
	fast := latencyCandidate(1)
	slow := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{fast, slow}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, true): window(250, 4),
			allScope(2, true): window(900, 4),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, fast, candidates[0])
}

func TestFirstTokenLatencySelectorKeepsChannelsAbsentFromTheWindow(t *testing.T) {
	// A channel with nothing in the window is UNKNOWN, not fast and not slow. This is
	// also what lets a channel recover: once its samples age out of the window it
	// returns to unknown and is routed to again, instead of being pinned by a value
	// that can never be updated.
	knownSlow := latencyCandidate(1)
	absent := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{knownSlow, absent}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, true): window(900, 4),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, absent, candidates[0])
}

func TestFirstTokenLatencySelectorIgnoresWindowsBelowSampleFloor(t *testing.T) {
	// A couple of slow samples must not evict a channel, so a thin window stays
	// unknown and the candidate is kept.
	thin := latencyCandidate(1)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{thin}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, true): window(9_000, MinimumLatencyThresholdSamples-1),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, thin, candidates[0])
}

func TestFirstTokenLatencySelectorFallsBackWhenAllKnownCandidatesFail(t *testing.T) {
	// Best-effort guarantee: filtering everything out returns the original list, so a
	// profile can never leave a request with no route.
	first := latencyCandidate(1)
	second := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{first, second}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, false): window(900, 4),
			allScope(2, false): window(800, 4),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(false))
	require.NoError(t, err)
	require.Equal(t, []*ChannelModelsCandidate{first, second}, candidates)
}

func TestFirstTokenLatencySelectorKeepsCandidatesWithoutAProvider(t *testing.T) {
	first := latencyCandidate(1)
	second := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{first, second}},
		nil,
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Equal(t, []*ChannelModelsCandidate{first, second}, candidates)
}

func TestFirstTokenLatencySelectorJudgesByTheMeanNotTheTail(t *testing.T) {
	// The ceiling reads the window's MEAN. A channel with a heavy tail -- here a P95 of
	// 32.5s, a real measurement from a deployment -- stays eligible as long as it is
	// usually fast, while a channel that is uniformly slower than the limit does not.
	//
	// This is the guard for the switch away from the percentile: judged by P95 over a
	// window measured in hours, the first channel was excluded by a 10s ceiling even
	// though it typically answered in a fraction of it.
	tailHeavyButUsuallyFast := latencyCandidate(1)
	uniformlySlow := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{tailHeavyButUsuallyFast, uniformlySlow}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, true): {AvgMs: 300, P95Ms: 32_500, SampleCount: 50},
			allScope(2, true): {AvgMs: 900, P95Ms: 1_000, SampleCount: 50},
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, tailHeavyButUsuallyFast, candidates[0])
}

func TestFirstTokenLatencySelectorTreatsAMissingMeanAsUnknown(t *testing.T) {
	// A row carrying a percentile but no usable mean must not read as instantly fast.
	// Every sample in the window is a positive latency by construction, so this only
	// happens when the row carried nothing usable -- which is unknown, and unknown
	// keeps the candidate.
	noMean := latencyCandidate(1)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{noMean, latencyCandidate(2)}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, true): {AvgMs: 0, P95Ms: 9_000, SampleCount: 50},
			allScope(2, true): {AvgMs: 100, P95Ms: 200, SampleCount: 50},
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Len(t, candidates, 2)
}

func TestFirstTokenLatencySelectorProbeOnlyReadsTheProbeScopedWindow(t *testing.T) {
	// Default (countRealTrafficLatency=false): the ceiling reads the window that
	// counts synthetic probes only. Channel 1's real traffic is slow but its probes
	// are not, so it survives; channel 2's probes are slow and it does not. The whole
	// switch is this scope choice -- there is no second number to reconcile.
	slowTrafficFastProbes := latencyCandidate(1)
	slowProbes := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{slowTrafficFastProbes, slowProbes}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			probeScope(1, true): window(100, 50),
			allScope(1, true):   window(9_000, 50),
			probeScope(2, true): window(9_000, 50),
			allScope(2, true):   window(100, 50),
		}),
		500,
		false,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, slowTrafficFastProbes, candidates[0])
}

func TestFirstTokenLatencySelectorCountingTrafficReadsTheAllScopedWindow(t *testing.T) {
	// The mirror case with the flag on: the same two channels swap verdicts, because
	// now the window that includes real traffic is the one being read.
	slowTrafficFastProbes := latencyCandidate(1)
	slowProbesFastTraffic := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{slowTrafficFastProbes, slowProbesFastTraffic}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			probeScope(1, true): window(100, 50),
			allScope(1, true):   window(9_000, 50),
			probeScope(2, true): window(9_000, 50),
			allScope(2, true):   window(100, 50),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, slowProbesFastTraffic, candidates[0])
}

func TestFirstTokenLatencySelectorPrefersTheMatchingStreamingWindow(t *testing.T) {
	// Streaming rows measure a first-token boundary and non-streaming rows a total
	// response time, so when both windows exist the request's own mode decides. Here
	// the streaming window breaches the ceiling and the non-streaming one does not.
	candidate := latencyCandidate(1)
	samples := map[latencyStatsKey]biz.ChannelLatencySample{
		allScope(1, true):  window(9_000, 50),
		allScope(1, false): window(100, 50),
	}

	nonStreaming, err := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{candidate}},
		latencyStats(samples),
		500,
		true,
	).Select(context.Background(), streamRequest(false))
	require.NoError(t, err)
	require.Len(t, nonStreaming, 1)
	require.Same(t, candidate, nonStreaming[0])

	// The same channel, judged as streaming, is over the ceiling -- and with only one
	// candidate the best-effort fallback returns it anyway, so assert the signal
	// directly rather than the filtered list.
	latencyMs, known := latencySignalForCandidate(candidate, streamRequest(true), latencyStats(samples), true)
	require.True(t, known)
	require.InDelta(t, 9_000.0, latencyMs, 0.001)
}

func TestFirstTokenLatencySelectorFallsBackToTheOtherModeWindow(t *testing.T) {
	// The default configuration runs probes NON-streaming, so a streaming request
	// finds the probe-scoped streaming window empty. Falling back to the other mode
	// keeps the ceiling working; without it the ceiling would silently pass every
	// channel, which is how a limit quietly stops existing.
	slowProbes := latencyCandidate(1)
	fastProbes := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{slowProbes, fastProbes}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			probeScope(1, false): window(9_000, 50),
			probeScope(2, false): window(100, 50),
		}),
		500,
		false,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(true))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, fastProbes, candidates[0])
}

func TestFirstTokenLatencySelectorUsesStreamingWindowWhenThePolicyForcesStreaming(t *testing.T) {
	// A require-stream channel is called upstream in streaming mode whatever the
	// caller asked for, so its streaming window is the comparable one. Channel 1 holds
	// both windows and only the streaming one breaches, so reading the wrong one would
	// keep it.
	forced := &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{
		ID:       1,
		Policies: objects.ChannelPolicies{Stream: objects.CapabilityPolicyRequire},
	}}}
	other := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{forced, other}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, true):  window(9_000, 50),
			allScope(1, false): window(100, 50),
			allScope(2, false): window(100, 50),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), streamRequest(false))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, other, candidates[0])
}

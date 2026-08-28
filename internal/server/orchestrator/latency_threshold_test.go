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

// latencyStatsKey mirrors the identity of one windowed statistic: a channel, the
// model the request asked for, and whether real traffic was admitted alongside
// probes.
type latencyStatsKey struct {
	channelID          int
	modelID            string
	includeRealTraffic bool
}

type latencyThresholdStatsProvider struct {
	samples map[latencyStatsKey]biz.ChannelLatencySample
}

func (p *latencyThresholdStatsProvider) ChannelLatencyStats(
	channelID int,
	modelID string,
	includeRealTraffic bool,
) (biz.ChannelLatencySample, bool) {
	sample, ok := p.samples[latencyStatsKey{
		channelID:          channelID,
		modelID:            modelID,
		includeRealTraffic: includeRealTraffic,
	}]

	return sample, ok
}

// probeScope and allScope name the two source scopes at the call site, so a fixture
// reads as "this channel's probe-only window for this model says X".
func probeScope(channelID int, modelID string) latencyStatsKey {
	return latencyStatsKey{channelID: channelID, modelID: modelID}
}

func allScope(channelID int, modelID string) latencyStatsKey {
	return latencyStatsKey{channelID: channelID, modelID: modelID, includeRealTraffic: true}
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

// modelRequest names the model being asked for, which is the dimension the ceiling
// looks the statistic up by.
func modelRequest(model string) *llm.Request {
	return &llm.Request{Model: model}
}

func streamingModelRequest(model string, stream bool) *llm.Request {
	return &llm.Request{Model: model, Stream: &stream}
}

const (
	modelA = "model-a"
	modelB = "model-b"
	modelC = "model-c"
)

func TestFirstTokenLatencySelectorFiltersKnownSlowCandidates(t *testing.T) {
	fast := latencyCandidate(1)
	slow := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{fast, slow}},
		latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, modelA): window(250, 4),
			allScope(2, modelA): window(900, 4),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
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
			allScope(1, modelA): window(900, 4),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
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
			allScope(1, modelA): window(9_000, biz.ChannelLatencyGateMinimumSamples-1),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
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
			allScope(1, modelA): window(900, 4),
			allScope(2, modelA): window(800, 4),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
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

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
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
			allScope(1, modelA): {AvgMs: 300, P95Ms: 32_500, SampleCount: 50},
			allScope(2, modelA): {AvgMs: 900, P95Ms: 1_000, SampleCount: 50},
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
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
			allScope(1, modelA): {AvgMs: 0, P95Ms: 9_000, SampleCount: 50},
			allScope(2, modelA): {AvgMs: 100, P95Ms: 200, SampleCount: 50},
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
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
			probeScope(1, modelA): window(100, 50),
			allScope(1, modelA):   window(9_000, 50),
			probeScope(2, modelA): window(9_000, 50),
			allScope(2, modelA):   window(100, 50),
		}),
		500,
		false,
	)

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
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
			probeScope(1, modelA): window(100, 50),
			allScope(1, modelA):   window(9_000, 50),
			probeScope(2, modelA): window(9_000, 50),
			allScope(2, modelA):   window(100, 50),
		}),
		500,
		true,
	)

	candidates, err := selector.Select(context.Background(), modelRequest(modelA))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, slowProbesFastTraffic, candidates[0])
}

func TestFirstTokenLatencySelectorJudgesEachModelByItsOwnWindow(t *testing.T) {
	// The whole point of keying on the model: one channel, two models, opposite
	// verdicts. Whichever model the request names is the one measured.
	//
	// This is the guard against a per-channel figure. Collapse the two windows into
	// one and both requests get the same answer, which means one of the two models is
	// being judged by measurements of the other.
	channel := latencyCandidate(1)
	samples := map[latencyStatsKey]biz.ChannelLatencySample{
		allScope(1, modelA): window(100, 50),
		allScope(1, modelB): window(9_000, 50),
	}

	askingForA, known := latencySignalForCandidate(channel, modelRequest(modelA), latencyStats(samples), true)
	require.True(t, known)
	require.InDelta(t, 100.0, askingForA, 0.001)

	askingForB, known := latencySignalForCandidate(channel, modelRequest(modelB), latencyStats(samples), true)
	require.True(t, known)
	require.InDelta(t, 9_000.0, askingForB, 0.001)
}

func TestFirstTokenLatencySelectorLeavesAnUnmeasuredModelUnjudged(t *testing.T) {
	// A model with no window of its own -- never probed, no real traffic -- is UNKNOWN,
	// so the candidate is kept and the ordering is left to the rest of the chain.
	// Borrowing the sibling model's number would exclude this channel on evidence
	// about a different model.
	channel := latencyCandidate(1)
	samples := map[latencyStatsKey]biz.ChannelLatencySample{
		allScope(1, modelA): window(9_000, 50),
	}

	_, known := latencySignalForCandidate(channel, modelRequest(modelC), latencyStats(samples), true)
	require.False(t, known)

	// And end to end: asking for the unmeasured model keeps both candidates, while
	// asking for the measured one excludes the slow channel.
	unmeasured, err := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{channel, latencyCandidate(2)}},
		latencyStats(samples),
		500,
		true,
	).Select(context.Background(), modelRequest(modelC))
	require.NoError(t, err)
	require.Len(t, unmeasured, 2)
}

func TestFirstTokenLatencySelectorTreatsAnUnnamedModelAsUnknown(t *testing.T) {
	// A request with no model names no window, so it cannot be judged. The empty
	// string must not become a bucket that every unnamed request shares.
	channel := latencyCandidate(1)

	_, known := latencySignalForCandidate(channel, modelRequest(""), latencyStats(
		map[latencyStatsKey]biz.ChannelLatencySample{
			allScope(1, ""): window(9_000, 50),
		},
	), true)
	require.False(t, known)
}

func TestFirstTokenLatencySelectorIgnoresStreamingMode(t *testing.T) {
	// Streaming mode is NOT part of the key. One window per channel and model serves
	// both modes, so the same request judged streaming and non-streaming gets the same
	// verdict -- including on a require-stream channel, whose upstream call is always
	// streaming whatever the caller asked for.
	//
	// Re-introducing the split would make this fail: the fixture holds one window, and
	// a mode-keyed lookup would miss it for at least one of the three requests.
	forced := &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{
		ID:       1,
		Policies: objects.ChannelPolicies{Stream: objects.CapabilityPolicyRequire},
	}}}
	samples := latencyStats(map[latencyStatsKey]biz.ChannelLatencySample{
		allScope(1, modelA): window(9_000, 50),
	})

	for _, req := range []*llm.Request{
		modelRequest(modelA),
		streamingModelRequest(modelA, true),
		streamingModelRequest(modelA, false),
	} {
		latencyMs, known := latencySignalForCandidate(forced, req, samples, true)
		require.True(t, known)
		require.InDelta(t, 9_000.0, latencyMs, 0.001)
	}
}

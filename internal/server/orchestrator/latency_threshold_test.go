package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

type latencyThresholdMetricsProvider struct {
	metrics map[int]*biz.AggregatedMetrics
	err     error
}

func (p *latencyThresholdMetricsProvider) GetChannelMetrics(_ context.Context, channelID int) (*biz.AggregatedMetrics, error) {
	if p.err != nil {
		return nil, p.err
	}

	return p.metrics[channelID], nil
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

func TestFirstTokenLatencySelectorFiltersKnownSlowCandidates(t *testing.T) {
	fast := latencyCandidate(1)
	slow := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{fast, slow}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {StreamingFirstTokenLatencyEWMA: 250, StreamingSampleCount: 4},
			2: {StreamingFirstTokenLatencyEWMA: 900, StreamingSampleCount: 4},
		}},
		500,
		true,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, fast, candidates[0])
}

func TestFirstTokenLatencySelectorKeepsUnknownSamples(t *testing.T) {
	knownSlow := latencyCandidate(1)
	unknown := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{knownSlow, unknown}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {StreamingFirstTokenLatencyEWMA: 900, StreamingSampleCount: 4},
			2: {StreamingFirstTokenLatencyEWMA: 100, StreamingSampleCount: 1},
		}},
		500,
		true,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, unknown, candidates[0])
}

func TestFirstTokenLatencySelectorFallsBackWhenAllKnownCandidatesFail(t *testing.T) {
	first := latencyCandidate(1)
	second := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{first, second}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {NonStreamingLatencyEWMA: 900, NonStreamingSampleCount: 4},
			2: {NonStreamingLatencyEWMA: 800, NonStreamingSampleCount: 4},
		}},
		500,
		true,
	)

	stream := false
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Equal(t, []*ChannelModelsCandidate{first, second}, candidates)
}

func TestFirstTokenLatencySelectorKeepsCandidatesWhenMetricsReadFails(t *testing.T) {
	first := latencyCandidate(1)
	second := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{first, second}},
		&latencyThresholdMetricsProvider{err: errors.New("metrics unavailable")},
		500,
		true,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Equal(t, []*ChannelModelsCandidate{first, second}, candidates)
}

func TestFirstTokenLatencySelectorUsesProbeSignalWithoutRealTraffic(t *testing.T) {
	// The reason probes are read at all: a channel that has never served a real
	// request carries no traffic EWMA, so before this it was permanently "unknown"
	// and the ceiling could never filter it however slow the probes measured it.
	fast := latencyCandidate(1)
	slow := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{fast, slow}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {ProbeFirstTokenLatencyEWMA: 200, ProbeSampleCount: 4},
			2: {ProbeFirstTokenLatencyEWMA: 8_000, ProbeSampleCount: 4},
		}},
		500,
		false,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, fast, candidates[0])
}

func TestFirstTokenLatencySelectorTakesTheWorseOfProbeAndTraffic(t *testing.T) {
	// Both sources measure the same thing, so a ceiling must honour whichever says
	// the channel is slow -- otherwise a fast probe would mask slow real traffic.
	slowTrafficFastProbe := latencyCandidate(1)
	fastTrafficSlowProbe := latencyCandidate(2)
	bothFast := latencyCandidate(3)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{slowTrafficFastProbe, fastTrafficSlowProbe, bothFast}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {StreamingFirstTokenLatencyEWMA: 900, StreamingSampleCount: 4, ProbeFirstTokenLatencyEWMA: 100, ProbeSampleCount: 4},
			2: {StreamingFirstTokenLatencyEWMA: 100, StreamingSampleCount: 4, ProbeFirstTokenLatencyEWMA: 900, ProbeSampleCount: 4},
			3: {StreamingFirstTokenLatencyEWMA: 100, StreamingSampleCount: 4, ProbeFirstTokenLatencyEWMA: 200, ProbeSampleCount: 4},
		}},
		500,
		true,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, bothFast, candidates[0])
}

func TestFirstTokenLatencySelectorIgnoresProbeSignalBelowSampleFloor(t *testing.T) {
	// One slow probe must not evict a channel; the same sample floor as real traffic
	// applies, so the signal stays unknown and the candidate is kept.
	thin := latencyCandidate(1)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{thin}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {ProbeFirstTokenLatencyEWMA: 8_000, ProbeSampleCount: MinimumLatencyThresholdSamples - 1},
		}},
		500,
		false,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, thin, candidates[0])
}

func TestFirstTokenLatencySelectorProbeOnlyIgnoresRealTraffic(t *testing.T) {
	// Default (countRealTrafficLatency=false): only probes may judge a channel. Slow
	// real traffic with no probe samples leaves the channel UNKNOWN, and unknown keeps
	// it -- while a channel whose probes are slow is filtered on that signal alone.
	slowTrafficNoProbes := latencyCandidate(1)
	slowProbes := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{slowTrafficNoProbes, slowProbes}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {StreamingFirstTokenLatencyEWMA: 9_000, StreamingSampleCount: 50},
			2: {ProbeFirstTokenLatencyEWMA: 9_000, ProbeSampleCount: 50},
		}},
		500,
		false,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, slowTrafficNoProbes, candidates[0])
}

func TestFirstTokenLatencySelectorProbeOnlyIgnoresFastTrafficOnASlowProbe(t *testing.T) {
	// The mirror case: fast real traffic must not rescue a channel whose probes are
	// slow, otherwise the flag would have no effect in the direction that matters.
	slowProbeFastTraffic := latencyCandidate(1)
	fastProbe := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{slowProbeFastTraffic, fastProbe}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {StreamingFirstTokenLatencyEWMA: 100, StreamingSampleCount: 50, ProbeFirstTokenLatencyEWMA: 9_000, ProbeSampleCount: 50},
			2: {ProbeFirstTokenLatencyEWMA: 100, ProbeSampleCount: 50},
		}},
		500,
		false,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, fastProbe, candidates[0])
}

func TestFirstTokenLatencySelectorProbeOnlyFallsBackWhenEveryProbeIsSlow(t *testing.T) {
	// Best-effort guarantee: filtering everything out returns the original list, so a
	// profile can never leave a request with no route.
	first := latencyCandidate(1)
	second := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{first, second}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {ProbeFirstTokenLatencyEWMA: 9_000, ProbeSampleCount: 4},
			2: {ProbeFirstTokenLatencyEWMA: 8_000, ProbeSampleCount: 4},
		}},
		500,
		false,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Equal(t, []*ChannelModelsCandidate{first, second}, candidates)
}

func TestFirstTokenLatencySelectorCountingTrafficUsesTrafficWithoutProbes(t *testing.T) {
	// With the flag on, real traffic alone is enough to filter -- the case the default
	// deliberately leaves unknown.
	slowTraffic := latencyCandidate(1)
	fastTraffic := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{slowTraffic, fastTraffic}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {StreamingFirstTokenLatencyEWMA: 9_000, StreamingSampleCount: 50},
			2: {StreamingFirstTokenLatencyEWMA: 100, StreamingSampleCount: 50},
		}},
		500,
		true,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, fastTraffic, candidates[0])
}

func TestFirstTokenLatencySelectorCountingTrafficRespectsTrafficSampleFloor(t *testing.T) {
	// The sample floor applies to the traffic source too: a couple of slow requests
	// must not evict a channel, so the signal stays unknown and the candidate is kept.
	thinTraffic := latencyCandidate(1)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{thinTraffic}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {StreamingFirstTokenLatencyEWMA: 9_000, StreamingSampleCount: MinimumLatencyThresholdSamples - 1},
		}},
		500,
		true,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Same(t, thinTraffic, candidates[0])
}

func TestFirstTokenLatencySelectorCountingTrafficFallsBackWhenAllFiltered(t *testing.T) {
	// All-filtered falls back in the counting-traffic mode as well, with the worse of
	// the two signals deciding each candidate.
	first := latencyCandidate(1)
	second := latencyCandidate(2)
	selector := WithFirstTokenLatencySelector(
		&staticCandidateSelector{candidates: []*ChannelModelsCandidate{first, second}},
		&latencyThresholdMetricsProvider{metrics: map[int]*biz.AggregatedMetrics{
			1: {StreamingFirstTokenLatencyEWMA: 9_000, StreamingSampleCount: 50, ProbeFirstTokenLatencyEWMA: 100, ProbeSampleCount: 50},
			2: {StreamingFirstTokenLatencyEWMA: 100, StreamingSampleCount: 50, ProbeFirstTokenLatencyEWMA: 8_000, ProbeSampleCount: 50},
		}},
		500,
		true,
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Equal(t, []*ChannelModelsCandidate{first, second}, candidates)
}

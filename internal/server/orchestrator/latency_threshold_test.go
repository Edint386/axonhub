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
	)

	stream := true
	candidates, err := selector.Select(context.Background(), &llm.Request{Stream: &stream})
	require.NoError(t, err)
	require.Equal(t, []*ChannelModelsCandidate{first, second}, candidates)
}

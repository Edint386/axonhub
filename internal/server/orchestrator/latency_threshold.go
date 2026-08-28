package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

// ChannelLatencyStatsProvider supplies the windowed first-token latency a ceiling
// judges a channel by. Implemented by biz.ChannelService, which recomputes the
// statistic from the requests table on a schedule; the lookup here is a map read, so
// no routing decision waits on a query.
type ChannelLatencyStatsProvider interface {
	ChannelLatencyStats(channelID int, modelID string, includeRealTraffic bool) (biz.ChannelLatencySample, bool)
}

// FirstTokenLatencySelector applies an API-key profile latency ceiling after
// the normal model/channel/stream filters and before load balancing. The
// statistic is channel-level; credential-aware eligibility remains a later phase
// because channel credentials are selected inside the outbound transformer.
type FirstTokenLatencySelector struct {
	wrapped       CandidateSelector
	statsProvider ChannelLatencyStatsProvider
	maxLatencyMs  int64
	// countRealTrafficLatency mirrors the profile flag of the same name: false counts
	// synthetic probe traffic only, true counts every source.
	countRealTrafficLatency bool
}

// WithFirstTokenLatencySelector creates a best-effort latency filter. Unknown
// statistics (no provider, no samples in the window, or too few samples) remain
// eligible. If every known candidate is over the threshold, the original set
// is returned so a profile cannot silently remove all routing options.
//
// countRealTrafficLatency selects which measurements feed the ceiling; see
// latencySignalForCandidate.
func WithFirstTokenLatencySelector(
	wrapped CandidateSelector,
	statsProvider ChannelLatencyStatsProvider,
	maxLatencyMs int64,
	countRealTrafficLatency bool,
) *FirstTokenLatencySelector {
	return &FirstTokenLatencySelector{
		wrapped:                 wrapped,
		statsProvider:           statsProvider,
		maxLatencyMs:            maxLatencyMs,
		countRealTrafficLatency: countRealTrafficLatency,
	}
}

func (s *FirstTokenLatencySelector) Select(ctx context.Context, req *llm.Request) ([]*ChannelModelsCandidate, error) {
	candidates, err := s.wrapped.Select(ctx, req)
	if err != nil || len(candidates) == 0 || s.maxLatencyMs <= 0 || s.statsProvider == nil {
		return candidates, err
	}

	eligible := make([]*ChannelModelsCandidate, 0, len(candidates))
	unknown := 0

	for _, candidate := range candidates {
		if candidate == nil || candidate.Channel == nil {
			eligible = append(eligible, candidate)
			unknown++

			continue
		}

		latencyMs, known := latencySignalForCandidate(candidate, req, s.statsProvider, s.countRealTrafficLatency)
		if !known {
			unknown++
		}

		if !known || latencyMs <= float64(s.maxLatencyMs) {
			eligible = append(eligible, candidate)
		}
	}

	if len(eligible) == 0 {
		log.Debug(ctx, "latency threshold had no eligible candidates; using best-effort fallback",
			log.Int("candidate_count", len(candidates)),
			log.Int64("max_first_token_latency_ms", s.maxLatencyMs),
		)

		return candidates, nil
	}

	// A ceiling that measured nothing is indistinguishable from a ceiling everything
	// satisfied: both pass every candidate silently. Say which it was, because "I set a
	// limit and nothing changed" has no other explanation available to an operator --
	// and with a per-model window this is a routine state for a model that is not
	// probed and carries no traffic.
	if log.DebugEnabled(ctx) && unknown == len(candidates) {
		log.Debug(ctx, "latency ceiling had no usable window for any candidate; every channel passed as unknown",
			log.String("model", req.Model),
			log.Int("candidate_count", len(candidates)),
			log.Int64("max_first_token_latency_ms", s.maxLatencyMs),
			log.Bool("count_real_traffic_latency", s.countRealTrafficLatency),
		)
	}

	if log.DebugEnabled(ctx) && len(eligible) != len(candidates) {
		log.Debug(ctx, "filtered candidates by API key latency threshold",
			log.Int("before", len(candidates)),
			log.Int("after", len(eligible)),
			log.Int64("max_first_token_latency_ms", s.maxLatencyMs),
		)
	}

	return eligible, nil
}

// latencySignalForCandidate reduces a channel's latency window to one number the
// ceiling can compare against, plus whether that number is known at all.
//
// countRealTrafficLatency selects the SCOPE of the window rather than merging two
// signals:
//   - false (the default): synthetic probe traffic only. Probes measure every channel
//     identically on one global cadence, which is what makes the numbers comparable
//     against a single ceiling.
//   - true: every source. The key is declaring that its own traffic is continuous
//     enough to keep a channel warm, so its real requests belong in the same window.
//
// Because the scope is a filter on one statistic, there is no rule reconciling two
// numbers -- and no way for a stale value to outvote a fresh one, which is what the
// previous two-EWMA arrangement could do: samples simply leave the window as they age.
//
// The value is the window's MEAN, not its P95. Both are computed by the same scan,
// so this is a one-line choice, but they answer different questions: the P95 asks
// "how slow is the slowest 5%" and the mean asks "how fast is this channel
// usually". A ceiling phrased as "maximum first-token latency" reads as the second
// question, and over a lookback window measured in hours the P95 is dominated by a
// handful of tail samples -- strict enough that a ceiling set by the intuition of an
// average excludes channels that are typically well inside it. Unknown always KEEPS
// the candidate (handled by the caller).
//
// The statistic is looked up by the model this request asked for. That is the same
// string the request's own row records, so the ceiling compares this model against
// measurements of this model and nothing else. A model with no window of its own --
// never probed, no real traffic -- is UNKNOWN, which keeps the candidate and leaves
// the ordering to the rest of the selector chain. Borrowing a sibling model's number
// would be worse than admitting ignorance: it looks authoritative and is not.
func latencySignalForCandidate(
	candidate *ChannelModelsCandidate,
	req *llm.Request,
	statsProvider ChannelLatencyStatsProvider,
	countRealTrafficLatency bool,
) (float64, bool) {
	if statsProvider == nil || candidate == nil || candidate.Channel == nil || req == nil {
		return 0, false
	}

	// An unnamed model names no window. Guarded here rather than only in the provider
	// because this is the decision point: the empty string must never become a bucket
	// that every unnamed request shares, whichever implementation is behind the
	// interface.
	if req.Model == "" {
		return 0, false
	}

	sample, ok := usableLatencyWindow(statsProvider, candidate.Channel.ID, req.Model, countRealTrafficLatency)
	if !ok {
		return 0, false
	}

	return sample.AvgMs, true
}

// usableLatencyWindow reports a window only when it holds enough samples to act on.
// A couple of slow samples must not evict a channel, so a thin window is unknown.
//
// The positivity check is on AvgMs because that is the value returned; every sample
// in the window is a positive latency by construction, so a non-positive mean means
// the row carried nothing usable.
func usableLatencyWindow(
	statsProvider ChannelLatencyStatsProvider,
	channelID int,
	modelID string,
	includeRealTraffic bool,
) (biz.ChannelLatencySample, bool) {
	sample, ok := statsProvider.ChannelLatencyStats(channelID, modelID, includeRealTraffic)
	if !ok || !sample.UsableForGate() {
		return biz.ChannelLatencySample{}, false
	}

	return sample, true
}

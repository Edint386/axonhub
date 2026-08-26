package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

// MinimumLatencyThresholdSamples prevents a single transient request from
// changing routing eligibility. The current in-memory metrics are channel-wide
// EWMA values, so this is intentionally conservative and best-effort.
const MinimumLatencyThresholdSamples int64 = 3

// FirstTokenLatencySelector applies an API-key profile latency ceiling after
// the normal model/channel/stream filters and before load balancing. The
// metrics are channel-level aggregates; credential-aware eligibility remains a
// later phase because channel credentials are selected inside the outbound
// transformer.
type FirstTokenLatencySelector struct {
	wrapped         CandidateSelector
	metricsProvider ChannelMetricsProvider
	maxLatencyMs    int64
	// countRealTrafficLatency mirrors the profile flag of the same name: false reads
	// synthetic probe latency only, true also admits real-traffic latency.
	countRealTrafficLatency bool
}

// WithFirstTokenLatencySelector creates a best-effort latency filter. Unknown
// metrics (no provider, too few samples, or a metrics read error) remain
// eligible. If every known candidate is over the threshold, the original set
// is returned so a profile cannot silently remove all routing options.
//
// countRealTrafficLatency selects which measurements may feed the ceiling; see
// latencySignalForCandidate.
func WithFirstTokenLatencySelector(
	wrapped CandidateSelector,
	metricsProvider ChannelMetricsProvider,
	maxLatencyMs int64,
	countRealTrafficLatency bool,
) *FirstTokenLatencySelector {
	return &FirstTokenLatencySelector{
		wrapped:                 wrapped,
		metricsProvider:         metricsProvider,
		maxLatencyMs:            maxLatencyMs,
		countRealTrafficLatency: countRealTrafficLatency,
	}
}

func (s *FirstTokenLatencySelector) Select(ctx context.Context, req *llm.Request) ([]*ChannelModelsCandidate, error) {
	candidates, err := s.wrapped.Select(ctx, req)
	if err != nil || len(candidates) == 0 || s.maxLatencyMs <= 0 || s.metricsProvider == nil {
		return candidates, err
	}

	eligible := make([]*ChannelModelsCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.Channel == nil {
			eligible = append(eligible, candidate)
			continue
		}

		metrics, metricsErr := s.metricsProvider.GetChannelMetrics(ctx, candidate.Channel.ID)
		latencyMs, known := latencySignalForCandidate(candidate, req, metrics, metricsErr, s.countRealTrafficLatency)
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

	if log.DebugEnabled(ctx) && len(eligible) != len(candidates) {
		log.Debug(ctx, "filtered candidates by API key latency threshold",
			log.Int("before", len(candidates)),
			log.Int("after", len(eligible)),
			log.Int64("max_first_token_latency_ms", s.maxLatencyMs),
		)
	}

	return eligible, nil
}

// latencySignalForCandidate reduces a channel's telemetry to one number the ceiling
// can compare against, plus whether that number is known at all.
//
// countRealTrafficLatency chooses the admissible sources:
//   - false (the default): the synthetic probe signal only. A channel with no usable
//     probe samples stays UNKNOWN however slow its real traffic is, because probes
//     measure every channel identically on one global cadence while real traffic is
//     whatever this key's callers happened to send.
//   - true: whichever of the probe and traffic signals are valid, taking the worse
//     one. Both measure the same thing, and a ceiling is a safety rail -- if either
//     source says the channel is slow, it is slow for filtering purposes.
//
// The sample floor applies identically to both sources, and unknown always KEEPS the
// candidate (handled by the caller).
func latencySignalForCandidate(
	candidate *ChannelModelsCandidate,
	req *llm.Request,
	metrics *biz.AggregatedMetrics,
	metricsErr error,
	countRealTrafficLatency bool,
) (float64, bool) {
	if metricsErr != nil || metrics == nil || candidate == nil || candidate.Channel == nil {
		return 0, false
	}

	probeMs, probeKnown := probeLatencySignal(metrics)

	if !countRealTrafficLatency {
		return probeMs, probeKnown
	}

	streaming := req != nil && req.Stream != nil && *req.Stream
	// A required streaming policy changes the actual upstream request even when
	// the caller omitted the stream flag, so evaluate it with TTFT telemetry.
	if candidate.Channel.Policies.Stream == objects.CapabilityPolicyRequire {
		streaming = true
	}

	trafficMs, trafficKnown := trafficLatencySignal(metrics, streaming)

	switch {
	case trafficKnown && probeKnown:
		return max(trafficMs, probeMs), true
	case probeKnown:
		// The point of reading probes: a channel with no real traffic yet used to be
		// permanently "unknown" and so could never be filtered by the ceiling.
		return probeMs, true
	case trafficKnown:
		return trafficMs, true
	default:
		return 0, false
	}
}

// probeLatencySignal reports the channel's synthetic-probe first-token EWMA.
//
// The probe's streaming mode is a single global policy value, so every channel is
// measured the same way and the numbers stay comparable against one ceiling. That
// mode may differ from the mode of the request being routed; the probe still
// answers the question the ceiling asks -- how long this channel takes to start
// responding -- better than having no signal at all.
func probeLatencySignal(metrics *biz.AggregatedMetrics) (float64, bool) {
	if metrics.ProbeSampleCount < MinimumLatencyThresholdSamples || metrics.ProbeFirstTokenLatencyEWMA <= 0 {
		return 0, false
	}

	return metrics.ProbeFirstTokenLatencyEWMA, true
}

func trafficLatencySignal(metrics *biz.AggregatedMetrics, streaming bool) (float64, bool) {
	if streaming {
		if metrics.StreamingSampleCount < MinimumLatencyThresholdSamples || metrics.StreamingFirstTokenLatencyEWMA <= 0 {
			return 0, false
		}

		return metrics.StreamingFirstTokenLatencyEWMA, true
	}

	if metrics.NonStreamingSampleCount < MinimumLatencyThresholdSamples || metrics.NonStreamingLatencyEWMA <= 0 {
		return 0, false
	}

	// Non-streaming responses do not expose a separate first-token boundary in
	// the current telemetry, so total latency is the documented fallback signal.
	return metrics.NonStreamingLatencyEWMA, true
}

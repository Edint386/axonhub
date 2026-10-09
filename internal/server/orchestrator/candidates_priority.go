package orchestrator

import (
	"context"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/log"
)

// candidatePriorityTier orders candidates by model association priority first
// (lower wins), then by channel priority (higher wins).
type candidatePriorityTier struct {
	modelPriority   int
	channelPriority int
}

func candidatePriorityTierOf(candidate *ChannelModelsCandidate) candidatePriorityTier {
	tier := candidatePriorityTier{modelPriority: candidate.Priority}
	if candidate.Channel != nil {
		tier.channelPriority = candidate.Channel.Priority
	}

	return tier
}

func compareCandidatePriorityTiers(a, b candidatePriorityTier) int {
	if a.modelPriority != b.modelPriority {
		return a.modelPriority - b.modelPriority
	}

	return b.channelPriority - a.channelPriority
}

// stickyCandidateEligible reports whether channelID may be pinned: it must be
// available and belong to the best available priority tier of the candidates
// left after quota routing, so a sticky channel never jumps ahead of a higher
// channel or model priority.
func stickyCandidateEligible(
	ctx context.Context,
	candidates []*ChannelModelsCandidate,
	channelID int,
	loadBalancer *LoadBalancer,
) bool {
	bestTier, ok := bestAvailablePriorityTier(ctx, candidates, loadBalancer)
	if !ok {
		return false
	}

	return lo.ContainsBy(candidates, func(candidate *ChannelModelsCandidate) bool {
		if candidate == nil || candidate.Channel == nil || candidate.Channel.ID != channelID || candidatePriorityTierOf(candidate) != bestTier {
			return false
		}

		_, unavailable := loadBalancer.HardUnavailableReason(ctx, candidate.Channel)

		return !unavailable
	})
}

func bestAvailablePriorityTier(ctx context.Context, candidates []*ChannelModelsCandidate, loadBalancer *LoadBalancer) (candidatePriorityTier, bool) {
	var best candidatePriorityTier
	found := false
	for _, candidate := range candidates {
		if candidate == nil || candidate.Channel == nil {
			continue
		}
		if _, unavailable := loadBalancer.HardUnavailableReason(ctx, candidate.Channel); unavailable {
			continue
		}

		tier := candidatePriorityTierOf(candidate)
		if !found || compareCandidatePriorityTiers(tier, best) < 0 {
			best = tier
			found = true
		}
	}

	return best, found
}

// splitHardUnavailableCandidates removes hard-unavailable channels from a
// priority group so they can be retried only after every available tier.
func splitHardUnavailableCandidates(
	ctx context.Context,
	loadBalancer *LoadBalancer,
	group []*ChannelModelsCandidate,
	model string,
) ([]*ChannelModelsCandidate, []*ChannelModelsCandidate) {
	if loadBalancer == nil {
		return group, nil
	}

	available := make([]*ChannelModelsCandidate, 0, len(group))
	var unavailable []*ChannelModelsCandidate
	for _, candidate := range group {
		reason, hardUnavailable := loadBalancer.HardUnavailableReason(ctx, candidate.Channel)
		if !hardUnavailable {
			available = append(available, candidate)
			continue
		}

		unavailable = append(unavailable, candidate)
		if log.DebugEnabled(ctx) && candidate.Channel != nil {
			tier := candidatePriorityTierOf(candidate)
			log.Debug(ctx, "deferred hard-unavailable channel candidate",
				log.String("model", model),
				log.Int("channel_id", candidate.Channel.ID),
				log.String("channel_name", candidate.Channel.Name),
				log.Int("model_priority", tier.modelPriority),
				log.Int("channel_priority", tier.channelPriority),
				log.String("reason", reason))
		}
	}

	return available, unavailable
}

// appendDeferredCandidates fills the remaining retry budget with the
// hard-unavailable candidates, keeping their priority order.
func appendDeferredCandidates(result, deferred []*ChannelModelsCandidate, requiredCount int) []*ChannelModelsCandidate {
	if len(result) >= requiredCount {
		return result
	}

	return append(result, deferred[:min(requiredCount-len(result), len(deferred))]...)
}

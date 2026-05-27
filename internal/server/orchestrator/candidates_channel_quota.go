package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

// ChannelQuotaSelector filters candidates whose configured usage quota is exhausted.
// It only changes candidate availability; priority ordering is still handled later by
// LoadBalancedSelector.
type ChannelQuotaSelector struct {
	wrapped CandidateSelector
	quota   *biz.QuotaService

	// FilteredCount holds the number of candidates removed by the last Select call.
	FilteredCount int
}

func WithChannelQuotaSelector(wrapped CandidateSelector, quota *biz.QuotaService) *ChannelQuotaSelector {
	return &ChannelQuotaSelector{
		wrapped: wrapped,
		quota:   quota,
	}
}

func (s *ChannelQuotaSelector) Select(ctx context.Context, req *llm.Request) ([]*ChannelModelsCandidate, error) {
	candidates, err := s.wrapped.Select(ctx, req)
	if err != nil {
		return nil, err
	}

	s.FilteredCount = 0
	if len(candidates) == 0 || s.quota == nil {
		return candidates, nil
	}

	filtered := make([]*ChannelModelsCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.Channel == nil ||
			candidate.Channel.Settings == nil || candidate.Channel.Settings.Quota == nil {
			filtered = append(filtered, candidate)
			continue
		}

		check, err := s.quota.CheckChannelQuota(ctx, candidate.Channel.ID, candidate.Channel.Settings.Quota)
		if err != nil {
			return nil, err
		}

		if check.Allowed {
			filtered = append(filtered, candidate)
			continue
		}

		s.FilteredCount++

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "ChannelQuotaSelector: filtered exhausted channel",
				log.String("model", req.Model),
				log.Int("channel_id", candidate.Channel.ID),
				log.String("channel_name", candidate.Channel.Name),
				log.String("message", check.Message),
			)
		}
	}

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "ChannelQuotaSelector: filtered candidates",
			log.String("model", req.Model),
			log.Int("before", len(candidates)),
			log.Int("after", len(filtered)),
			log.Int("filtered", s.FilteredCount),
		)
	}

	return filtered, nil
}

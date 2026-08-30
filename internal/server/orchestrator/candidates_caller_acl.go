package orchestrator

import (
	"context"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
)

// CallerACLSelector applies the channel-owned hard access boundary for the
// authenticated AxonHub caller API key.
type CallerACLSelector struct {
	wrapped  CandidateSelector
	apiKeyID int
}

func WithCallerACLSelector(wrapped CandidateSelector, apiKeyID int) *CallerACLSelector {
	return &CallerACLSelector{wrapped: wrapped, apiKeyID: apiKeyID}
}

func (s *CallerACLSelector) Select(ctx context.Context, req *llm.Request) ([]*ChannelModelsCandidate, error) {
	candidates, err := s.wrapped.Select(ctx, req)
	if err != nil {
		return nil, err
	}

	return lo.Filter(candidates, func(candidate *ChannelModelsCandidate, _ int) bool {
		return candidate.Channel.AllowsCallerAPIKey(s.apiKeyID)
	}), nil
}

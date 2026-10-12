package orchestrator

import (
	"context"
	"fmt"
	"math/rand/v2"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
)

// selectCandidates creates a middleware that selects available channel model candidates for the model.
// This is the second step in the inbound pipeline, moved from outbound transformer.
// If no valid candidates are found, it returns ErrInvalidModel to fail fast.
func selectCandidates(inbound *PersistentInboundTransformer, quotaProvider ProviderQuotaStatusProvider, systemService biz.QuotaRoutingSettingsProvider) pipeline.Middleware {
	return pipeline.OnLlmRequest("select-candidates", func(ctx context.Context, llmRequest *llm.Request) (*llm.Request, error) {
		// Only select candidates once
		if len(inbound.state.ChannelModelsCandidates) > 0 {
			return llmRequest, nil
		}

		selector := inbound.state.CandidateSelector

		if inbound.state.APIKey != nil {
			selector = WithCallerACLSelector(selector, inbound.state.APIKey.ID)
		}

		// Project-level profile filtering (upper boundary)
		if inbound.state.APIKey != nil {
			if project := inbound.state.APIKey.Edges.Project; project != nil {
				if projectProfile := project.GetActiveProfile(); projectProfile != nil {
					if len(projectProfile.ChannelIDs) > 0 {
						selector = WithSelectedChannelsSelector(selector, projectProfile.ChannelIDs)
					}

					if len(projectProfile.ChannelTags) > 0 {
						selector = WithChannelTagsFilterSelector(selector, projectProfile.ChannelTags, projectProfile.ChannelTagsMatchMode)
					}
				}
			}
		}

		// Key-level profile filtering (narrows further within project scope)
		if profile := inbound.state.APIKey.GetActiveProfile(); profile != nil {
			if len(profile.ChannelIDs) > 0 {
				selector = WithSelectedChannelsSelector(selector, profile.ChannelIDs)
			}

			if len(profile.ChannelTags) > 0 {
				selector = WithChannelTagsFilterSelector(selector, profile.ChannelTags, profile.ChannelTagsMatchMode)
			}
		}

		// Apply Google native tools filter (only for Gemini native API format)
		if llmRequest.APIFormat == llm.APIFormatGeminiContents {
			selector = WithGoogleNativeToolsSelector(selector)
		}

		// Apply Anthropic native tools filter (only for Anthropic message API format)
		if llmRequest.APIFormat == llm.APIFormatAnthropicMessage {
			selector = WithAnthropicNativeToolsSelector(selector)
		}

		selector = WithStreamPolicySelector(selector)

		if profile := inbound.state.APIKey.GetActiveProfile(); profile != nil && profile.MaxFirstTokenLatencyMs != nil {
			selector = WithFirstTokenLatencySelector(selector, inbound.state.ChannelService, *profile.MaxFirstTokenLatencyMs, profile.CountRealTrafficLatency)
		}
		channelQuotaSelector := WithChannelQuotaSelector(selector, inbound.state.QuotaService)
		selector = channelQuotaSelector

		gate := NewQuotaRoutingGate(quotaProvider, systemService.QuotaRoutingSettingsOrDefault(ctx))

		if len(inbound.state.LoadBalancers) > 0 {
			selector = WithRoutingPolicyLoadBalancedSelector(
				selector,
				inbound.state.LoadBalancers,
				inbound.state.RetryPolicyProvider,
				inbound.state.RequestService,
				inbound.state.APIKey,
				&inbound.state.RoutingPolicy,
				gate,
			)
		} else {
			selector = WithQuotaRoutingSelector(selector, gate)
		}

		candidates, err := selector.Select(ctx, llmRequest)
		if err != nil {
			return nil, err
		}

		candidates = randomizeAliasModelOrder(candidates)

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "selected candidates",
				log.Int("candidate_count", len(candidates)),
				log.String("model", llmRequest.Model),
				log.String("load_balance_strategy", inbound.state.RoutingPolicy.LoadBalancerStrategy),
				log.String("trace_sticky_mode", string(inbound.state.RoutingPolicy.TraceStickyMode)),
				log.Any("candidates", lo.Map(candidates, func(candidate *ChannelModelsCandidate, _ int) map[string]any {
					return map[string]any{
						"channel_name": candidate.Channel.Name,
						"channel_id":   candidate.Channel.ID,
						"priority":     candidate.Priority,
						"models": lo.Map(candidate.Models, func(entry biz.ChannelModelEntry, _ int) map[string]any {
							return map[string]any{
								"request_model": entry.RequestModel,
								"actual_model":  entry.ActualModel,
								"source":        entry.Source,
							}
						}),
					}
				})),
			)
		}

		if len(candidates) == 0 {
			if channelQuotaSelector.FilteredCount > 0 || gate.DroppedCount() > 0 {
				return nil, NewQuotaExhaustedError(llmRequest.Model)
			}
			return nil, fmt.Errorf("%w: %s", biz.ErrInvalidModel, llmRequest.Model)
		}

		// Store candidates directly (no need to extract channels)
		inbound.state.ChannelModelsCandidates = candidates

		return llmRequest, nil
	})
}

func randomizeAliasModelOrder(candidates []*ChannelModelsCandidate) []*ChannelModelsCandidate {
	return randomizeAliasModelOrderWithRand(candidates, rand.IntN)
}

func randomizeAliasModelOrderWithRand(candidates []*ChannelModelsCandidate, intN func(int) int) []*ChannelModelsCandidate {
	if len(candidates) == 0 {
		return candidates
	}

	randomized := make([]*ChannelModelsCandidate, len(candidates))
	for i, candidate := range candidates {
		if candidate == nil || len(candidate.Models) < 2 {
			randomized[i] = candidate
			continue
		}

		cloned := *candidate
		if order := aliasModelOrderWithRand(candidate.Models, intN); order != nil {
			cloned.Models = permuteByOrder(candidate.Models, order)
			// The per-model endpoint formats are index-aligned with Models, so they
			// must follow the same order; otherwise a target is sent with another
			// target's protocol.
			if len(candidate.modelAPIFormats) == len(candidate.Models) {
				cloned.modelAPIFormats = permuteByOrder(candidate.modelAPIFormats, order)
				cloned.APIFormat = cloned.modelAPIFormats[0]
			}
		}
		randomized[i] = &cloned
	}

	return randomized
}

func randomizeDuplicateRequestModelGroupsWithRand(models []biz.ChannelModelEntry, intN func(int) int) []biz.ChannelModelEntry {
	order := aliasModelOrderWithRand(models, intN)
	if order == nil {
		return models
	}

	return permuteByOrder(models, order)
}

// aliasModelOrderWithRand returns a random order for models in which the targets
// of every alias (entries sharing a request model) are shuffled among the
// positions that alias already holds. order[i] is the original index of the
// entry placed at i. It returns nil when no alias has more than one target.
func aliasModelOrderWithRand(models []biz.ChannelModelEntry, intN func(int) int) []int {
	if len(models) < 2 {
		return nil
	}

	requestModels := make([]string, 0, len(models))
	positionsByRequestModel := make(map[string][]int, len(models))
	for i, model := range models {
		if _, seen := positionsByRequestModel[model.RequestModel]; !seen {
			requestModels = append(requestModels, model.RequestModel)
		}
		positionsByRequestModel[model.RequestModel] = append(positionsByRequestModel[model.RequestModel], i)
	}

	var order []int
	for _, requestModel := range requestModels {
		positions := positionsByRequestModel[requestModel]
		if len(positions) < 2 {
			continue
		}

		if order == nil {
			order = make([]int, len(models))
			for i := range order {
				order[i] = i
			}
		}

		shuffled := append([]int(nil), positions...)
		for i := len(shuffled) - 1; i > 0; i-- {
			j := intN(i + 1)
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		}
		for k, position := range positions {
			order[position] = shuffled[k]
		}
	}

	return order
}

func permuteByOrder[T any](items []T, order []int) []T {
	permuted := make([]T, len(order))
	for i, from := range order {
		permuted[i] = items[from]
	}

	return permuted
}

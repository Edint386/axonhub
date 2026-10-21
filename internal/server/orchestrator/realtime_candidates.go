package orchestrator

// Realtime (OpenAI Realtime API) candidate selection lives in this custom (fork) file so that
// the upstream-owned Process pipeline stays untouched. SelectRealtimeCandidates mirrors the
// context setup, the PersistenceState and the inbound middleware list of Process; whenever
// Process changes, diff its inbound middleware list against selectRealtimeMiddlewares (see
// CUSTOM_BRANCH_RULES.md §3).

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/realtime/dialect/bailian"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// ErrRealtimeRPMExceeded is returned by RealtimeRPMAdmit when the channel's local RPM limit is
// exhausted for the current minute. The returned error also wraps the *LocalRPMExhaustedError
// (and therefore ErrLocalRPMExhausted plus its synthetic 429 httpclient.Error) so the API layer
// can render it exactly like the chat pipeline does.
var ErrRealtimeRPMExceeded = errors.New("realtime: channel local rpm limit exceeded")

// PrepareRealtimeContext installs the mutable request container and the system authz bypass,
// exactly like Process does before candidate selection. It intentionally does not install
// biz.WithModelLibraryPriceMemo: realtime sessions bill many responses over a long time and
// must not pin a single price lookup for the whole session.
func PrepareRealtimeContext(ctx context.Context) context.Context {
	// API key providers cannot return a derived context, so install the shared request
	// container before provider selection mutates it.
	ctx = contexts.EnsureContainer(ctx)

	// The context is system bypassed to allow the orchestrator to access the system settings.
	return authz.WithSystemBypass(ctx, "realtime-session")
}

// RealtimeSelection is the result of SelectRealtimeCandidates.
type RealtimeSelection struct {
	// RequestedModel is the model name after API key profile mapping (PersistenceState.OriginalModel
	// semantics): the name to echo to clients. When the profile mapped the requested name this is
	// the mapped name, not the upstream ActualModel of a candidate.
	RequestedModel string
	// Candidates are the selected channel candidates in selection order (already truncated by the
	// load balancer to the retry budget).
	Candidates []*ChannelModelsCandidate
}

// RealtimeSelectionRequest describes one realtime selection.
type RealtimeSelectionRequest struct {
	// Model is the client-facing model name before API key profile mapping.
	Model string
	// Mode selects the transport (WebSocket sessions or WebRTC calls).
	Mode biz.RealtimeMode
	// Intent is the raw intent query value ("", "transcription" or "quicksilver"). WebSocket
	// selection drops candidates whose dialect cannot serve the intent (Bailian serves neither
	// transcription nor quicksilver sessions) before the load balancer truncates the list.
	Intent string
	// Headers are the inbound request headers; they may be nil. When set they are exposed to
	// model association header conditions through llm.Request.RawRequest.
	Headers http.Header
}

// SelectRealtimeCandidates selects the channel candidates for a realtime session or call with
// no intent. It is SelectRealtime with RealtimeSelectionRequest{Model, Mode, Headers}; WebRTC
// call creation and existing callers use it unchanged.
func (processor *ChatCompletionOrchestrator) SelectRealtimeCandidates(ctx context.Context, model string, mode biz.RealtimeMode, rawHeaders http.Header) (*RealtimeSelection, error) {
	return processor.SelectRealtime(ctx, RealtimeSelectionRequest{Model: model, Mode: mode, Headers: rawHeaders})
}

// SelectRealtime selects the channel candidates for a realtime session or call.
//
// It builds a PersistenceState like Process, wraps the default channel selector with the realtime
// capability filter for the request's mode and intent and runs the inbound middlewares
// enforceQuota, checkApiKeyModelAccess, applyModelMapping and selectCandidates on a synthetic
// realtime request. Errors are returned unchanged so the API layer can map them like chat errors
// (biz.ErrInvalidModel, quota errors). An empty candidate list surfaces as biz.ErrInvalidModel.
func (processor *ChatCompletionOrchestrator) SelectRealtime(ctx context.Context, sel RealtimeSelectionRequest) (*RealtimeSelection, error) {
	apiKey, _ := contexts.GetAPIKey(ctx)

	model, mode, rawHeaders := sel.Model, sel.Mode, sel.Headers

	// Get retry policy from system settings.
	retryPolicy := processor.SystemService.RetryPolicyOrDefault(ctx)

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "realtime candidate selection started",
			log.String("model", model),
			log.String("mode", string(mode)),
			log.String("intent", sel.Intent),
			log.Bool("has_api_key", apiKey != nil),
		)
	}

	state := &PersistenceState{
		APIKey:              apiKey,
		RequestService:      processor.RequestService,
		UsageLogService:     processor.UsageLogService,
		SystemService:       processor.SystemService,
		ChannelService:      processor.ChannelService,
		QuotaService:        processor.QuotaService,
		PromptProvider:      processor.PromptProvider,
		PromptProtecter:     processor.PromptProtecter,
		RetryPolicyProvider: processor.SystemService,
		CandidateSelector:   withRealtimeCapableSelector(processor.channelSelector, mode, sel.Intent),
		LoadBalancers: map[string]*LoadBalancer{
			biz.LoadBalancerStrategyAdaptive:       processor.adaptiveLoadBalancer,
			biz.LoadBalancerStrategyFailover:       processor.failoverLoadBalancer,
			biz.LoadBalancerStrategyCircuitBreaker: processor.circuitBreakerLoadBalancer,
			biz.LoadBalancerStrategyRoundRobin:     processor.roundRobinLoadBalancer,
		},
		RoutingPolicy:         deriveRoutingPolicy(retryPolicy, apiKey, nil),
		ModelMapper:           processor.ModelMapper,
		Proxy:                 processor.proxy,
		CurrentCandidateIndex: 0,
	}
	state.AliasFailoverEnabled = retryPolicy.Enabled // fork: see alias_failover.go

	req := &llm.Request{
		Model:       model,
		RequestType: llm.RequestTypeRealtime,
		APIFormat:   llm.APIFormatOpenAIRealtime,
		Stream:      lo.ToPtr(true),
		RawRequest:  &httpclient.Request{Headers: rawHeaders},
	}

	inbound, _ := NewPersistentTransformers(state, processor.Inbound)

	var err error
	for _, middleware := range selectRealtimeMiddlewares(processor, inbound) {
		req, err = middleware.OnInboundLlmRequest(ctx, req)
		if err != nil {
			return nil, err
		}
	}

	candidates := state.ChannelModelsCandidates
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: %s", biz.ErrInvalidModel, req.Model)
	}

	requested := state.OriginalModel
	if requested == "" {
		requested = req.Model
	}

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "realtime candidates selected",
			log.String("requested_model", requested),
			log.String("mode", string(mode)),
			log.Int("candidate_count", len(candidates)),
		)
	}

	return &RealtimeSelection{
		RequestedModel: requested,
		Candidates:     candidates,
	}, nil
}

// selectRealtimeMiddlewares returns the inbound middlewares SelectRealtimeCandidates runs, in
// order. It is the realtime mirror of the inbound middleware list in Process: the request-shaping
// middlewares (reasoning effort, prompts, pass-through, persistence) are deliberately absent because
// the relay never runs the transformer pipeline.
func selectRealtimeMiddlewares(processor *ChatCompletionOrchestrator, inbound *PersistentInboundTransformer) []pipeline.Middleware {
	return []pipeline.Middleware{
		enforceQuota(inbound, processor.QuotaService),
		checkApiKeyModelAccess(inbound),
		applyModelMapping(inbound),
		selectCandidates(inbound, processor.quotaProvider, processor.SystemService),
	}
}

// RealtimeActualModel returns the upstream model name a candidate serves for the requested name.
// It prefers the model entry whose RequestModel matches, then the first entry's ActualModel, and
// finally the requested name itself.
func RealtimeActualModel(candidate *ChannelModelsCandidate, requested string) string {
	if candidate == nil {
		return requested
	}

	for _, entry := range candidate.Models {
		if entry.RequestModel == requested && entry.ActualModel != "" {
			return entry.ActualModel
		}
	}

	if len(candidate.Models) > 0 && candidate.Models[0].ActualModel != "" {
		return candidate.Models[0].ActualModel
	}

	return requested
}

// RealtimeLimiter exposes this instance's per-channel concurrency limiter manager so the realtime
// relay shares admission control with the chat pipeline. It may be nil in test setups.
func (processor *ChatCompletionOrchestrator) RealtimeLimiter() *ChannelLimiterManager {
	return processor.channelLimiterManager
}

// RealtimeRPMAdmit consumes one request slot of the channel's local RPM budget, mirroring
// withRateLimitAdmission: the limit is read from the channel's rate-limit settings and tracked by
// this instance's ChannelRequestTracker. It returns nil when the channel has no RPM limit (or the
// instance has no tracker) and an error wrapping ErrRealtimeRPMExceeded when the budget is spent.
func (processor *ChatCompletionOrchestrator) RealtimeRPMAdmit(ctx context.Context, ch *biz.Channel) error {
	if processor.rateLimitTracker == nil || ch == nil || ch.Channel == nil {
		return nil
	}

	if ch.Settings == nil || ch.Settings.RateLimit == nil {
		return nil
	}

	limit := ch.Settings.RateLimit.RPM
	if limit == nil || *limit <= 0 {
		return nil
	}

	if !processor.rateLimitTracker.TryAcquireRequest(ch.ID, *limit) {
		log.Debug(ctx, "realtime channel local RPM admission rejected",
			log.Int("channel_id", ch.ID),
			log.String("channel_name", ch.Name),
			log.Int64("rpm_limit", *limit),
		)

		return fmt.Errorf("%w: %w", ErrRealtimeRPMExceeded, newLocalRPMExhaustedError(ch, *limit))
	}

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "realtime channel local RPM admission acquired",
			log.Int("channel_id", ch.ID),
			log.String("channel_name", ch.Name),
			log.Int64("rpm_limit", *limit),
		)
	}

	return nil
}

// Realtime intents the capability filter distinguishes (the raw intent query value).
const (
	realtimeIntentTranscription = "transcription"
	realtimeIntentQuicksilver   = "quicksilver"
)

// realtimeCapableSelector drops candidates whose channel cannot serve realtime traffic in the
// given mode and intent. It is installed as the innermost decorator
// (PersistenceState.CandidateSelector) so that it runs before the caller ACL, profile, quota and
// load-balancer truncation decorators that selectCandidates adds on top: a candidate the relay
// would reject anyway must not push a usable channel out of the truncated list.
type realtimeCapableSelector struct {
	wrapped CandidateSelector
	mode    biz.RealtimeMode
	intent  string
}

// withRealtimeCapableSelector wraps a selector with the realtime capability filter for mode and
// intent.
func withRealtimeCapableSelector(wrapped CandidateSelector, mode biz.RealtimeMode, intent string) *realtimeCapableSelector {
	return &realtimeCapableSelector{wrapped: wrapped, mode: mode, intent: intent}
}

// Select filters the wrapped selector's candidates down to the realtime-capable ones. req.Model
// is the (profile-mapped) requested model used to resolve each candidate's upstream model.
func (s *realtimeCapableSelector) Select(ctx context.Context, req *llm.Request) ([]*ChannelModelsCandidate, error) {
	candidates, err := s.wrapped.Select(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(candidates) == 0 {
		return candidates, nil
	}

	requested := ""
	if req != nil {
		requested = req.Model
	}

	return lo.Filter(candidates, func(candidate *ChannelModelsCandidate, _ int) bool {
		reason := realtimeIncapableReason(candidate, s.mode, s.intent, requested)
		if reason == "" {
			return true
		}

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "realtime candidate dropped",
				log.Int("channel_id", candidate.Channel.ID),
				log.String("channel_name", candidate.Channel.Name),
				log.String("mode", string(s.mode)),
				log.String("intent", s.intent),
				log.String("reason", reason),
			)
		}

		return false
	}), nil
}

// realtimeIncapableReason returns why the candidate cannot serve the requested model in the given
// mode and intent, or "" when it can. It is side-effect free: it checks that a credential is
// available without resolving one (resolving would record contexts.ChannelAPIKey in the shared
// request container and could refresh OAuth tokens for channels the caller never dials).
// WebSocket mode rejects OAuth credentials inside RealtimeTarget (they only support WebRTC calls
// through the Codex live base); the relay re-checks every condition before dialing.
func realtimeIncapableReason(candidate *ChannelModelsCandidate, mode biz.RealtimeMode, intent, requested string) string {
	if candidate == nil || candidate.Channel == nil || candidate.Channel.Channel == nil {
		return "nil candidate"
	}

	ch := candidate.Channel

	target, err := ch.RealtimeTarget(mode)
	if err != nil {
		return "no realtime target: " + err.Error()
	}

	if target.Dialect == biz.RealtimeDialectBailian {
		switch {
		case mode == biz.RealtimeModeWebRTC:
			return "bailian channels only support websocket sessions"
		case intent == realtimeIntentTranscription:
			return "bailian channels do not support transcription sessions"
		case intent == realtimeIntentQuicksilver:
			return "intent=quicksilver requires an openai channel"
		case !bailian.Supported(RealtimeActualModel(candidate, requested)):
			return "not a qwen-omni realtime model"
		}
	}

	if !ch.HasRealtimeCredential() {
		return "no realtime credential"
	}

	return ""
}

package orchestrator

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/pipeline"
)

// fork: same-channel failover across the targets of a multi-target model alias
// (one request model mapped to several upstream models, see
// biz.Channel.GetModelEntryGroups). Free tiers such as Groq rate-limit every
// upstream model separately, so a target that fails says nothing about its
// siblings. Switching to a sibling therefore does not spend the same-channel
// retry budget; see pipeline.AliasFailover. It still follows the retry policy's
// master switch through PersistenceState.AliasFailoverEnabled.

var _ pipeline.AliasFailover = (*PersistentOutboundTransformer)(nil)

// HasNextAliasTarget reports whether the model entry after the current one is
// another target of the same alias. Alias targets share a request model and are
// listed together (GetModelEntryGroups); randomizeAliasModelOrder only shuffles
// them within the positions they hold.
func (p *PersistentOutboundTransformer) HasNextAliasTarget() bool {
	if p == nil || p.state == nil || !p.state.AliasFailoverEnabled {
		return false
	}

	// A Responses continuation without a replayable snapshot is pinned to the
	// target that owns previous_response_id (selectResponsesContinuationCandidate),
	// so a sibling target would only re-run the same model or fail recovery. Leave
	// those attempts to the upstream retry rules.
	if c := p.state.ResponsesContinuation; c != nil && c.previous != "" && !c.complete {
		return false
	}

	candidate := p.state.CurrentCandidate
	current := p.state.CurrentModelIndex
	if candidate == nil || candidate.Channel == nil || current < 0 || current+1 >= len(candidate.Models) {
		return false
	}

	return candidate.Models[current+1].RequestModel == candidate.Models[current].RequestModel
}

// NextAliasTarget switches to the next alias target in the current channel when
// a sibling target may not share err. The switch reuses PrepareForRetry, which
// advances to the next model whenever one is left, so every per-attempt reset
// stays in one place.
func (p *PersistentOutboundTransformer) NextAliasTarget(ctx context.Context, err error) (bool, bool) {
	if !p.HasNextAliasTarget() {
		return false, false
	}

	channel := p.state.CurrentCandidate.Channel
	if !isAliasFailoverError(err, channel) {
		return false, false
	}

	if prepareErr := p.PrepareForRetry(ctx); prepareErr != nil {
		log.Warn(ctx, "channel left service, skipping its remaining alias targets",
			log.Int("channel_id", channel.ID),
			log.Cause(prepareErr),
		)

		return false, true
	}

	return true, false
}

// isAliasFailoverError reports whether err is a failure that another target of
// the same alias may not share: rate limits (Retry-After included), a missing
// or retired model, a request too large for the model's token budget, empty
// output, and the channel's regular retryable errors (interrupted connections,
// 5xx and the channel's own retryable codes and patterns).
//
// Local admission rejections and circuit-breaker skips apply to the whole
// channel or alias, network timeouts to the whole upstream host, and other
// client errors such as 400/401/403 hit every target alike, so they keep the
// regular, budgeted retry path.
func isAliasFailoverError(err error, ch *biz.Channel) bool {
	if err == nil {
		return false
	}

	// Checked before the status codes: local queue and RPM rejections carry a
	// synthetic 429.
	if errors.Is(err, errSkipCandidateByCircuitBreaker) || isChannelQueueError(err) || isLocalRPMExhaustedError(err) {
		return false
	}

	// Dial, TLS handshake and response-header timeouts hit every target on the
	// host, like the pipeline's response timeouts, which never switch targets.
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return false
	}

	if errors.Is(err, pipeline.ErrEmptyResponse) ||
		errors.Is(err, pipeline.ErrEmptyStreamChunks) ||
		errors.Is(err, pipeline.ErrEmptyAggregatedBody) {
		return true
	}

	switch ExtractStatusCodeFromError(err) {
	case http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		return true
	}

	return isRetryableErrorForChannel(err, ch)
}

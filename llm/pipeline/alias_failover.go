package pipeline

import (
	"context"
	"log/slog"
)

// AliasFailover is implemented by outbound transformers that can move to the
// next target of a multi-target model alias inside the same channel.
//
// fork: providers such as free tiers rate-limit every upstream model
// separately, so a failure on one alias target says nothing about its
// siblings. Switching between targets therefore has its own budget instead of
// the same-channel retry budget: every switch advances to a target that has
// not been tried in this channel, so an alias group is exhausted after
// len(targets)-1 switches.
type AliasFailover interface {
	// HasNextAliasTarget reports whether the current attempt has another alias
	// target left in the current channel.
	HasNextAliasTarget() bool

	// NextAliasTarget switches to the next alias target when err allows it.
	// abandonChannel reports that the current channel can no longer serve the
	// request, so the pipeline must skip same-channel retry as well.
	NextAliasTarget(ctx context.Context, err error) (switched bool, abandonChannel bool)
}

// maxAliasSwitchesPerChannel caps alias switches inside one channel. Alias
// groups end on their own; the cap only guards against an outbound that
// reports a switch without advancing.
const maxAliasSwitchesPerChannel = 32

// hasAliasFailoverTarget reports whether the outbound can still switch to
// another alias target. Such a switch needs the same pre-commit stream
// buffering as a same-channel retry.
func (p *pipeline) hasAliasFailoverTarget() bool {
	aliasFailover, ok := p.Outbound.(AliasFailover)

	return ok && aliasFailover.HasNextAliasTarget()
}

// nextAliasTarget asks the outbound to move to the next alias target. The
// switch goes to a different upstream model, so the caller retries at once,
// without the retry delay. Response timeouts keep upstream semantics and leave
// the channel instead.
func (p *pipeline) nextAliasTarget(
	ctx context.Context,
	lastErr error,
	timeoutRetry bool,
	aliasSwitches *int,
) (switched bool, abandonChannel bool) {
	if timeoutRetry || *aliasSwitches >= maxAliasSwitchesPerChannel {
		return false, false
	}

	aliasFailover, ok := p.Outbound.(AliasFailover)
	if !ok {
		return false, false
	}

	switched, abandonChannel = aliasFailover.NextAliasTarget(ctx, lastErr)
	if switched {
		*aliasSwitches++

		slog.WarnContext(ctx, "request process failed, switching to next alias target",
			slog.Any("error", lastErr),
			slog.Int("alias_switches", *aliasSwitches),
		)
	}

	return switched, abandonChannel
}

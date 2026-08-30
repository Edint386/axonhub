package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	entrequest "github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/server/biz"
)

// RequestCount is a real-traffic aggregate: it is incremented at selection time and
// feeds the round-robin load score. A synthetic probe must not enter it, because the
// recording side leaves the same counter alone for a probe -- an increment here would
// have nothing to balance it and would make the channel look busier than its callers
// made it.
func TestTrackSelectionSkipsSyntheticProbeTraffic(t *testing.T) {
	candidate := &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{ID: 3}}}

	t.Run("real traffic is counted", func(t *testing.T) {
		tracker := &mockSelectionTracker{selections: make(map[int]int)}
		lb := NewLoadBalancer(&mockRetryPolicyProvider{}, tracker)

		lb.TrackSelection(context.Background(), candidate)
		lb.TrackSelection(contexts.WithSource(context.Background(), entrequest.SourceAPI), candidate)
		lb.TrackSelection(contexts.WithSource(context.Background(), entrequest.SourcePlayground), candidate)

		require.Equal(t, 3, tracker.selections[3])
	})

	t.Run("probe traffic is refused", func(t *testing.T) {
		tracker := &mockSelectionTracker{selections: make(map[int]int)}
		lb := NewLoadBalancer(&mockRetryPolicyProvider{}, tracker)

		lb.TrackSelection(contexts.WithSource(context.Background(), entrequest.SourceTest), candidate)

		require.Empty(t, tracker.selections)
	})
}

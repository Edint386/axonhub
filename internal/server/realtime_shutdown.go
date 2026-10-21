package server

import (
	"context"
	"sync"
	"time"

	"github.com/looplj/axonhub/internal/log"
)

// Realtime sessions run on hijacked connections, which http.Server.Shutdown neither tracks nor
// waits for. Server.Shutdown therefore calls drainRealtimeSessions (fork hook, see
// CUSTOM_BRANCH_RULES.md §5) after the lifecycle was cancelled, so every live session,
// sideband and call monitor can finish its teardown and persist usage before fx closes the
// database.

// realtimeDrainer waits for the live sessions of one relay; it returns how many were still
// running when it gave up.
type realtimeDrainer func(ctx context.Context) int

var (
	realtimeDrainMu  sync.Mutex
	realtimeDrainers []realtimeDrainer
)

// registerRealtimeDrainer is called by registerRealtimeRoutes for each relay it creates.
func registerRealtimeDrainer(fn realtimeDrainer) {
	realtimeDrainMu.Lock()
	defer realtimeDrainMu.Unlock()

	realtimeDrainers = append(realtimeDrainers, fn)
}

// realtimeDrainTimeout caps the wait when the shutdown context carries no deadline.
const realtimeDrainTimeout = 30 * time.Second

// drainRealtimeSessions waits for every registered relay, bounded by ctx.
func drainRealtimeSessions(ctx context.Context) {
	realtimeDrainMu.Lock()
	drainers := append([]realtimeDrainer(nil), realtimeDrainers...)
	realtimeDrainMu.Unlock()

	if len(drainers) == 0 {
		return
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, realtimeDrainTimeout)
		defer cancel()
	}

	for _, drain := range drainers {
		if remaining := drain(ctx); remaining > 0 {
			log.Warn(ctx, "realtime: shutdown gave up waiting for live sessions", log.Int("remaining", remaining))
		}
	}
}

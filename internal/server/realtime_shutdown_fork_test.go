package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/lifecycle"
)

// TestServerShutdown_DrainsRealtimeSessions guards the fork hook in Server.Shutdown
// (`defer drainRealtimeSessions(ctx) // fork: see realtime_shutdown.go`). Without it the
// process closes the database while hijacked realtime sessions are still persisting usage.
func TestServerShutdown_DrainsRealtimeSessions(t *testing.T) {
	called := make(chan context.Context, 1)

	registerRealtimeDrainer(func(ctx context.Context) int {
		called <- ctx
		return 0
	})

	srv := &Server{lifetime: lifecycle.New()}
	require.NoError(t, srv.Shutdown(t.Context()))

	select {
	case ctx := <-called:
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline, "drain context must be bounded")
	default:
		t.Fatal("Server.Shutdown did not call drainRealtimeSessions; the fork hook in server.go is missing")
	}

	// The lifecycle was cancelled before the drain ran, which is what ends the sessions.
	require.Error(t, srv.lifetime.Context().Err())
}

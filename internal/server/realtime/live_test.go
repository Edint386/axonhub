package realtime

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLiveSessions_DrainWaitsForLeaveAndRejectsNewEntries(t *testing.T) {
	l := newLiveSessions()

	leave, ok := l.Enter()
	require.True(t, ok)
	require.Equal(t, 1, l.Active())

	done := make(chan int, 1)

	go func() {
		done <- l.Drain(t.Context())
	}()

	// Drain is waiting; new sessions are refused while it does. A probe that still got in
	// (Drain had not started yet) must leave again, or it would hold the drain itself.
	require.Eventually(t, func() bool {
		probeLeave, ok := l.Enter()
		if ok {
			probeLeave()
		}

		return !ok
	}, time.Second, 5*time.Millisecond)

	select {
	case <-done:
		t.Fatal("drain returned before the live session left")
	case <-time.After(50 * time.Millisecond):
	}

	leave()
	leave() // idempotent

	select {
	case remaining := <-done:
		require.Equal(t, 0, remaining)
	case <-time.After(time.Second):
		t.Fatal("drain did not return after the last session left")
	}
}

func TestLiveSessions_DrainGivesUpOnContext(t *testing.T) {
	l := newLiveSessions()

	leave, ok := l.Enter()
	require.True(t, ok)

	defer leave()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	remaining := l.Drain(ctx)

	require.Equal(t, 1, remaining)
	require.Less(t, time.Since(start), time.Second)
}

func TestRelayDrain_NilSafe(t *testing.T) {
	var r *Relay

	require.Equal(t, 0, r.Drain(t.Context()))
	require.Equal(t, 0, (&Relay{Config: Config{}.withDefaults()}).Drain(t.Context()))
}

func TestRelayAcquireSession_RefusesWhileDraining(t *testing.T) {
	r := &Relay{Config: Config{}.withDefaults(), Limits: NewSessionLimits(4, 2), live: newLiveSessions()}

	release, err := r.acquireSession(7)
	require.NoError(t, err)
	require.Equal(t, 1, r.live.Active())
	require.Equal(t, 1, r.Limits.Active())

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	require.Equal(t, 1, r.Drain(ctx))

	_, err = r.acquireSession(7)
	require.ErrorIs(t, err, ErrDraining)
	require.Equal(t, http.StatusServiceUnavailable, acquireStatus(err))
	require.Equal(t, http.StatusTooManyRequests, acquireStatus(ErrTooManySessions))

	// The refused attempt left no slot behind; releasing the live one frees both counters.
	require.Equal(t, 1, r.Limits.Active())

	release()
	release()

	require.Equal(t, 0, r.live.Active())
	require.Equal(t, 0, r.Limits.Active())
}

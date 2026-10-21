package realtime

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// ErrDraining is returned when a session is requested while the relay is shutting down.
var ErrDraining = errors.New("realtime: server is shutting down")

// liveSessions counts the relay goroutines whose teardown must finish before the process
// closes its database: WebSocket sessions, sidebands and call monitors. http.Server.Shutdown
// does not wait for hijacked connections, so the relay waits for them itself.
type liveSessions struct {
	mu       sync.Mutex
	cond     *sync.Cond
	count    int
	draining bool
}

func newLiveSessions() *liveSessions {
	l := &liveSessions{}
	l.cond = sync.NewCond(&l.mu)

	return l
}

// Enter registers one live session. It fails once Drain has started. The returned leave
// function is idempotent.
func (l *liveSessions) Enter() (leave func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.draining {
		return func() {}, false
	}

	l.count++

	var once sync.Once

	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.count--
			l.cond.Broadcast()
			l.mu.Unlock()
		})
	}, true
}

// Active returns the number of live sessions.
func (l *liveSessions) Active() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.count
}

// Drain rejects new sessions and waits until the live ones have left or ctx is done. It
// returns the number of sessions still running when it gave up (0 when all finished).
func (l *liveSessions) Drain(ctx context.Context) int {
	l.mu.Lock()
	l.draining = true
	l.mu.Unlock()

	stop := context.AfterFunc(ctx, func() {
		l.mu.Lock()
		l.cond.Broadcast()
		l.mu.Unlock()
	})
	defer stop()

	l.mu.Lock()
	defer l.mu.Unlock()

	for l.count > 0 && ctx.Err() == nil {
		l.cond.Wait()
	}

	return l.count
}

// acquireSession takes a session-limit slot for apiKeyID (0 = global cap only) and registers
// the session as live. The returned release undoes both and is idempotent. It fails with
// ErrTooManySessions when a cap is reached and ErrDraining once shutdown started.
func (r *Relay) acquireSession(apiKeyID int) (func(), error) {
	releaseLimit, err := r.Limits.Acquire(apiKeyID)
	if err != nil {
		return nil, err
	}

	leave := func() {}

	if r.live != nil {
		var ok bool

		leave, ok = r.live.Enter()
		if !ok {
			releaseLimit()
			return nil, ErrDraining
		}
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			leave()
			releaseLimit()
		})
	}, nil
}

// acquireStatus maps an acquireSession error to the HTTP status answered before the upgrade.
func acquireStatus(err error) int {
	if errors.Is(err, ErrDraining) {
		return http.StatusServiceUnavailable
	}

	return http.StatusTooManyRequests
}

// Drain stops accepting sessions and waits for the live ones (sessions, sidebands and call
// monitors) to finish their teardown and persist usage, until ctx is done. It is called from
// Server.Shutdown after the lifecycle was cancelled, which is what makes the sessions end.
func (r *Relay) Drain(ctx context.Context) int {
	if r == nil || r.live == nil {
		return 0
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, r.Config.ResponseDrain+r.Config.CloseGrace+r.Config.WriteTimeout+drainGrace)
		defer cancel()
	}

	return r.live.Drain(ctx)
}

// drainGrace is the extra time Drain allows for persistence beyond the teardown budget when
// the caller's context has no deadline of its own.
const drainGrace = 5 * time.Second

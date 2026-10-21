package realtime

import (
	"errors"
	"sync"
)

// ErrTooManySessions is returned when the global or per-key concurrent session cap is reached.
var ErrTooManySessions = errors.New("realtime: too many concurrent realtime sessions")

// SessionLimits counts live relay sessions (WebSocket sessions, sidebands and call monitors)
// globally and per API key.
type SessionLimits struct {
	mu     sync.Mutex
	max    int
	maxKey int
	total  int
	perKey map[int]int
}

// NewSessionLimits creates the counters; non-positive caps mean unlimited.
func NewSessionLimits(max, maxPerKey int) *SessionLimits {
	return &SessionLimits{max: max, maxKey: maxPerKey, perKey: map[int]int{}}
}

// Acquire reserves one slot for apiKeyID (0 when the session has no API key). The returned
// release function is idempotent.
func (l *SessionLimits) Acquire(apiKeyID int) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.max > 0 && l.total >= l.max {
		return nil, ErrTooManySessions
	}

	if apiKeyID != 0 && l.maxKey > 0 && l.perKey[apiKeyID] >= l.maxKey {
		return nil, ErrTooManySessions
	}

	l.total++

	if apiKeyID != 0 {
		l.perKey[apiKeyID]++
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()

			l.total--

			if apiKeyID != 0 {
				if l.perKey[apiKeyID] <= 1 {
					delete(l.perKey, apiKeyID)
				} else {
					l.perKey[apiKeyID]--
				}
			}
		})
	}, nil
}

// Active returns the number of live sessions.
func (l *SessionLimits) Active() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.total
}

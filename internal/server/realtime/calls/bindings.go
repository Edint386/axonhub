package calls

import (
	"sync"
	"time"
)

// Binding remembers a call created through the gateway so that later sideband joins, hangups
// and usage records can be attributed to the same channel, credential and API key. The
// exported fields are set before Put and must not be modified afterwards; only
// MarkResponseSeen mutates a stored binding.
type Binding struct {
	// CallID is the upstream call id (rtc_... or a UUID).
	CallID string
	// ChannelID is the channel the call was created on.
	ChannelID int
	// ActualModel is the upstream model name; RequestedModel is the client-facing name.
	ActualModel, RequestedModel string
	// Variant is the inbound protocol variant.
	Variant Variant
	// ProjectID, APIKeyID and RequestID identify the caller and the persisted call request.
	ProjectID int
	APIKeyID  int
	RequestID int
	// CredentialFingerprint identifies the exact upstream credential used at create time so
	// the sideband dials with the same identity.
	CredentialFingerprint string
	// Target is the upstream target the call was created on.
	Target Target
	// CreatedAt is the binding creation time; Put fills it when zero.
	CreatedAt time.Time

	mu   sync.Mutex
	seen map[string]struct{}
}

// MarkResponseSeen records a usage-bearing response id and reports whether it was new. It
// is safe for concurrent use and is used to de-duplicate response.done across the monitor
// and client sidebands attached to the same call.
func (b *Binding) MarkResponseSeen(responseID string) bool {
	if responseID == "" {
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.seen == nil {
		b.seen = make(map[string]struct{})
	}

	if _, dup := b.seen[responseID]; dup {
		return false
	}

	b.seen[responseID] = struct{}{}

	return true
}

// Bindings is an in-memory call_id → Binding store with lazy TTL expiry (checked on Get,
// pruned opportunistically on Put and Len) and a per-project cap that evicts the oldest
// binding of the project. All methods are safe for concurrent use.
type Bindings struct {
	mu            sync.Mutex
	ttl           time.Duration
	perProjectCap int
	byID          map[string]*Binding
	byProject     map[int]map[string]*Binding
	now           func() time.Time
}

// NewBindings creates a store. A ttl <= 0 disables expiry and a perProjectCap <= 0 disables
// the per-project cap.
func NewBindings(ttl time.Duration, perProjectCap int) *Bindings {
	return &Bindings{
		ttl:           ttl,
		perProjectCap: perProjectCap,
		byID:          make(map[string]*Binding),
		byProject:     make(map[int]map[string]*Binding),
		now:           time.Now,
	}
}

// Put stores b under b.CallID, replacing any previous binding with that id. It prunes expired
// bindings and enforces the per-project cap by evicting the project's oldest bindings.
func (s *Bindings) Put(b *Binding) {
	if b == nil || b.CallID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}

	s.pruneLocked(now)

	if prev, ok := s.byID[b.CallID]; ok {
		s.removeLocked(prev)
	}

	s.byID[b.CallID] = b

	project := s.byProject[b.ProjectID]
	if project == nil {
		project = make(map[string]*Binding)
		s.byProject[b.ProjectID] = project
	}

	project[b.CallID] = b

	for s.perProjectCap > 0 && len(project) > s.perProjectCap {
		oldest := s.oldestLocked(project, b)
		if oldest == nil {
			break
		}

		s.removeLocked(oldest)
	}
}

// Get returns the live binding for id. An expired binding is removed and reported absent.
func (s *Bindings) Get(id string) (*Binding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.byID[id]
	if !ok {
		return nil, false
	}

	if s.expiredLocked(b, s.now()) {
		s.removeLocked(b)

		return nil, false
	}

	return b, true
}

// Delete removes the binding for id, if any.
func (s *Bindings) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if b, ok := s.byID[id]; ok {
		s.removeLocked(b)
	}
}

// Len returns the number of live bindings after pruning expired ones.
func (s *Bindings) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(s.now())

	return len(s.byID)
}

// expiredLocked reports whether b is past the TTL at now.
func (s *Bindings) expiredLocked(b *Binding, now time.Time) bool {
	return s.ttl > 0 && now.Sub(b.CreatedAt) >= s.ttl
}

// pruneLocked removes every expired binding.
func (s *Bindings) pruneLocked(now time.Time) {
	if s.ttl <= 0 {
		return
	}

	for _, b := range s.byID {
		if s.expiredLocked(b, now) {
			s.removeLocked(b)
		}
	}
}

// removeLocked unlinks b from both indexes.
func (s *Bindings) removeLocked(b *Binding) {
	if cur, ok := s.byID[b.CallID]; ok && cur == b {
		delete(s.byID, b.CallID)
	}

	if project, ok := s.byProject[b.ProjectID]; ok {
		if cur, ok := project[b.CallID]; ok && cur == b {
			delete(project, b.CallID)
		}

		if len(project) == 0 {
			delete(s.byProject, b.ProjectID)
		}
	}
}

// oldestLocked returns the oldest binding of a project other than keep, or nil.
func (s *Bindings) oldestLocked(project map[string]*Binding, keep *Binding) *Binding {
	var oldest *Binding

	for _, b := range project {
		if b == keep {
			continue
		}

		if oldest == nil || b.CreatedAt.Before(oldest.CreatedAt) ||
			(b.CreatedAt.Equal(oldest.CreatedAt) && b.CallID < oldest.CallID) {
			oldest = b
		}
	}

	return oldest
}

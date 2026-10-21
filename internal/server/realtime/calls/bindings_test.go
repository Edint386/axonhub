package calls

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock is a settable clock for Bindings tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

func newTestBindings(ttl time.Duration, perProjectCap int) (*Bindings, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	s := NewBindings(ttl, perProjectCap)
	s.now = clock.Now

	return s, clock
}

func TestBindings_PutGetDelete(t *testing.T) {
	s, clock := newTestBindings(time.Hour, 0)

	b := &Binding{CallID: "rtc_1", ChannelID: 7, ProjectID: 1, APIKeyID: 3, Variant: VariantCodexV3, Target: backendTarget}
	s.Put(b)

	require.Equal(t, clock.Now(), b.CreatedAt, "Put fills CreatedAt")

	got, ok := s.Get("rtc_1")
	require.True(t, ok)
	require.Same(t, b, got)
	require.Equal(t, 1, s.Len())

	_, ok = s.Get("rtc_missing")
	require.False(t, ok)

	t.Run("replace keeps a single entry", func(t *testing.T) {
		b2 := &Binding{CallID: "rtc_1", ChannelID: 8, ProjectID: 1}
		s.Put(b2)

		got, ok := s.Get("rtc_1")
		require.True(t, ok)
		require.Same(t, b2, got)
		require.Equal(t, 1, s.Len())
	})

	t.Run("replace across projects moves the project index", func(t *testing.T) {
		b3 := &Binding{CallID: "rtc_1", ChannelID: 9, ProjectID: 2}
		s.Put(b3)

		require.Equal(t, 1, s.Len())
		require.Empty(t, s.byProject[1])
		require.Len(t, s.byProject[2], 1)
	})

	s.Delete("rtc_1")

	_, ok = s.Get("rtc_1")
	require.False(t, ok)
	require.Equal(t, 0, s.Len())
	require.Empty(t, s.byProject)

	s.Delete("rtc_1")
	s.Put(nil)
	s.Put(&Binding{})
	require.Equal(t, 0, s.Len())
}

func TestBindings_TTLExpiry(t *testing.T) {
	s, clock := newTestBindings(time.Hour, 0)

	s.Put(&Binding{CallID: "rtc_old", ProjectID: 1})
	clock.Advance(30 * time.Minute)
	s.Put(&Binding{CallID: "rtc_new", ProjectID: 1})

	_, ok := s.Get("rtc_old")
	require.True(t, ok)

	clock.Advance(30 * time.Minute)

	_, ok = s.Get("rtc_old")
	require.False(t, ok, "expired on Get at exactly ttl")

	got, ok := s.Get("rtc_new")
	require.True(t, ok)
	require.Equal(t, "rtc_new", got.CallID)
	require.Equal(t, 1, s.Len())

	t.Run("put prunes expired entries", func(t *testing.T) {
		clock.Advance(time.Hour)
		require.Equal(t, 1, len(s.byID), "still stored until pruned")

		s.Put(&Binding{CallID: "rtc_later", ProjectID: 2})
		require.Len(t, s.byID, 1)
		require.Equal(t, 1, s.Len())

		_, ok := s.byID["rtc_new"]
		require.False(t, ok)
		require.Empty(t, s.byProject[1])
	})

	t.Run("explicit CreatedAt is honoured", func(t *testing.T) {
		s.Put(&Binding{CallID: "rtc_backdated", ProjectID: 2, CreatedAt: clock.Now().Add(-2 * time.Hour)})

		_, ok := s.Get("rtc_backdated")
		require.False(t, ok)
	})

	t.Run("zero ttl never expires", func(t *testing.T) {
		s, clock := newTestBindings(0, 0)
		s.Put(&Binding{CallID: "rtc_forever", ProjectID: 1})
		clock.Advance(1000 * time.Hour)

		_, ok := s.Get("rtc_forever")
		require.True(t, ok)
		require.Equal(t, 1, s.Len())
	})
}

func TestBindings_PerProjectCap(t *testing.T) {
	s, clock := newTestBindings(time.Hour, 2)

	s.Put(&Binding{CallID: "rtc_p1_a", ProjectID: 1})
	clock.Advance(time.Second)
	s.Put(&Binding{CallID: "rtc_p1_b", ProjectID: 1})
	clock.Advance(time.Second)
	s.Put(&Binding{CallID: "rtc_p2_a", ProjectID: 2})
	clock.Advance(time.Second)
	s.Put(&Binding{CallID: "rtc_p1_c", ProjectID: 1})

	_, ok := s.Get("rtc_p1_a")
	require.False(t, ok, "oldest of project 1 evicted")

	_, ok = s.Get("rtc_p1_b")
	require.True(t, ok)

	_, ok = s.Get("rtc_p1_c")
	require.True(t, ok)

	_, ok = s.Get("rtc_p2_a")
	require.True(t, ok, "other projects untouched")

	require.Equal(t, 3, s.Len())

	t.Run("newest put survives even when oldest by time", func(t *testing.T) {
		s.Put(&Binding{CallID: "rtc_p1_d", ProjectID: 1, CreatedAt: clock.Now().Add(-10 * time.Minute)})

		_, ok := s.Get("rtc_p1_d")
		require.True(t, ok)

		_, ok = s.Get("rtc_p1_b")
		require.False(t, ok)

		require.Len(t, s.byProject[1], 2)
	})

	t.Run("equal timestamps evict deterministically", func(t *testing.T) {
		s, _ := newTestBindings(time.Hour, 1)
		s.Put(&Binding{CallID: "rtc_z", ProjectID: 5})
		s.Put(&Binding{CallID: "rtc_a", ProjectID: 5})

		_, ok := s.Get("rtc_z")
		require.False(t, ok)

		_, ok = s.Get("rtc_a")
		require.True(t, ok)
	})

	t.Run("cap zero is unlimited", func(t *testing.T) {
		s, _ := newTestBindings(time.Hour, 0)
		for i := range 50 {
			s.Put(&Binding{CallID: fmt.Sprintf("rtc_%d", i), ProjectID: 1})
		}

		require.Equal(t, 50, s.Len())
	})
}

func TestBinding_MarkResponseSeen(t *testing.T) {
	b := &Binding{CallID: "rtc_1"}

	require.True(t, b.MarkResponseSeen("resp_1"))
	require.False(t, b.MarkResponseSeen("resp_1"))
	require.True(t, b.MarkResponseSeen("resp_2"))
	require.False(t, b.MarkResponseSeen("resp_2"))
	require.False(t, b.MarkResponseSeen(""), "empty ids are never new")
}

func TestBindings_Concurrent(t *testing.T) {
	s, clock := newTestBindings(time.Minute, 8)

	const (
		workers = 16
		rounds  = 200
	)

	var wg sync.WaitGroup

	for w := range workers {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			for i := range rounds {
				id := fmt.Sprintf("rtc_%d_%d", w, i%10)
				b := &Binding{CallID: id, ProjectID: w % 3, APIKeyID: w}
				s.Put(b)

				if got, ok := s.Get(id); ok {
					got.MarkResponseSeen(fmt.Sprintf("resp_%d", i))
					got.MarkResponseSeen(fmt.Sprintf("resp_%d", i))
				}

				if i%7 == 0 {
					s.Delete(id)
				}

				if i%50 == 0 {
					clock.Advance(time.Second)
				}

				_ = s.Len()
			}
		}(w)
	}

	wg.Wait()

	for project, entries := range s.byProject {
		require.LessOrEqual(t, len(entries), 8, "project %d over cap", project)

		for id, b := range entries {
			require.Same(t, b, s.byID[id])
		}
	}

	require.Equal(t, len(s.byID), s.Len())
}

func TestBinding_MarkResponseSeenConcurrent(t *testing.T) {
	b := &Binding{CallID: "rtc_1"}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fresh int
	)

	for range 32 {
		wg.Go(func() {
			for i := range 100 {
				if b.MarkResponseSeen(fmt.Sprintf("resp_%d", i)) {
					mu.Lock()
					fresh++
					mu.Unlock()
				}
			}
		})
	}

	wg.Wait()
	require.Equal(t, 100, fresh, "each response id is new exactly once")
}

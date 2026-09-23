package clock

import (
	"sort"
	"sync"
	"time"
)

// ManualClock is a controllable Clock for tests: Now() advances only when the
// test calls Advance, and After(d) timers fire exactly when a deadline has
// been passed — deterministically, with no wall time and no goroutines of its
// own. Timers are delivered in deadline order (registration order on ties).
type ManualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
	nextID int
}

type manualTimer struct {
	id    int
	at    time.Time
	ch    chan time.Time
	fired bool
}

// NewManualClock builds a clock starting at the given instant.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start, timers: []*manualTimer{}}
}

// Now implements application.Clock.
func (m *ManualClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// After implements application.Clock: the channel fires when Advance moves
// the clock to or past now+d.
func (m *ManualClock) After(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	t := &manualTimer{id: m.nextID, at: m.now.Add(d), ch: make(chan time.Time, 1)}
	m.nextID++
	m.timers = append(m.timers, t)
	m.mu.Unlock()
	return t.ch
}

// Advance moves the clock forward by d and fires every due timer (deadline
// order). It blocks only long enough to send on each timer's buffered channel.
func (m *ManualClock) Advance(d time.Duration) {
	m.mu.Lock()
	target := m.now.Add(d)
	fired := make([]*manualTimer, 0, len(m.timers))
	rest := make([]*manualTimer, 0, len(m.timers))
	for _, t := range m.timers {
		if !t.fired && !t.at.After(target) {
			t.fired = true
			fired = append(fired, t)
		} else if !t.fired {
			rest = append(rest, t)
		}
	}
	sort.SliceStable(fired, func(i, j int) bool { return fired[i].at.Before(fired[j].at) })
	m.now = target
	m.timers = rest
	m.mu.Unlock()
	for _, t := range fired {
		t.ch <- target // buffered (cap 1): never blocks
	}
}

// AdvanceTo moves the clock to an absolute instant (convenience for tests).
func (m *ManualClock) AdvanceTo(t time.Time) {
	if delta := t.Sub(m.Now()); delta > 0 {
		m.Advance(delta)
	}
}

// Package store holds the in-memory (I/O-free) EventStore and GameStore
// implementations of spec §2.1. They are the v1 adapters: append-only,
// audience-filtered, with waiter notification for long-poll holds.
package store

import (
	"context"
	"sync"
	"time"

	"forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
)

// MemoryEventStore is the in-memory EventStore (spec §2.1: infrastructure/
// store). Events are kept per game in append order; seq numbers come from the
// domain (per-game, monotonic from 1), and each event gains its RFC3339 ts
// from the Clock when the batch is appended (§10). Privacy filtering lives in
// Range: a viewer's stream is public events plus that seat's own private ones.
type MemoryEventStore struct {
	mu      sync.RWMutex
	clock   application.Clock
	events  map[string][]application.EventOut
	waiters map[string][]*waiter
}

// waiter is one pending WaitNew registration. closeOnce guards the wake-up
// channel: two Appends can both collect the same waiter before WaitNew
// deregisters it (the collection happens under the store lock, the close
// outside it), and a second close of an already-closed channel would panic
// the whole process — so the close is exactly-once by construction.
type waiter struct {
	after     int64
	ch        chan struct{}
	closeOnce sync.Once
}

// NewMemoryEventStore builds the store; clock stamps appended events.
func NewMemoryEventStore(clock application.Clock) *MemoryEventStore {
	return &MemoryEventStore{clock: clock, events: map[string][]application.EventOut{}, waiters: map[string][]*waiter{}}
}

// Append records one batch and wakes every waiter whose `after` is below the
// game's new last seq. It returns the first appended seq (domain-assigned).
func (s *MemoryEventStore) Append(_ context.Context, gameID string, evts []domain.Event) (int64, error) {
	if len(evts) == 0 {
		return 0, nil
	}
	now := s.clock.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	first := int64(0)
	var wake []*waiter
	for i, e := range evts {
		out := application.EventOut{Seq: e.Seq, GameID: gameID, TS: now, Type: e.Type, Audience: e.Audience, Payload: e.Payload}
		if i == 0 {
			first = out.Seq
		}
		s.events[gameID] = append(s.events[gameID], out)
	}
	last := s.events[gameID][len(s.events[gameID])-1].Seq
	for _, w := range s.waiters[gameID] {
		if last > w.after {
			wake = append(wake, w)
		}
	}
	s.mu.Unlock()
	for _, w := range wake {
		w.closeOnce.Do(func() { close(w.ch) }) // exactly once, even if collected twice (see waiter)
	}
	return first, nil
}

// Range returns the viewer-filtered events with seq > after (spec §2.2):
// public audience or the viewer's own seat-scoped events, ascending seq. An
// empty result is an empty slice, never nil.
func (s *MemoryEventStore) Range(_ context.Context, gameID string, after int64, viewerSeatID string) ([]application.EventOut, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.events[gameID]
	out := []application.EventOut{}
	for _, e := range all {
		if e.Seq <= after {
			continue
		}
		if e.Audience.Public || (viewerSeatID != "" && e.Audience.SeatID == viewerSeatID) {
			out = append(out, e)
		}
	}
	return out, nil
}

// LastSeq returns the game's highest assigned seq (0 when it has none).
func (s *MemoryEventStore) LastSeq(_ context.Context, gameID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.events[gameID]
	if len(all) == 0 {
		return 0, nil
	}
	return all[len(all)-1].Seq, nil
}

// WaitNew blocks until some event with seq > after exists for the game, or
// ctx is done. The check-then-register happens under one lock acquisition so
// an append between check and registration can never be missed.
func (s *MemoryEventStore) WaitNew(ctx context.Context, gameID string, after int64) {
	s.mu.Lock()
	if last := s.lastLocked(gameID); last > after {
		s.mu.Unlock()
		return
	}
	w := &waiter{after: after, ch: make(chan struct{})}
	s.waiters[gameID] = append(s.waiters[gameID], w)
	s.mu.Unlock()

	select {
	case <-w.ch:
	case <-ctx.Done():
	}
	s.mu.Lock()
	ws := s.waiters[gameID]
	for i, x := range ws {
		if x == w {
			s.waiters[gameID] = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

func (s *MemoryEventStore) lastLocked(gameID string) int64 {
	all := s.events[gameID]
	if len(all) == 0 {
		return 0
	}
	return all[len(all)-1].Seq
}

// All returns the game's FULL unfiltered history — public and every seat's
// private events. Extra beyond the EventStore port: it backs the future
// /debug endpoint (spec §11.2), which must see everything unfiltered.
func (s *MemoryEventStore) All(gameID string) []application.EventOut {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.events[gameID]
	out := make([]application.EventOut, len(all))
	copy(out, all)
	return out
}

// Games returns the ids of games with at least one event (debug helper).
func (s *MemoryEventStore) Games() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.events))
	for id := range s.events {
		ids = append(ids, id)
	}
	return ids
}

var _ application.EventStore = (*MemoryEventStore)(nil)

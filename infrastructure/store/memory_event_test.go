package store

import (
	"context"
	"testing"

	"forgejo.lan/mrhollen/ai-coup/domain"
	clock "forgejo.lan/mrhollen/ai-coup/infrastructure/time"
)

// TestAppendWakesWaiterExactlyOnce reproduces, deterministically, the
// double-collection window of the long-poll wake-up: a waiter that is still on
// the registration list when TWO appends arrive (exactly what happens between
// an append's lock release and the woken WaitNew deregistering itself). The
// second close of the already-closed channel panicked the whole process; the
// exactly-once guard makes this sequence safe.
func TestAppendWakesWaiterExactlyOnce(t *testing.T) {
	s := NewMemoryEventStore(clock.NewWallClock())

	// One registered waiter, never deregistered — standing in for the race
	// window where WaitNew has not yet removed itself from the list.
	w := &waiter{after: 0, ch: make(chan struct{})}
	s.mu.Lock()
	s.waiters["g_test"] = append(s.waiters["g_test"], w)
	s.mu.Unlock()

	// Two consecutive appends both collect the same waiter (the collection is
	// under the lock; the closes happen outside it, in this order).
	if _, err := s.Append(context.Background(), "g_test", []domain.Event{{Seq: 1, Type: "action_declared"}}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if _, err := s.Append(context.Background(), "g_test", []domain.Event{{Seq: 2, Type: "coins_changed"}}); err != nil {
		t.Fatalf("second append: %v (a double close of the wake channel panicked here before the fix)", err)
	}

	select {
	case <-w.ch: // closed exactly once
	default:
		t.Fatal("waiter was never woken")
	}
}

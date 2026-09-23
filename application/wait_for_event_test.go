package application_test

import (
	"context"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
)

// TestWaitForEvent pins the A13 auto-client wait primitive: immediate return
// when newer events already exist, a 404 for unknown games, wake-up exactly
// when an event lands (no polling cadence), and clean ctx-cancellation.
func TestWaitForEvent(t *testing.T) {
	h, res := createGameAt(t, []playerSpec{{"A", "human"}, {"B", "human"}}, 0, false, 0, nil, nil)

	// Newer events already exist (game_created/turn_start): return at once.
	start := time.Now()
	if err := h.app.WaitForEvent(context.Background(), res.GameID, 0); err != nil {
		t.Fatalf("WaitForEvent with existing events: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("existing-event wait took %s — must return immediately", elapsed)
	}

	// Unknown game ⇒ 404 AppError, not a block.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	werr := h.app.WaitForEvent(ctx, "g_nosuchgame", 0)
	cancel()
	ae, ok := app.AsAppError(werr)
	if !ok || ae.Status != 404 {
		t.Fatalf("unknown-game WaitForEvent = %v, want 404 AppError", werr)
	}

	// Wakes exactly when an event lands (seat A holds the first turn).
	lastSeq := h.lastSeq(t, res.GameID)
	woke := make(chan error, 1)
	go func() { woke <- h.app.WaitForEvent(context.Background(), res.GameID, lastSeq) }()
	h.mustAct(t, res, res.Seats[0].ID, app.ActionIn{Type: "income"})
	select {
	case err := <-woke:
		if err != nil {
			t.Fatalf("WaitForEvent after a new event: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForEvent never woke when an event landed")
	}

	// Cancellation returns promptly (nothing is owed yet to B... the game may
	// owe B a turn now, so ask for a seq that will not arrive).
	far := h.lastSeq(t, res.GameID) + 1_000_000
	cctx, ccancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() { gone <- h.app.WaitForEvent(cctx, res.GameID, far) }()
	time.Sleep(50 * time.Millisecond) // let the waiter register
	ccancel()
	select {
	case err := <-gone:
		if err != nil {
			t.Fatalf("cancelled WaitForEvent: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled WaitForEvent did not return")
	}
}

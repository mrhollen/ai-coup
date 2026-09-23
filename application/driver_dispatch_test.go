package application_test

import (
	"strings"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	llm "forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestDriver_NeverDrivesHumanSingleSeatDecisions pins the §6.3 boundary at the
// single-seat decision seam (the turn and window seams already checked seat
// type): a flip choice / exchange finish addressed to a HUMAN seat must NOT
// be dispatched to the driver — the human client answers it, and driving it
// would race the client's own submission ("that decision was already made")
// and silently rob the player of the choice. Regression test for the A13
// stress suite catching exactly this in a 6-seat game.
func TestDriver_NeverDrivesHumanSingleSeatDecisions(t *testing.T) {
	// Find a seed where HUMAN seat 0 takes the first turn.
	h, res := createGameAt(t, []playerSpec{{"Ada", "human"}, {"Vito", "llm"}}, 0, true, 0, nil, nil)

	// Vito owes the exchange's challenge window once it opens: queue his pass.
	h.driver.Queue(res.Seats[1].ID, llm.CandidateStep(app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))

	// Ada declares an exchange (ambassador): the claim window opens for Vito,
	// and once it settles the EXCHANGE FINISH decision is addressed to ADA —
	// a human. The driver must not touch it.
	h.mustAct(t, res, res.Seats[0].ID, app.ActionIn{Type: "exchange", Character: "ambassador"})

	var decisionID string
	h.waitFor(t, "Ada's exchange-finish decision open (human-addressed)", func() bool {
		snap := h.snapshot(t, res.GameID, res.Seats[0].ID)
		if snap.Current != nil && snap.Current.Phase == "single_seat_decision" && snap.Current.Decision != nil {
			if snap.Current.Decision.Kind != "exchange_finish" || snap.Current.Decision.SeatID != res.Seats[0].ID {
				return false
			}
			decisionID = snap.Current.Decision.ID
			return true
		}
		return false
	})

	for _, in := range h.driver.Calls() {
		if in.Kind == app.KindSingleSeatChoice && in.SeatID == res.Seats[0].ID {
			t.Fatalf("the driver was dispatched for a human seat's decision %s: %+v — §6.3 forbids driving human seats", decisionID, in)
		}
	}

	// The human still answers her own decision through the public path.
	h.choose(t, res, res.Seats[0].ID, decisionID, app.ChoiceIn{ExchangeFinish: &app.ExchangeFinishIn{NoSwap: true}})
}

// TestRedispatchExactlyOnceAfterDeadWindow pins the §9 slot-release property
// that redispatchIfOwedLocked exists for: a window flight stale-cancelled by a
// close still holds its per-seat slot until its goroutine's defer frees it —
// cancellation does not remove the map entry, and the cancel plus the owed
// turn are committed in ONE batch (the timeout commit), so drivePendingLocked
// must skip the seat whose slot is still occupied. The turn that becomes owed
// to the SAME seat in that very batch must therefore be dispatched EXACTLY
// ONCE — by the redispatch from the dying flight's defer, which runs after
// the commit released the game lock. Zero dispatches would strand the game on
// the owed turn forever (a turn owes no timer); two would violate "at most one
// dispatch per (structure, seat)".
func TestRedispatchExactlyOnceAfterDeadWindow(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}
	gateB := make(chan struct{}) // never closed: B's window flight dies on cancellation
	h, res := createGameAt(t, players, 0 /* A opens */, false, 500, func(h *harness) {
		// B's FA-block response holds his seat slot (barrier); the decision he
		// owes NEXT — the turn that falls to him once the timeout settles the
		// claim — is a plain income. Exactly these two pops, in this order.
		h.driver.Queue(seatIDn(2), llm.GatedStep(gateB, app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
		h.driver.Queue(seatIDn(2), llm.CandidateStep(app.Candidate{Kind: app.KindTurnAction, Action: "income"}))
	}, nil)
	aID := res.Seats[0].ID

	h.mustAct(t, res, aID, app.ActionIn{Type: "foreign_aid"}) // block window over {B}

	// B's gated flight is in flight before the deadline expires.
	h.waitFor(t, "B window dispatch in flight", func() bool { return len(h.driver.Calls()) == 1 })
	h.cl.Advance(600 * time.Millisecond) // (c): timeout auto-passes B, claim survives, turn → B

	// The game must not strand on B's turn: the timeout commit cancelled B's
	// flight while his slot was still occupied (drivePending skipped him); the
	// cancelled flight's defer then frees the slot and redispatches B's owed
	// turn (income), which advances back to A.
	h.waitFor(t, "B's redispatched income committed and turn returned to A", func() bool {
		income := false
		for _, e := range h.stream(t, res.GameID, aID) {
			if e.Type != "action_declared" || e.Payload["actor_id"] != seatIDn(2) {
				continue
			}
			if a, _ := e.Payload["action"].(map[string]any); a != nil && a["type"] == "income" {
				income = true
			}
		}
		cur := h.snapshot(t, res.GameID, aID).Current
		return income && phase(cur) == "turn_action" && cur.TurnSeatID == aID
	})

	var turnCalls int
	for _, in := range h.driver.Calls() {
		if in.SeatID == seatIDn(2) && in.Kind == app.KindTurnAction {
			turnCalls++
		}
	}
	if turnCalls != 1 {
		t.Fatalf("B was dispatched for his owed turn %d times, want exactly 1 (calls: %+v)", turnCalls, h.driver.Calls())
	}

	// The dead window's flight must have been stale-cancelled by the timeout
	// close — and a cancellation must not surface as a terminal driver_error.
	logs := h.logBuf.String()
	if !strings.Contains(logs, `"player_id":"`+seatIDn(2)+`"`) ||
		!strings.Contains(logs, `"reason":"window_closed:timeout"`) {
		t.Fatalf("expected llm_cancelled for B's dead-window flight; logs: %s", tail(logs))
	}
	if strings.Contains(logs, `"msg":"driver_error"`) {
		t.Fatalf("a stale cancellation must not log driver_error; logs: %s", tail(logs))
	}
}

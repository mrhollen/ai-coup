package application_test

import (
	"strings"
	"testing"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestDriverDefaultCoupAtMandatoryThreshold is the full-path regression for
// the pre-fix stall edge: an LLM seat driven to exactly 10 coins whose scripted
// driver exhausts its retry budget (K+1 illegal candidates) must receive the
// kind-safe default — a legal coup against the active opponent — which is
// accepted, logged as decision_fallback with default=coup, and advances the
// turn instead of stalling until timeout/forfeit. The human seat incomes
// between B's turns so that B reaches exactly 10 on his ninth owed turn
// (2 starting coins + 8 incomes).
func TestDriverDefaultCoupAtMandatoryThreshold(t *testing.T) {
	bad := app.Candidate{Kind: app.KindTurnAction, Action: "tax", Character: "captain"} // invalid claim: always rejected
	h, res := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}, 1, false, 0, func(h *harness) {
		// B's queue: 8 legal incomes (2→10 coins), then K+1=4 illegal
		// candidates that burn the retry budget and force the kind default.
		for i := 0; i < 8; i++ {
			h.driver.Queue(seatIDn(2), llm.CandidateStep(app.Candidate{Kind: app.KindTurnAction, Action: "income"}))
		}
		for i := 0; i < 4; i++ { // K = 3 retries (app.maxDriverRetries) + first try
			h.driver.Queue(seatIDn(2), llm.CandidateStep(bad))
		}
	}, nil)
	aID, bID := res.Seats[0].ID, res.Seats[1].ID

	// Drive the human seat: B starts, turns alternate, and A incomes once per
	// interval — 8 times total before B's ninth owed turn (at exactly 10).
	for i := 0; i < 8; i++ {
		h.waitFor(t, "turn back at human seat A", func() bool {
			snap := h.snapshot(t, res.GameID, aID)
			return snap.Current.TurnSeatID == aID
		})
		if got := h.coinsOf(t, res, bID); got < 2 || got > 10 {
			t.Fatalf("B coins mid-run = %d, want within [2,10]", got)
		}
		h.mustAct(t, res, aID, app.ActionIn{Type: "income"})
	}

	// B's ninth owed turn is at exactly 10 coins: the scripted bad candidates
	// burn K retries and the kind-safe default coup must land.
	h.waitFor(t, "kind-default coup applied for B", func() bool {
		for _, e := range h.stream(t, res.GameID, aID) {
			if e.Type == "action_declared" && e.Payload["actor_id"] == bID {
				act, _ := e.Payload["action"].(map[string]any)
				if act != nil && act["type"] == "coup" && act["target_id"] == aID {
					return true
				}
			}
		}
		return false
	})

	calls := h.driver.Calls()
	if len(calls) != 12 { // 8 incomes + first try + K=3 retries
		t.Fatalf("Decide calls = %d, want 12", len(calls))
	}
	for i := 9; i < 12; i++ {
		if calls[i].Feedback == "" {
			t.Fatalf("call %d: empty Feedback — retry loop must feed the rejection back (§6.4.4)", i)
		}
	}

	logs := h.logBuf.String()
	if strings.Contains(logs, `"msg":"decision_fallback_failed"`) || strings.Contains(logs, `"msg":"driver_error"`) {
		t.Fatalf("no fallback/driver failure may be logged for the mandatory-coup default: %s", logs)
	}
	if !strings.Contains(logs, `"msg":"decision_fallback"`) || !strings.Contains(logs, `"default":"coup"`) {
		t.Fatalf("expected decision_fallback with default=coup; logs: %s", logs)
	}

	// No stall: the coup landed on A (two face-down cards), so the cascade
	// suspends on A's flip choice; answer it and verify play resumes at A's
	// next turn — the decision did not wedge until timeout/forfeit.
	h.waitFor(t, "A owes the post-coup flip choice", func() bool {
		snap := h.snapshot(t, res.GameID, aID)
		return snap.Current != nil && snap.Current.Phase == "single_seat_decision" && snap.Current.Decision != nil
	})
	dID := h.snapshot(t, res.GameID, aID).Current.Decision.ID
	if _, err := h.choose(t, res, aID, dID, app.ChoiceIn{FlipChoice: "face_down_0"}); err != nil {
		t.Fatalf("answer flip choice: %v", err)
	}
	h.waitFor(t, "turn back at A after the cascade finishes", func() bool {
		snap := h.snapshot(t, res.GameID, aID)
		return snap.Current.Phase == "turn_action" && snap.Current.TurnSeatID == aID
	})
	if got := h.coinsOf(t, res, bID); got != 3 { // 10 − 7 coup cost (spec §4.1)
		t.Fatalf("B coins after default coup = %d, want 3", got)
	}
}

package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestWindowTerminationMatrix covers §14.2's all-four-combination matrix:
// require_all_responses × response_timeout {none, T}, asserting close reason,
// auto-pass bookkeeping, and that LLM seats always get answered.
func TestWindowTerminationMatrix(t *testing.T) {
	A, B, C := "A", "B", "C" // seat names; ids s_1,s_2,s_3 deterministically
	players := []playerSpec{{name: A, typ: "human"}, {name: B, typ: "llm"}, {name: C, typ: "llm"}}

	t.Run("no_require_all_no_timeout_open_end_fcfs", func(t *testing.T) {
		gateC := make(chan struct{})
		h, res := createGameAt(t, players, 0 /* A first */, false, 0, func(h *harness) {
			h.driver.Queue(seatIDn(3), llm.GatedStep(gateC, app.Candidate{Kind: app.KindWindowResponse, Response: "challenge"}))
		}, nil)
		aID, cID := res.Seats[0].ID, res.Seats[2].ID

		h.mustAct(t, res, aID, app.ActionIn{Type: "tax", Character: "duke"})
		if phase(h.snapshot(t, res.GameID, aID).Current) != "window_open" {
			t.Fatal("window did not open after tax declaration")
		}

		// Open-ended FCFS (combination 1): no timeout armed — the window must
		// still be open well past where a timeout would have fired.
		h.cl.Advance(60 * time.Second)
		if phase(h.snapshot(t, res.GameID, aID).Current) != "window_open" {
			t.Fatal("open-ended window closed without a response (A8/§5.2.5 combination 1)")
		}

		// First non-PASS wins: C's challenge closes it for everyone.
		close(gateC)
		h.waitFor(t, "window close", func() bool { return phase(h.snapshot(t, res.GameID, aID).Current) != "window_open" })
		evts := h.stream(t, res.GameID, aID)
		closed := findEvents(evts, "window_closed")
		if len(closed) != 1 || closed[0].Payload["reason"] != "first_response" {
			t.Fatalf("want exactly one window_closed first_response, got %+v", findEvents(evts, "window_closed"))
		}
		if decider, _ := closed[0].Payload["decider_seat_id"].(string); decider != cID {
			t.Fatalf("decider = %q, want %q", decider, cID)
		}
	})

	t.Run("no_require_all_timeout_auto_pass_and_proceeds", func(t *testing.T) {
		h, res := createGameAt(t, players, 0, false, 500, nil, nil)
		aID := res.Seats[0].ID
		coinsBefore := h.coinsOf(t, res, aID)

		h.mustAct(t, res, aID, app.ActionIn{Type: "tax", Character: "duke"})
		h.waitFor(t, "llm responses in flight", func() bool { return len(h.driver.Calls()) == 2 }) // B and C dispatched
		h.cl.Advance(600 * time.Millisecond)                                                       // (c): at T unresponded seats auto-pass
		h.waitFor(t, "timeout close committed", func() bool { return windowCloseReason(h.stream(t, res.GameID, aID)) != "" })

		evts := h.stream(t, res.GameID, aID)
		closed := findEvents(evts, "window_closed")
		if len(closed) != 1 || closed[0].Payload["reason"] != "timeout" {
			t.Fatalf("want window_closed timeout, got %+v", closed)
		}
		// Auto-pass bookkeeping: one pass event per unresponded seat.
		passes := findEvents(evts, "response_received")
		if len(passes) != 2 {
			t.Fatalf("want 2 auto-pass response_received events, got %d", len(passes))
		}
		for _, p := range passes {
			resp, _ := p.Payload["response"].(map[string]any)
			if resp["type"] != "pass" {
				t.Fatalf("auto pass recorded as %+v", resp)
			}
		}
		// Unopposed claim proceeds: +3 tax coins on the claimant.
		var gotTotal int
		for _, e := range findEvents(evts, "coins_changed") {
			if e.Payload["seat_id"] == aID && e.Payload["reason"] == "tax" {
				gotTotal, _ = e.Payload["total"].(int)
			}
		}
		if gotTotal != coinsBefore+3 {
			t.Fatalf("tax proceeds: total=%d want %d", gotTotal, coinsBefore+3)
		}
	})

	t.Run("require_all_no_timeout_all_responded", func(t *testing.T) {
		// C is a HUMAN here: the test answers the window as C (server-driven
		// seats have no client stream), and B (LLM) passes via its driver.
		h, res := createGameAt(t, []playerSpec{{name: A, typ: "human"}, {name: B, typ: "llm"}, {name: C, typ: "human"}}, 0, true, 0, func(h *harness) {
			h.driver.Queue(seatIDn(2), llm.CandidateStep(app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
		}, nil)
		aID, cID := res.Seats[0].ID, res.Seats[2].ID

		h.mustAct(t, res, aID, app.ActionIn{Type: "tax", Character: "duke"})
		// B (LLM) passes automatically; C answers manually → every eligible
		// seat responded ⇒ close by all_responded (combination 3).
		h.waitFor(t, "B pass applied", func() bool { return len(findEvents(h.stream(t, res.GameID, aID), "response_received")) == 1 })
		if _, err := h.respond(t, res, cID, app.ResponseIn{Type: "pass"}, ""); err != nil {
			t.Fatalf("C's pass rejected: %v", err)
		}

		evts := h.stream(t, res.GameID, aID)
		closed := findEvents(evts, "window_closed")
		if len(closed) != 1 || closed[0].Payload["reason"] != "all_responded" {
			t.Fatalf("want window_closed all_responded, got %+v", closed)
		}
		if len(findEvents(evts, "response_received")) != 2 {
			t.Fatal("both eligible seats must have exactly one response event")
		}
	})

	t.Run("require_all_timeout_earliest_wins", func(t *testing.T) {
		// (i) timeout before everyone has responded.
		gateB, gateC := make(chan struct{}), make(chan struct{})
		h, res := createGameAt(t, players, 0, true, 500, func(h *harness) {
			h.driver.Queue(seatIDn(2), llm.GatedStep(gateB, app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
		}, nil)
		aID := res.Seats[0].ID
		h.mustAct(t, res, aID, app.ActionIn{Type: "tax", Character: "duke"})
		h.waitFor(t, "fan-out", func() bool { return len(h.driver.Calls()) == 2 })
		h.cl.Advance(600 * time.Millisecond) // beats the (never-coming) C response
		h.waitFor(t, "timeout close committed", func() bool { return windowCloseReason(h.stream(t, res.GameID, aID)) != "" })
		if reason := windowCloseReason(h.stream(t, res.GameID, aID)); reason != "timeout" {
			t.Fatalf("want timeout close, got %q", reason)
		}
		close(gateB) // released candidates must be discarded, not applied

		// (ii) all_responded before the deadline. C is human: the test answers
		// as C; B (LLM) passes via its driver.
		h2, res2 := createGameAt(t, []playerSpec{{name: A, typ: "human"}, {name: B, typ: "llm"}, {name: C, typ: "human"}}, 0, true, 500, func(h *harness) {
			h.driver.Queue(seatIDn(2), llm.CandidateStep(app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
		}, nil)
		a2, c2 := res2.Seats[0].ID, res2.Seats[2].ID
		h2.mustAct(t, res2, a2, app.ActionIn{Type: "tax", Character: "duke"})
		h2.waitFor(t, "B pass applied", func() bool { return len(findEvents(h2.stream(t, res2.GameID, a2), "response_received")) == 1 })
		if _, err := h2.respond(t, res2, c2, app.ResponseIn{Type: "pass"}, ""); err != nil {
			t.Fatalf("C's pass rejected: %v", err)
		}
		if reason := windowCloseReason(h2.stream(t, res2.GameID, a2)); reason != "all_responded" {
			t.Fatalf("want all_responded close, got %q", reason)
		}
		close(gateC)
	})
}

func phase(cur *app.CurrentSnap) string { return cur.Phase }

func windowCloseReason(evts []app.EventOut) string {
	closed := findEvents(evts, "window_closed")
	if len(closed) == 0 {
		return ""
	}
	r, _ := closed[0].Payload["reason"].(string)
	return r
}

// TestStaleAndOnceResponses_A16 asserts the A16 semantics end to end: one
// response per seat while open ("you have already responded"), stale responses
// after close hit the resolved state through normal validation with 409 and
// "the window has already been resolved", mismatched window ids are rejected
// the same way, and every rejection lands as a private rejection event (A9).
func TestStaleAndOnceResponses_A16(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}, {name: "C", typ: "human"}}
	gateB := make(chan struct{})
	h, res := createGameAt(t, players, 0, true /* keep the window open on passes */, 0, func(h *harness) {
		h.driver.Queue(seatIDn(2), llm.GatedStep(gateB, app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
	}, nil)
	aID, cID := res.Seats[0].ID, res.Seats[2].ID

	h.mustAct(t, res, aID, app.ActionIn{Type: "tax", Character: "duke"}) // window over {B,C}

	// C passes (accepted — passes never close by themselves).
	if _, err := h.respond(t, res, cID, app.ResponseIn{Type: "pass"}, ""); err != nil {
		t.Fatalf("first C pass rejected: %v", err)
	}
	// Second attempt while the window is still open ⇒ once-ness rejection.
	_, err := h.respond(t, res, cID, app.ResponseIn{Type: "challenge"}, "")
	if got := errCode(t, err); got != "already_resolved" {
		t.Fatalf("second C response code = %q, want already_resolved", got)
	}
	ae, _ := app.AsAppError(err)
	if ae.Message != "you have already responded" {
		t.Fatalf("message = %q", ae.Message)
	}

	// B (gated, in flight) passes ⇒ require-all close.
	close(gateB)
	h.waitFor(t, "all_responded close", func() bool { return windowCloseReason(h.stream(t, res.GameID, aID)) != "" })

	// Late response after close ⇒ stale rejection, normal validation path.
	_, err = h.respond(t, res, cID, app.ResponseIn{Type: "challenge"}, "")
	if got := errCode(t, err); got != "already_resolved" {
		t.Fatalf("late response code = %q", got)
	}
	ae, _ = app.AsAppError(err)
	if ae.Message != "the window has already been resolved" {
		t.Fatalf("stale message = %q", ae.Message)
	}
	if ae.Status != 409 {
		t.Fatalf("stale status = %d, want 409", ae.Status)
	}
	// A mismatched window_id is rejected with the same stale semantics (A16).
	_, err = h.respond(t, res, cID, app.ResponseIn{Type: "pass"}, "w_deadbeef0000")
	if got := errCode(t, err); got != "already_resolved" {
		t.Fatalf("mismatched window_id code = %q", got)
	}
	ae2, _ := app.AsAppError(err)
	if ae2.Message != "the window has already been resolved" {
		t.Fatalf("mismatched window_id message = %q", ae2.Message)
	}

	// A9: every semantic rejection is a private event on C's stream only —
	// the double response, the late response and the mismatched window id.
	cStream := h.stream(t, res.GameID, cID)
	rej := findEvents(cStream, "rejection")
	if len(rej) != 3 {
		t.Fatalf("want 3 rejection events on C's stream, got %d", len(rej))
	}
	for _, r := range rej {
		if r.Audience.SeatID != cID {
			t.Fatalf("rejection audience = %+v, want seat %s", r.Audience, cID)
		}
	}
	codes := map[string]bool{}
	for _, r := range rej {
		codes[r.Payload["code"].(string)] = true
	}
	if !codes["already_resolved"] {
		t.Fatalf("rejection codes = %v", codes)
	}
	aStream := h.stream(t, res.GameID, aID)
	if len(findEvents(aStream, "rejection")) != 0 {
		t.Fatal("A's stream contains C's private rejection events")
	}
}

// TestLLMCancellationOnWindowClose is the cancellation property: when a peer's
// response wins the window, every other in-flight LLM decision for that game
// is cancelled immediately — the blocked driver observes ctx cancellation and
// the discarded candidate never touches engine state.
func TestLLMCancellationOnWindowClose(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}, {name: "C", typ: "llm"}}
	h, res := createGameAt(t, players, 1 /* B holds the first turn */, false, 0, func(h *harness) {
		// B's act candidate AND C's blocked response must be queued before any
		// dispatch pops them (the fan-out happens inside CreateGame's commit).
		// A Duke-tax costs nothing: the window's opening must not depend on a
		// banked player.
		h.driver.Queue(seatIDn(2), llm.CandidateStep(app.Candidate{Kind: app.KindTurnAction, Action: "tax", Character: "duke"}))
		h.driver.Queue(seatIDn(3), llm.BlockStep())
	}, nil)
	aID := res.Seats[0].ID

	h.waitFor(t, "attack window open", func() bool { return phase(h.snapshot(t, res.GameID, aID).Current) == "window_open" })

	// C's response is now in flight (blocked); A challenges to win the window.
	h.waitFor(t, "C dispatch in flight", func() bool { return len(h.driver.Calls()) == 2 })
	if _, err := h.respond(t, res, aID, app.ResponseIn{Type: "challenge"}, ""); err != nil {
		t.Fatalf("A's challenge rejected: %v", err)
	}

	h.waitFor(t, "C cancellation observed", func() bool { return contains(h.driver.Canceled(), seatIDn(3)) })
	if got := h.driver.Canceled(); len(got) != 1 || !contains(got, seatIDn(3)) {
		t.Fatalf("canceled seats = %v, want exactly [s_…3]", got)
	}
	logs := h.logBuf.String()
	if !strings.Contains(logs, `"msg":"llm_cancelled"`) || !strings.Contains(logs, `"reason":"window_closed:first_response"`) {
		t.Fatalf("expected llm_cancelled log with window reason; logs: %s", tail(logs))
	}
	// The discarded late candidate must not have produced a rejection event on
	// C's stream nor any engine effect.
	if rej := findEvents(h.events.All(res.GameID), "rejection"); len(rej) != 0 {
		t.Fatalf("unexpected rejection events: %+v", rej)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func tail(s string) string {
	const n = 2000
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// TestDriverRetryFeedbackThenDefault is the A4 no-permadeath property at app
// level: K=3 illegal-candidate retries with table-voice feedback, then the
// kind-safe default — the game never stalls on a bad driver.
func TestDriverRetryFeedbackThenDefault(t *testing.T) {
	t.Run("turn_action_default_income", func(t *testing.T) {
		bad := app.Candidate{Kind: app.KindTurnAction, Action: "tax", Character: "captain"} // invalid claim, always rejected
		h, res := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}, 1, false, 0, func(h *harness) {
			h.driver.Queue(seatIDn(2), llm.CandidateStep(bad), llm.CandidateStep(bad), llm.CandidateStep(bad), llm.CandidateStep(bad))
		}, nil)
		bID := res.Seats[1].ID

		h.waitFor(t, "fallback income applied", func() bool {
			for _, e := range h.stream(t, res.GameID, res.Seats[0].ID) {
				if e.Type == "action_declared" && e.Payload["actor_id"] == bID && e.Payload["action"].(map[string]any)["type"] == "income" {
					return true
				}
			}
			return false
		})
		calls := h.driver.Calls()
		if len(calls) != 4 { // first try + K=3 retries
			t.Fatalf("Decide calls = %d, want 4", len(calls))
		}
		for i := 1; i < 4; i++ {
			if calls[i].Feedback == "" {
				t.Fatalf("call %d: empty Feedback — retry loop must feed the rejection back (§6.4.4)", i)
			}
		}
		if calls[1].Feedback != "you cannot claim that character for that action" {
			t.Fatalf("feedback = %q (must be the domain table-voice message)", calls[1].Feedback)
		}
		// The turn advanced past B: world moved, no stall.
		snap := h.snapshot(t, res.GameID, res.Seats[0].ID)
		if snap.Current.Phase != "turn_action" || snap.Current.TurnSeatID != res.Seats[0].ID {
			t.Fatalf("after fallback the next turn should be A's: %+v", snap.Current)
		}
		if !strings.Contains(h.logBuf.String(), `"msg":"decision_fallback"`) {
			t.Fatal("expected a decision_fallback log line")
		}
	})

	t.Run("window_response_default_pass", func(t *testing.T) {
		bad := app.Candidate{Kind: app.KindWindowResponse, Response: "nuke"} // unparseable response
		h, res := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}, 0, false, 0, func(h *harness) {
			h.driver.Queue(seatIDn(2), llm.CandidateStep(bad), llm.CandidateStep(bad), llm.CandidateStep(bad), llm.CandidateStep(bad))
		}, nil)
		aID := res.Seats[0].ID
		h.mustAct(t, res, aID, app.ActionIn{Type: "tax", Character: "duke"}) // window over {B}

		h.waitFor(t, "fallback pass applied", func() bool {
			for _, e := range h.stream(t, res.GameID, aID) {
				if e.Type == "response_received" && e.Payload["seat_id"] == res.Seats[1].ID {
					return true
				}
			}
			return false
		})
		calls := h.driver.Calls()
		if len(calls) != 4 || calls[1].Feedback != "not a valid response" {
			t.Fatalf("calls=%d feedback=%q", len(calls), calls[1].Feedback)
		}
		// Passes never close an FCFS window: structure still open, no crash.
		if phase(h.snapshot(t, res.GameID, aID).Current) != "window_open" {
			t.Fatal("fallback pass should leave the FCFS window open")
		}
	})
}

// TestPendingDecisionTimeoutAppliesEngineDefault: with a response timeout, a
// pending single-seat decision times out to its engine default (A4/A5) and a
// late driver answer arriving afterwards is discarded without effect.
func TestPendingDecisionTimeoutAppliesEngineDefault(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}
	h, res := createGameAt(t, players, 0, false, 1000, nil, nil)
	aID, bID := res.Seats[0].ID, res.Seats[1].ID

	h.pumpForeignAid(t, res, aID, 7, 1500*time.Millisecond, nil) // A banks to ≥7 via FA rounds

	gateB := make(chan struct{})
	// B's pending flip-choice answer will be gated (queued last ⇒ popped next).
	h.driver.Queue(bID, llm.GatedStep(gateB, app.Candidate{Kind: app.KindSingleSeatChoice, Choice: "face_down_1"}))

	h.mustAct(t, res, aID, app.ActionIn{Type: "coup", TargetID: bID}) // B (2 cards) owes a flip choice
	h.waitFor(t, "flip decision pending", func() bool { return phase(h.snapshot(t, res.GameID, aID).Current) == "single_seat_decision" })

	h.cl.Advance(1500 * time.Millisecond) // expire: engine default face_down_0 applies
	h.waitFor(t, "timeout flip committed", func() bool { return len(findEvents(h.stream(t, res.GameID, aID), "card_flipped")) > 0 })
	evts := h.stream(t, res.GameID, aID)
	flipped := findEvents(evts, "card_flipped")
	if len(flipped) != 1 {
		t.Fatalf("want exactly one card_flipped after timeout, got %d", len(flipped))
	}
	if reason, _ := flipped[0].Payload["reason"].(string); reason != "coup" {
		t.Fatalf("flip reason = %q", reason)
	}

	// The gated (late) driver answer now arrives: discarded, no second flip.
	close(gateB)
	h.waitFor(t, "late candidate discarded", func() bool { return strings.Contains(h.logBuf.String(), `"msg":"llm_discarded"`) })
	if got := len(findEvents(h.stream(t, res.GameID, aID), "card_flipped")); got != 1 {
		t.Fatalf("second flip applied from a stale decision answer: %d flips", got)
	}
}

// TestSubmitDecisionValidation covers the payload-level (400 class) checks of
// the single decisions endpoint.
func TestSubmitDecisionValidation(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}
	// No post-creation predicate: every case below is rejected at payload level
	// (400 class) before any engine validation, so banked coins are irrelevant.
	h, res := createGameAt(t, players, 0, false, 0, nil, nil)
	aID := res.Seats[0].ID

	cases := []struct {
		name string
		in   app.DecisionIn
	}{
		{"unknown kind", app.DecisionIn{Kind: "zap"}},
		{"act missing action", app.DecisionIn{Kind: "act"}},
		{"unknown action type", app.DecisionIn{Kind: "act", Action: &app.ActionIn{Type: "teleport"}}},
		{"claim without character", app.DecisionIn{Kind: "act", Action: &app.ActionIn{Type: "tax"}}},
		{"coup without target", app.DecisionIn{Kind: "act", Action: &app.ActionIn{Type: "coup"}}},
		{"respond missing response", app.DecisionIn{Kind: "respond"}},
		{"respond bad type", app.DecisionIn{Kind: "respond", Response: &app.ResponseIn{Type: "nuke"}}},
		{"choose without choice", app.DecisionIn{Kind: "choose", DecisionID: "d_x"}},
		{"choose both kinds", app.DecisionIn{Kind: "choose", DecisionID: "d_x", Choice: &app.ChoiceIn{FlipChoice: "face_down_0", ExchangeFinish: &app.ExchangeFinishIn{NoSwap: true}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.app.SubmitDecision(context.Background(), res.GameID, aID, tc.in)
			if got := errCode(t, err); got != "invalid_payload" {
				t.Fatalf("code = %q, want invalid_payload", got)
			}
		})
	}

	// Semantics still reachable through normal validation: acting with a valid
	// action while it is your own turn works; then acting again is rejected.
	h.mustAct(t, res, aID, app.ActionIn{Type: "income"})
	if _, err := h.act(t, res, aID, app.ActionIn{Type: "income"}); errCode(t, err) != "not_your_turn" {
		t.Fatalf("second action in same turn not rejected: %v", err)
	}

	// Finished games reject with game_over (all-human game ⇒ no in-flight
	// drivers, so flipping the status directly is race-free).
	hh, res2 := createGameAt(t, []playerSpec{{name: "X", typ: "human"}, {name: "Y", typ: "human"}}, 0, false, 0, nil, nil)
	xID := res2.Seats[0].ID
	hh.forceFinished(t, res2, xID)
	if _, err := hh.act(t, res2, xID, app.ActionIn{Type: "income"}); errCode(t, err) != "game_over" {
		t.Fatalf("act on finished game not rejected with game_over: %v", err)
	}
}

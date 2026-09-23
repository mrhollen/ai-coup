package application_test

import (
	"context"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestLongPollBehaviors covers §14.2 long-poll semantics against the in-memory
// EventStore with a manual clock: immediate return, hold expiry with an empty
// 200 (A1), prompt return on a new audience-relevant event without any clock
// advance, idempotent `after`, and no poll stream for LLM seats.
func TestLongPollBehaviors(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}

	t.Run("immediate_return_with_existing_events", func(t *testing.T) {
		h, res := createGameAt(t, players, 0 /* A holds turn; B owes nothing yet */, false, 0, nil, nil)
		aID := res.Seats[0].ID
		start := h.cl.Now()
		out, err := h.app.PollEvents(context.Background(), res.GameID, 0, aID, app.DefaultPollHold)
		if err != nil {
			t.Fatalf("PollEvents: %v", err)
		}
		if len(out.Events) < 2 { // game_created + turn_start at minimum
			t.Fatalf("want existing events returned immediately, got %d", len(out.Events))
		}
		base := h.lastSeq(t, res.GameID)
		if out.LastSeq != base {
			t.Fatalf("last_seq=%d want store position %d", out.LastSeq, base)
		}
		// No game time elapsed: nothing could have fired.
		if !h.cl.Now().Equal(start) {
			t.Fatal("poll consumed manual clock time — it should not")
		}
	})

	t.Run("hold_expiry_returns_empty_200", func(t *testing.T) {
		h, res := createGameAt(t, players, 0, false, 0, nil, nil)
		aID := res.Seats[0].ID
		base := h.lastSeq(t, res.GameID)

		done := make(chan *app.PollResult, 1)
		go func() {
			out, err := h.app.PollEvents(context.Background(), res.GameID, base, aID, app.DefaultPollHold)
			if err != nil {
				t.Errorf("PollEvents: %v", err)
				return
			}
			done <- out
		}()

		select {
		case <-done:
			t.Fatal("poll returned before the hold expired — no events exist")
		case <-time.After(150 * time.Millisecond): // wall-clock sanity, not game time
		}
		h.cl.Advance(app.DefaultPollHold) // A1: deadline hit
		select {
		case out := <-done:
			if len(out.Events) != 0 {
				t.Fatalf("empty-200 must carry no events, got %d", len(out.Events))
			}
			if out.Events == nil {
				t.Fatal("events must be a non-nil empty slice (JSON [])")
			}
			if out.LastSeq != base {
				t.Fatalf("last_seq=%d want %d", out.LastSeq, base)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("poll did not return after the hold expired")
		}
	})

	t.Run("prompt_return_on_new_audience_event_no_clock_advance", func(t *testing.T) {
		h, res := createGameAt(t, players, 0, false, 0, nil, nil)
		aID := res.Seats[0].ID
		base := h.lastSeq(t, res.GameID)

		done := make(chan *app.PollResult, 1)
		go func() {
			out, err := h.app.PollEvents(context.Background(), res.GameID, base, aID, app.DefaultPollHold)
			if err != nil {
				t.Errorf("PollEvents: %v", err)
				return
			}
			done <- out
		}()

		// A acts while the poll is holding: the append must wake it promptly.
		h.waitFor(t, "poll hold timer armed", func() bool { return h.cl.Pending() == 1 })
		h.mustAct(t, res, aID, app.ActionIn{Type: "income"})

		select {
		case out := <-done:
			if len(out.Events) == 0 {
				t.Fatal("new audience-relevant events were not returned")
			}
			if out.Events[0].Type != "action_declared" {
				t.Fatalf("first new event = %s, want action_declared", out.Events[0].Type)
			}
			if out.LastSeq <= base {
				t.Fatalf("last_seq=%d must advance past base %d", out.LastSeq, base)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("poll did not wake on the new event")
		}
	})

	t.Run("llm_seats_have_no_poll_stream", func(t *testing.T) {
		h, res := createGameAt(t, players, 0, false, 0, nil, nil)
		bID := res.Seats[1].ID
		if _, err := h.app.PollEvents(context.Background(), res.GameID, 0, bID, app.DefaultPollHold); errCode(t, err) != "unauthorized" {
			t.Fatalf("LLM-seat poll must be rejected unauthorized, got %v", err)
		}
		if _, err := h.app.GetSnapshot(context.Background(), res.GameID, bID); errCode(t, err) != "unauthorized" {
			t.Fatalf("LLM-seat snapshot must be rejected unauthorized, got %v", err)
		}
	})
}

// TestAudienceFiltering asserts the §8 privacy guarantees end to end: another
// seat's private events (exchange reveals, flip-choice requests) never appear
// in a human's stream; rejections are private to the offending seat only; and
// snapshot decision options are seat-scoped to the addressed viewer.
func TestAudienceFiltering(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}, {name: "C", typ: "human"}}
	gateB := make(chan struct{})
	h, res := createGameAt(t, players, 1 /* B first */, true, 0, func(h *harness) {
		h.driver.Queue(seatIDn(2), llm.GatedStep(gateB, app.Candidate{Kind: app.KindTurnAction, Action: "exchange", Character: "ambassador"}))
	}, nil)
	aID, cID := res.Seats[0].ID, res.Seats[2].ID

	// Release B's gated exchange declaration ⇒ window over {A, C}.
	close(gateB)
	h.waitFor(t, "exchange window open", func() bool { return phase(h.snapshot(t, res.GameID, aID).Current) == "window_open" })

	// C's non-grammar counterclaim is rejected (private to C); then C passes.
	if _, err := h.respond(t, res, cID, app.ResponseIn{Type: "counterclaim", Character: "duke"}, ""); errCode(t, err) != "not_eligible" {
		t.Fatalf("C's out-of-grammar counterclaim should be not_eligible, got %v", err)
	}
	h.respond(t, res, cID, app.ResponseIn{Type: "pass"}, "")
	h.respond(t, res, aID, app.ResponseIn{Type: "pass"}, "") // all responded ⇒ claim proceeds

	h.waitFor(t, "exchange finish decision pending", func() bool {
		s := h.snapshot(t, res.GameID, aID)
		return s.Current.Phase == "single_seat_decision" && s.Current.Decision != nil && s.Current.Decision.SeatID == res.Seats[1].ID
	})

	// --- Poll-stream privacy -------------------------------------------------
	for _, viewer := range []string{aID, cID} {
		stream := h.stream(t, res.GameID, viewer)
		if evs := findEvents(stream, "exchange_revealed"); len(evs) != 0 {
			t.Fatalf("viewer %s stream leaked exchange_revealed: %+v", viewer, evs)
		}
		if evs := findEvents(stream, "card_flip_choice_requested"); len(evs) != 0 {
			t.Fatalf("viewer %s stream leaked card_flip_choice_requested", viewer)
		}
	}
	aStream := h.stream(t, res.GameID, aID)
	if evs := findEvents(aStream, "rejection"); len(evs) != 0 {
		t.Fatalf("A's stream contains C's private rejection events: %+v", evs)
	}
	cStream := h.stream(t, res.GameID, cID)
	rej := findEvents(cStream, "rejection")
	if len(rej) != 1 || rej[0].Payload["code"] != "not_eligible" {
		t.Fatalf("C's stream must contain exactly the not_eligible rejection, got %+v", rej)
	}

	// B's own stream does carry its private reveal (audience tagging works).
	bStream, err := h.events.Range(context.Background(), res.GameID, 0, res.Seats[1].ID)
	if err != nil {
		t.Fatalf("Range(B): %v", err)
	}
	if len(findEvents(bStream, "exchange_revealed")) != 1 {
		t.Fatal("B's own stream is missing its exchange_revealed event")
	}

	// --- Snapshot privacy ----------------------------------------------------
	// The decision is addressed to B (an LLM): no human snapshot may carry its
	// options; the kind + seat are public.
	for _, viewer := range []string{aID, cID} {
		s := h.snapshot(t, res.GameID, viewer)
		if s.Current.Decision.Options != nil {
			t.Fatalf("viewer %s snapshot leaked decision options (addressed to B)", viewer)
		}
		if s.Current.Decision.Kind != "exchange_finish" || s.Current.Decision.SeatID != res.Seats[1].ID {
			t.Fatalf("decision identity not public: %+v", s.Current.Decision)
		}
	}

	// Face-down cards stay private in snapshots too: A's snapshot shows only A.
	sA := h.snapshot(t, res.GameID, aID)
	if sA.YourFaceDown == nil || len(sA.YourFaceDown) != 2 {
		t.Fatalf("A's own face-down cards missing from its snapshot: %+v", sA.YourFaceDown)
	}

	// --- The addressed (human) viewer DOES see options ------------------------
	// New scenario: A coups C (a human with two cards) ⇒ flip choice for C.
	g2, res2 := newFlipChoiceGame(t)
	c2ID, a2ID := res2.Seats[2].ID, res2.Seats[0].ID
	h.waitFor(t, "flip choice pending", func() bool { return phase(g2.snapshot(t, res2.GameID, a2ID).Current) == "single_seat_decision" })

	sC := g2.snapshot(t, res2.GameID, c2ID)
	flat, ok := sC.Current.Decision.Options.([]string)
	if !ok || len(flat) != 2 || flat[0] != "face_down_0" || flat[1] != "face_down_1" {
		t.Fatalf("addressed viewer C must see positional options, got %#v", sC.Current.Decision.Options)
	}
	if opts := g2.snapshot(t, res2.GameID, a2ID).Current.Decision.Options; opts != nil {
		t.Fatal("non-addressed viewer A must NOT see the flip-choice options")
	}
	// The request event itself is private to C.
	if evs := findEvents(g2.stream(t, res2.GameID, a2ID), "card_flip_choice_requested"); len(evs) != 0 {
		t.Fatal("A's stream leaked C's card_flip_choice_requested")
	}
	if evs := findEvents(g2.stream(t, res2.GameID, c2ID), "card_flip_choice_requested"); len(evs) != 1 {
		t.Fatal("C's stream is missing its own card_flip_choice_requested")
	}

	// C answers the decision through the same single endpoint.
	if _, err := g2.choose(t, res2, c2ID, sC.Current.Decision.ID, app.ChoiceIn{FlipChoice: "face_down_0"}); err != nil {
		t.Fatalf("C's flip choice rejected: %v", err)
	}
	g2.waitFor(t, "flip applied", func() bool { return len(findEvents(g2.stream(t, res2.GameID, a2ID), "card_flipped")) == 1 })

	// And an LLM viewer is refused streams and snapshots entirely.
	if _, err := g2.app.PollEvents(context.Background(), res2.GameID, 0, res2.Seats[1].ID, app.DefaultPollHold); errCode(t, err) != "unauthorized" {
		t.Fatalf("LLM poll must be unauthorized: %v", err)
	}
}

// newFlipChoiceGame builds [A human, B llm, C human] where A (first turn,
// banked to ≥7 by the FA pump) has just couped C, leaving a flip-choice
// decision addressed to C (a HUMAN — no driver involvement).
func newFlipChoiceGame(t *testing.T) (*harness, *app.CreateGameResult) {
	t.Helper()
	h, res := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}, {name: "C", typ: "human"}}, 0, false, 500, nil, nil)
	aID, cID := res.Seats[0].ID, res.Seats[2].ID
	h.pumpForeignAid(t, res, aID, 7, 600*time.Millisecond, func(hh *harness, seat string) {
		if seat == cID {
			hh.mustAct(t, res, seat, app.ActionIn{Type: "income"})
		}
	})
	if _, err := h.act(t, res, aID, app.ActionIn{Type: "coup", TargetID: cID}); err != nil {
		t.Fatalf("A's coup rejected: %v", err)
	}
	return h, res
}

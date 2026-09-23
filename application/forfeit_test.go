package application_test

import (
	"context"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestForfeitVoteInitiationRules covers A15's initiation rejections: self
// target, already-inactive target, one-active-vote-per-game — each with the
// normative table-voice message and a private rejection event (A9).
func TestForfeitVoteInitiationRules(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "human"}, {name: "C", typ: "llm"}}
	h, res := createGameAt(t, players, 0, false, 0, nil, nil)
	aID, bID, cID := res.Seats[0].ID, res.Seats[1].ID, res.Seats[2].ID

	// Self target (code + normative message + status).
	_, selfErr := h.initiateVote(t, res.GameID, aID, aID)
	if got := errCode(t, selfErr); got != "self_target" {
		t.Fatalf("self-target code = %q", got)
	}
	ae, _ := app.AsAppError(selfErr)
	if ae.Message != "you cannot target your own seat" || ae.Status != 409 {
		t.Fatalf("self-target: %q status %d", ae.Message, ae.Status)
	}

	// One active vote at a time (A15).
	first, err := h.initiateVote(t, res.GameID, aID, cID)
	if err != nil {
		t.Fatalf("initiate against LLM seat: %v", err)
	}
	if first.MajorityNeeded != 2 || len(first.VoteID) == 0 {
		t.Fatalf("vote open result = %+v (two human voters ⇒ majority 2)", first)
	}
	_, dupErr := h.initiateVote(t, res.GameID, aID, bID)
	if got := errCode(t, dupErr); got != "already_resolved" {
		t.Fatalf("second initiation code = %q", got)
	}
	ae, _ = app.AsAppError(dupErr)
	if ae.Message != "a forfeit vote is already in progress" {
		t.Fatalf("one-vote message = %q", ae.Message)
	}

	// Close the vote by majority so we can test the eliminated target.
	if _, err := h.castVote(t, res.GameID, aID, true); err != nil {
		t.Fatalf("A yes: %v", err)
	}
	out, err := h.castVote(t, res.GameID, bID, true)
	if err != nil {
		t.Fatalf("B yes: %v", err)
	}
	if !out.Closed {
		t.Fatal("two yes votes with majority 2 must close the ballot")
	}

	// Target already eliminated ⇒ target_not_active.
	_, initErr := h.initiateVote(t, res.GameID, aID, cID)
	if got := errCode(t, initErr); got != "target_not_active" {
		t.Fatalf("eliminated-target code = %q", got)
	}
	ae, _ = app.AsAppError(initErr)
	if ae.Message != "you cannot target an eliminated player" {
		t.Fatalf("target message = %q", ae.Message)
	}

	// A9: the semantic rejections sit privately on A's stream.
	rej := findEvents(h.stream(t, res.GameID, aID), "rejection")
	codes := map[string]int{}
	for _, r := range rej {
		codes[r.Payload["code"].(string)]++
	}
	if codes["self_target"] != 1 || codes["already_resolved"] != 1 || codes["target_not_active"] != 1 {
		t.Fatalf("rejection codes on initiator stream = %v", codes)
	}
	if n := len(findEvents(h.stream(t, res.GameID, bID), "rejection")); n != 0 {
		t.Fatal("B's stream contains A's rejection events")
	}
}

// TestForfeitVoteBallotRules covers the §7 ballot: early majority close either
// way, tie exhaustion ⇒ not_forfeited, double-ballot rejection (already
// voted), and casting on a closed/absent vote.
func TestForfeitVoteBallotRules(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "human"}, {name: "C", typ: "human"}, {name: "D", typ: "llm"}}
	h, res := createGameAt(t, players, 0 /* human first ⇒ quiet table */, false, 0, nil, nil)
	aID, bID, cID, dID := res.Seats[0].ID, res.Seats[1].ID, res.Seats[2].ID, res.Seats[3].ID

	// Majority NO: A+B+C eligible, majority 2. Yes+No stalls; the final No
	// exhausts the ballot ⇒ tie-close with not_forfeited (§7).
	v, err := h.initiateVote(t, res.GameID, aID, dID)
	if err != nil || v.MajorityNeeded != 2 {
		t.Fatalf("initiate: %v %+v", err, v)
	}
	if out, _ := h.castVote(t, res.GameID, aID, true); out.Closed {
		t.Fatal("1 yes of majority 2 must not close")
	}
	if out, _ := h.castVote(t, res.GameID, bID, false); out.Closed {
		t.Fatal("1 yes + 1 no must not close")
	}
	out, err := h.castVote(t, res.GameID, cID, false)
	if err != nil {
		t.Fatalf("C's ballot: %v", err)
	}
	if !out.Closed {
		t.Fatal("exhausted ballot (tie 1-2... no-majority) must close")
	}
	resolved := findEvents(h.stream(t, res.GameID, aID), "forfeit_vote_resolved")
	if len(resolved) != 1 || resolved[0].Payload["result"] != "not_forfeited" {
		t.Fatalf("tie-close result = %+v", resolved)
	}
	tally, _ := resolved[0].Payload["tally"].(map[string]any)
	if tally["yes"] != 1 || tally["no"] != 2 || tally["eligible"] != 3 {
		t.Fatalf("tally = %+v", tally)
	}

	// Double ballot from the same voter / cast after close.
	_, closedErr := h.castVote(t, res.GameID, aID, true)
	if got := errCode(t, closedErr); got != "vote_closed" {
		t.Fatalf("cast after close code = %q", got)
	}

	// Majority YES closes early: new vote (previous resolved ⇒ allowed).
	if _, err := h.initiateVote(t, res.GameID, aID, dID); err != nil {
		t.Fatalf("re-initiate after resolution: %v", err)
	}
	if out, _ := h.castVote(t, res.GameID, aID, true); out.Closed {
		t.Fatal("1 yes must not close")
	}
	out, err = h.castVote(t, res.GameID, bID, true)
	if err != nil || !out.Closed {
		t.Fatalf("second yes must close early: closed=%v err=%v", out != nil && out.Closed, err)
	}
	resolved = findEvents(h.stream(t, res.GameID, aID), "forfeit_vote_resolved")
	last := resolved[len(resolved)-1]
	if last.Payload["result"] != "forfeited" {
		t.Fatalf("yes-majority result = %v", last.Payload["result"])
	}
	elim := findEvents(h.stream(t, res.GameID, aID), "eliminated")
	if len(elim) != 1 || elim[0].Payload["seat_id"] != dID || elim[0].Payload["reason"] != "forfeit" {
		t.Fatalf("forfeited elimination = %+v", elim)
	}
	// Forfeited cards stay unknowns (§7): placeholders only, never revealed.
	cards, _ := elim[0].Payload["cards"].([]any)
	for _, c := range cards {
		cm, _ := c.(map[string]any)
		if cm["card"] != "forfeited" {
			t.Fatalf("forfeited card revealed: %+v", cm)
		}
	}
}

// TestForfeitVoteVoidsWhenTargetDiesByPlay (A15): a pending vote whose target
// is eliminated by ordinary play voids with no effect, in the same batch as
// the elimination. The target must be down to its LAST card for a coup to kill
// it outright (a two-card target merely owes a flip choice and survives), so
// C first loses one card to a busted claim.
func TestForfeitVoteVoidsWhenTargetDiesByPlay(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "human"}, {name: "C", typ: "llm"}}
	// C must not hold Duke: the busted-claim penalty that leaves C with one
	// card has to bust deterministically (Ruling 7 — no refusal path).
	h, res := createGameAt(t, players, 0, false, 1000 /* timeout for the pump's FA windows */, nil, dealtNoCharacter(2, domain.Duke))
	aID, bID, cID := res.Seats[0].ID, res.Seats[1].ID, res.Seats[2].ID

	h.pumpForeignAid(t, res, aID, 7, 1500*time.Millisecond, func(hh *harness, seat string) {
		if seat == bID {
			hh.mustAct(t, res, seat, app.ActionIn{Type: "income"})
		}
	})

	if _, err := h.initiateVote(t, res.GameID, aID, cID); err != nil {
		t.Fatalf("initiate vote against C: %v", err)
	}
	// Whittle C to one card: C (LLM) declares a Duke-tax and A busts it — the
	// penalty flips one of C's two cards.
	h.driver.Queue(cID, llm.CandidateStep(app.Candidate{Kind: app.KindTurnAction, Action: "tax", Character: "duke"}))
	h.mustAct(t, res, aID, app.ActionIn{Type: "income"}) // pass the turn A → B → C
	h.mustAct(t, res, bID, app.ActionIn{Type: "income"})
	h.waitFor(t, "C's tax window open", func() bool { return phase(h.snapshot(t, res.GameID, aID).Current) == "window_open" })
	if _, err := h.respond(t, res, aID, app.ResponseIn{Type: "challenge"}, ""); err != nil {
		t.Fatalf("A's challenge rejected: %v", err)
	}
	// C (two cards) owes a penalty FLIP CHOICE — the cascade suspends on it;
	// its response timeout then applies the engine default and leaves C with
	// one card.
	h.cl.Advance(1500 * time.Millisecond)
	h.waitFor(t, "turn back at A", func() bool {
		s := h.snapshot(t, res.GameID, aID)
		return s.Current.Phase == "turn_action" && s.Current.TurnSeatID == aID
	})

	// Coup on C's single remaining card: the flip runs inline and eliminates C
	// by play in this same batch — voiding the pending vote.
	if _, err := h.act(t, res, aID, app.ActionIn{Type: "coup", TargetID: cID}); err != nil {
		t.Fatalf("A's coup: %v", err)
	}

	evts := h.stream(t, res.GameID, aID)
	resolved := findEvents(evts, "forfeit_vote_resolved")
	if len(resolved) != 1 || resolved[0].Payload["result"] != "voided" {
		t.Fatalf("pending vote must void with no effect, got %+v", resolved)
	}
	elim := findEvents(evts, "eliminated")
	if len(elim) != 1 || elim[0].Payload["reason"] != "coup" {
		t.Fatalf("C's elimination must be by the coup, not forfeit: %+v", elim)
	}
	// The void landed AFTER the elimination in the same batch.
	if resolved[0].Seq < elim[0].Seq {
		t.Fatal("voided event must follow the elimination that caused it")
	}
}

// TestForfeitVoteTwoPlayersWin (§7 two-player edge): forfeiting your only
// opponent is simply winning — game_over follows in the same batch.
func TestForfeitVoteTwoPlayersWin(t *testing.T) {
	h, res := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}, 0, false, 0, nil, nil)
	aID, bID := res.Seats[0].ID, res.Seats[1].ID

	v, err := h.initiateVote(t, res.GameID, aID, bID)
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	if v.MajorityNeeded != 1 { // only the initiator is an eligible voter
		t.Fatalf("majority = %d, want 1", v.MajorityNeeded)
	}
	out, err := h.castVote(t, res.GameID, aID, true)
	if err != nil || !out.Closed {
		t.Fatalf("single-yes must close: closed=%v err=%v", out == nil && false, err)
	}

	evts := h.stream(t, res.GameID, aID)
	for _, typ := range []string{"forfeit_vote_resolved", "eliminated", "game_over"} {
		if len(findEvents(evts, typ)) != 1 {
			t.Fatalf("missing %s in same-batch cascade: %+v", typ, findEvents(evts, typ))
		}
	}
	over := findEvents(evts, "game_over")[0]
	if over.Payload["winner_seat_id"] != aID {
		t.Fatalf("winner = %v, want A", over.Payload["winner_seat_id"])
	}
	snap := h.snapshot(t, res.GameID, aID)
	if snap.Status != "finished" || snap.WinnerID == nil || *snap.WinnerID != aID {
		t.Fatalf("snapshot after forfeit win = %+v", snap)
	}
	// Polls afterwards return no new events (A1): the audience-filtered range
	// past the last seq is empty and stays so.
	base := h.lastSeq(t, res.GameID)
	evts, rngErr := h.events.Range(context.Background(), res.GameID, base, aID)
	if rngErr != nil || len(evts) != 0 {
		t.Fatalf("poll on finished game must be empty: %v %+v", rngErr, evts)
	}
}

// TestForfeitVoteAutoNoRespondersMidWindow (§7 at app level): forfeiting the
// ONLY responder of an open window auto-closes it (auto_no_responders); the
// still-alive claimant's unopposed claim then survives and runs, and in a
// two-player game the elimination is also a game_over.
func TestForfeitVoteAutoNoRespondersMidWindow(t *testing.T) {
	h, res := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}, 0 /* A first */, false, 0, nil, nil)
	aID, bID := res.Seats[0].ID, res.Seats[1].ID

	coinsBefore := h.coinsOf(t, res, aID)
	h.mustAct(t, res, aID, app.ActionIn{Type: "tax", Character: "duke"}) // window over {B}
	h.waitFor(t, "window open over {B}", func() bool { return phase(h.snapshot(t, res.GameID, aID).Current) == "window_open" })

	// Mid-window: A forfeits B — the only seat eligible to respond.
	if _, err := h.initiateVote(t, res.GameID, aID, bID); err != nil {
		t.Fatalf("initiate mid-window: %v", err)
	}
	out, err := h.castVote(t, res.GameID, aID, true) // only eligible voter; majority 1
	if err != nil || !out.Closed {
		t.Fatalf("vote must close: closed=%v err=%v", out != nil && out.Closed, err)
	}

	evts := h.stream(t, res.GameID, aID)
	closed := findEvents(evts, "window_closed")
	if len(closed) != 1 || closed[0].Payload["reason"] != "auto_no_responders" {
		t.Fatalf("window must auto-close with no responders: %+v", closed)
	}
	// The claimant survived the forfeit wave, so the unopposed tax proceeds.
	var gotTotal int
	for _, e := range findEvents(evts, "coins_changed") {
		if e.Payload["seat_id"] == aID && e.Payload["reason"] == "tax" {
			gotTotal, _ = e.Payload["total"].(int)
		}
	}
	if gotTotal != coinsBefore+3 {
		t.Fatalf("unopposed tax must proceed after auto-close: total=%d want %d", gotTotal, coinsBefore+3)
	}
	elim := findEvents(evts, "eliminated")
	if len(elim) != 1 || elim[0].Payload["reason"] != "forfeit" {
		t.Fatalf("B must be eliminated by the forfeit: %+v", elim)
	}
	over := findEvents(evts, "game_over")
	if len(over) != 1 || over[0].Payload["winner_seat_id"] != aID {
		t.Fatalf("game_over missing/wrong winner: %+v", over)
	}
}

// TestForfeitVoteNonVoterRejection: a voter eliminated by play while the ballot
// is pending can no longer cast (not_a_voter); the remaining voters still
// decide the ballot. C must be down to its last card first, so a busted-claim
// penalty precedes the coup (a two-card target would merely owe a flip choice).
func TestForfeitVoteNonVoterRejection(t *testing.T) {
	h, res := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "human"}, {name: "C", typ: "human"}, {name: "D", typ: "llm"}}, 0, false, 1000, nil, dealtNoCharacter(2, domain.Duke))
	aID, bID, cID, dID := res.Seats[0].ID, res.Seats[1].ID, res.Seats[2].ID, res.Seats[3].ID

	h.pumpForeignAid(t, res, aID, 7, 1500*time.Millisecond, func(hh *harness, seat string) {
		switch seat {
		case bID, cID:
			hh.mustAct(t, res, seat, app.ActionIn{Type: "income"})
		}
	})

	if _, err := h.initiateVote(t, res.GameID, aID, dID); err != nil { // voters {A,B,C}, majority 2
		t.Fatalf("initiate: %v", err)
	}
	// Bust voter C's claim (C deals no Duke): C is left with one card. D's next
	// two queue steps serve its window pass and its subsequent turn income.
	h.driver.Queue(dID, llm.CandidateStep(app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
	h.driver.Queue(dID, llm.CandidateStep(app.Candidate{Kind: app.KindTurnAction, Action: "income"}))
	h.mustAct(t, res, aID, app.ActionIn{Type: "income"}) // A → B
	h.mustAct(t, res, bID, app.ActionIn{Type: "income"}) // B → C
	if _, err := h.act(t, res, cID, app.ActionIn{Type: "tax", Character: "duke"}); err != nil {
		t.Fatalf("C's tax: %v", err)
	}
	h.waitFor(t, "C's tax window open", func() bool { return phase(h.snapshot(t, res.GameID, aID).Current) == "window_open" })
	if _, err := h.respond(t, res, aID, app.ResponseIn{Type: "challenge"}, ""); err != nil {
		t.Fatalf("A's challenge: %v", err)
	}
	// C (two cards, human — no driver) owes a penalty FLIP CHOICE; its
	// response timeout applies the engine default, then D takes its turn.
	h.cl.Advance(1500 * time.Millisecond)
	h.waitFor(t, "turn back at A (D incomes in between)", func() bool {
		s := h.snapshot(t, res.GameID, aID)
		return s.Current.Phase == "turn_action" && s.Current.TurnSeatID == aID
	})

	// Coup on C's single remaining card eliminates the voter by play.
	if _, err := h.act(t, res, aID, app.ActionIn{Type: "coup", TargetID: cID}); err != nil {
		t.Fatalf("coup on C: %v", err)
	}
	h.waitFor(t, "C eliminated by play", func() bool {
		for _, e := range h.stream(t, res.GameID, aID) {
			if e.Type == "eliminated" && e.Payload["seat_id"] == cID {
				return true
			}
		}
		return false
	})
	_, noVoterErr := h.castVote(t, res.GameID, cID, true)
	if got := errCode(t, noVoterErr); got != "not_a_voter" {
		t.Fatalf("eliminated voter cast code = %q (err=%v)", got, noVoterErr)
	}
	if _, err := h.castVote(t, res.GameID, aID, true); err != nil {
		t.Fatalf("A's ballot: %v", err)
	}
	out, err := h.castVote(t, res.GameID, bID, true) // majority of the original three ⇒ forfeited
	if err != nil || !out.Closed {
		t.Fatalf("ballot should still be decidable: closed=%v err=%v", out == nil && false, err)
	}
}

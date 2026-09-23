package domain

import (
	"fmt"
	"strings"
	"testing"
)

// These tests are the one-to-one mapping of the normative edge-case list
// (spec §15 / test plan §14.1). The shared invariant checker runs after every
// transition automatically (batchWith → CheckInvariants), so any violation
// panics immediately instead of being asserted at the end.

const msgMustCoup = "you must take a coup while you have ten or more coins"

// --- §15.1 / 2: busted Contessa bluff ----------------------------------------

func TestBustedContessaBluff_TwoCards_EliminatedInOneSequence(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	// A attacks, T bluffs (holds NO contessa), C busts the bluff.
	setHands(t, g, [][]Character{
		{Assassin, Captain}, // A — holds the assassin, but nobody challenges A
		{Duke, Ambassador},  // T — two cards, no contessa
		{Contessa, Duke},    // C — the challenger
	})
	g.Seats[0].Coins = 5 // 3 for the sunk assassination + 2 left
	g.TurnSeatIndex = 0

	evs := mustAct(t, func() ([]Event, error) {
		return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Assassinate, Character: Assassin, TargetID: g.Seats[1].ID})
	})
	if len(findEvents(evs, "coins_changed")) != 1 || findEvents(evs, "coins_changed")[0].Payload["delta"] != -3 {
		t.Fatalf("sunk cost at declaration missing: %v", eventTypes(evs))
	}
	// T counterclaims Contessa; the nested window replaces the attack window.
	evs2 := mustAct(t, func() ([]Event, error) {
		return g.SubmitWindowResponse(g.Seats[1].ID, WindowResponse{Kind: RespCounterclaim, Character: Contessa})
	})
	if len(findEvents(evs2, "window_closed")) != 1 || len(findEvents(evs2, "window_opened")) != 1 {
		t.Fatalf("counterclaim must close W1 and open W2 in one batch: %v", eventTypes(evs2))
	}
	if g.window == nil || g.window.Kind != WindowCounterclaimChallenge || g.window.ClaimantID != g.Seats[1].ID {
		t.Fatalf("nested window not innermost: %+v", g.window)
	}
	// C challenges the bluff. T does NOT hold contessa → claim_failed, penalty
	// to T. T has two cards → flip-choice decision suspends the cascade.
	evs3 := mustAct(t, func() ([]Event, error) {
		return g.SubmitWindowResponse(g.Seats[2].ID, WindowResponse{Kind: RespChallenge})
	})
	if len(findEvents(evs3, "claim_failed")) != 1 {
		t.Fatalf("claim_failed missing: %v", eventTypes(evs3))
	}
	if len(findEvents(evs3, "action_fizzled")) != 0 {
		t.Fatal("a busted offensive block never emits action_fizzled (the attack is still pending)")
	}
	if g.window != nil || g.pending == nil || g.pending.SeatID != g.Seats[1].ID {
		t.Fatalf("cascade must be suspended on T's flip choice: window=%v pending=%v", g.window, g.pending)
	}

	// T picks which card flips; the assassination then proceeds on what remains.
	evs4 := mustAct(t, func() ([]Event, error) {
		return g.AnswerDecision(g.Seats[1].ID, g.pending.ID, Choice{FlipOption: "face_down_0"})
	})
	cascade := append(append([]Event{}, evs3...), evs4...)
	tTypes := eventTypes(cascade)
	// Exactly two influence losses for T in this single sequence.
	flipsT := 0
	for _, e := range cascade {
		if e.Type == "card_flipped" && e.Payload["seat_id"] == g.Seats[1].ID {
			flipsT++
		}
	}
	if flipsT != 2 {
		t.Fatalf("defender flipped %d cards, want 2 (penalty + proceeding attack): %v", flipsT, tTypes)
	}
	if !g.Seats[1].Eliminated() || len(g.Seats[1].FaceUp) != 2 {
		t.Fatalf("defender not eliminated with both cards face-up: %+v", g.Seats[1])
	}
	if len(findEvents(append(evs3, evs4...), "eliminated")) != 1 {
		t.Fatal("eliminated event missing")
	}
	if g.Seats[0].Coins != 2 {
		t.Fatalf("attacker coins = %d, want 2 (the 3-coin assassination cost is sunk)", g.Seats[0].Coins)
	}

	// Exactly one window ever open at a time: two windows total across the
	// whole sequence (W1 replaced by W2), none opened after settlement.
	all := append(append(evs, evs2...), append(evs3, evs4...)...)
	if got := len(findEvents(all, "window_opened")); got != 2 {
		t.Fatalf("window_opened count = %d, want 2 (no re-opened windows)", got)
	}
	openeds := findEvents(all, "window_opened")
	lastOpenSeq := max(openeds[0].Seq, openeds[1].Seq)
	firstSettleSeq := findEvents(all, "claim_failed")[0].Seq
	if lastOpenSeq > firstSettleSeq {
		t.Fatal("a window opened after the cascade settled — a superseded claim reopened")
	}
	if g.window != nil || g.pending != nil {
		t.Fatalf("no structure may remain open: %v", eventTypes(all))
	}
	// Turn advanced once, after everything.
	if len(findEvents(evs4, "turn_start")) != 1 {
		t.Fatalf("turn must advance once at the end of the sequence: %v", tTypes)
	}
}

func TestBustedContessaBluff_OneCard_PenaltyEliminates_AssassinationFizzles(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{
		{Assassin, Captain}, // A
		{Duke, Ambassador},  // T — only ONE face-down card (lost one earlier)
		{Contessa, Duke},    // C
	})
	dropToFaceUp(t, g, 1)
	g.Seats[0].Coins = 5
	g.TurnSeatIndex = 0

	mustAct(t, func() ([]Event, error) {
		return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Assassinate, Character: Assassin, TargetID: "s1"})
	})
	mustAct(t, func() ([]Event, error) {
		return g.SubmitWindowResponse(g.Seats[1].ID, WindowResponse{Kind: RespCounterclaim, Character: Contessa})
	})
	evs3 := mustAct(t, func() ([]Event, error) {
		return g.SubmitWindowResponse(g.Seats[2].ID, WindowResponse{Kind: RespChallenge})
	})

	// T's single face-down card IS the penalty flip: eliminated by the penalty
	// alone; there is no second card for the attack to hit.
	if len(findEvents(evs3, "card_flipped")) != 1 {
		t.Fatalf("exactly one flip (the penalty) must exist: %v", eventTypes(evs3))
	}
	elim := findEvents(evs3, "eliminated")
	if len(elim) != 1 || elim[0].Payload["reason"] != "penalty" {
		t.Fatalf("defender must be eliminated BY THE PENALTY: %v", elim)
	}
	fizzled := findEvents(evs3, "action_fizzled")
	if len(fizzled) != 1 || fizzled[0].Payload["reason"] != FizzleTargetEliminated {
		t.Fatalf("assassination must fizzle with target_eliminated: %v", fizzled)
	}
	if g.Seats[0].Coins != 2 {
		t.Fatalf("attacker coins = %d, want 2 (3 sunk)", g.Seats[0].Coins)
	}
	if g.window != nil || g.pending != nil {
		t.Fatal("cascade must complete synchronously: no structure left open")
	}
	if len(findEvents(evs3, "turn_start")) != 1 {
		t.Fatalf("turn must advance after the fizzle: %v", eventTypes(evs3))
	}
}

// --- §15.3: mandatory Coup ----------------------------------------------------

func coupRejectionCases(t *testing.T, coins int) []Action {
	t.Helper()
	b := "s1" // steal/assassin target (has 2 starting coins)
	return []Action{
		{Kind: Income},
		{Kind: ForeignAid},
		{Kind: Tax, Character: Duke},
		{Kind: Steal, Character: Captain, TargetID: b},
		{Kind: Assassinate, Character: Assassin, TargetID: b},
		{Kind: Exchange, Character: Ambassador},
	}
}

func runMandatoryCoup(t *testing.T, coins int, wantMustCoup bool) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	dropToFaceUp(t, g, 1) // victim auto-flips: keeps the coup path synchronous
	g.Seats[0].Coins = coins
	g.TurnSeatIndex = 0

	for _, a := range coupRejectionCases(t, coins) {
		_, err := g.DeclareTurnAction(g.Seats[0].ID, a)
		if wantMustCoup {
			if err == nil {
				t.Fatalf("coins=%d: action %v accepted, want must-coup rejection", coins, a.Kind)
			}
			mustLegacy(t, err, CodeMustCoup, msgMustCoup)
		} else if err != nil {
			t.Fatalf("coins=%d: action %v rejected: %v (must be free choice below 10)", coins, a.Kind, err)
		}
		g = newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM}) // fresh game per attempt
		setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
		dropToFaceUp(t, g, 1)
		g.Seats[0].Coins = coins
		g.TurnSeatIndex = 0
	}

	if wantMustCoup {
		evs := mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Coup, TargetID: "s1"}) })
		if len(findEvents(evs, "coins_changed")) != 1 || findEvents(evs, "coins_changed")[0].Payload["delta"] != -7 {
			t.Fatalf("coup must pay 7 at declaration: %v", eventTypes(evs))
		}
	} else {
		if _, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Income}); err != nil {
			t.Fatalf("income at %d coins must be legal: %v", coins, err)
		}
	}
}

func TestMandatoryCoup_AtExactlyTen(t *testing.T) { runMandatoryCoup(t, 10, true) }
func TestMandatoryCoup_AtEleven(t *testing.T)     { runMandatoryCoup(t, 11, true) }
func TestMandatoryCoup_NotAtNine(t *testing.T)    { runMandatoryCoup(t, 9, false) }

// --- §15.4: Steal target coin counts ------------------------------------------

func TestSteal_ZeroCoins_InvalidTarget(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	g.Seats[1].Coins = 0
	g.TurnSeatIndex = 0

	_, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Steal, Character: Captain, TargetID: "s1"})
	mustLegacy(t, err, CodeNoCoinsToSteal, "that player has no coins to steal from")
}

func TestSteal_OneCoin_TakesOne(t *testing.T) {
	run := func(targetCoins int, wantTaken int) {
		t.Run(fmt.Sprintf("target_%d", targetCoins), func(t *testing.T) {
			g := newTestGame(t, WindowSettings{RequireAllResponses: true}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
			setHands(t, g, [][]Character{
				{Captain, Duke}, // A — really holds the captain
				{Assassin, Contessa},
				{Ambassador, Duke},
			})
			g.Seats[1].Coins = targetCoins
			g.TurnSeatIndex = 0

			mustAct(t, func() ([]Event, error) {
				return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Steal, Character: Captain, TargetID: "s1"})
			})
			evs := passAll(t, g) // claim survives unchallenged

			given := findEvents(evs, "coins_changed")
			taken := 0
			for _, e := range given {
				if e.Payload["reason"] == CoinsStealTook {
					taken = e.Payload["delta"].(int)
				}
			}
			if taken != wantTaken {
				t.Fatalf("took %d coins, want %d (min(2, target))", taken, wantTaken)
			}
			if g.Seats[1].Coins != targetCoins-wantTaken || g.Seats[0].Coins != 2+wantTaken {
				t.Fatalf("coin counts = %d/%d, want %d/%d", g.Seats[0].Coins, g.Seats[1].Coins, 2+wantTaken, targetCoins-wantTaken)
			}
		})
	}
	run(1, 1) // §15.4: one-coin target yields exactly min(2,1)=1
	run(5, 2) // capped at 2
}

// Also covers the §4.5 eliminated/missing-target messages for steal & coup.
func TestTargetValidation_Messages(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}, {Ambassador, Duke}})
	g.TurnSeatIndex = 0    // turn held by an active seat while s1 is eliminated below
	eliminateSeat(t, g, 1) // s1 is eliminated: target validation must reject it
	g.Seats[0].Coins = 9

	cases := []struct {
		a       Action
		code    string
		message string
	}{
		{Action{Kind: Steal, Character: Captain, TargetID: "s1"}, CodeStealTargetGone, "that player is eliminated"},
		{Action{Kind: Coup, TargetID: "s1"}, CodeTargetEliminated, "you cannot target an eliminated player"},
		{Action{Kind: Assassinate, Character: Assassin, TargetID: "s1"}, CodeTargetEliminated, "you cannot target an eliminated player"},
		{Action{Kind: Coup, TargetID: "nobody"}, CodeNoSuchSeat, "no such seat"},
		{Action{Kind: Steal, Character: Captain, TargetID: "s0"}, CodeSelfTarget, "you cannot target your own seat"},
		{Action{Kind: Tax}, CodeMissingClaim, "that action requires a claimed character"},
		{Action{Kind: Tax, Character: Assassin}, CodeInvalidClaim, "you cannot claim that character for that action"},
	}
	for _, c := range cases {
		if _, err := g.DeclareTurnAction(g.Seats[0].ID, c.a); err == nil {
			t.Fatalf("%+v accepted, want rejection", c.a)
		} else {
			mustLegacy(t, err, c.code, c.message)
		}
	}
}

// --- §15.5: first non-PASS closes for all --------------------------------------

func TestWindow_FirstNonPass_ClosesForAll_SeatsCancelled_LateRejected(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{
		{Captain, Ambassador}, // A claims tax but holds NO duke
		{Duke, Contessa},      // B — challenger
		{Assassin, Duke},      // C — would-be late responder
	})
	g.TurnSeatIndex = 0

	mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Tax, Character: Duke}) })
	evs := mustAct(t, func() ([]Event, error) {
		return g.SubmitWindowResponse(g.Seats[1].ID, WindowResponse{Kind: RespChallenge})
	})

	// Window closed by B's challenge; decider recorded on window_closed.
	closed := findEvents(evs, "window_closed")
	if len(closed) != 1 || closed[0].Payload["decider_seat_id"] != g.Seats[1].ID || closed[0].Payload["reason"] != "first_response" {
		t.Fatalf("window_closed = %v, want decider B / first_response", closed)
	}

	// Late/stale responses from ANY client are rejected and discarded (§5.2.6):
	// the game is unchanged and a private rejection event can be issued.
	for _, late := range []struct {
		seat string
		r    WindowResponse
	}{
		{g.Seats[2].ID, WindowResponse{Kind: RespChallenge}},
		{g.Seats[2].ID, WindowResponse{Kind: RespPass}},
		{g.Seats[1].ID, WindowResponse{Kind: RespPass}}, // decider trying twice
	} {
		_, err := g.SubmitWindowResponse(late.seat, late.r)
		mustLegacy(t, err, CodeWindowResolved, "the window has already been resolved")
		rej := g.MakeRejectionEvent(late.seat, CodeWindowResolved, err.Error())
		if rej.Audience.Public || rej.Audience.SeatID != late.seat {
			t.Fatalf("rejection event not private to offending seat: %+v", rej)
		}
	}

	// No window may reopen for the settled claim (the cascade here suspends on
	// A's penalty flip choice — g.pending holds it, no window exists).
	if g.window != nil {
		t.Fatal("settled claim re-opened a window")
	}

	// Per-seat once-ness while a window is still OPEN (A16).
	g2 := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g2, [][]Character{{Duke, Captain}, {Assassin, Contessa}, {Ambassador, Duke}})
	g2.TurnSeatIndex = 0
	mustAct(t, func() ([]Event, error) {
		return g2.DeclareTurnAction(g2.Seats[0].ID, Action{Kind: Tax, Character: Duke})
	})
	if _, err := g2.SubmitWindowResponse(g2.Seats[1].ID, WindowResponse{Kind: RespPass}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if _, err := g2.SubmitWindowResponse(g2.Seats[1].ID, WindowResponse{Kind: RespPass}); err == nil {
		t.Fatal("second response by the same seat accepted while window open")
	} else {
		mustLegacy(t, err, CodeAlreadyResponded, "you have already responded")
	}
	if g2.window == nil {
		t.Fatal("passes must not close a require-all=false window by themselves")
	}
}

// --- §15.6: nested block-challenge cascade, no re-opened windows ---------------

func TestNestedBlockChallengeCascade_NoReopen(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{
		{Assassin, Captain},    // A attacks (holds captain — never challenged here)
		{Ambassador, Duke},     // B blocks with ambassador (really holds it)
		{Contessa, Ambassador}, // C busts nothing; just challenges the block
		{Duke, Contessa},       // D
	})
	g.Seats[0].Coins = 5
	g.TurnSeatIndex = 0

	evs := mustAct(t, func() ([]Event, error) {
		return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Steal, Character: Captain, TargetID: "s1"})
	})
	w1 := findEvents(evs, "window_opened")
	if len(w1) != 1 || w1[0].Payload["kind"] != "challenge" {
		t.Fatalf("attack window wrong: %v", w1)
	}

	evs2 := mustAct(t, func() ([]Event, error) {
		return g.SubmitWindowResponse(g.Seats[1].ID, WindowResponse{Kind: RespCounterclaim, Character: Ambassador})
	})
	w2 := findEvents(evs2, "window_opened")
	if len(w2) != 1 || w2[0].Payload["kind"] != "counterclaim_challenge" {
		t.Fatalf("nested window wrong: %v", w2)
	}
	if g.window == nil || g.window.ID != w2[0].Payload["window_id"] {
		t.Fatal("the nested window must be the (only) innermost window")
	}

	dropToFaceUp(t, g, 2) // C has one card → its penalty auto-resolves: no suspension
	evs3 := mustAct(t, func() ([]Event, error) {
		return g.SubmitWindowResponse(g.Seats[2].ID, WindowResponse{Kind: RespChallenge})
	})
	if len(findEvents(evs3, "claim_succeeded")) != 1 {
		t.Fatalf("block claim must succeed (B holds ambassador): %v", eventTypes(evs3))
	}
	fizzled := findEvents(evs3, "action_fizzled")
	if len(fizzled) != 1 || fizzled[0].Payload["reason"] != FizzleCounterclaimSuccess {
		t.Fatalf("original steal must fizzle with counterclaim_success: %v", fizzled)
	}

	all := append(append(evs, evs2...), evs3...)
	if got := len(findEvents(all, "window_opened")); got != 2 {
		t.Fatalf("window_opened count = %d, want exactly 2 (never re-opened)", got)
	}
	lastOpenSeq := findEvents(all, "window_opened")[1].Seq
	for _, e := range all {
		if e.Seq > lastOpenSeq && (e.Type == "window_opened" || e.Type == "card_flip_choice_requested") {
			t.Fatalf("event %s after cascade opened something", e.Type)
		}
	}
	if g.window != nil || g.pending != nil {
		t.Fatal("no window or decision may remain open")
	}
	if len(findEvents(evs3, "turn_start")) != 1 {
		t.Fatalf("turn must advance exactly once at the end: %v", eventTypes(evs3))
	}
	// B revealed the ambassador: it left the hand, was shuffled back into the
	// Court Deck (net-zero), and a replacement drew in — so B still holds two
	// face-down cards, gained no face-up card, and the deck contains the card.
	if len(g.Seats[1].FaceDown) != 2 || len(g.Seats[1].FaceUp) != 0 {
		t.Fatalf("blocker hand after successful reveal = %+v / %+v, want 2 face-down, none face-up", g.Seats[1].FaceDown, g.Seats[1].FaceUp)
	}
	// B's revealed card was Card{ambassador, copy 0} (first-match identity):
	// it must have been shuffled back into the Court Deck.
	if !deckContains(g, Card{Character: Ambassador, Copy: 0}) {
		t.Fatal("the revealed ambassador card must be back in the Court Deck")
	}
	if !g.Seats[2].Eliminated() || len(g.Seats[2].FaceUp) != 2 || len(g.Seats[2].FaceDown) != 0 {
		t.Fatalf("challenger C should be eliminated with both cards face-up: %+v", g.Seats[2])
	}
	// The steal never took effect: coins unchanged.
	if g.Seats[0].Coins != 5 || g.Seats[1].Coins != 2 {
		t.Fatalf("coins moved despite a successful block: %d/%d", g.Seats[0].Coins, g.Seats[1].Coins)
	}
}

// --- §15.7: elimination publishes both cards ------------------------------------

func TestElimination_PublishesBothCards(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	dropToFaceUp(t, g, 1) // victim already lost one: contessa is face-up (public)
	g.Seats[0].Coins = 9
	g.TurnSeatIndex = 0

	// The Coup flips the last face-down card (assassin) — the eliminated event
	// must publish BOTH cards publicly, in their public order.
	evs := mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Coup, TargetID: "s1"}) })

	elim := findEvents(evs, "eliminated")
	if len(elim) != 1 {
		t.Fatalf("eliminated event missing: %v", eventTypes(evs))
	}
	if !elim[0].Audience.Public || elim[0].Payload["seat_id"] != g.Seats[1].ID || elim[0].Payload["reason"] != "coup" {
		t.Fatalf("eliminated event wrong: %+v", elim[0])
	}
	cards, ok := elim[0].Payload["cards"].([]map[string]any)
	if !ok || len(cards) != 2 {
		t.Fatalf("eliminated cards = %v, want both public cards", elim[0].Payload["cards"])
	}
	got := []string{cards[0]["card"].(string), cards[1]["card"].(string)}
	want := []string{"contessa", "assassin"} // face-up order: previously flipped first
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("eliminated cards = %v, want %v (full public list)", got, want)
	}
	v := g.Seats[1]
	if len(v.FaceUp) != 2 || v.FaceUp[0].Character != Contessa || v.FaceUp[1].Character != Assassin {
		t.Fatalf("face-up lists inconsistent after elimination: %+v", v.FaceUp)
	}
	// Public stream replays perfectly: the flip event carries the same card.
	if f := findEvents(evs, "card_flipped"); len(f) != 1 || f[0].Payload["card"] != "assassin" {
		t.Fatalf("card_flipped inconsistent with eliminated: %v", f)
	}
}

// --- §15.8: exchange at minimum deck size ---------------------------------------

func TestExchange_MinDeckSize_ThreeCards(t *testing.T) {
	var types []SeatType
	for i := 0; i < 6; i++ {
		if i == 0 {
			types = append(types, SeatHuman)
		} else {
			types = append(types, SeatLLM)
		}
	}
	g := newTestGame(t, WindowSettings{RequireAllResponses: true}, types)
	setHands(t, g, [][]Character{
		{Ambassador, Duke}, // A exchanges
		{Assassin, Captain},
		{Contessa, Ambassador},
		{Duke, Assassin},
		{Captain, Contessa},
		{Ambassador, Captain},
	})
	if g.Deck.Size() != 3 {
		t.Fatalf("6-player deck = %d, want 3 (minimum)", g.Deck.Size())
	}
	g.TurnSeatIndex = 0

	evs := mustAct(t, func() ([]Event, error) {
		return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Exchange, Character: Ambassador})
	})
	if len(findEvents(evs, "window_opened")) != 1 {
		t.Fatalf("exchange at deck size 3 must be legal (always is): %v", eventTypes(evs))
	}
	passAll(t, g)

	// While the finish decision is pending the two drawn cards are out of the
	// deck: it may hold down to ONE card but NEVER zero.
	if g.pending == nil || g.pending.Kind != DecisionExchangeFinish {
		t.Fatalf("exchange must suspend on the finish decision: %v", g.Phase())
	}
	if g.Deck.Size() < 1 {
		t.Fatalf("deck hit %d — exhaustion is impossible (§3.2.1)", g.Deck.Size())
	}
	evs2 := answerDefault(t, g) // no-swap: both drawn cards return
	if len(findEvents(evs2, "exchange_completed")) != 1 {
		t.Fatalf("exchange must complete: %v", eventTypes(evs2))
	}
	if g.Deck.Size() != 3 {
		t.Fatalf("deck after exchange = %d, want 3 (net-zero)", g.Deck.Size())
	}
}

// --- §15.9: forfeit vote resolves a stuck LLM seat mid-window -------------------

func TestForfeitVote_StuckLLM_MidWindow(t *testing.T) {
	t.Run("auto_no_responders_when_only_responder_forfeited", func(t *testing.T) {
		g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
		setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
		g.TurnSeatIndex = 0
		mustAct(t, func() ([]Event, error) {
			return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Steal, Character: Captain, TargetID: "s1"})
		})
		if len(g.window.Eligible) != 1 || g.window.Eligible[0] != "s1" {
			t.Fatalf("window eligible = %v, want [s1] (the stuck LLM)", g.window.Eligible)
		}

		// H human votes the stuck LLM seat out while its window is still open.
		evsV := mustAct(t, func() ([]Event, error) { return g.InitiateForfeitVote("s0", "s1") })
		if len(findEvents(evsV, "forfeit_vote_opened")) != 1 {
			t.Fatalf("vote must open: %v", eventTypes(evsV))
		}
		evsC := mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s0", true) }) // sole voter: majority 1

		if len(findEvents(evsC, "forfeit_vote_resolved")) != 1 || findEvents(evsC, "forfeit_vote_resolved")[0].Payload["result"] != VoteResultForfeited {
			t.Fatalf("vote must resolve forfeited: %v", eventTypes(evsC))
		}
		closed := findEvents(evsC, "window_closed")
		if len(closed) != 1 || closed[0].Payload["reason"] != "auto_no_responders" {
			t.Fatalf("window must auto-close with auto_no_responders: %v", closed)
		}
		fizzled := findEvents(evsC, "action_fizzled")
		if len(fizzled) != 1 || fizzled[0].Payload["reason"] != FizzleTargetForfeited {
			t.Fatalf("attack must fizzle with target_forfeited: %v", fizzled)
		}
		goEv := findEvents(evsC, "game_over")
		if len(goEv) != 1 || goEv[0].Payload["winner_seat_id"] != "s0" {
			t.Fatalf("game must continue to game_over for the survivor: %v", eventTypes(evsC))
		}
	})

	t.Run("rescopes_when_other_responders_remain", func(t *testing.T) {
		g := newTestGame(t, WindowSettings{RequireAllResponses: true}, []SeatType{SeatHuman, SeatLLM, SeatHuman, SeatHuman})
		setHands(t, g, [][]Character{
			{Duke, Captain},      // H1 initiator
			{Assassin, Contessa}, // L1 the stuck LLM (target of the steal)
			{Ambassador, Duke},   // H2 actor
			{Contessa, Captain},  // H3
		})
		g.TurnSeatIndex = 2
		mustAct(t, func() ([]Event, error) {
			return g.DeclareTurnAction(g.Seats[2].ID, Action{Kind: Steal, Character: Captain, TargetID: "s1"})
		})

		evsV := mustAct(t, func() ([]Event, error) { return g.InitiateForfeitVote("s0", "s1") })
		vv := findEvents(evsV, "forfeit_vote_opened")[0]
		if vv.Payload["majority_needed"] != 2 {
			t.Fatalf("majority = %v, want 2 of 3 eligible voters", vv.Payload["majority_needed"])
		}
		mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s0", true) })
		evsC := mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s2", true) }) // 2nd yes → early majority close

		if len(findEvents(evsC, "forfeit_vote_resolved")) != 1 {
			t.Fatalf("vote must resolve on the second yes: %v", eventTypes(evsC))
		}
		// The window survives with a re-scoped responder set (L1 removed).
		if g.window == nil {
			t.Fatal("window must still be open after the forfeit")
		}
		got := strings.Join(g.window.Eligible, ",")
		if got != "s0,s3" {
			t.Fatalf("rescoped eligible = %q, want s0,s3", got)
		}
		evsP := passAll(t, g) // remaining seats pass → require-all closes it
		assertClosedReason(t, evsP, "all_responded")
		if len(findEvents(evsP, "action_fizzled")) != 1 || findEvents(evsP, "action_fizzled")[0].Payload["reason"] != FizzleTargetForfeited {
			t.Fatalf("steal must fizzle (target forfeited): %v", eventTypes(evsP))
		}
		if g.Status == Finished {
			t.Fatal("3 active seats remain: no game over")
		}
	})

	t.Run("require_all_closes_when_rescope_leaves_only_responded_seats", func(t *testing.T) {
		g := newTestGame(t, WindowSettings{RequireAllResponses: true}, []SeatType{SeatHuman, SeatLLM, SeatHuman})
		setHands(t, g, [][]Character{
			{Duke, Captain},      // H1
			{Assassin, Contessa}, // L the stuck LLM (target of the steal)
			{Ambassador, Duke},   // H2 actor
		})
		g.TurnSeatIndex = 2
		mustAct(t, func() ([]Event, error) {
			return g.DeclareTurnAction(g.Seats[2].ID, Action{Kind: Steal, Character: Captain, TargetID: "s1"})
		})
		// H1 passes first; the window waits on the stuck LLM.
		mustAct(t, func() ([]Event, error) { return g.SubmitWindowResponse("s0", WindowResponse{Kind: RespPass}) })
		if g.window == nil {
			t.Fatal("window must still be open (LLM has not responded)")
		}

		// Vote out the stuck LLM: the rescoped window is left with only H1, who
		// already responded → require-all closes it immediately (no stall).
		mustAct(t, func() ([]Event, error) { return g.InitiateForfeitVote("s0", "s1") })
		mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s0", true) })
		evsC := mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s2", true) })

		closed := findEvents(evsC, "window_closed")
		if len(closed) != 1 || closed[0].Payload["reason"] != "all_responded" {
			t.Fatalf("rescoped window must close with all_responded: %v", eventTypes(evsC))
		}
		if g.window != nil || g.pending != nil {
			t.Fatal("no structure may remain open")
		}
		fizzled := findEvents(evsC, "action_fizzled")
		if len(fizzled) != 1 || fizzled[0].Payload["reason"] != FizzleTargetForfeited {
			t.Fatalf("steal must fizzle (target forfeited): %v", eventTypes(evsC))
		}
	})
}

// --- §15.10: two-player forfeit = win --------------------------------------------

func TestForfeitVote_TwoPlayers_Win(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	g.TurnSeatIndex = 0

	mustAct(t, func() ([]Event, error) { return g.InitiateForfeitVote("s0", "s1") })
	evs := mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s0", true) })

	if len(findEvents(evs, "forfeit_vote_resolved")) != 1 || findEvents(evs, "forfeit_vote_resolved")[0].Payload["result"] != VoteResultForfeited {
		t.Fatalf("vote result: %v", eventTypes(evs))
	}
	elim := findEvents(evs, "eliminated")
	if len(elim) != 1 || elim[0].Payload["reason"] != "forfeit" {
		t.Fatalf("forfeit elimination missing: %v", elim)
	}
	cards := elim[0].Payload["cards"].([]map[string]any)
	for _, c := range cards {
		if c["card"] != "forfeited" {
			t.Fatalf("unrevealed forfeit cards must stay unknowns, got %v", c["card"])
		}
	}
	if len(g.Seats[1].FaceDown) != 2 {
		t.Fatal("forfeited cards must remain in the seat state for belief tracking (deck-composition reasoning)")
	}
	goEv := findEvents(evs, "game_over")
	if len(goEv) != 1 || goEv[0].Payload["winner_seat_id"] != "s0" {
		t.Fatalf("two-player forfeit is a win: %v", eventTypes(evs))
	}
	if g.Status != Finished || g.WinnerID != "s0" {
		t.Fatal("game must be finished with the initiator as winner")
	}
}

// --- §15.11: coins unbounded, no treasury cap --------------------------------------

// A seat holding more coins than the physical 50-coin tray is legal and every
// movement at that level must work with exact counts: the engine enforces no
// treasury cap and no supply limit (§3.2.5). (Mandatory Coup keeps such a
// seat draining by 7 per own turn — the point here is that NOTHING clamps or
// rejects these high-coin states, and the accounting stays exact.)
func TestCoins_Unbounded_NoTreasuryCap(t *testing.T) {
	g := newTestGame(t, WindowSettings{RequireAllResponses: true},
		[]SeatType{SeatHuman, SeatLLM, SeatLLM, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{
		{Captain, Duke},
		{Assassin, Contessa},
		{Ambassador, Captain},
		{Duke, Assassin},
		{Contessa, Ambassador},
	})
	for i := 1; i < len(g.Seats); i++ {
		dropToFaceUp(t, g, i) // auto-flips keep every coup synchronous
	}
	g.Seats[0].Coins = 61 // above the physical tray size: no cap may clamp this
	g.TurnSeatIndex = 0

	initialTotal := 0
	for _, s := range g.Seats {
		initialTotal += s.Coins
	}
	incomes := 0
	coupons := 0      // each pays 7 out of the tracked total (into the unmodeled treasury)
	const budget = 14 // enough actions to move through several high-coin states
	var firstBatch []Event
	for steps := 0; steps < budget && g.Status == InProgress; steps++ {
		switch g.Phase() {
		case PhaseTurnAction:
			cur := g.turnSeat()
			if cur.Coins >= MandatoryCoupThreshold {
				target := ""
				for _, s := range g.Seats {
					if s.Active() && s.ID != cur.ID {
						target = s.ID
						break
					}
				}
				evs := mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(cur.ID, Action{Kind: Coup, TargetID: target}) })
				coupons++
				if steps == 0 {
					firstBatch = evs // A's first mandatory coup, paid from above the tray
				}
			} else {
				incomes++
				mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(cur.ID, Action{Kind: Income}) })
			}
		case PhaseWindowOpen:
			for g.window != nil {
				passAll(t, g)
			}
		case PhaseSingleSeatDecision:
			answerDefault(t, g)
		default:
			t.Fatalf("unexpected phase %s", g.Phase())
		}
		if err := g.CheckInvariants(); err != nil {
			t.Fatalf("invariant broken mid-run: %v", err)
		}
	}

	// The engine processed a coin state above the physical 50-coin tray without
	// any cap error: A's first coup paid 7 out of a 61-coin hoard, landing on 54.
	highPaid := false
	for _, e := range firstBatch {
		if e.Type == "coins_changed" && e.Payload["reason"] == CoinsCoupPaid && e.Payload["total"].(int) > 50 {
			highPaid = true
		}
	}
	if !highPaid {
		t.Fatalf("expected a coup payment from above the tray size in %v", eventTypes(firstBatch))
	}

	// Exact accounting: tracked total = start + incomes − 7 per coup (coups pay
	// the unmodeled treasury; flips move no coins; nothing is clamped at 50).
	total := 0
	for _, s := range g.Seats {
		total += s.Coins
		if s.Coins < 0 {
			t.Fatalf("negative coins on %s: %d", s.ID, s.Coins)
		}
	}
	if total != initialTotal+incomes-7*coupons {
		t.Fatalf("coin accounting off at high values: total %d, want %d (start %d + %d incomes - 7x%d coups)", total, initialTotal+incomes-7*coupons, initialTotal, incomes, coupons)
	}
	if g.Seats[0].Coins > 61 {
		t.Fatalf("A coins = %d exceeds its starting hoard without a legal source", g.Seats[0].Coins)
	}
}

// --- §15.12: mandatory reveal — no refusal path (rules.md Ruling 7) ----------------

func TestChallenge_MandatoryReveal_PenaltyOnlyWhenAbsent(t *testing.T) {
	t.Run("holding_the_character_must_reveal", func(t *testing.T) {
		g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
		setHands(t, g, [][]Character{
			{Duke, Captain},      // A claims tax and HOLDS the duke
			{Assassin, Contessa}, // B challenges (one card → auto penalty)
			{Ambassador, Duke},   // C
		})
		dropToFaceUp(t, g, 1)
		g.TurnSeatIndex = 0
		deckBefore := g.Deck.Size()

		mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Tax, Character: Duke}) })
		evs := mustAct(t, func() ([]Event, error) {
			return g.SubmitWindowResponse(g.Seats[1].ID, WindowResponse{Kind: RespChallenge})
		})

		if len(findEvents(evs, "claim_failed")) != 0 {
			t.Fatalf("holding the character must NOT fail the claim: %v", eventTypes(evs))
		}
		okEv := findEvents(evs, "claim_succeeded")
		if len(okEv) != 1 || okEv[0].Payload["character"] != "duke" {
			t.Fatalf("claim_succeeded missing/wrong: %v", okEv)
		}
		flips := findEvents(evs, "card_flipped")
		var revealed, penalty bool
		for _, f := range flips {
			if f.Payload["seat_id"] == "s0" && f.Payload["card"] == "duke" && f.Payload["reason"] == FlipSuccessfulReveal {
				revealed = true
			}
			if f.Payload["seat_id"] == "s1" && f.Payload["reason"] == FlipPenaltyFailedChallenge {
				penalty = true
			}
		}
		if !revealed || !penalty {
			t.Fatalf("want successful_reveal for A and penalty flip for B: %v", flips)
		}
		// Net-zero deck (rules.md step 4): shuffled back + replacement drawn.
		if g.Deck.Size() != deckBefore {
			t.Fatalf("deck size %d → %d, want net zero", deckBefore, g.Deck.Size())
		}
		if len(g.Seats[0].FaceDown) != 2 {
			t.Fatalf("claimant must still hold two face-down cards after reveal+redraw: %v", g.Seats[0].FaceDown)
		}
		if g.Seats[0].Coins != 5 { // 2 + 3 tax, the claim survived
			t.Fatalf("tax coins = %d, want 5", g.Seats[0].Coins)
		}
	})

	t.Run("absent_character_only_then_penalty", func(t *testing.T) {
		g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
		setHands(t, g, [][]Character{
			{Captain, Ambassador}, // A claims tax but holds NO duke
			{Assassin, Contessa},  // B challenges (one card)
			{Duke, Duke},          // C — dukes exist in the game, just not A's hand
		})
		dropToFaceUp(t, g, 0) // A ends with ONE face-down → penalty auto-resolves
		g.TurnSeatIndex = 0

		mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Tax, Character: Duke}) })
		evs := mustAct(t, func() ([]Event, error) {
			return g.SubmitWindowResponse(g.Seats[1].ID, WindowResponse{Kind: RespChallenge})
		})

		failed := findEvents(evs, "claim_failed")
		if len(failed) != 1 || failed[0].Payload["claimed_character"] != "duke" {
			t.Fatalf("claim_failed missing/wrong: %v", failed)
		}
		if len(findEvents(evs, "claim_succeeded")) != 0 {
			t.Fatal("absent character must not succeed")
		}
		if g.Seats[0].Coins != 2 {
			t.Fatalf("busted claim takes NO effect: A has %d coins, want 2", g.Seats[0].Coins)
		}
		var penaltyFlipped bool
		for _, f := range findEvents(evs, "card_flipped") {
			if f.Payload["seat_id"] == "s0" && f.Payload["reason"] == FlipPenaltyFailedClaim {
				penaltyFlipped = true
			}
		}
		if !penaltyFlipped {
			t.Fatalf("penalty flip for the busted claimant missing: %v", eventTypes(evs))
		}

		// Ruling 7: there is NO refusal outcome anywhere in the stream — no
		// event type or payload admits "chose not to reveal".
		for _, e := range evs {
			for _, part := range []string{e.Type, fmt.Sprint(e.Payload)} {
				if strings.Contains(strings.ToLower(part), "refus") || strings.Contains(strings.ToLower(part), "decline") {
					t.Fatalf("refusal outcome in event stream: %+v", e)
				}
			}
		}
	})
}

// --- Standing rule tests (spec §14.1 "plus the standing rule tests") --------------

func TestSunkCost_VoidedOffensiveClaim_Lost(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{
		{Duke, Captain},      // A bluffs the assassin (holds none)
		{Assassin, Contessa}, // B — target: untouched by a voided claim
		{Ambassador, Duke},   // C busts the bluff
	})
	g.Seats[0].Coins = 5
	g.TurnSeatIndex = 0

	evDecl := mustAct(t, func() ([]Event, error) {
		return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Assassinate, Character: Assassin, TargetID: "s1"})
	})
	evs2 := mustAct(t, func() ([]Event, error) {
		return g.SubmitWindowResponse(g.Seats[2].ID, WindowResponse{Kind: RespChallenge})
	})

	// A was busted with two cards → flip choice; answer it to finish the void.
	if g.pending == nil {
		t.Fatalf("penalty must suspend on the flip choice: %v", eventTypes(evs2))
	}
	evs3 := answerDefault(t, g)

	all := append(append(evDecl, evs2...), evs3...)
	sunk := 0
	for _, e := range all {
		if e.Type == "coins_changed" && e.Payload["seat_id"] == "s0" {
			sunk += e.Payload["delta"].(int)
		}
	}
	if sunk != -3 {
		t.Fatalf("net coin movement for A = %d, want exactly -3 (sunk cost, never refunded)", sunk)
	}
	for _, e := range all {
		if e.Type == "card_flipped" && e.Payload["seat_id"] == "s1" {
			t.Fatalf("voided offensive claim must not touch the target: %+v", e)
		}
	}
	if len(g.Seats[1].FaceDown) != 2 || len(g.Seats[1].FaceUp) != 0 {
		t.Fatalf("target hand changed by a voided claim: %+v", g.Seats[1])
	}
	if g.Seats[0].Coins != 2 {
		t.Fatalf("A coins = %d, want 2 (5 - sunk 3)", g.Seats[0].Coins)
	}
}

func TestFailedDefensiveClaim_NoBlock_Proceeds(t *testing.T) {
	type caseT struct {
		name        string
		attack      ActionKind
		claimChar   Character // what the attacker claims
		blockChar   Character // what the target counterclaims (and does NOT hold)
		targetCoins int
	}
	cases := []caseT{
		{"assassinate_contessa", Assassinate, Assassin, Contessa, 2},
		{"steal_captain", Steal, Captain, Captain, 4},
		{"steal_ambassador", Steal, Ambassador, Ambassador, 4},
		{"foreign_aid_duke", ForeignAid, CharacterNone, Duke, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
			// A: attacker; T: blocker (never holds blockChar); C: challenger.
			var aHand, tHand, cHand []Character
			switch c.attack {
			case Assassinate:
				aHand, tHand, cHand = []Character{Duke, Captain}, []Character{Assassin, Captain}, []Character{Contessa, Ambassador}
			case Steal:
				if c.blockChar == Captain {
					aHand, tHand, cHand = []Character{Captain, Duke}, []Character{Ambassador, Contessa}, []Character{Duke, Assassin}
				} else {
					aHand, tHand, cHand = []Character{Ambassador, Duke}, []Character{Captain, Contessa}, []Character{Duke, Assassin}
				}
			case ForeignAid:
				aHand, tHand, cHand = []Character{Assassin, Captain}, []Character{Contessa, Ambassador}, []Character{Duke, Duke}
			}
			setHands(t, g, [][]Character{aHand, tHand, cHand})
			g.Seats[1].Coins = c.targetCoins
			g.TurnSeatIndex = 0

			var act Action
			switch c.attack {
			case Assassinate:
				act = Action{Kind: Assassinate, Character: Assassin, TargetID: "s1"}
				g.Seats[0].Coins = 5 // pay the sunk 3
			case Steal:
				act = Action{Kind: Steal, Character: c.claimChar, TargetID: "s1"}
			case ForeignAid:
				act = Action{Kind: ForeignAid}
			}
			mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(g.Seats[0].ID, act) })

			// T counterclaims a character it does NOT hold; C busts the bluff.
			evs2 := mustAct(t, func() ([]Event, error) {
				return g.SubmitWindowResponse(g.Seats[1].ID, WindowResponse{Kind: RespCounterclaim, Character: c.blockChar})
			})
			if len(findEvents(evs2, "window_opened")) != 1 || findEvents(evs2, "window_opened")[0].Payload["kind"] != "counterclaim_challenge" {
				t.Fatalf("nested window for the block must open: %v", eventTypes(evs2))
			}
			evs3 := mustAct(t, func() ([]Event, error) {
				return g.SubmitWindowResponse(g.Seats[2].ID, WindowResponse{Kind: RespChallenge})
			})

			if len(findEvents(evs3, "claim_failed")) != 1 || findEvents(evs3, "claim_failed")[0].Payload["claimed_character"] != c.blockChar.String() {
				t.Fatalf("block must fail: %v", eventTypes(evs3))
			}
			if len(findEvents(evs3, "action_fizzled")) != 0 && findEvents(evs3, "action_fizzled")[0].Payload["reason"] == FizzleCounterclaimSuccess {
				t.Fatal("no counterclaim_success fizzle on a failed defensive claim")
			}

			// T holds two cards, so the penalty suspends on its flip choice;
			// answering it lets the attack PROCEED to full effect — the
			// load-bearing "no block" asymmetry (§4.3).
			if g.pending == nil {
				t.Fatalf("penalty must suspend on T's flip choice: %v", eventTypes(evs3))
			}
			all := append([]Event{}, evs3...)
			all = append(all, answerDefault(t, g)...)

			switch c.attack {
			case Assassinate:
				// Either the attack fizzled (T's last card was the penalty) or T
				// lost a second card to it — both are "proceeded as if unopposed"
				// under the liveness re-check. Exactly one of the two must hold.
				fizzled := len(findEvents(all, "action_fizzled")) == 1 && findEvents(all, "action_fizzled")[0].Payload["reason"] == FizzleTargetEliminated
				hit := false
				for _, e := range all {
					if e.Type == "card_flipped" && e.Payload["seat_id"] == "s1" && e.Payload["reason"] == FlipAssassinate {
						hit = true
					}
				}
				if !fizzled && !hit {
					t.Fatalf("failed defensive claim must let the assassination proceed: %v", eventTypes(all))
				}
			case Steal:
				moved := false
				for _, e := range all {
					if e.Type == "coins_changed" && e.Payload["reason"] == CoinsStealTook {
						moved = true
					}
				}
				if !moved {
					t.Fatalf("failed defensive claim must let the steal proceed: %v", eventTypes(all))
				}
			case ForeignAid:
				got := g.Seats[0].Coins
				if got != 2+2 {
					t.Fatalf("FA must proceed after a busted block: A has %d coins, want 4", got)
				}
			}
		})
	}
}

func TestWindow_GrammarPerSituation(t *testing.T) {
	mk := func() *Game {
		g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM, SeatLLM})
		setHands(t, g, [][]Character{
			{Assassin, Captain},
			{Duke, Contessa},
			{Ambassador, Duke},
			{Contessa, Ambassador},
		})
		g.TurnSeatIndex = 0
		return g
	}

	t.Run("steal_window", func(t *testing.T) {
		g := mk()
		mustAct(t, func() ([]Event, error) {
			return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Steal, Character: Captain, TargetID: "s1"})
		})
		if got := joinTokens(g.AllowedResponsesFor("s1")); got != "pass,counterclaim_captain,counterclaim_ambassador,challenge" {
			t.Fatalf("target grammar = %q", got)
		}
		if got := joinTokens(g.AllowedResponsesFor("s2")); got != "pass,challenge" {
			t.Fatalf("bystander grammar = %q", got)
		}
		// A non-target may not counterclaim; the target may not claim contessa.
		if _, err := g.SubmitWindowResponse("s2", WindowResponse{Kind: RespCounterclaim, Character: Captain}); err == nil {
			t.Fatal("bystander counterclaim accepted")
		} else {
			mustLegacy(t, err, CodeNotEligible, "you cannot respond to that window")
		}
		if _, err := g.SubmitWindowResponse("s1", WindowResponse{Kind: RespCounterclaim, Character: Contessa}); err == nil {
			t.Fatal("target counterclaim of a non-blocking character accepted")
		} else {
			mustLegacy(t, err, CodeNotEligible, "you cannot respond to that window")
		}
		if tok := g.AllowedResponsesFor("s0"); tok != nil {
			t.Fatalf("the claimant must not be eligible in its own window: %v", tok)
		}
	})

	t.Run("assassinate_window", func(t *testing.T) {
		g := mk()
		g.Seats[0].Coins = 5
		mustAct(t, func() ([]Event, error) {
			return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Assassinate, Character: Assassin, TargetID: "s1"})
		})
		if got := joinTokens(g.AllowedResponsesFor("s1")); got != "pass,counterclaim_contessa,challenge" {
			t.Fatalf("target grammar = %q", got)
		}
	})

	t.Run("foreign_aid_window_no_challenge", func(t *testing.T) {
		g := mk()
		mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: ForeignAid}) })
		for _, id := range []string{"s1", "s2", "s3"} {
			if got := joinTokens(g.AllowedResponsesFor(id)); got != "pass,counterclaim_duke" {
				t.Fatalf("FA grammar for %s = %q (no direct challenge may exist)", id, got)
			}
			if _, err := g.SubmitWindowResponse(id, WindowResponse{Kind: RespChallenge}); err == nil {
				t.Fatalf("direct challenge of Foreign Aid accepted by %s", id)
			} else {
				mustLegacy(t, err, CodeNotEligible, "you cannot respond to that window")
			}
		}
	})

	t.Run("tax_and_exchange_windows", func(t *testing.T) {
		g := mk()
		mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Tax, Character: Duke}) })
		if got := joinTokens(g.AllowedResponsesFor("s1")); got != "pass,challenge" {
			t.Fatalf("tax grammar = %q", got)
		}
		g2 := mk()
		mustAct(t, func() ([]Event, error) {
			return g2.DeclareTurnAction(g2.Seats[0].ID, Action{Kind: Exchange, Character: Ambassador})
		})
		if got := joinTokens(g2.AllowedResponsesFor("s2")); got != "pass,challenge" {
			t.Fatalf("exchange grammar = %q", got)
		}
	})

	t.Run("nested_block_window", func(t *testing.T) {
		g := mk()
		mustAct(t, func() ([]Event, error) {
			return g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Steal, Character: Captain, TargetID: "s1"})
		})
		mustAct(t, func() ([]Event, error) {
			return g.SubmitWindowResponse("s1", WindowResponse{Kind: RespCounterclaim, Character: Ambassador})
		})
		if got := joinTokens(g.AllowedResponsesFor("s0")); got != "pass,challenge" {
			t.Fatalf("nested grammar for original attacker = %q", got)
		}
		if got := g.AllowedResponsesFor("s1"); got != nil {
			t.Fatalf("blocker must be excluded from its own nested window: %v", got)
		}
	})
}

// --- Forfeit vote resolution outcomes (spec §7 / §10.1) ---------------------------

func forfeitSetup(t *testing.T, humanCount int) *Game {
	t.Helper()
	var types []SeatType
	for i := 0; i < humanCount; i++ {
		types = append(types, SeatHuman)
	}
	types = append(types, SeatLLM) // the target (last seat)
	g := newTestGame(t, WindowSettings{}, types)
	hands := make([][]Character, len(types))
	pairs := [][]Character{
		{Duke, Captain}, {Assassin, Contessa}, {Ambassador, Duke},
		{Contessa, Ambassador}, {Captain, Assassin},
	}
	for i := range hands {
		hands[i] = pairs[i%len(pairs)]
	}
	setHands(t, g, hands)
	return g
}

func TestForfeitVote_MajorityEarlyClose(t *testing.T) {
	g := forfeitSetup(t, 3) // H1 initiator, H2, H3 voters; L target. Majority of 3 = 2.
	targetID := fmt.Sprintf("s%d", len(g.Seats)-1)

	evsV := mustAct(t, func() ([]Event, error) { return g.InitiateForfeitVote("s0", targetID) })
	if vv := findEvents(evsV, "forfeit_vote_opened"); len(vv) != 1 || vv[0].Payload["majority_needed"] != 2 {
		t.Fatalf("vote opened wrong: %v", vv)
	}

	mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s1", true) }) // first yes: not yet a majority
	if g.vote == nil || g.vote.Result != "" {
		t.Fatal("vote must stay open before the majority is reached")
	}
	evs := mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s0", true) }) // second yes: early close

	resolved := findEvents(evs, "forfeit_vote_resolved")
	if len(resolved) != 1 || resolved[0].Payload["result"] != VoteResultForfeited {
		t.Fatalf("early majority must resolve forfeited: %v", resolved)
	}
	tally := resolved[0].Payload["tally"].(map[string]any)
	if tally["yes"] != 2 || tally["no"] != 0 || tally["eligible"] != 3 {
		t.Fatalf("tally = %v, want yes:2 no:0 eligible:3", tally)
	}
	if _, err := g.CastForfeitVote("s2", true); err == nil {
		t.Fatal("ballot for a closed vote accepted")
	} else {
		mustLegacy(t, err, CodeVoteResolved, "the forfeit vote has already been resolved")
	}
	if !g.Seats[len(g.Seats)-1].Forfeited {
		t.Fatal("target must be instantly eliminated on yes-majority")
	}
}

func TestForfeitVote_TieClose_NotForfeited(t *testing.T) {
	g := forfeitSetup(t, 4) // H1..H4 voters; majority of 4 = 3.
	targetID := fmt.Sprintf("s%d", len(g.Seats)-1)
	mustAct(t, func() ([]Event, error) { return g.InitiateForfeitVote("s0", targetID) })

	// Two yes, two no: every eligible voter casts without either side reaching
	// the majority of 3 → tie-close with not_forfeited.
	mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s1", true) })
	mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s2", false) })
	evs := mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s3", true) }) // 2 yes — still open (majority 3)
	if g.vote != nil && g.vote.Result == "" {
		evs = mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s0", false) }) // 2-2 exhausted: closes here
	}
	resolved := findEvents(evs, "forfeit_vote_resolved")
	if len(resolved) != 1 || resolved[0].Payload["result"] != VoteResultNotForfeited {
		t.Fatalf("exhausted tie must resolve not_forfeited: %v", eventTypes(evs))
	}
	if g.Seats[len(g.Seats)-1].Eliminated() {
		t.Fatal("target must be unaffected by a not_forfeited result")
	}
}

func TestForfeitVote_VoidedOnTargetDeath(t *testing.T) {
	g := forfeitSetup(t, 2) // H1 initiator+voter, H2 voter; L target
	targetID := fmt.Sprintf("s%d", len(g.Seats)-1)
	setHands(t, g, [][]Character{
		{Duke, Captain},
		{Assassin, Contessa},
		{Ambassador, Duke},
	})
	dropToFaceUp(t, g, len(g.Seats)-1) // target has ONE card left

	mustAct(t, func() ([]Event, error) { return g.InitiateForfeitVote("s0", targetID) })
	g.TurnSeatIndex = 0
	g.Seats[0].Coins = 9
	evs := mustAct(t, func() ([]Event, error) { return g.DeclareTurnAction("s0", Action{Kind: Coup, TargetID: targetID}) })

	// The coup eliminates the vote's target by play while the vote is pending →
	// the vote voids with no effect.
	resolved := findEvents(evs, "forfeit_vote_resolved")
	if len(resolved) != 1 || resolved[0].Payload["result"] != VoteResultVoided {
		t.Fatalf("vote must void on target death: %v", eventTypes(evs))
	}
	for _, e := range evs {
		if e.Type == "eliminated" && e.Payload["reason"] == ElimForfeit {
			t.Fatal("no forfeit elimination may happen for a voided vote")
		}
	}
	if g.vote != nil {
		t.Fatal("voided vote must leave the active slot")
	}
}

func TestForfeitVote_Rejections(t *testing.T) {
	g := forfeitSetup(t, 3)
	targetID := fmt.Sprintf("s%d", len(g.Seats)-1)

	if _, err := g.InitiateForfeitVote("s0", "s0"); err == nil {
		t.Fatal("self-target accepted")
	} else {
		mustLegacy(t, err, CodeSelfTarget, "you cannot target your own seat")
	}

	evsV := mustAct(t, func() ([]Event, error) { return g.InitiateForfeitVote("s0", targetID) })
	_ = evsV
	if _, err := g.InitiateForfeitVote("s1", targetID); err == nil {
		t.Fatal("second concurrent vote accepted (A15: one active vote per game)")
	} else {
		mustLegacy(t, err, CodeVoteExists, "a forfeit vote is already in progress")
	}

	// LLM seats cannot initiate nor vote.
	if _, err := g.InitiateForfeitVote(targetID, "s0"); err == nil {
		t.Fatal("LLM initiator accepted")
	} else {
		mustLegacy(t, err, CodeNotEligible, "only a human player may initiate a forfeit vote")
	}
	if _, err := g.CastForfeitVote(targetID, true); err == nil {
		t.Fatal("LLM voter accepted")
	} else {
		mustLegacy(t, err, CodeNotAVoter, "you are not eligible to cast this vote")
	}

	mustAct(t, func() ([]Event, error) { return g.CastForfeitVote("s1", true) })
	if _, err := g.CastForfeitVote("s1", false); err == nil {
		t.Fatal("second ballot from the same voter accepted")
	} else {
		mustLegacy(t, err, CodeAlreadyVoted, "you have already voted")
	}

	// Target eliminated → re-initiation rejected.
	g2 := forfeitSetup(t, 2)
	target2 := fmt.Sprintf("s%d", len(g2.Seats)-1)
	dropToFaceUp(t, g2, len(g2.Seats)-1)
	g2.TurnSeatIndex = 0
	g2.Seats[0].Coins = 9
	mustAct(t, func() ([]Event, error) { return g2.DeclareTurnAction("s0", Action{Kind: Coup, TargetID: target2}) }) // eliminates the target
	if _, err := g2.InitiateForfeitVote("s0", target2); err == nil {
		t.Fatal("vote against an eliminated seat accepted")
	} else {
		mustLegacy(t, err, CodeVoteTargetInactive, "you cannot target an eliminated player")
	}
	if _, err := g2.InitiateForfeitVote("s0", "no-such-seat"); err == nil {
		t.Fatal("vote against a missing seat accepted")
	} else {
		mustLegacy(t, err, CodeNoSuchSeat, "no such seat")
	}
}

// --- small test utilities -------------------------------------------------------

// mustAct runs one engine entry point and fails the test on any error.
func mustAct(t *testing.T, fn func() ([]Event, error)) []Event {
	t.Helper()
	evs, err := fn()
	if err != nil {
		t.Fatalf("engine call failed: %v", err)
	}
	return evs
}

func joinTokens(tokens []string) string {
	return strings.Join(tokens, ",")
}

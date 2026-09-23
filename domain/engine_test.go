package domain

import (
	"fmt"
	"math/rand"
	"testing"
)

// testIDGen is a deterministic ID source matching the §13 shape (kind + 12 hex).
type testIDGen struct{ n int }

func (g *testIDGen) New(kind string) string {
	g.n++
	return fmt.Sprintf("%s_%012d", kind, g.n)
}

// newTestGame builds a game with the given seat types and window settings.
// Card layout is deterministic (fixed rng seed) but almost always overridden
// via setHands; coins start at 2 per rules.md Setup. It also verifies the
// initial event batch shape (game_created, turn_start; seqs from 1).
func newTestGame(t *testing.T, settings WindowSettings, types []SeatType) *Game {
	t.Helper()
	specs := make([]SeatSpec, len(types))
	for i, tp := range types {
		specs[i] = SeatSpec{ID: fmt.Sprintf("s%d", i), Name: fmt.Sprintf("P%d", i+1), Type: tp}
	}
	g, evs, err := NewGame("g_test", specs, settings, &testIDGen{}, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("NewGame: %v", err)
	}
	if len(evs) != 2 || evs[0].Type != "game_created" || evs[1].Type != "turn_start" {
		t.Fatalf("initial batch = %v, want [game_created turn_start]", eventTypes(evs))
	}
	if evs[0].Seq != 1 || evs[1].Seq != 2 {
		t.Fatalf("initial seqs = %d,%d, want 1,2", evs[0].Seq, evs[1].Seq)
	}
	return g
}

// setHands replaces every seat's face-down hand (resetting coins to 2 and
// clearing face-up cards / forfeit flags) and refills the Court Deck with the
// remaining cards of the 15-card set in deterministic order. Hands must
// respect the three-copies limit; per-seat totals and deck size stay within
// the §3.2 invariants (a seat may hold fewer than 2 face-down only if the test
// also moves the difference to FaceUp).
func setHands(t *testing.T, g *Game, hands [][]Character) {
	t.Helper()
	if len(hands) != len(g.Seats) {
		t.Fatalf("hands for %d seats, got %d", len(g.Seats), len(hands))
	}
	pool := FullDeck()
	for i, h := range hands {
		s := &g.Seats[i]
		s.FaceDown = nil
		s.FaceUp = nil
		s.Forfeited = false
		s.Coins = 2
		for _, ch := range h {
			idx := -1
			for j, p := range pool {
				if p.Character == ch {
					idx = j
					break
				}
			}
			if idx < 0 {
				t.Fatalf("cannot assign %s to seat %s: not enough copies", ch, s.ID)
			}
			s.FaceDown = append(s.FaceDown, pool[idx])
			pool = append(pool[:idx], pool[idx+1:]...)
		}
	}
	g.Deck.order = pool
	if err := g.CheckInvariants(); err != nil {
		t.Fatalf("setHands broke invariants: %v", err)
	}
}

// dropToFaceUp moves one of the seat's face-down cards to its face-up list,
// modelling a previously lost influence (the seat keeps exactly 2 cards total).
func dropToFaceUp(t *testing.T, g *Game, seatIdx int) {
	t.Helper()
	s := &g.Seats[seatIdx]
	if len(s.FaceDown) < 2 {
		t.Fatalf("seat %s has only %d face-down cards", s.ID, len(s.FaceDown))
	}
	card := s.FaceDown[1]
	s.FaceDown = s.FaceDown[:1]
	s.FaceUp = append(s.FaceUp, card)
}

// eliminateSeat flips all of the seat's face-down cards face-up (both lost):
// the seat is eliminated and its turn/eligibility/target status drop out.
func eliminateSeat(t *testing.T, g *Game, seatIdx int) {
	t.Helper()
	s := &g.Seats[seatIdx]
	for _, card := range s.FaceDown {
		s.FaceUp = append(s.FaceUp, card)
	}
	s.FaceDown = nil
	if err := g.CheckInvariants(); err != nil {
		t.Fatalf("eliminateSeat broke invariants: %v", err)
	}
}

// deckContains reports whether the exact card identity is in the Court Deck.
func deckContains(g *Game, c Card) bool {
	for _, p := range g.Deck.order {
		if p == c {
			return true
		}
	}
	return false
}

// moveToFront reorders the deck (test-only; same package) so that the named
// cards sit on top, in the given order, preserving all other relative order.
func moveToFront(order []Card, front []Card) []Card {
	var rest []Card
	for _, c := range order {
		keep := true
		for _, f := range front {
			if c == f {
				keep = false
				break
			}
		}
		if keep {
			rest = append(rest, c)
		}
	}
	return append(front, rest...)
}

func eventTypes(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func findEvents(evs []Event, typ string) []Event {
	var out []Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// mustLegacy asserts err is a *LegacyError with the given code and exact
// normative message (§4.5 strings are normative).
func mustLegacy(t *testing.T, err error, code, message string) {
	t.Helper()
	le, ok := err.(*LegacyError)
	if !ok {
		t.Fatalf("error = %T (%v), want *LegacyError", err, err)
	}
	if le.Code != code || le.Message != message {
		t.Fatalf("legacy error = {%q %q}, want {%q %q}", le.Code, le.Message, code, message)
	}
}

// passAll has every eligible seat in the open window respond with pass, in
// seat order. With RequireAllResponses set this deterministically closes the
// window (reason all_responded); it returns every batch produced by the sweep
// concatenated.
func passAll(t *testing.T, g *Game) []Event {
	t.Helper()
	if g.window == nil {
		t.Fatalf("no open window to pass")
	}
	var all []Event
	for _, id := range g.window.Eligible {
		evs, err := g.SubmitWindowResponse(id, WindowResponse{Kind: RespPass})
		if err != nil {
			t.Fatalf("pass from %s: %v", id, err)
		}
		all = append(all, evs...)
	}
	return all
}

// answerDefault answers the pending single-seat decision with its engine-safe
// default (A4): first flip option / no-swap.
func answerDefault(t *testing.T, g *Game) []Event {
	t.Helper()
	p := g.pending
	if p == nil {
		t.Fatalf("no pending decision")
	}
	evs, err := g.AnswerDecision(p.SeatID, p.ID, p.Kind.DefaultChoice())
	if err != nil {
		t.Fatalf("answer default: %v", err)
	}
	return evs
}

// --- Setup & basic flows -----------------------------------------------------

func TestNewGame_SetupAndInvariants(t *testing.T) {
	for n := 2; n <= 6; n++ {
		var types []SeatType
		for i := 0; i < n; i++ {
			if i == 0 {
				types = append(types, SeatHuman)
			} else {
				types = append(types, SeatLLM)
			}
		}
		g := newTestGame(t, WindowSettings{}, types)
		if g.Deck.Size() != 15-2*n {
			t.Fatalf("%d players: deck size %d, want %d", n, g.Deck.Size(), 15-2*n)
		}
		for _, s := range g.Seats {
			if len(s.FaceDown) != 2 || s.Coins != 2 {
				t.Fatalf("seat %s: %d face-down / %d coins, want 2/2", s.ID, len(s.FaceDown), s.Coins)
			}
		}
		if g.Phase() != PhaseTurnAction || g.TurnID != 1 {
			t.Fatalf("initial phase/turn = %s/%d, want turn_action/1", g.Phase(), g.TurnID)
		}
	}

	for _, bad := range [][]SeatType{{SeatHuman}, {}} {
		specs := make([]SeatSpec, len(bad))
		for i, tp := range bad {
			specs[i] = SeatSpec{ID: fmt.Sprintf("s%d", i), Type: tp}
		}
		if _, _, err := NewGame("g_bad", specs, WindowSettings{}, &testIDGen{}, rand.New(rand.NewSource(1))); err == nil {
			t.Fatalf("%d seats accepted, want rejection", len(bad))
		}
	}

	specs := []SeatSpec{{ID: "a", Type: SeatLLM}, {ID: "b", Type: SeatLLM}}
	if _, _, err := NewGame("g_bad", specs, WindowSettings{}, &testIDGen{}, rand.New(rand.NewSource(1))); err == nil {
		t.Fatal("no-human game accepted, want rejection")
	}
}

func TestIncome_TurnAdvances(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	s0 := g.Seats[0].ID
	g.TurnSeatIndex = 0

	evs, err := g.DeclareTurnAction(s0, Action{Kind: Income})
	if err != nil {
		t.Fatalf("income: %v", err)
	}
	want := []string{"action_declared", "coins_changed", "turn_start"}
	if got := eventTypes(evs); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("income batch = %v, want %v", got, want)
	}
	if g.Seats[0].Coins != 3 {
		t.Fatalf("coins after income = %d, want 3", g.Seats[0].Coins)
	}
	if g.TurnSeatIndex != 1 || g.TurnID != 2 {
		t.Fatalf("turn advanced to seat %d turn %d, want seat 1 turn 2", g.TurnSeatIndex, g.TurnID)
	}

	// Declaring an action while it is not your turn: §4.5 message.
	if _, err := g.DeclareTurnAction(s0, Action{Kind: Income}); err != nil {
		mustLegacy(t, err, CodeNotYourTurn, "it is not your turn")
	} else {
		t.Fatal("second seat's action by first seat accepted")
	}
}

func TestCoup_TwoCards_FlipChoice(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{
		{Duke, Captain},      // A (actor), 2+7 needed below
		{Assassin, Contessa}, // B (victim), two cards → choice
		{Ambassador, Duke},   // C
	})
	g.Seats[0].Coins = 9 // 9 ≥ 7 but < 10: coup legal, not mandatory
	g.TurnSeatIndex = 0

	evs, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Coup, TargetID: g.Seats[1].ID})
	if err != nil {
		t.Fatalf("coup: %v", err)
	}
	if got := eventTypes(evs); len(got) != 3 || got[0] != "action_declared" || got[1] != "coins_changed" || got[2] != "card_flip_choice_requested" {
		t.Fatalf("coup batch = %v, want [action_declared coins_changed card_flip_choice_requested]", got)
	}
	if g.pending == nil || g.pending.Kind != DecisionFlipChoice || g.pending.SeatID != g.Seats[1].ID {
		t.Fatalf("pending decision = %+v, want flip choice for victim", g.pending)
	}
	req := findEvents(evs, "card_flip_choice_requested")
	if len(req) != 1 || req[0].Audience.Public || req[0].Audience.SeatID != g.Seats[1].ID {
		t.Fatalf("flip choice request not private to victim: %+v", req)
	}
	// The choice is suspended: no turn_start yet, phase = single_seat_decision.
	if g.Phase() != PhaseSingleSeatDecision || g.TurnID != 1 {
		t.Fatalf("phase/turn during decision = %s/%d", g.Phase(), g.TurnID)
	}

	evs2, err := g.AnswerDecision(g.Seats[1].ID, g.pending.ID, Choice{FlipOption: "face_down_0"})
	if err != nil {
		t.Fatalf("answer flip: %v", err)
	}
	// Victim had [assassin, contessa]: face_down_0 = assassin flips.
	flips := findEvents(evs2, "card_flipped")
	if len(flips) != 1 || flips[0].Payload["card"] != "assassin" || flips[0].Payload["reason"] != "coup" {
		t.Fatalf("flip events = %v, want assassin/coup", flips)
	}
	if len(findEvents(evs2, "turn_start")) != 1 {
		t.Fatalf("turn did not advance after flip choice: %v", eventTypes(evs2))
	}
	b := g.Seats[1]
	if len(b.FaceDown) != 1 || b.FaceUp[0].Character != Assassin || b.FaceDown[0].Character != Contessa {
		t.Fatalf("victim hand after flip = %+v / %+v", b.FaceDown, b.FaceUp)
	}
	if g.Seats[0].Coins != 2 {
		t.Fatalf("actor coins after coup = %d, want 2 (9-7)", g.Seats[0].Coins)
	}

	// Stale / unknown decision id is rejected (§4.5).
	if _, err := g.AnswerDecision(g.Seats[0].ID, "d_x", Choice{FlipOption: "face_down_0"}); err == nil {
		t.Fatal("answer for an unknown decision id accepted")
	} else {
		mustLegacy(t, err, CodeDecisionMade, "that decision was already made")
	}
}

func TestCoup_OneCard_AutoFlipNoDecision(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}, {Ambassador, Duke}})
	dropToFaceUp(t, g, 1) // victim: 1 face-down (assassin), 1 face-up (contessa)
	g.Seats[0].Coins = 9
	g.TurnSeatIndex = 0

	evs, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Coup, TargetID: "s1"})
	if err != nil {
		t.Fatalf("coup: %v", err)
	}
	if got := eventTypes(evs); len(got) != 5 || got[2] != "card_flipped" || got[3] != "eliminated" || got[4] != "turn_start" {
		t.Fatalf("auto-flip batch = %v, want action_declared/coins_changed/card_flipped/eliminated/turn_start", got)
	}
	if g.pending != nil {
		t.Fatal("no decision may be pending for a one-card victim (auto-resolve)")
	}
	v := g.Seats[1]
	if !v.Eliminated() || len(v.FaceUp) != 2 || v.FaceUp[0].Character != Contessa || v.FaceUp[1].Character != Assassin {
		t.Fatalf("victim not eliminated correctly: %+v", v)
	}
	if g.Status == Finished {
		t.Fatal("3 players remain: the game must continue")
	}
}

func TestExchange_FullFlow(t *testing.T) {
	type outcome struct {
		name   string
		choice Choice
		want   []Character // expected exchanger hand after the finish
	}
	for _, oc := range []outcome{
		{"no_swap", Choice{ExchangeNoSwap: true}, []Character{Assassin, Contessa}},
		// Keep LEFT (duke) by swapping face_down_1 (contessa): [assassin duke].
		{"keep_left", Choice{ExchangeDrawnIdx: 0, ExchangeHandOpt: "face_down_1"}, []Character{Assassin, Duke}},
		// Keep RIGHT (captain) by swapping face_down_0 (assassin): [captain contessa].
		{"keep_right", Choice{ExchangeDrawnIdx: 1, ExchangeHandOpt: "face_down_0"}, []Character{Captain, Contessa}},
	} {
		t.Run(oc.name, func(t *testing.T) {
			g := newTestGame(t, WindowSettings{RequireAllResponses: true}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
			setHands(t, g, [][]Character{
				{Assassin, Contessa},  // A (exchanger)
				{Ambassador, Duke},    // B
				{Ambassador, Captain}, // C
			})
			// Force the draw: reorder the (identity-unique) deck so exactly
			// [duke copy1, captain copy1] sit on top.
			g.Deck.order = moveToFront(g.Deck.order, []Card{
				{Character: Duke, Copy: 1},
				{Character: Captain, Copy: 1},
			})

			g.TurnSeatIndex = 0
			evs, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Exchange, Character: Ambassador})
			if err != nil {
				t.Fatalf("exchange declare: %v", err)
			}
			if len(findEvents(evs, "window_opened")) != 1 {
				t.Fatalf("expected window_opened, got %v", eventTypes(evs))
			}
			evs = append(evs, passAll(t, g)...)

			d := g.pending
			if d == nil || d.Kind != DecisionExchangeFinish {
				t.Fatalf("pending after passes = %+v, want exchange finish", d)
			}
			rev := findEvents(evs, "exchange_revealed")
			if len(rev) != 1 || rev[0].Audience.Public || rev[0].Audience.SeatID != g.Seats[0].ID {
				t.Fatalf("exchange_revealed must be private to exchanger: %+v", rev)
			}
			if drawn, ok := rev[0].Payload["drawn"].([]string); !ok || len(drawn) != 2 || drawn[0] != "duke" || drawn[1] != "captain" {
				t.Fatalf("private reveal = %v, want [duke captain]", rev[0].Payload["drawn"])
			}
			if g.Deck.Size() != 15-6-2 {
				t.Fatalf("deck during suspended exchange = %d, want 1 (two drawn cards out)", g.Deck.Size())
			}

			evs2, err := g.AnswerDecision(g.Seats[0].ID, d.ID, oc.choice)
			if err != nil {
				t.Fatalf("exchange finish: %v", err)
			}
			if len(findEvents(evs2, "exchange_completed")) != 1 || len(findEvents(evs2, "turn_start")) != 1 {
				t.Fatalf("finish batch = %v, want exchange_completed + turn_start", eventTypes(evs2))
			}

			var got []Character
			for _, c := range g.Seats[0].FaceDown {
				got = append(got, c.Character)
			}
			if len(got) != len(oc.want) {
				t.Fatalf("hand after %s = %v", oc.name, got)
			}
			for i := range oc.want {
				if got[i] != oc.want[i] {
					t.Fatalf("hand after %s = %v, want %v", oc.name, got, oc.want)
				}
			}
			if g.Deck.Size() != 15-6 {
				t.Fatalf("deck size after exchange = %d, want %d (net zero)", g.Deck.Size(), 9)
			}
		})
	}
}

func TestFlipChoice_IndexSemantics(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	g.Seats[0].Coins = 9
	g.TurnSeatIndex = 0

	if _, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Coup, TargetID: g.Seats[1].ID}); err != nil {
		t.Fatalf("coup: %v", err)
	}
	// face_down_1 = the SECOND card (contessa) flips; assassin stays face-down.
	if _, err := g.AnswerDecision(g.Seats[1].ID, g.pending.ID, Choice{FlipOption: "face_down_1"}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	v := g.Seats[1]
	if len(v.FaceDown) != 1 || v.FaceDown[0].Character != Assassin {
		t.Fatalf("face-down after choosing face_down_1 = %v, want [assassin]", v.FaceDown)
	}
	if len(v.FaceUp) != 1 || v.FaceUp[0].Character != Contessa {
		t.Fatalf("face-up after choosing face_down_1 = %v, want [contessa]", v.FaceUp)
	}

	// Out-of-range options are rejected.
	g2 := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g2, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	g2.Seats[0].Coins = 9
	g2.TurnSeatIndex = 0
	if _, err := g2.DeclareTurnAction(g2.Seats[0].ID, Action{Kind: Coup, TargetID: g2.Seats[1].ID}); err != nil {
		t.Fatalf("coup: %v", err)
	}
	if _, err := g2.AnswerDecision(g2.Seats[1].ID, g2.pending.ID, Choice{FlipOption: "face_down_5"}); err == nil {
		t.Fatal("out-of-range flip option accepted")
	} else {
		mustLegacy(t, err, CodeInvalidChoice, "that is not a valid option")
	}
	// The decision is addressed to exactly one seat (§5.4).
	if _, err := g2.AnswerDecision(g2.Seats[0].ID, g2.pending.ID, Choice{FlipOption: "face_down_0"}); err == nil {
		t.Fatal("answer by the wrong seat accepted")
	} else {
		mustLegacy(t, err, CodeDecisionNotAddressed, "this decision is not addressed to you")
	}
}

func TestWindowTermination_Matrix(t *testing.T) {
	type combo struct {
		requireAll bool
		timeoutMs  int
		name       string
	}
	for _, c := range []combo{
		{name: "fcfs_no_timeout", requireAll: false, timeoutMs: 0},
		{name: "fcfs_with_timeout", requireAll: false, timeoutMs: 45000},
		{name: "require_all_no_timeout", requireAll: true, timeoutMs: 0},
		{name: "require_all_with_timeout", requireAll: true, timeoutMs: 45000},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGame(t, WindowSettings{RequireAllResponses: c.requireAll, TimeoutMs: c.timeoutMs},
				[]SeatType{SeatHuman, SeatLLM, SeatLLM})
			setHands(t, g, [][]Character{
				{Duke, Captain},      // A claims tax (holds duke → survives)
				{Assassin, Contessa}, // B
				{Ambassador, Duke},   // C
			})
			g.TurnSeatIndex = 0
			if _, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Tax, Character: Duke}); err != nil {
				t.Fatalf("tax: %v", err)
			}

			switch c.name {
			case "fcfs_no_timeout":
				// Everyone passes → window stays open (open-ended FCFS, §5.2.5 row 1).
				passAll(t, g)
				if g.window == nil || g.Phase() != PhaseWindowOpen {
					t.Fatal("all-pass with require-all=false and no timeout must keep the window open")
				}

			case "fcfs_with_timeout":
				passAll(t, g)
				if g.window == nil {
					t.Fatal("timeout must not close the window before it expires")
				}
				evs, err := g.ExpireWindow()
				if err != nil {
					t.Fatalf("expire: %v", err)
				}
				assertClosedReason(t, evs, "timeout")
				assertClaimSurvivedTax(t, g)

			case "require_all_no_timeout":
				evs := passAll(t, g)
				assertClosedReason(t, evs, "all_responded")
				assertClaimSurvivedTax(t, g)

			case "require_all_with_timeout":
				// Partial responses, then the deadline fires: unresponded seats
				// auto-pass and the window closes with reason timeout (row 4).
				if _, err := g.SubmitWindowResponse(g.Seats[1].ID, WindowResponse{Kind: RespPass}); err != nil {
					t.Fatalf("pass: %v", err)
				}
				evs, err := g.ExpireWindow()
				if err != nil {
					t.Fatalf("expire: %v", err)
				}
				assertClosedReason(t, evs, "timeout")
				assertClaimSurvivedTax(t, g)
			}
		})
	}
}

func assertClosedReason(t *testing.T, evs []Event, reason string) {
	t.Helper()
	closed := findEvents(evs, "window_closed")
	if len(closed) != 1 {
		t.Fatalf("window_closed count = %d, want 1 (%v)", len(closed), eventTypes(evs))
	}
	if closed[0].Payload["reason"] != reason {
		t.Fatalf("close reason = %v, want %s", closed[0].Payload["reason"], reason)
	}
}

// assertClaimSurvivedTax checks the tax claim survived unopposed: +3 coins,
// action took effect, turn advanced, no window re-opened.
func assertClaimSurvivedTax(t *testing.T, g *Game) {
	t.Helper()
	if g.window != nil || g.pending != nil {
		t.Fatal("structure still open after settlement")
	}
	if g.Seats[0].Coins != 5 { // 2 + 3
		t.Fatalf("actor coins = %d, want 5", g.Seats[0].Coins)
	}
	if g.Phase() != PhaseTurnAction || g.TurnID != 2 {
		t.Fatalf("phase/turn after settlement = %s/%d", g.Phase(), g.TurnID)
	}
}

func TestDecisionTimeout_Defaults(t *testing.T) {
	// Flip choice with timeout: engine default = first face-down card (A4).
	g := newTestGame(t, WindowSettings{TimeoutMs: 1000}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	g.Seats[0].Coins = 9
	g.TurnSeatIndex = 0
	if _, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Coup, TargetID: g.Seats[1].ID}); err != nil {
		t.Fatalf("coup: %v", err)
	}
	evs, err := g.ExpireWindow()
	if err != nil {
		t.Fatalf("expire decision: %v", err)
	}
	flips := findEvents(evs, "card_flipped")
	if len(flips) != 1 || flips[0].Payload["card"] != "assassin" {
		t.Fatalf("timeout flip = %v, want default first card (assassin)", flips)
	}
	if g.pending != nil || g.Phase() != PhaseTurnAction {
		t.Fatalf("after timeout decision: pending=%v phase=%s", g.pending != nil, g.Phase())
	}

	// Exchange finish with timeout: engine default = no-swap (A4).
	g2 := newTestGame(t, WindowSettings{RequireAllResponses: true, TimeoutMs: 1000}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g2, [][]Character{{Assassin, Contessa}, {Ambassador, Duke}, {Ambassador, Captain}})
	g2.TurnSeatIndex = 0
	if _, err := g2.DeclareTurnAction(g2.Seats[0].ID, Action{Kind: Exchange, Character: Ambassador}); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	passAll(t, g2)
	evs2, err := g2.ExpireWindow()
	if err != nil {
		t.Fatalf("expire exchange decision: %v", err)
	}
	if len(findEvents(evs2, "exchange_completed")) != 1 {
		t.Fatalf("default no-swap must complete the exchange: %v", eventTypes(evs2))
	}
	if g2.Seats[0].FaceDown[0].Character != Assassin || g2.Seats[0].FaceDown[1].Character != Contessa {
		t.Fatalf("hand changed by no-swap default: %v", g2.Seats[0].FaceDown)
	}
	if g2.Deck.Size() != 15-6 {
		t.Fatalf("deck size = %d, want 9 (net zero)", g2.Deck.Size())
	}

	// No timeout configured → nothing may expire.
	g3 := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g3, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	g3.Seats[0].Coins = 9
	g3.TurnSeatIndex = 0
	if _, err := g3.DeclareTurnAction(g3.Seats[0].ID, Action{Kind: Coup, TargetID: g3.Seats[1].ID}); err != nil {
		t.Fatalf("coup: %v", err)
	}
	if _, err := g3.ExpireWindow(); err == nil {
		t.Fatal("expire accepted with no timeout configured")
	}
}

func TestGameOver_LastSeat(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM})
	setHands(t, g, [][]Character{{Duke, Captain}, {Assassin, Contessa}})
	dropToFaceUp(t, g, 1) // B: one card left
	g.Seats[0].Coins = 9
	g.TurnSeatIndex = 0

	evs, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Coup, TargetID: g.Seats[1].ID})
	if err != nil {
		t.Fatalf("coup: %v", err)
	}
	if len(findEvents(evs, "game_over")) != 1 {
		t.Fatalf("game_over missing from batch: %v", eventTypes(evs))
	}
	goEv := findEvents(evs, "game_over")[0]
	if goEv.Payload["winner_seat_id"] != g.Seats[0].ID {
		t.Fatalf("winner = %v, want seat 0", goEv.Payload["winner_seat_id"])
	}
	if g.Status != Finished || g.WinnerID != g.Seats[0].ID || g.Phase() != PhaseFinished {
		t.Fatalf("state after game over: status=%d winner=%s phase=%s", g.Status, g.WinnerID, g.Phase())
	}

	// Nothing may happen in a finished game.
	if _, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Income}); err == nil {
		t.Fatal("action accepted after game over")
	} else {
		mustLegacy(t, err, CodeGameOver, "the game is over")
	}
	if _, err := g.SubmitWindowResponse(g.Seats[0].ID, WindowResponse{Kind: RespPass}); err == nil {
		t.Fatal("window response accepted after game over")
	} else {
		mustLegacy(t, err, CodeWindowResolved, "the window has already been resolved")
	}
	if _, err := g.ExpireWindow(); err == nil {
		t.Fatal("expiry accepted after game over")
	}
}

func TestTurnOrder_SkipsEliminated(t *testing.T) {
	g := newTestGame(t, WindowSettings{}, []SeatType{SeatHuman, SeatLLM, SeatLLM})
	setHands(t, g, [][]Character{
		{Duke, Captain},
		{Assassin, Contessa}, // B eliminated below (1 down + 1 up)
		{Ambassador, Duke},
	})
	dropToFaceUp(t, g, 1)
	g.TurnSeatIndex = 0

	// A takes income: turn must skip the one-card-less... B is still active (1 card).
	// Coup B to eliminate, then check ordering.
	g.Seats[0].Coins = 9
	if _, err := g.DeclareTurnAction(g.Seats[0].ID, Action{Kind: Coup, TargetID: g.Seats[1].ID}); err != nil {
		t.Fatalf("coup: %v", err)
	}
	if !g.Seats[1].Eliminated() {
		t.Fatal("B should be eliminated")
	}
	// Turn is now with C (skipped B), then back to A.
	if g.TurnSeatIndex != 2 || g.TurnID != 2 {
		t.Fatalf("turn after elimination = seat %d turn %d, want seat 2 turn 2", g.TurnSeatIndex, g.TurnID)
	}
	if _, err := g.DeclareTurnAction(g.Seats[2].ID, Action{Kind: Income}); err != nil {
		t.Fatalf("C income: %v", err)
	}
	if g.TurnSeatIndex != 0 || g.TurnID != 3 {
		t.Fatalf("turn after C = seat %d turn %d, want seat 0 turn 3 (wrapped past eliminated B)", g.TurnSeatIndex, g.TurnID)
	}

	// Eliminated seats cannot act.
	if _, err := g.DeclareTurnAction(g.Seats[1].ID, Action{Kind: Income}); err == nil {
		t.Fatal("eliminated seat may act")
	} else {
		mustLegacy(t, err, CodeEliminated, "you are eliminated — you cannot act")
	}
}

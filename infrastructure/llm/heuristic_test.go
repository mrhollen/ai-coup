package llm

import (
	"context"
	"testing"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
)

func TestHeuristic_NameAndCapability(t *testing.T) {
	h := NewHeuristicDriver()
	if got := h.Name(); got != "heuristic" {
		t.Fatalf("Name() = %q, want heuristic", got)
	}
	var lc app.LLMCapable = h
	if !lc.CanDriveLLM() {
		t.Fatal("CanDriveLLM() = false, want true (heuristic is real play, §6.2)")
	}
}

// turnWorld builds a minimal, legal-looking WorldView: seat s_1 ("you") with
// the given purse/hand, followed by the listed opponent seats.
func turnWorld(youCoins int, youCards []domain.Character, opponents ...app.SeatView) app.DecisionInput {
	seats := []app.SeatView{{ID: "s_1", Name: "You", Type: "llm", Coins: youCoins, FaceUp: nil, Active: true}}
	for _, o := range opponents {
		if o.ID == "" {
			o = app.SeatView{ID: "s_x", Name: "Opp", Type: "llm", Active: true}
		}
		seats = append(seats, o)
	}
	return app.DecisionInput{
		GameID: "g_test",
		SeatID: "s_1",
		Kind:   app.KindTurnAction,
		World:  app.WorldView{TurnID: 5, DeckSize: 9, You: seats[0], YourCards: youCards, Seats: seats},
	}
}

func richOpp(id string, coins int) app.SeatView {
	return app.SeatView{ID: id, Name: "Opp", Type: "llm", Coins: coins, Active: true}
}

var twoCards = []domain.Character{domain.Assassin, domain.Captain}

// TestHeuristic_DeterministicPerGame pins A7's replayability contract: two
// fresh drivers make IDENTICAL candidate sequences for the same game id (the
// streams are seeded from the id hash), and a different game id diverges.
func TestHeuristic_DeterministicPerGame(t *testing.T) {
	h1, h2, other := NewHeuristicDriver(), NewHeuristicDriver(), NewHeuristicDriver()
	ctx := context.Background()
	opponents := []app.SeatView{richOpp("s_2", 3), richOpp("s_3", 7), app.SeatView{ID: "s_4", Name: "Poor", Type: "llm", Active: true}}

	sameDiverge := 0
	for k := 0; k < 500; k++ {
		in := turnWorld(k%12, twoCards, opponents...) // purse sweeps 0..11 across the run
		a, err := h1.Decide(ctx, in)
		b, err := h2.Decide(ctx, in)
		if err != nil || a != b {
			t.Fatalf("draw %d: same game id diverged (a=%+v b=%+v err=%v)", k, a, b, err)
		}
		inOther := in
		inOther.GameID = "g_other"
		c, _ := other.Decide(ctx, inOther)
		if c != a {
			sameDiverge++
		}
	}
	if sameDiverge == 0 {
		t.Fatal("different game id never diverged over 500 draws — the per-game seed is not working")
	}
}

// TestHeuristic_MandatoryCoup: at ≥10 coins every candidate is a coup against
// an active opponent (the engine would reject anything else); below the
// threshold coup never appears.
func TestHeuristic_MandatoryCoup(t *testing.T) {
	h := NewHeuristicDriver()
	ctx := context.Background()
	for _, coins := range []int{10, 15, 50} {
		in := turnWorld(coins, twoCards, richOpp("s_2", 1), richOpp("s_3", 0))
		c, err := h.Decide(ctx, in)
		if err != nil {
			t.Fatalf("coins=%d: %v", coins, err)
		}
		if c.Action != "coup" || (c.TargetID != "s_2" && c.TargetID != "s_3") {
			t.Errorf("coins=%d: got %+v, want coup on an active opponent", coins, c)
		}
	}
	for k := 0; k < 300; k++ {
		c, _ := h.Decide(ctx, turnWorld(9, twoCards, richOpp("s_2", 1)))
		if c.Action == "coup" {
			t.Fatalf("draw %d at 9 coins: coup is not allowed below the mandatory threshold", k)
		}
	}
}

// TestHeuristic_TurnPolicyAlwaysLegal replays many draws over a sweep of world
// shapes and asserts every candidate respects the engine's turn rules (target
// validity, claim vocabulary per action kind, mandatory-coup precondition) —
// so no heuristic candidate ever consumes an A4 retry.
func TestHeuristic_TurnPolicyAlwaysLegal(t *testing.T) {
	h := NewHeuristicDriver()
	ctx := context.Background()

	cases := []app.DecisionInput{
		turnWorld(0, twoCards, richOpp("s_2", 0), app.SeatView{ID: "s_3", Name: "Gone", Type: "llm"}),
		turnWorld(1, twoCards, richOpp("s_2", 5)),
		turnWorld(4, []domain.Character{domain.Duke}, richOpp("s_2", 0)), // one-card hand
		turnWorld(9, twoCards, richOpp("s_2", 3), richOpp("s_3", 1), richOpp("s_4", 0)),
		turnWorld(10, twoCards, richOpp("s_2", 2)),                        // mandatory coup
		turnWorld(11, []domain.Character{domain.Duke}, richOpp("s_2", 9)), // mandatory coup, one card left
	}
	claims := map[string]map[domain.Character]bool{
		"tax":         {domain.Duke: true},
		"steal":       {domain.Captain: true, domain.Ambassador: true},
		"assassinate": {domain.Assassin: true},
		"exchange":    {domain.Ambassador: true},
	}
	for ci, in := range cases {
		coinsOf := map[string]int{}
		for _, s := range in.World.Seats {
			coinsOf[s.ID] = s.Coins
		}
		for k := 0; k < 400; k++ {
			c, err := h.Decide(ctx, in)
			if err != nil {
				t.Fatalf("case %d draw %d: %v", ci, k, err)
			}
			kind, ok := domain.ActionKindFromName(c.Action)
			if !ok {
				t.Fatalf("case %d draw %d: unknown action %q", ci, k, c.Action)
			}
			if kind.RequiresTarget() && (c.TargetID == "" || c.TargetID == in.SeatID) {
				t.Fatalf("case %d draw %d: %s without a valid target: %+v", ci, k, c.Action, c)
			}
			if kind == domain.Coup && in.World.You.Coins < domain.MandatoryCoupThreshold {
				t.Fatalf("case %d draw %d: coup below the mandatory threshold (9 or fewer coins)", ci, k)
			}
			if kind == domain.Steal && coinsOf[c.TargetID] < 1 {
				t.Fatalf("case %d draw %d: steal from a broke seat %s (the engine would reject it)", ci, k, c.TargetID)
			}
			if opts := kind.ClaimOptions(); opts != nil {
				ch, ok := domain.CharacterFromName(c.Character)
				if !ok || !claims[c.Action][ch] {
					t.Fatalf("case %d draw %d: %s claimed invalid character %q", ci, k, c.Action, c.Character)
				}
			} else if c.Character != "" {
				t.Fatalf("case %d draw %d: %s carries a claim but has none by rule", ci, k, c.Action)
			}
			if kind == domain.Exchange && len(in.World.YourCards) != 2 {
				t.Fatalf("case %d draw %d: exchange without a two-card hand", ci, k)
			}
		}
	}
}

// TestHeuristic_WindowsAlwaysPass pins the A7 window policy that guarantees
// unattended termination.
func TestHeuristic_WindowsAlwaysPass(t *testing.T) {
	h := NewHeuristicDriver()
	in := app.DecisionInput{
		GameID: "g_test", SeatID: "s_1", Kind: app.KindWindowResponse,
		Request: app.Request{WindowID: "w_1", TriggerSummary: "Vito claims Assassin against you", EligibleActions: []string{"pass", "counterclaim_captain", "challenge"}},
	}
	for k := 0; k < 20; k++ {
		c, err := h.Decide(context.Background(), in)
		if err != nil {
			t.Fatalf("draw %d: %v", k, err)
		}
		if c.Response != "pass" || c.Character != "" {
			t.Fatalf("draw %d: window response = %+v, want plain pass (A7)", k, c)
		}
	}
}

// TestHeuristic_ChoiceDefaults pins the A4 single-seat defaults.
func TestHeuristic_ChoiceDefaults(t *testing.T) {
	h := NewHeuristicDriver()
	flip := app.DecisionInput{GameID: "g_test", SeatID: "s_1", Kind: app.KindSingleSeatChoice, Request: app.Request{ChoiceKind: "flip_choice", Options: []string{"face_down_0", "face_down_1"}}}
	c, err := h.Decide(context.Background(), flip)
	if err != nil || c.Choice != "face_down_0" {
		t.Fatalf("flip choice = %+v (err %v), want first option face_down_0", c, err)
	}
	exch := app.DecisionInput{GameID: "g_test", SeatID: "s_1", Kind: app.KindSingleSeatChoice, Request: app.Request{ChoiceKind: "exchange_finish"}}
	c, err = h.Decide(context.Background(), exch)
	if err != nil || c.Choice != "no_swap" {
		t.Fatalf("exchange finish = %+v (err %v), want no_swap", c, err)
	}
	empty := app.DecisionInput{GameID: "g_test", SeatID: "s_1", Kind: app.KindSingleSeatChoice, Request: app.Request{}}
	c, _ = h.Decide(context.Background(), empty)
	if c.Choice != "no_swap" {
		t.Fatalf("untyped choice = %q, want the no-swap side default (the kind is never untyped in practice)", c.Choice)
	}
}

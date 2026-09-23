package application

import (
	mathrand "math/rand"
	"strconv"
	"testing"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// kindTestIDs is a throwaway domain.IDGen for direct domain.Game construction
// in these unit tests (the harness's seqIDs lives in the external test package).
type kindTestIDs struct{ n int }

func (g *kindTestIDs) New(kind string) string {
	g.n++
	return "k_" + kind + "_" + strconv.Itoa(g.n)
}

// TestKindSafeTurnActionAtMandatoryThreshold is the focused boundary test for
// the app-level kind-safe turn default (rules.md Ruling 1): a seat at EXACTLY
// 10 coins must get a coup as its default — and that default must be LEGAL
// per engine validation against the first active non-self seat; one coin below
// the threshold the default stays income. An eliminated early seat is skipped.
func TestKindSafeTurnActionAtMandatoryThreshold(t *testing.T) {
	newGame := func() *domain.Game {
		g, _, err := domain.NewGame("g_kinddefault", []domain.SeatSpec{
			{ID: "s_aaaaaaaaaaaa", Name: "A", Type: domain.SeatHuman},
			{ID: "s_bbbbbbbbbbbb", Name: "B", Type: domain.SeatLLM},
			{ID: "s_cccccccccccc", Name: "C", Type: domain.SeatLLM},
		}, domain.WindowSettings{}, &kindTestIDs{}, mathrand.New(mathrand.NewSource(7)))
		if err != nil {
			t.Fatalf("NewGame: %v", err)
		}
		return g
	}

	firstActiveNonSelf := func(g *domain.Game, self string) string {
		for _, s := range g.Seats {
			if s.ID != self && s.Active() {
				return s.ID
			}
		}
		t.Fatalf("no active non-self seat found (game already won?)")
		return ""
	}

	t.Run("exactly_10_coins_is_a_legal_coup_at_the_first_active_nonself_seat", func(t *testing.T) {
		g := newGame()
		actor := &g.Seats[g.TurnSeatIndex]          // who holds the turn is rng-chosen; legality is per-actor
		actor.Coins = domain.MandatoryCoupThreshold // exactly 10

		a, name := kindSafeTurnAction(g, actor.ID)
		if name != "coup" || a.Kind != domain.Coup {
			t.Fatalf("default at %d coins = %+v/%q, want coup", actor.Coins, a, name)
		}
		if want := firstActiveNonSelf(g, actor.ID); a.TargetID != want {
			t.Fatalf("coup target = %s, want first active non-self seat %s", a.TargetID, want)
		}
		if err := g.ValidateAction(actor.ID, a); err != nil {
			t.Fatalf("the mandatory-coup default must pass engine validation: %v", err)
		}
	})

	t.Run("one_coin_below_threshold_stays_income", func(t *testing.T) {
		g := newGame()
		actor := &g.Seats[g.TurnSeatIndex]
		actor.Coins = domain.MandatoryCoupThreshold - 1 // exactly 9

		a, name := kindSafeTurnAction(g, actor.ID)
		if name != "income" || a.Kind != domain.Income {
			t.Fatalf("default at %d coins = %+v/%q, want income", actor.Coins, a, name)
		}
		if err := g.ValidateAction(actor.ID, a); err != nil {
			t.Fatalf("income default must pass engine validation: %v", err)
		}
	})

	t.Run("eliminated_seat_is_not_a_coup_target", func(t *testing.T) {
		g := newGame()
		actor := &g.Seats[g.TurnSeatIndex]
		victim := firstActiveNonSelf(g, actor.ID)
		for i := range g.Seats {
			if g.Seats[i].ID == victim {
				g.Seats[i].FaceDown = nil // both cards gone: eliminated
			}
		}
		actor.Coins = domain.MandatoryCoupThreshold

		a, name := kindSafeTurnAction(g, actor.ID)
		if name != "coup" || a.Kind != domain.Coup {
			t.Fatalf("default at %d coins = %+v/%q, want coup", actor.Coins, a, name)
		}
		if a.TargetID == victim {
			t.Fatalf("coup targeted the eliminated seat %s", victim)
		}
		if err := g.ValidateAction(actor.ID, a); err != nil {
			t.Fatalf("default targeting %s must pass engine validation: %v", a.TargetID, err)
		}
	})
}

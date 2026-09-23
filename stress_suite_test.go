package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand"
	"sync"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
	llm "forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
	store "forgejo.lan/mrhollen/ai-coup/infrastructure/store"
	clock "forgejo.lan/mrhollen/ai-coup/infrastructure/time"
)

// stressSeqIDs is a deterministic IDGen (kind + zero-padded counter, same shape
// as the application test harness) so game i always receives the same g_/s_ ids.
type stressSeqIDs struct {
	mu sync.Mutex
	n  map[string]int
}

func newStressSeqIDs() *stressSeqIDs { return &stressSeqIDs{n: map[string]int{}} }

func (g *stressSeqIDs) New(kind string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n[kind]++
	return fmt.Sprintf("%s_%012d", kind, g.n[kind])
}

var _ app.IDGen = (*stressSeqIDs)(nil)

// TestStressUnattendedHeuristicGames is the A13/§14.3 no-permadeath stress
// suite: 50 unattended full games with seat counts varying 2..6 across runs,
// every game mixing one scripted "human" auto-client with the remaining seats
// driven by the REAL heuristic MoveDriver (spec §14.3 names the mix explicitly
// because a game needs at least one human seat, and an FCFS no-timeout window
// only terminates when EVERY eligible responder answers — which the heuristic
// always-pass policy and this auto client both guarantee). Window settings are
// require_all=true with NO timeout, exactly as A13 prescribes.
//
// Assertions per game: it reaches status finished WITH a winner within a
// 2-minute wall budget (a hung window or stalled turn trips the budget); the
// domain invariant checker has run after every transition throughout (the
// engine's batchWith panics on any violation, so reaching finished is itself
// the per-transition proof). Suite assertion: total wall < 10 minutes.
//
// Determinism / non-flakiness: one shared sequential ID generator across all
// games, a per-game seeded deck RNG, and heuristic streams seeded from the
// game id — the identical 50 trajectories replay on every run. No fixed port
// is ever bound or dialed (pure in-process use-case calls; §14.3 discipline).
func TestStressUnattendedHeuristicGames(t *testing.T) {
	const total = 50
	perGameBudget := 2 * time.Minute // A13
	suiteStart := time.Now()

	ids := newStressSeqIDs() // shared: game i always gets the same deterministic ids
	for i := 0; i < total; i++ {
		seats := 2 + i%5 // cycle 2,3,4,5,6 across runs (§14.3 "varying seat counts")

		a := app.NewApplication(app.Dependencies{
			Events: store.NewMemoryEventStore(clock.NewWallClock()),
			Games:  store.NewMemoryGameStore(),
			Clock:  clock.NewWallClock(),
			IDs:    ids,
			Driver: llm.NewHeuristicDriver(),
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Rng:    mathrand.New(mathrand.NewSource(int64(1000 + i))), // per-game deck seed
		})

		res, err := a.CreateGame(context.Background(), app.CreateGameRequest{
			Players:           stressPlayers(seats), // seat 0 is the scripted human; the rest LLM
			RequireAll:        true,                 // A13
			ResponseTimeoutMs: 0,                    // A13: no timeout — termination must not depend on one
		})
		if err != nil {
			t.Fatalf("game %d/%d create (%d seats): %v", i+1, total, seats, err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		go stressAutoClient(ctx, t, a, res.GameID, res.Seats[0].ID)
		st, ferr := stressWaitForFinished(t, a, res.GameID, perGameBudget)
		cancel()                          // stops the auto client (ctx wakes any WaitForEvent)
		if err := a.Close(); err != nil { // §9 shutdown point: cancels in-flight decisions
			t.Fatalf("game %d/%d close: %v", i+1, total, err)
		}

		switch {
		case ferr != nil:
			t.Fatalf("game %d/%d (%d seats): %v — no-permeadeath violation (A13)", i+1, total, seats, ferr)
		case st.WinnerID == nil:
			t.Fatalf("game %d/%d finished without a winner", i+1, total)
		default:
			if !stressSeatHas(res, *st.WinnerID) {
				t.Fatalf("game %d/%d: winner %s is not one of the seats %v", i+1, total, *st.WinnerID, res.Seats)
			}
			t.Logf("game %2d/%d seats=%d finished winner=%s turn_id=%d", i+1, total, seats, *st.WinnerID, st.TurnID)
		}
	}

	wall := time.Since(suiteStart)
	if wall > 10*time.Minute {
		t.Errorf("suite wall %s exceeds the 10-minute budget (A13)", wall)
	}
	t.Logf("%d unattended heuristic games finished in %s", total, wall.Round(time.Millisecond))
}

// stressPlayers builds one human seat followed by seats-1 LLM seats (blank
// names take the A14 defaults).
func stressPlayers(seats int) []app.PlayerSpec {
	out := make([]app.PlayerSpec, 0, seats)
	out = append(out, app.PlayerSpec{Name: "", Type: "human"})
	for i := 1; i < seats; i++ {
		out = append(out, app.PlayerSpec{Name: "", Type: "llm"})
	}
	return out
}

func stressSeatHas(res *app.CreateGameResult, id string) bool {
	for _, s := range res.Seats {
		if s.ID == id {
			return true
		}
	}
	return false
}

// stressWaitForFinished polls DebugGame (the locked state view — safe against
// concurrent driver application) until the game finishes or the budget trips.
func stressWaitForFinished(t *testing.T, a *app.Application, gameID string, budget time.Duration) (*app.DebugState, error) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		st, err := a.DebugGame(gameID)
		if err != nil {
			return nil, fmt.Errorf("debug view: %w", err)
		}
		if st.Status == "finished" {
			return st, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, fmt.Errorf("did not reach finished within %s (hung window or stalled turn)", budget)
}

// stressAutoClient is the scripted "human" of §14.3: it consumes ONLY public
// use cases (GetSnapshot + SubmitDecision + WaitForEvent — exactly what a real
// client can do), never the aggregate directly. Policy mirrors the spec's bot
// so the game provably terminates: >=10 coins => coup (mandatory, else every
// other act is rejected and this loop would stall), income otherwise; windows
// get a pass exactly once per window; single-seat decisions take the engine
// defaults (first flip option / no-swap). It waits on the event store between
// actions — no busy polling, no fixed cadence.
func stressAutoClient(ctx context.Context, t *testing.T, a *app.Application, gameID, seatID string) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		snap, err := a.GetSnapshot(ctx, gameID, seatID)
		if err != nil {
			return // app closed (shutdown); the main test owns failure reporting
		}
		if snap.Status == "finished" {
			return
		}

		var submitted bool
		var subErr error
		switch cur := snap.Current; {
		case cur != nil && cur.Phase == "turn_action" && cur.TurnSeatID == seatID:
			submitted, subErr = stressAct(ctx, t, a, gameID, seatID, snap)
		case cur != nil && cur.Phase == "window_open" && len(cur.Window.YourAllowedResponses) > 0 && !stressAlreadyResponded(snap, seatID):
			var passResult *app.SubmitDecisionResult
			passResult, subErr = a.SubmitDecision(ctx, gameID, seatID, app.DecisionIn{
				Kind:     app.DecisionKindRespond,
				Response: &app.ResponseIn{Type: "pass"},
			})
			submitted = passResult != nil
		case cur != nil && cur.Phase == "single_seat_decision" && cur.Decision != nil && cur.Decision.SeatID == seatID:
			var chooseResult *app.SubmitDecisionResult
			chooseResult, subErr = a.SubmitDecision(ctx, gameID, seatID, app.DecisionIn{
				Kind:       app.DecisionKindChoose,
				DecisionID: cur.Decision.ID,
				Choice:     stressDefaultChoice(cur.Decision.Kind),
			})
			submitted = chooseResult != nil
		}

		if subErr != nil {
			// A semantic rejection here would be a protocol mistake in THIS
			// auto client (heuristic seats are driven separately and never go
			// through SubmitDecision). Report it but keep looping — the per-game
			// wall budget converts any real stall into a test failure.
			t.Errorf("stress auto-client %s in game %s: unexpected rejection: %v", seatID, gameID, subErr)
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if !submitted {
			// Nothing owed to us: block until the table changes. WaitForEvent's
			// check-then-register is atomic in the store, so no wake-up is lost.
			if werr := a.WaitForEvent(ctx, gameID, snap.LastEventSeq); werr != nil && ctx.Err() == nil {
				t.Errorf("stress auto-client %s: WaitForEvent: %v", seatID, werr)
				return
			}
		}
	}
}

// stressAct picks the auto client's turn action from the snapshot alone: a
// coup (mandatory at >=10 coins — targeting the first active opponent, which
// always exists while the game is unfinished and this seat holds the turn) or
// income.
func stressAct(ctx context.Context, t *testing.T, a *app.Application, gameID, seatID string, snap *app.Snapshot) (bool, error) {
	t.Helper()
	myCoins := 0
	for _, s := range snap.Seats {
		if s.ID == seatID {
			myCoins = s.Coins
		}
	}
	if myCoins >= domain.MandatoryCoupThreshold {
		for _, s := range snap.Seats {
			if s.Active && s.ID != seatID {
				_, err := a.SubmitDecision(ctx, gameID, seatID, app.DecisionIn{
					Kind:   app.DecisionKindAct,
					Action: &app.ActionIn{Type: "coup", TargetID: s.ID},
				})
				return true, err
			}
		}
	}
	_, err := a.SubmitDecision(ctx, gameID, seatID, app.DecisionIn{Kind: app.DecisionKindAct, Action: &app.ActionIn{Type: "income"}})
	return true, err
}

// stressDefaultChoice is the auto client's single-seat answer: the A4 engine
// defaults (first flip option / no-swap), in the wire shapes.
func stressDefaultChoice(kind string) *app.ChoiceIn {
	if kind == "flip_choice" {
		return &app.ChoiceIn{FlipChoice: "face_down_0"}
	}
	return &app.ChoiceIn{ExchangeFinish: &app.ExchangeFinishIn{NoSwap: true}}
}

// stressAlreadyResponded reports whether this seat already answered the open
// window (responses are per-seat once, A16 — no duplicate passes).
func stressAlreadyResponded(snap *app.Snapshot, seatID string) bool {
	if snap.Current == nil || snap.Current.Window == nil {
		return false
	}
	_, ok := snap.Current.Window.ResponsesSoFar[seatID]
	return ok
}

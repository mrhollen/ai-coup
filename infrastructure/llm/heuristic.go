package llm

import (
	"context"
	"hash/fnv"
	"math/rand"
	"sync"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
)

// HeuristicDriver is the deterministic built-in strategy of spec §6.2 [A7].
// It claims LLMCapable so games containing LLM seats run with it, and its
// randomness is seeded from a hash of the game id (mixed with the seat id), so
// the same game id replays identically — unattended simulated games are
// replayable (§14.3/A13).
//
// Policy (deliberately simple; this comment IS the documentation):
//
//   - Turn action: coins ≥ 10 ⇒ coup a uniformly random active opponent
//     (mandatory at that purse anyway). Otherwise with weight 60% take free
//     money — income, foreign_aid, or tax (claiming duke) chosen uniformly —
//     and with weight 40% one of the other legal character actions available
//     to this seat: steal from a seat holding at least one coin (claiming
//     captain) or exchange (claiming ambassador), picked uniformly among the
//     available ones. If neither is available (one-card hand against all-
//     broke opponents) fall back to free money. Targets are uniform over the
//     valid seats. Claims are asserted without checking hand contents —
//     bluffs included; windows decide.
//   - Response windows: always pass. This is what makes require-all games
//     terminate under unattended simulation (§6.2/A7) and the documented
//     reason an FCFS no-timeout window must not be used for unattended play.
//   - Single-seat choices: the engine defaults (A4) — first flip option,
//     no-swap exchange finish.
type HeuristicDriver struct {
	mu   sync.Mutex
	rngs map[string]*rand.Rand // gameID+seatID → that seat's private stream
}

// NewHeuristicDriver builds a fresh driver; per-game streams are created lazily
// on first use and persist for the driver's lifetime (bounded by the number of
// seats ever dispatched — negligible).
func NewHeuristicDriver() *HeuristicDriver {
	return &HeuristicDriver{rngs: map[string]*rand.Rand{}}
}

var _ app.MoveDriver = (*HeuristicDriver)(nil)
var _ app.LLMCapable = (*HeuristicDriver)(nil)

// Name implements app.MoveDriver.
func (h *HeuristicDriver) Name() string { return "heuristic" }

// CanDriveLLM implements app.LLMCapable (§6.2): heuristic play is real play,
// so LLM seats are admitted at game creation when this driver is configured.
func (h *HeuristicDriver) CanDriveLLM() bool { return true }

// Decide implements app.MoveDriver: one pure policy evaluation per input. The
// context is unused (no I/O), and no error is ever returned — every candidate
// the policy can emit is parseable by the application layer, and kind-safety
// of the fallbacks is guaranteed by construction.
func (h *HeuristicDriver) Decide(_ context.Context, in app.DecisionInput) (app.Candidate, error) {
	rng := h.rngFor(in.GameID, in.SeatID)
	switch in.Kind {
	case app.KindTurnAction:
		return turnCandidate(rng, in), nil
	case app.KindWindowResponse:
		return app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}, nil
	default: // KindSingleSeatChoice
		return choiceDefault(in.Request), nil
	}
}

// rngFor returns the seat's private random stream, seeded deterministically
// from the game id and seat id. Two fresh drivers therefore make identical
// sequences of draws for the same (game, seat), which is what makes a replayed
// game id reproduce the same play.
func (h *HeuristicDriver) rngFor(gameID, seatID string) *rand.Rand {
	key := gameID + "\x1f" + seatID
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rngs[key]; ok {
		return r
	}
	f := fnv.New64a()
	f.Write([]byte(key))
	r := rand.New(rand.NewSource(int64(f.Sum64())))
	h.rngs[key] = r
	return r
}

// turnCandidate picks this seat's declared action for its turn (A7 policy).
func turnCandidate(rng *rand.Rand, in app.DecisionInput) app.Candidate {
	me := in.World.You
	opponents := activeOpponents(in.World)
	if len(opponents) == 0 { // degenerate: nobody left to interact with
		return app.Candidate{Kind: app.KindTurnAction, Action: "income"}
	}
	// Mandatory coup first: at ≥10 coins every other action is rejected by the
	// engine ("you must take a coup…").
	if me.Coins >= domain.MandatoryCoupThreshold {
		return app.Candidate{Kind: app.KindTurnAction, Action: "coup", TargetID: opponents[rng.Intn(len(opponents))]}
	}
	if rng.Float64() < 0.6 { // free money (A7): one of the three zero-risk earners
		switch rng.Intn(3) {
		case 0:
			return app.Candidate{Kind: app.KindTurnAction, Action: "income"}
		case 1:
			return app.Candidate{Kind: app.KindTurnAction, Action: "foreign_aid"}
		default:
			return app.Candidate{Kind: app.KindTurnAction, Action: "tax", Character: "duke"}
		}
	}
	// 40% branch (A7): steal a coin-rich seat or exchange; uniform over the
	// available actions.
	var pool []app.Candidate
	rich := opponentsWithCoins(in.World, opponents)
	if len(rich) > 0 {
		pool = append(pool, app.Candidate{Kind: app.KindTurnAction, Action: "steal", TargetID: rich[rng.Intn(len(rich))], Character: "captain"})
	}
	if len(in.World.YourCards) == 2 { // exchange needs a two-card hand
		pool = append(pool, app.Candidate{Kind: app.KindTurnAction, Action: "exchange", Character: "ambassador"})
	}
	if len(pool) == 0 { // degenerate: fall back to free money
		return app.Candidate{Kind: app.KindTurnAction, Action: "income"}
	}
	return pool[rng.Intn(len(pool))]
}

// choiceDefault answers a single-seat decision with the A4 engine defaults:
// first flip option (positional), no-swap exchange finish. An untyped kind
// (never produced by the application dispatcher) falls to the no-swap side —
// worst case one feedback retry, and the app's own A4 default is the final
// backstop either way.
func choiceDefault(req app.Request) app.Candidate {
	switch req.ChoiceKind {
	case "flip_choice":
		opt := "face_down_0"
		if len(req.Options) > 0 {
			opt = req.Options[0]
		}
		return app.Candidate{Kind: app.KindSingleSeatChoice, Choice: opt}
	default: // exchange_finish (and untyped)
		return app.Candidate{Kind: app.KindSingleSeatChoice, Choice: "no_swap"}
	}
}

// activeOpponents lists every active seat other than the driver's own, in seat
// order (visibility-law safe: activeness is public, §8).
func activeOpponents(w app.WorldView) []string {
	var out []string
	for _, s := range w.Seats {
		if s.Active && s.ID != w.You.ID {
			out = append(out, s.ID)
		}
	}
	return out
}

// opponentsWithCoins filters the active opponents down to seats holding at
// least one coin — the only legal steal targets (engine: "that player has no
// coins to steal from").
func opponentsWithCoins(w app.WorldView, opponents []string) []string {
	var out []string
	coins := map[string]int{}
	for _, s := range w.Seats {
		coins[s.ID] = s.Coins
	}
	for _, id := range opponents {
		if coins[id] >= 1 {
			out = append(out, id)
		}
	}
	return out
}

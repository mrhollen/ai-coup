package domain

import "fmt"

// WindowKind is the flavour of a response window (spec §10.1 `kind`).
type WindowKind int

const (
	// WindowChallenge is the window guarding an offensive claim (a claimed
	// turn action) or — for Foreign Aid, which claims no character — the
	// untargeted block window against it.
	WindowChallenge WindowKind = iota
	// WindowCounterclaimChallenge is the nested window opened when a blocking
	// counterclaim must itself survive challenge before the block takes effect
	// (spec §4.1/§5.2). It replaces the attack window in the state machine and
	// never stacks with it (§5.3).
	WindowCounterclaimChallenge
)

func (k WindowKind) String() string {
	if k == WindowChallenge {
		return "challenge"
	}
	return "counterclaim_challenge"
}

// WindowCloseReason is the recorded reason a window closed (spec §5.2.5).
type WindowCloseReason int

const (
	CloseFirstResponse    WindowCloseReason = iota // (a) first non-PASS response
	CloseAllResponded                              // (b) every eligible seat responded (require-all)
	CloseTimeout                                   // (c) response timeout expired
	CloseAutoNoResponders                          // elimination/forfeit left no eligible responders (§7)
)

func (r WindowCloseReason) String() string {
	switch r {
	case CloseFirstResponse:
		return "first_response"
	case CloseAllResponded:
		return "all_responded"
	case CloseTimeout:
		return "timeout"
	default:
		return "auto_no_responders"
	}
}

// WindowSettings are the game-creator-chosen window settings (spec §5.2.5,
// defaults A8). Durations are plain values — no time.Time in the domain (§2).
type WindowSettings struct {
	RequireAllResponses bool
	TimeoutMs           int // 0 = no timeout
}

// ResponseKind is one grammar token a seat may play inside a window.
type ResponseKind int

const (
	RespPass ResponseKind = iota
	RespCounterclaim
	RespChallenge
)

func (k ResponseKind) String() string {
	switch k {
	case RespPass:
		return "pass"
	case RespCounterclaim:
		return "counterclaim"
	default:
		return "challenge"
	}
}

// WindowResponse is a single seat's response to the open window. Character is
// meaningful only for counterclaims (the character claimed as the block).
type WindowResponse struct {
	Kind      ResponseKind
	Character Character
}

// ClaimRole tells whose claim an open window examines.
type ClaimRole int

const (
	// ClaimOffensive: the window guards a claimed turn action; its claimant is
	// the acting seat and its resolution decides whether the action's effects
	// run.
	ClaimOffensive ClaimRole = iota
	// ClaimDefensive: the window guards a blocking counterclaim (nested); its
	// claimant is the blocker and its resolution decides whether the original
	// attack fizzles or proceeds.
	ClaimDefensive
)

// openWindow is the single, innermost response window of a game (§5.3). It is
// self-contained: when a blocking counterclaim replaces the current window,
// everything the outer cascade still needs is carried in GuardAction/BlockerID,
// which is exactly what makes "no superseded claim ever reopens" hold — there
// is no stored frame to return to, only the guard.
type openWindow struct {
	ID         string // w_… (spec §13)
	Kind       WindowKind
	ClaimantID string // seat whose claim this window examines
	ClaimChar  Character
	TargetID   string // target of the guarded offensive attack; "" when untargeted

	Eligible  []string                  // eligible seat ids, in seat order (§5.1)
	Responded map[string]WindowResponse // responses so far (each seat at most once, A16)
	Settings  WindowSettings            // snapshot of the game settings at open
	DeciderID string                    // set on close: first non-PASS seat ("" for b/c/d closes)

	GuardRole   ClaimRole // offensive | defensive
	GuardAction Action    // the action whose fate this window decides
	ActorID     string    // actor of GuardAction (offensive: the claimant; defensive guard: the ORIGINAL attacker, for fizzle attribution)
	BlockerID   string    // defensive guard only: who counterclaimed

	// ActionSeq is the seq of GuardAction's action_declared event — carried so
	// nested windows and fizzle events can reference it (§10.1 trigger /
	// action_fizzled.action_seq).
	ActionSeq int64
}

// isEligible reports whether the seat may respond in this window.
func (w *openWindow) isEligible(seatID string) bool {
	for _, id := range w.Eligible {
		if id == seatID {
			return true
		}
	}
	return false
}

// responseAllowed applies the per-seat grammar of §5.1 (the window "shape").
func (w *openWindow) responseAllowed(seatID string, r WindowResponse) bool {
	switch w.Kind {
	case WindowChallenge:
		switch w.GuardAction.Kind {
		case ForeignAid:
			// No direct challenge exists for Foreign Aid (§5.1): every
			// eligible seat may only pass or counterclaim Duke.
			return r.Kind == RespPass || (r.Kind == RespCounterclaim && r.Character == Duke)
		case Tax, Exchange:
			return r.Kind == RespPass || r.Kind == RespChallenge
		default: // attack window: steal / assassinate
			switch r.Kind {
			case RespPass, RespChallenge:
				return true
			case RespCounterclaim:
				return seatID == w.TargetID && characterIn(w.GuardAction.Kind.BlockOptions(), r.Character)
			}
		}
	case WindowCounterclaimChallenge:
		// The blocker is excluded from its own nested window by construction;
		// every remaining eligible seat may pass or challenge.
		return r.Kind == RespPass || r.Kind == RespChallenge
	}
	return false
}

// responseTokensForSeat returns the public grammar tokens for one seat — the
// same vocabulary a human sees at the table and what DecisionInput.Request
// carries (spec §2.2/§5.1). It is window mechanics, not advice.
func (w *openWindow) responseTokensForSeat(seatID string) []string {
	tokens := []string{"pass"}
	switch w.Kind {
	case WindowChallenge:
		switch w.GuardAction.Kind {
		case ForeignAid:
			tokens = append(tokens, "counterclaim_duke")
		case Tax, Exchange:
			tokens = append(tokens, "challenge")
		default: // attack window
			if seatID == w.TargetID {
				for _, c := range w.GuardAction.Kind.BlockOptions() {
					tokens = append(tokens, "counterclaim_"+c.String())
				}
			}
			tokens = append(tokens, "challenge")
		}
	case WindowCounterclaimChallenge:
		tokens = append(tokens, "challenge")
	}
	return tokens
}

// allEligibleResponded reports whether every eligible seat has responded —
// termination (b) when RequireAllResponses is set (§5.2.5). Passes count.
func (w *openWindow) allEligibleResponded() bool {
	for _, id := range w.Eligible {
		if _, ok := w.Responded[id]; !ok {
			return false
		}
	}
	return true
}

// --- Single-seat decisions (spec §5.4) ------------------------------------

// DecisionKind is the kind of pending single-seat decision.
type DecisionKind int

const (
	// DecisionFlipChoice: a seat with exactly two face-down cards loses
	// influence and chooses which flips (positional options, §5.4).
	DecisionFlipChoice DecisionKind = iota
	// DecisionExchangeFinish: the exchanging actor chooses keep-nothing vs
	// swap one drawn card for one hand card (§11.2(a)-(c)).
	DecisionExchangeFinish
)

func (k DecisionKind) String() string {
	if k == DecisionFlipChoice {
		return "flip_choice"
	}
	return "exchange_finish"
}

// Choice is an answer to a single-seat decision. Flip choices use the
// positional option identifier ("face_down_0" | "face_down_1", §5.4);
// exchange finish mirrors the three wire outcomes of §11.2(a)-(c).
type Choice struct {
	FlipOption       string // flip choice: one of the option identifiers
	ExchangeNoSwap   bool   // (a) no swap: both drawn cards return, hand unchanged
	ExchangeDrawnIdx int    // (b)/(c) which drawn card is KEPT: 0 = left, 1 = right
	ExchangeHandOpt  string // (b)/(c) which face-down hand card returns ("face_down_i")
}

// pendingDecision is the single pending single-seat decision of a game. It
// honors the same response_timeout as windows (§5.4); when none is set it
// waits indefinitely (LLM seats always answer via driver fallbacks, §6.4).
type pendingDecision struct {
	ID         string // d_… (spec §13)
	Kind       DecisionKind
	SeatID     string // the single addressed seat
	Options    []string
	DrawnCards []Card // exchange finish only — private to the seat (§8)
	TimeoutMs  int

	// resume runs the remainder of the suspended cascade once the decision is
	// answered (or its engine default is applied on timeout, A4). It may end
	// in turn_start or game_over; if it suspends again it sets a NEW pending
	// decision itself.
	resume func(g *Game, c Choice) []Event
}

// DefaultChoice returns the engine-safe driver-fallback answer for a decision
// kind (A4): flip choice defaults to the first face-down card, exchange finish
// to no-swap.
func (k DecisionKind) DefaultChoice() Choice {
	if k == DecisionFlipChoice {
		return Choice{FlipOption: "face_down_0"}
	}
	return Choice{ExchangeNoSwap: true}
}

// flipOptionIndex parses a positional flip/exchange-hand option identifier.
func flipOptionIndex(opt string) (int, bool) {
	const prefix = "face_down_"
	if len(opt) <= len(prefix) || opt[:len(prefix)] != prefix {
		return 0, false
	}
	n := 0
	for _, r := range opt[len(prefix):] {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// --- Forfeit votes (spec §7) ------------------------------------------------

// VoteResult is the complete result vocabulary of a resolved forfeit vote
// (§7, §10.1).
const (
	VoteResultForfeited    = "forfeited"     // yes-majority; target eliminated
	VoteResultNotForfeited = "not_forfeited" // no-majority first, or exhausted without majority (tie)
	VoteResultVoided       = "voided"        // target eliminated by play while the vote was pending
)

// ForfeitVote is the out-of-band ballot that may coexist with any in-game
// structure (§3.1 note): it is NOT part of the turn/window state machine.
type ForfeitVote struct {
	ID             string // v_… (spec §13)
	TargetID       string
	InitiatorID    string
	EligibleVoters []string        // active HUMAN seats other than the target, at open (§7)
	MajorityNeeded int             // strictly more than half of len(EligibleVoters)
	Cast           map[string]bool // voter seat → yes(true)/no(false); one ballot per voter

	Result string // "" while open; VoteResult* once resolved
}

// --- Window & decision construction (bound into Game by the engine) ----

// eligibleSeats returns all active seat ids except excludeID, in seat order.
func (g *Game) eligibleSeats(excludeID string) []string {
	var out []string
	for _, s := range g.Seats {
		if s.Active() && s.ID != excludeID {
			out = append(out, s.ID)
		}
	}
	return out
}

// newOffensiveWindow builds the window guarding a claimed turn action (§5.1).
func (g *Game) newOffensiveWindow(actor *Seat, a Action, declaredSeq int64) *openWindow {
	w := &openWindow{
		ID:          g.ids.New("w"),
		Kind:        WindowChallenge,
		ClaimantID:  actor.ID,
		ActorID:     actor.ID,
		GuardAction: a,
		GuardRole:   ClaimOffensive,
		Settings:    g.Settings,
		Eligible:    g.eligibleSeats(actor.ID),
		Responded:   map[string]WindowResponse{},
		ActionSeq:   declaredSeq,
	}
	if a.Kind.ClaimOptions() != nil {
		w.ClaimChar = a.Character
	}
	if a.Kind.RequiresTarget() {
		w.TargetID = a.TargetID
	}
	return w
}

// newBlockWindow builds the nested window for a blocking counterclaim (§5.2).
// It REPLACES the current window in the state machine; the original cascade is
// carried inside its guard so nothing ever reopens (spec §5.3).
func (g *Game) newBlockWindow(original Action, blocker *Seat, char Character, outer *openWindow) *openWindow {
	w := &openWindow{
		ID:          g.ids.New("w"),
		Kind:        WindowCounterclaimChallenge,
		ClaimantID:  blocker.ID,
		BlockerID:   blocker.ID,
		ActorID:     outer.ActorID, // the ORIGINAL attacker (fizzle attribution)
		GuardAction: original,
		GuardRole:   ClaimDefensive,
		Settings:    g.Settings,
		Eligible:    g.eligibleSeats(blocker.ID),
		Responded:   map[string]WindowResponse{},
		ActionSeq:   outer.ActionSeq,
	}
	w.ClaimChar = char
	if original.Kind.RequiresTarget() {
		w.TargetID = original.TargetID
	}
	return w
}

// newDecision builds a pending single-seat decision with the game's timeout
// settings (§5.4) and its positional options.
func (g *Game) newDecision(kind DecisionKind, seatID string, options []string, drawn []Card) *pendingDecision {
	seat := g.findSeat(seatID)
	if kind == DecisionExchangeFinish && len(options) == 0 {
		for i := range seat.FaceDown {
			options = append(options, fmt.Sprintf("face_down_%d", i))
		}
	}
	return &pendingDecision{
		ID:         g.ids.New("d"),
		Kind:       kind,
		SeatID:     seatID,
		Options:    options,
		DrawnCards: drawn,
		TimeoutMs:  g.Settings.TimeoutMs,
	}
}

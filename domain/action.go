package domain

// ActionKind is one of the seven Coup (2012) actions (spec §4.1).
type ActionKind int

const (
	Income      ActionKind = iota // +1 coin, no claim, no window
	ForeignAid                    // +2 coins unless blocked by a surviving Duke counterclaim
	Coup                          // pay 7; target loses one influence; mandatory at ≥ 10 coins
	Tax                           // claim Duke; +3 coins if the claim survives
	Steal                         // claim Captain or Ambassador; take min(2, target coins)
	Assassinate                   // claim Assassin; pay 3 (sunk); target loses one influence
	Exchange                      // claim Ambassador; draw 2, optionally swap one, return 2
)

// String returns the wire name of the action (§10/§11).
func (k ActionKind) String() string {
	switch k {
	case Income:
		return "income"
	case ForeignAid:
		return "foreign_aid"
	case Coup:
		return "coup"
	case Tax:
		return "tax"
	case Steal:
		return "steal"
	case Assassinate:
		return "assassinate"
	case Exchange:
		return "exchange"
	default:
		return "invalid_action"
	}
}

// ActionKindFromName parses the wire name of an action.
func ActionKindFromName(name string) (ActionKind, bool) {
	switch name {
	case "income":
		return Income, true
	case "foreign_aid":
		return ForeignAid, true
	case "coup":
		return Coup, true
	case "tax":
		return Tax, true
	case "steal":
		return Steal, true
	case "assassinate":
		return Assassinate, true
	case "exchange":
		return Exchange, true
	default:
		return 0, false
	}
}

// ClaimOptions returns the characters that may be claimed to perform this
// action (spec §4.1). Base actions return nil: they are not claims and any
// supplied character is ignored on the wire.
func (k ActionKind) ClaimOptions() []Character {
	switch k {
	case Tax:
		return []Character{Duke}
	case Steal:
		return []Character{Captain, Ambassador}
	case Assassinate:
		return []Character{Assassin}
	case Exchange:
		return []Character{Ambassador}
	default:
		return nil
	}
}

// Cost returns the coins paid up front at declaration. For claimed actions the
// cost is sunk immediately and never refunded (spec §4.1 timing rule).
func (k ActionKind) Cost() int {
	switch k {
	case Coup:
		return 7
	case Assassinate:
		return 3
	default:
		return 0
	}
}

// RequiresTarget reports whether the action names a target seat (Coup, Steal,
// Assassinate).
func (k ActionKind) RequiresTarget() bool {
	switch k {
	case Coup, Steal, Assassinate:
		return true
	default:
		return false
	}
}

// OpensWindow reports whether declaring this action opens a response window
// for other seats to challenge or block it (spec §4.1). Income and Coup have
// no window; their effects are immediate.
func (k ActionKind) OpensWindow() bool {
	switch k {
	case ForeignAid, Tax, Steal, Assassinate, Exchange:
		return true
	default:
		return false
	}
}

// BlockOptions returns the characters a seat may counterclaim with to block an
// attack of this kind (spec §5.1, rules.md Ruling 3). Only the target of an
// attack may block it; Foreign Aid is untargeted, so any other active seat may
// counterclaim Duke against it — see window.blockingCharacters.
func (k ActionKind) BlockOptions() []Character {
	switch k {
	case Steal:
		return []Character{Captain, Ambassador}
	case Assassinate:
		return []Character{Contessa}
	default:
		return nil
	}
}

// Action is a concrete action declaration on the turn of its actor.
type Action struct {
	Kind      ActionKind
	Character Character // claimed character; required when Kind.ClaimOptions() != nil
	TargetID  string    // required when Kind.RequiresTarget()
}

// MandatoryCoupThreshold is the coin count at which Coup becomes the only
// legal turn action (rules.md Ruling 1, "ten or more").
const MandatoryCoupThreshold = 10

// LegacyError is a descriptive legality error in table-voice (spec §4.5). Its
// Message is normative: these exact strings are fed back to LLM drivers as
// tool-result feedback and shown to human clients, and must never reveal
// hidden information (e.g. "you don't hold a captain").
type LegacyError struct {
	Code    string // stable machine-readable code for the application layer to map onto HTTP codes (§11.1)
	Message string // normative table-voice message from §4.5
}

func (e *LegacyError) Error() string { return e.Message }

func legacyErr(code, message string) *LegacyError {
	return &LegacyError{Code: code, Message: message}
}

// Legacy error codes. The application layer maps these onto the HTTP error
// vocabulary of §11.1 (not_your_turn, not_eligible, already_resolved, …).
const (
	CodeNotYourTurn          = "not_your_turn"
	CodeEliminated           = "eliminated"
	CodeNotEnoughCoins       = "not_enough_coins"
	CodeMustCoup             = "must_coup"
	CodeNoSuchSeat           = "no_such_seat"
	CodeTargetEliminated     = "target_eliminated"
	CodeStealTargetGone      = "steal_target_gone"
	CodeNoCoinsToSteal       = "no_coins_to_steal"
	CodeSelfTarget           = "self_target"
	CodeMissingClaim         = "missing_claim"
	CodeInvalidClaim         = "invalid_claim"
	CodeNotEligible          = "not_eligible"
	CodeAlreadyResponded     = "already_responded"
	CodeWindowResolved       = "window_resolved"
	CodeDecisionMade         = "decision_made"
	CodeDecisionNotAddressed = "decision_not_addressed"
	CodeInvalidChoice        = "invalid_choice"
	CodeGameOver             = "game_over"
	CodeNothingToExpire      = "nothing_to_expire"
	CodeInvalidPayload       = "invalid_payload"
	CodeVoteExists           = "vote_exists"
	CodeVoteResolved         = "vote_resolved"
	CodeVoteTargetInactive   = "target_not_active"
	CodeNotAVoter            = "not_a_voter"
	CodeAlreadyVoted         = "already_voted"
)

// ValidateAction checks a turn-action declaration against the current game
// state (spec §4.5). It is read-only: it never mutates the game. A nil result
// means the action is legal; otherwise the *LegacyError carries the exact
// table-voice rejection message.
func (g *Game) ValidateAction(seatID string, a Action) error {
	if g == nil || g.Status == Finished {
		return legacyErr(CodeGameOver, "the game is over")
	}
	if g.window != nil || g.pending != nil {
		// The turn is suspended while an open structure waits for responses:
		// no action may be declared until it settles (spec §4.5 first row).
		return legacyErr(CodeNotYourTurn, "it is not your turn")
	}
	seat := g.findSeat(seatID)
	if seat == nil {
		return legacyErr(CodeNoSuchSeat, "no such seat")
	}
	if seat.Eliminated() {
		return legacyErr(CodeEliminated, "you are eliminated — you cannot act")
	}
	if g.turnSeat().ID != seatID {
		return legacyErr(CodeNotYourTurn, "it is not your turn")
	}

	// Claim checks: claimed actions must name one of their allowed characters.
	// Claims are never refused up front on hand contents — the window decides
	// (§4.5: "claims are always allowed").
	if opts := a.Kind.ClaimOptions(); opts != nil {
		switch {
		case a.Character == CharacterNone:
			return legacyErr(CodeMissingClaim, "that action requires a claimed character")
		case !characterIn(opts, a.Character):
			return legacyErr(CodeInvalidClaim, "you cannot claim that character for that action")
		}
	}

	// Target checks.
	if a.Kind.RequiresTarget() {
		switch {
		case a.TargetID == "" || a.TargetID == seatID:
			// A missing or self target is rejected in the same voice as a
			// foreign one: the table never tells you who exists.
			if a.TargetID == seatID {
				return legacyErr(CodeSelfTarget, "you cannot target your own seat")
			}
			return legacyErr(CodeNoSuchSeat, "no such seat")
		default:
			target := g.findSeat(a.TargetID)
			if target == nil {
				return legacyErr(CodeNoSuchSeat, "no such seat")
			}
			if target.Eliminated() {
				if a.Kind == Steal {
					return legacyErr(CodeStealTargetGone, "that player is eliminated")
				}
				return legacyErr(CodeTargetEliminated, "you cannot target an eliminated player")
			}
			if a.Kind == Steal && target.Coins < 1 {
				return legacyErr(CodeNoCoinsToSteal, "that player has no coins to steal from")
			}
		}
	}

	// Mandatory Coup (rules.md Ruling 1): at ≥ 10 coins Coup is the ONLY legal
	// action on the turn.
	if seat.Coins >= MandatoryCoupThreshold && a.Kind != Coup {
		return legacyErr(CodeMustCoup, "you must take a coup while you have ten or more coins")
	}

	// Sunk costs must be payable at declaration (§4.1 timing rule).
	if seat.Coins < a.Kind.Cost() {
		return legacyErr(CodeNotEnoughCoins, "not enough coins")
	}
	return nil
}

// ValidateWindowResponse checks a response to the currently open window for a
// seat (spec §5.2). It is read-only. Per-seat once-ness (A16) and the per-seat
// response grammar (§5.1) are both enforced here with the §4.5 messages.
func (g *Game) ValidateWindowResponse(seatID string, r WindowResponse) error {
	if g == nil || g.Status == Finished || g.window == nil {
		return legacyErr(CodeWindowResolved, "the window has already been resolved")
	}
	w := g.window
	if !w.isEligible(seatID) {
		return legacyErr(CodeNotEligible, "you cannot respond to that window")
	}
	if _, done := w.Responded[seatID]; done {
		return legacyErr(CodeAlreadyResponded, "you have already responded")
	}
	if !w.responseAllowed(seatID, r) {
		return legacyErr(CodeNotEligible, "you cannot respond to that window")
	}
	return nil
}

// ValidateChoice checks an answer to the pending single-seat decision (spec
// §5.4). It is read-only.
func (g *Game) ValidateChoice(seatID string, decisionID string, c Choice) error {
	if g == nil || g.pending == nil || g.pending.ID != decisionID {
		return legacyErr(CodeDecisionMade, "that decision was already made")
	}
	p := g.pending
	if p.SeatID != seatID {
		return legacyErr(CodeDecisionNotAddressed, "this decision is not addressed to you")
	}
	switch p.Kind {
	case DecisionFlipChoice:
		idx, ok := flipOptionIndex(c.FlipOption)
		if !ok || idx >= len(g.findSeat(seatID).FaceDown) {
			return legacyErr(CodeInvalidChoice, "that is not a valid option")
		}
	case DecisionExchangeFinish:
		if c.ExchangeNoSwap {
			return nil
		}
		if c.ExchangeDrawnIdx < 0 || c.ExchangeDrawnIdx >= len(p.DrawnCards) {
			return legacyErr(CodeInvalidChoice, "that is not a valid option")
		}
		idx, ok := flipOptionIndex(c.ExchangeHandOpt)
		if !ok || idx >= len(g.findSeat(seatID).FaceDown) {
			return legacyErr(CodeInvalidChoice, "that is not a valid option")
		}
	default:
		return legacyErr(CodeInvalidChoice, "that is not a valid option")
	}
	return nil
}

func characterIn(chars []Character, c Character) bool {
	for _, x := range chars {
		if x == c {
			return true
		}
	}
	return false
}

package domain

import (
	"fmt"
	"math/rand"
)

// GameStatus is the coarse game state.
type GameStatus int

const (
	InProgress GameStatus = iota
	Finished
)

// Phase is the snapshot vocabulary for what the engine is waiting on
// (§11.2: turn_action | window_open | single_seat_decision | finished).
type Phase string

const (
	PhaseTurnAction         Phase = "turn_action"
	PhaseWindowOpen         Phase = "window_open"
	PhaseSingleSeatDecision Phase = "single_seat_decision"
	PhaseFinished           Phase = "finished"
)

// IDGen supplies opaque identifiers (spec §13). It is the domain-side port:
// application/ports.go carries the same interface (kind "g","s","w","d","v")
// and the composition root binds the real implementation; tests inject a
// deterministic one. Domain never mints IDs by itself.
type IDGen interface {
	New(kind string) string
}

// Event is one entry in a game's append-only event stream (spec §10). The
// domain assigns Seq itself (per-game, monotonic from 1) so events can refer
// to earlier seqs within the same batch (e.g. window_opened.trigger.action_seq
// points at its own action_declared); ts is NOT a domain concern and is added
// by the application layer from its Clock port when materializing wire events.
type Event struct {
	Seq      int64
	GameID   string
	Type     string
	Audience Audience
	Payload  map[string]any
}

// Audience tags an event as public or scoped to one seat (§8, §10). Only the
// exchange reveals and the flip-choice request are seat-scoped.
type Audience struct {
	Public bool
	SeatID string // non-empty ⇒ private to that seat
}

func audiencePublic() Audience            { return Audience{Public: true} }
func audienceSeat(seatID string) Audience { return Audience{SeatID: seatID} }

// Game is the Coup aggregate: seats in turn order, the Court Deck, the single
// active structure (window XOR pending decision), the out-of-band forfeit
// ballot, and the monotonic event sequence counter (spec §3.1/§3.2). All
// mutation happens synchronously inside one method call; callers hold one
// mutex per game (spec §9) — the domain itself is lock-free.
type Game struct {
	ID            string
	Seats         []Seat // seat order = turn order
	Deck          CourtDeck
	TurnSeatIndex int
	TurnID        int // monotonic from 1; assigned when a turn starts
	Settings      WindowSettings

	Status   GameStatus
	WinnerID string // set when Status == Finished

	window  *openWindow      // exactly one of window/pending while in progress
	pending *pendingDecision // (§3.2.6); a forfeit vote may coexist with either

	vote *ForfeitVote // out-of-band ballot; nil when none is active
	ids  IDGen
	rng  *rand.Rand
	seq  int64 // last assigned event seq (next is seq+1)
}

// SeatSpec is one seat as supplied at game creation (spec §6.1).
type SeatSpec struct {
	ID   string
	Name string
	Type SeatType
}

// NewGame builds a fresh game: shuffles the 15-card deck, deals two face-down
// cards to each seat in seat order, forms the Court Deck from the remainder,
// picks the random starting seat (rules.md Ruling 6) and emits the initial
// batch [game_created, turn_start]. Durations/ids/rng are injected so the
// domain stays pure and test-deterministic.
func NewGame(id string, specs []SeatSpec, settings WindowSettings, ids IDGen, rng *rand.Rand) (*Game, []Event, error) {
	if len(specs) < 2 || len(specs) > 6 {
		return nil, nil, legacyErr(CodeInvalidPayload, "a game needs between two and six seats")
	}
	humans := 0
	seen := make(map[string]bool, len(specs))
	for _, s := range specs {
		if s.ID == "" || seen[s.ID] {
			return nil, nil, legacyErr(CodeInvalidPayload, "seat ids must be unique and non-empty")
		}
		seen[s.ID] = true
		if s.Type == SeatHuman {
			humans++
		}
	}
	if humans < 1 {
		return nil, nil, legacyErr(CodeInvalidPayload, "a game needs at least one human seat")
	}

	g := &Game{
		ID:       id,
		Settings: settings,
		Status:   InProgress,
		TurnID:   1,
		ids:      ids,
		rng:      rng,
	}
	deck := NewCourtDeck(FullDeck(), rng) // full shuffle = dealing shuffle
	for _, s := range specs {
		g.Seats = append(g.Seats, Seat{ID: s.ID, Name: s.Name, Type: s.Type, Coins: 2})
		g.Seats[len(g.Seats)-1].FaceDown = deck.DrawTop(2)
	}
	g.Deck = *deck
	start := rng.Intn(len(g.Seats)) // random starting seat (Ruling 6)
	g.TurnSeatIndex = start

	var evs []Event
	g.batchWith(&evs, func() {
		evs = append(evs, g.evGameCreated(), g.evTurnStart())
	})
	return g, evs, nil
}

// turnSeat returns the seat whose turn is currently held.
func (g *Game) turnSeat() *Seat { return &g.Seats[g.TurnSeatIndex] }

func (g *Game) findSeat(id string) *Seat {
	for i := range g.Seats {
		if g.Seats[i].ID == id {
			return &g.Seats[i]
		}
	}
	return nil
}

// ActiveSeats lists seat ids of seats that may act, in seat order.
func (g *Game) ActiveSeats() []string {
	var out []string
	for _, s := range g.Seats {
		if s.Active() {
			out = append(out, s.ID)
		}
	}
	return out
}

// Phase reports the snapshot phase vocabulary (§11.2).
func (g *Game) Phase() Phase {
	if g.Status == Finished {
		return PhaseFinished
	}
	switch {
	case g.window != nil:
		return PhaseWindowOpen
	case g.pending != nil:
		return PhaseSingleSeatDecision
	default:
		return PhaseTurnAction
	}
}

// seatHolds reports whether the seat holds the character among face-down
// cards. This is the ONLY place hand contents are consulted — it powers
// reveal-or-penalty (rules.md Ruling 7) and nothing else; no code path may use
// it to gate claims or responses up front.
func (g *Game) seatHolds(seatID string, c Character) bool {
	s := g.findSeat(seatID)
	if s == nil {
		return false
	}
	for _, card := range s.FaceDown {
		if card.Character == c {
			return true
		}
	}
	return false
}

// --- Event emission --------------------------------------------------------

func (g *Game) nextSeq() int64 { g.seq++; return g.seq }

func (g *Game) mkEvent(typ string, aud Audience, payload map[string]any) Event {
	return Event{Seq: g.nextSeq(), GameID: g.ID, Type: typ, Audience: aud, Payload: payload}
}

// MakeRejectionEvent builds a private rejection event for the offending seat's
// stream (A9): rejected submissions get their own seq so clients can reconcile
// them from history alone. The application layer calls this under the game
// lock and appends the result.
func (g *Game) MakeRejectionEvent(seatID, code, message string) Event {
	return g.mkEvent("rejection", audienceSeat(seatID), map[string]any{
		"seat_id": seatID, "code": code, "message": message,
	})
}

func (g *Game) evGameCreated() Event {
	seats := make([]map[string]any, len(g.Seats))
	for i, s := range g.Seats {
		seats[i] = map[string]any{"id": s.ID, "name": s.Name, "type": s.Type.String()}
	}
	return g.mkEvent("game_created", audiencePublic(), map[string]any{
		"seats": seats,
		"window_settings": map[string]any{
			"require_all_responses": g.Settings.RequireAllResponses,
			"response_timeout_ms":   g.Settings.TimeoutMs,
		},
		"first_turn_seat_id": g.turnSeat().ID,
	})
}

func (g *Game) evTurnStart() Event {
	return g.mkEvent("turn_start", audiencePublic(), map[string]any{
		"turn_id": g.TurnID,
		"seat_id": g.turnSeat().ID,
	})
}

// actionText composes the table-voice declaration line. Only the Assassinate
// form is normative (spec §10.1 example); the rest follow the same voice.
func actionText(a Action, actorName, targetName string) string {
	switch a.Kind {
	case Income:
		return fmt.Sprintf("%s takes Income", actorName)
	case ForeignAid:
		return fmt.Sprintf("%s takes Foreign Aid", actorName)
	case Coup:
		return fmt.Sprintf("%s declares a Coup against %s (paid 7)", actorName, targetName)
	case Tax:
		return fmt.Sprintf("%s claims Duke and takes Tax", actorName)
	case Steal:
		return fmt.Sprintf("%s claims %s and declares a Steal against %s", actorName, a.Character.String(), targetName)
	case Assassinate:
		return fmt.Sprintf("%s claims Assassin and declares an Assassinate against %s (paid 3)", actorName, targetName)
	case Exchange:
		return fmt.Sprintf("%s claims Ambassador and performs an Exchange", actorName)
	}
	return ""
}

func (g *Game) evActionDeclared(turnID int, actorID string, a Action) Event {
	actor := g.findSeat(actorID)
	actionMap := map[string]any{"type": a.Kind.String(), "coins_paid": a.Kind.Cost()}
	if opts := a.Kind.ClaimOptions(); opts != nil && a.Character != CharacterNone {
		actionMap["character"] = a.Character.String()
	}
	if a.Kind.RequiresTarget() && actor != nil {
		if t := g.findSeat(a.TargetID); t != nil {
			actionMap["target_id"] = a.TargetID
		}
	}
	text := actionText(a, actorNameOr(actor), targetNameOr(g.findSeat(a.TargetID)))
	return g.mkEvent("action_declared", audiencePublic(), map[string]any{
		"turn_id":  turnID,
		"actor_id": actorID,
		"action":   actionMap,
		"text":     text,
	})
}

func actorNameOr(s *Seat) string {
	if s == nil {
		return "?"
	}
	return s.Name
}
func targetNameOr(s *Seat) string {
	if s == nil {
		return "?"
	}
	return s.Name
}

func (g *Game) evWindowOpened(w *openWindow, triggerActorID string, triggerSeq int64) Event {
	payload := map[string]any{
		"window_id":             w.ID,
		"kind":                  w.Kind.String(),
		"trigger":               map[string]any{"action_seq": triggerSeq, "actor_id": triggerActorID},
		"eligible":              append([]string{}, w.Eligible...),
		"require_all_responses": w.Settings.RequireAllResponses,
		"timeout_ms":            w.Settings.TimeoutMs,
	}
	if w.GuardAction.Kind.RequiresTarget() && w.TargetID != "" {
		payload["target_seat_id"] = w.TargetID
	}
	return g.mkEvent("window_opened", audiencePublic(), payload)
}

func (g *Game) evResponseReceived(windowID, seatID string, r WindowResponse) Event {
	resp := map[string]any{"type": r.Kind.String()}
	if r.Kind == RespCounterclaim {
		resp["character"] = r.Character.String()
	}
	return g.mkEvent("response_received", audiencePublic(), map[string]any{
		"window_id": windowID,
		"seat_id":   seatID,
		"response":  resp,
	})
}

func (g *Game) evWindowClosed(windowID string, reason WindowCloseReason, deciderID string) Event {
	payload := map[string]any{
		"window_id": windowID,
		"reason":    reason.String(),
	}
	if deciderID != "" {
		payload["decider_seat_id"] = deciderID
	}
	return g.mkEvent("window_closed", audiencePublic(), payload)
}

func (g *Game) evClaimSucceeded(seatID string, c Character, challengerID string) Event {
	return g.mkEvent("claim_succeeded", audiencePublic(), map[string]any{
		"seat_id":            seatID,
		"character":          c.String(),
		"challenger_seat_id": challengerID,
	})
}

func (g *Game) evClaimFailed(seatID string, c Character, challengerID string) Event {
	return g.mkEvent("claim_failed", audiencePublic(), map[string]any{
		"seat_id":            seatID,
		"claimed_character":  c.String(),
		"challenger_seat_id": challengerID,
	})
}

// Card flip reasons (spec §10.1 card_flipped.reason).
const (
	FlipCoup                   = "coup"
	FlipAssassinate            = "assassinate"
	FlipPenaltyFailedClaim     = "penalty_failed_claim"
	FlipPenaltyFailedChallenge = "penalty_failed_challenge"
	FlipSuccessfulReveal       = "successful_reveal"
)

// Elimination reasons (spec §10.1 eliminated.reason).
const (
	ElimCoup            = "coup"
	ElimAssassinate     = "assassinate"
	ElimPenalty         = "penalty"          // failed offensive claim
	ElimFailedChallenge = "failed_challenge" // failed challenge against a surviving claim
	ElimForfeit         = "forfeit"
)

// elimReasonFor maps a flip reason to the elimination-reason vocabulary.
func elimReasonFor(flip string) string {
	switch flip {
	case FlipCoup:
		return ElimCoup
	case FlipAssassinate:
		return ElimAssassinate
	case FlipPenaltyFailedClaim:
		return ElimPenalty
	case FlipPenaltyFailedChallenge:
		return ElimFailedChallenge
	default:
		return "" // successful_reveal never eliminates
	}
}

// Fizzle reasons (spec §10.1 action_fizzled.reason).
const (
	FizzleCounterclaimSuccess = "counterclaim_success"
	FizzleTargetEliminated    = "target_eliminated"
	FizzleTargetForfeited     = "target_forfeited"
)

func (g *Game) evCardFlipped(seatID, card, reason string) Event {
	return g.mkEvent("card_flipped", audiencePublic(), map[string]any{
		"seat_id": seatID,
		"card":    card,
		"reason":  reason,
	})
}

// coinsChangedReasons (spec §10.1 coins_changed.reason).
const (
	CoinsIncome            = "income"
	CoinsForeignAid        = "foreign_aid"
	CoinsTax               = "tax"
	CoinsCoupPaid          = "coup_paid"
	CoinsAssassinationPaid = "assassination_paid"
	CoinsStealGiven        = "steal_given"
	CoinsStealTook         = "steal_took"
)

func (g *Game) evCoinsChanged(seatID string, delta, total int, reason string) Event {
	return g.mkEvent("coins_changed", audiencePublic(), map[string]any{
		"seat_id": seatID,
		"delta":   delta,
		"total":   total,
		"reason":  reason,
	})
}

func (g *Game) evActionFizzled(actionSeq int64, actorID, reason string) Event {
	return g.mkEvent("action_fizzled", audiencePublic(), map[string]any{
		"action_seq": actionSeq,
		"actor_id":   actorID,
		"reason":     reason,
	})
}

// evEliminated emits the public elimination event. For a forfeited seat the
// unrevealed cards are listed only as {"card":"forfeited"} placeholders — they
// remain unknowns for belief tracking and stay out of FaceUp (§7).
func (g *Game) evEliminated(seatID, reason string, seats ...*Seat) Event {
	s := seats[0]
	cards := make([]map[string]any, 0, len(s.FaceUp)+len(s.FaceDown))
	for _, c := range s.FaceUp {
		cards = append(cards, map[string]any{"card": c.Character.String()})
	}
	if s.Forfeited {
		for range s.FaceDown {
			cards = append(cards, map[string]any{"card": "forfeited"})
		}
	}
	return g.mkEvent("eliminated", audiencePublic(), map[string]any{
		"seat_id": seatID,
		"reason":  reason,
		"cards":   cards,
	})
}

func (g *Game) evExchangeRevealed(seatID string, drawn []Card, d *pendingDecision) Event {
	return g.mkEvent("exchange_revealed", audienceSeat(seatID), map[string]any{
		"seat_id": seatID,
		"drawn":   charactersAsStrings(drawn),
		"decision": map[string]any{
			"id":   d.ID,
			"kind": d.Kind.String(),
			"options": map[string]any{
				"drawn_index":  []int{0, 1},
				"hand_options": append([]string{}, d.Options...),
			},
			"timeout_ms": d.TimeoutMs,
		},
	})
}

func (g *Game) evFlipChoiceRequested(seatID string, d *pendingDecision) Event {
	return g.mkEvent("card_flip_choice_requested", audienceSeat(seatID), map[string]any{
		"seat_id": seatID,
		"decision": map[string]any{
			"id":         d.ID,
			"kind":       d.Kind.String(),
			"options":    append([]string{}, d.Options...),
			"timeout_ms": d.TimeoutMs,
		},
	})
}

func (g *Game) evExchangeCompleted(seatID string) Event {
	return g.mkEvent("exchange_completed", audiencePublic(), map[string]any{
		"seat_id": seatID,
	})
}

func (g *Game) evForfeitVoteOpened(v *ForfeitVote) Event {
	return g.mkEvent("forfeit_vote_opened", audiencePublic(), map[string]any{
		"vote_id":           v.ID,
		"target_seat_id":    v.TargetID,
		"initiator_seat_id": v.InitiatorID,
		"eligible_voters":   append([]string{}, v.EligibleVoters...),
		"majority_needed":   v.MajorityNeeded,
	})
}

func (g *Game) evForfeitVoteCast(voteID, voterID string, yes bool) Event {
	v := "no"
	if yes {
		v = "yes"
	}
	return g.mkEvent("forfeit_vote_cast", audiencePublic(), map[string]any{
		"vote_id":       voteID,
		"voter_seat_id": voterID,
		"vote":          v,
	})
}

func (g *Game) evForfeitVoteResolved(v *ForfeitVote) Event {
	yes, no := 0, 0
	for _, voted := range v.Cast {
		if voted {
			yes++
		} else {
			no++
		}
	}
	return g.mkEvent("forfeit_vote_resolved", audiencePublic(), map[string]any{
		"vote_id": v.ID,
		"result":  v.Result,
		"tally":   map[string]any{"yes": yes, "no": no, "eligible": len(v.EligibleVoters)},
	})
}

func (g *Game) evGameOver() Event {
	return g.mkEvent("game_over", audiencePublic(), map[string]any{
		"winner_seat_id": g.WinnerID,
	})
}

func charactersAsStrings(cards []Card) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.Character.String()
	}
	return out
}

// --- Turn & cascade bookkeeping -------------------------------------------

// batchWith runs the engine mutation `fn` (validation must have happened
// before the call) and then hard-checks every invariant of §3.2, panicking on
// violation: these are internal-consistency guards, not user-input checks.
func (g *Game) batchWith(evs *[]Event, fn func()) {
	fn()
	if err := g.CheckInvariants(); err != nil {
		panic(fmt.Sprintf("domain invariant violated in game %s: %v", g.ID, err))
	}
}

// advanceTurn moves to the next active seat in fixed seat order (skipping
// eliminated seats) and starts that turn. Only called when the game is still
// in progress and no further structure is pending.
func (g *Game) advanceTurn(evs *[]Event) {
	n := len(g.Seats)
	for step := 1; step <= n; step++ {
		idx := (g.TurnSeatIndex + step) % n
		if g.Seats[idx].Active() {
			g.TurnSeatIndex = idx
			break
		}
	}
	g.TurnID++
	*evs = append(*evs, g.evTurnStart())
}

// checkGameOver ends the game immediately when exactly one active seat remains
// (§4.4) and reports whether it did.
func (g *Game) checkGameOver(evs *[]Event) bool {
	if g.Status == Finished {
		return true
	}
	var winner string
	active := 0
	for _, s := range g.Seats {
		if s.Active() {
			active++
			winner = s.ID
		}
	}
	if active != 1 {
		return false
	}
	g.Status = Finished
	g.WinnerID = winner
	*evs = append(*evs, g.evGameOver())
	return true
}

// finishCascade closes out a resolution: game-over check first, then the next
// turn starts (turn_id advances). Exactly one path per cascade calls this.
func (g *Game) finishCascade(evs *[]Event) {
	if !g.checkGameOver(evs) {
		g.advanceTurn(evs)
	}
}

// checkEliminationSeat emits the public eliminated event when the seat just
// lost its last influence and voids any pending forfeit vote targeted at it.
func (g *Game) checkEliminationSeat(seatID, reason string, evs *[]Event) {
	s := g.findSeat(seatID)
	if s == nil || !s.Eliminated() || s.Forfeited {
		return
	}
	*evs = append(*evs, g.evEliminated(seatID, reason, s))
	g.voidVoteIfTargetGone(seatID, evs)
}

// voidVoteIfTargetGone implements the §7 rule: if the vote's target ceases to
// be eligible while the vote is pending (eliminated by play), the vote voids
// with no effect.
func (g *Game) voidVoteIfTargetGone(eliminatedSeatID string, evs *[]Event) {
	if g.vote != nil && g.vote.Result == "" && g.vote.TargetID == eliminatedSeatID {
		g.vote.Result = VoteResultVoided
		*evs = append(*evs, g.evForfeitVoteResolved(g.vote))
		g.vote = nil
	}
}

// addCoins applies a treasury/actor coin movement and emits coins_changed.
func (g *Game) addCoins(s *Seat, delta int, reason string, evs *[]Event) {
	s.Coins += delta
	*evs = append(*evs, g.evCoinsChanged(s.ID, delta, s.Coins, reason))
}

// --- Windows: construction --------------------------------------------------

// --- The engine entry points -------------------------------------------------

// DeclareTurnAction declares and applies one turn action for the seat holding
// the turn (spec §4). Sunk costs are paid at declaration; effects of claimed
// actions run only after their window settles. Returns the batch of events to
// append for this resolution step (§10.2: one batch per step). On error the
// game is unchanged.
func (g *Game) DeclareTurnAction(seatID string, a Action) ([]Event, error) {
	if err := g.ValidateAction(seatID, a); err != nil {
		return nil, err
	}
	var evs []Event
	g.batchWith(&evs, func() {
		actor := g.turnSeat()
		declared := g.evActionDeclared(g.TurnID, actor.ID, a)
		evs = append(evs, declared)

		if cost := a.Kind.Cost(); cost > 0 {
			reason := CoinsAssassinationPaid
			if a.Kind == Coup {
				reason = CoinsCoupPaid
			}
			g.addCoins(actor, -cost, reason, &evs)
		}

		if !a.Kind.OpensWindow() {
			// Base actions apply immediately: no window exists for income/coup.
			switch a.Kind {
			case Income:
				g.addCoins(actor, 1, CoinsIncome, &evs)
				g.finishCascade(&evs)
			case Coup:
				g.loseInfluence(a.TargetID, FlipCoup, func(e *[]Event) { g.finishCascade(e) }, &evs)
			}
			return
		}

		w := g.newOffensiveWindow(actor, a, declared.Seq)
		g.window = w
		evs = append(evs, g.evWindowOpened(w, actor.ID, declared.Seq))
	})
	return evs, nil
}

// SubmitWindowResponse records one seat's response to the open window and —
// for the first non-PASS response or a require-all/timeout termination — runs
// the full synchronous resolution cascade (§5.2/§5.3). Passes never close a
// window by themselves. On error the game is unchanged.
func (g *Game) SubmitWindowResponse(seatID string, r WindowResponse) ([]Event, error) {
	if err := g.ValidateWindowResponse(seatID, r); err != nil {
		return nil, err
	}
	var evs []Event
	g.batchWith(&evs, func() {
		w := g.window
		w.Responded[seatID] = r
		respEv := g.evResponseReceived(w.ID, seatID, r)
		evs = append(evs, respEv)

		if r.Kind == RespPass {
			if w.Settings.RequireAllResponses && w.allEligibleResponded() {
				g.settleWindow(CloseAllResponded, "", 0, &evs)
			}
			return
		}
		// First non-PASS wins (§5.2.2), regardless of seat type. The seq of the
		// triggering response is carried as the trigger of any nested window it
		// opens (see settleWindow).
		g.settleWindow(CloseFirstResponse, seatID, respEv.Seq, &evs)
	})
	return evs, nil
}

// AnswerDecision answers the pending single-seat decision addressed to seatID
// (spec §5.4). The stored continuation then runs the rest of the suspended
// cascade synchronously. On error the game is unchanged.
func (g *Game) AnswerDecision(seatID, decisionID string, c Choice) ([]Event, error) {
	if err := g.ValidateChoice(seatID, decisionID, c); err != nil {
		return nil, err
	}
	var evs []Event
	g.batchWith(&evs, func() {
		p := g.pending
		g.pending = nil
		evs = append(evs, p.resume(g, c)...)
	})
	return evs, nil
}

// ExpireWindow is the domain-side timeout tick (spec §5.2.5(c)/§5.4): the
// application layer's Clock fires it when a deadline passes. For an open
// window: unresponded seats auto-pass and the window closes with reason
// `timeout`. For a pending decision honoring a timeout: its engine-safe
// default is applied (A4). The domain itself never sleeps or reads a clock.
func (g *Game) ExpireWindow() ([]Event, error) {
	if g.Status == Finished {
		return nil, legacyErr(CodeGameOver, "the game is over")
	}
	if g.window == nil && (g.pending == nil || g.pending.TimeoutMs == 0) {
		return nil, legacyErr(CodeNothingToExpire, "there is no structure waiting to time out")
	}
	var evs []Event
	g.batchWith(&evs, func() {
		if g.window != nil {
			w := g.window
			for _, id := range w.Eligible { // auto-pass bookkeeping (A: passes are events)
				if _, ok := w.Responded[id]; !ok {
					pass := WindowResponse{Kind: RespPass}
					w.Responded[id] = pass
					evs = append(evs, g.evResponseReceived(w.ID, id, pass))
				}
			}
			g.settleWindow(CloseTimeout, "", 0, &evs)
			return
		}
		p := g.pending
		g.pending = nil
		evs = append(evs, p.resume(g, p.Kind.DefaultChoice())...)
	})
	return evs, nil
}

// --- Forfeit votes (spec §7) --------------------------------------------------

// InitiateForfeitVote opens a forfeit ballot against a stuck seat. Any active
// HUMAN seat may initiate; the target must be an ACTIVE, different seat. At
// most one active vote per game (A15). The ballot is out-of-band: it does not
// pause the turn/window state machine.
func (g *Game) InitiateForfeitVote(initiatorID, targetID string) ([]Event, error) {
	if g.Status == Finished {
		return nil, legacyErr(CodeGameOver, "the game is over")
	}
	initiator := g.findSeat(initiatorID)
	if initiator == nil {
		return nil, legacyErr(CodeNoSuchSeat, "no such seat")
	}
	if !initiator.Active() {
		return nil, legacyErr(CodeEliminated, "you are eliminated — you cannot act")
	}
	if initiator.Type != SeatHuman {
		return nil, legacyErr(CodeNotEligible, "only a human player may initiate a forfeit vote")
	}
	target := g.findSeat(targetID)
	if target == nil {
		return nil, legacyErr(CodeNoSuchSeat, "no such seat")
	}
	if target.ID == initiator.ID {
		return nil, legacyErr(CodeSelfTarget, "you cannot target your own seat")
	}
	if !target.Active() {
		return nil, legacyErr(CodeVoteTargetInactive, "you cannot target an eliminated player")
	}
	if g.vote != nil && g.vote.Result == "" {
		return nil, legacyErr(CodeVoteExists, "a forfeit vote is already in progress")
	}

	var evs []Event
	g.batchWith(&evs, func() {
		voters := make([]string, 0, len(g.Seats))
		for _, s := range g.Seats {
			if s.Type == SeatHuman && s.Active() && s.ID != targetID {
				voters = append(voters, s.ID)
			}
		}
		g.vote = &ForfeitVote{
			ID:             g.ids.New("v"),
			TargetID:       targetID,
			InitiatorID:    initiatorID,
			EligibleVoters: voters,
			MajorityNeeded: len(voters)/2 + 1, // strictly more than half (§7)
			Cast:           map[string]bool{},
		}
		evs = append(evs, g.evForfeitVoteOpened(g.vote))
	})
	return evs, nil
}

// CastForfeitVote records one yes/no ballot. Only the other active HUMAN seats
// may vote (LLM seats do not vote in v1); each voter once. The ballot closes
// early the moment either side reaches the majority fixed at opening (§7).
func (g *Game) CastForfeitVote(voterID string, yes bool) ([]Event, error) {
	v := g.vote
	if v == nil || v.Result != "" {
		return nil, legacyErr(CodeVoteResolved, "the forfeit vote has already been resolved")
	}
	voter := g.findSeat(voterID)
	eligible := false
	for _, id := range v.EligibleVoters {
		if id == voterID {
			eligible = true
			break
		}
	}
	if voter == nil || !eligible || !voter.Active() {
		return nil, legacyErr(CodeNotAVoter, "you are not eligible to cast this vote")
	}
	if _, ok := v.Cast[voterID]; ok {
		return nil, legacyErr(CodeAlreadyVoted, "you have already voted")
	}

	var evs []Event
	g.batchWith(&evs, func() {
		v := g.vote
		v.Cast[voterID] = yes
		evs = append(evs, g.evForfeitVoteCast(v.ID, voterID, yes))
		g.settleVoteTally(&evs)
	})
	return evs, nil
}

// settleVoteTally applies the §7 close rules after each cast: early majority
// (either side), exhausted ballot without majority (tie-close ⇒
// not_forfeited), or — [ASSUMPTION] — all remaining uncast voters no longer
// active ⇒ not_forfeited.
func (g *Game) settleVoteTally(evs *[]Event) {
	v := g.vote
	yes, no := 0, 0
	for _, voted := range v.Cast {
		if voted {
			yes++
		} else {
			no++
		}
	}
	switch {
	case yes >= v.MajorityNeeded:
		g.resolveVote(VoteResultForfeited, evs)
	case no >= v.MajorityNeeded:
		g.resolveVote(VoteResultNotForfeited, evs)
	case len(v.Cast) == len(v.EligibleVoters):
		g.resolveVote(VoteResultNotForfeited, evs) // tie / exhausted without majority
	default:
		allGone := true
		for _, id := range v.EligibleVoters {
			if _, cast := v.Cast[id]; !cast && g.findSeat(id).Active() {
				allGone = false
				break
			}
		}
		if allGone {
			g.resolveVote(VoteResultNotForfeited, evs)
		}
	}
}

// resolveVote finalizes the ballot. A yes-majority instantly eliminates the
// target (forfeit semantics: unrevealed cards stay unknowns, §7), then any
// pending structure resolves per normal elimination rules; a vote whose target
// was already eliminated by play voids with no effect (handled in
// voidVoteIfTargetGone).
func (g *Game) resolveVote(result string, evs *[]Event) {
	v := g.vote
	v.Result = result
	*evs = append(*evs, g.evForfeitVoteResolved(v))

	if result != VoteResultForfeited {
		g.vote = nil
		return
	}
	target := g.findSeat(v.TargetID)
	target.Forfeited = true // cards stay face-down as "forfeited" unknowns
	*evs = append(*evs, g.evEliminated(target.ID, ElimForfeit, target))

	g.vote = nil
	g.settleStructuresAfterForfeit(evs)
}

// settleStructuresAfterForfeit resolves pending structures when a seat is
// eliminated by forfeit vote (§7): an open window with no remaining eligible
// responders auto-closes (reason auto_no_responders); a pending single-seat
// decision addressed to the forfeited seat is voided and its engine default
// applied (A15); otherwise only the game-over check applies (the two-player
// edge: forfeiting your only opponent is simply winning).
func (g *Game) settleStructuresAfterForfeit(evs *[]Event) {
	switch {
	case g.window != nil:
		w := g.window
		rescoped := make([]string, 0, len(w.Eligible))
		for _, id := range w.Eligible {
			if g.findSeat(id).Active() {
				rescoped = append(rescoped, id)
			}
		}
		if len(rescoped) == 0 {
			g.settleWindow(CloseAutoNoResponders, "", 0, evs) // claim survives (or fizzles per §7 paths)
			return
		}
		w.Eligible = rescoped // window continues with its reduced responder set
		// With require-all, the forfeit may have removed the last unresponded
		// seat: every remaining eligible seat has then already responded and the
		// window must close now (termination (b), §5.2.5) — otherwise it would
		// stall with no one left who could respond.
		if w.Settings.RequireAllResponses && w.allEligibleResponded() {
			g.settleWindow(CloseAllResponded, "", 0, evs)
		}
	case g.pending != nil:
		p := g.pending
		g.pending = nil
		*evs = append(*evs, p.resume(g, p.Kind.DefaultChoice())...) // A15 safe default
	default:
		if !g.checkGameOver(evs) && !g.turnSeat().Active() {
			g.advanceTurn(evs) // the forfeited seat held the turn
		}
	}
}

// --- Read-only views for the application layer -------------------------------

// WindowView is a read-only snapshot of the open window (§11.2). Nil when no
// window is open.
type WindowView struct {
	ID        string
	Kind      WindowKind
	TargetID  string // "" when untargeted
	Settings  WindowSettings
	Eligible  []string
	Responses map[string]string // seat id → "pass" | "challenge" | "counterclaim:<character>"
	ActionSeq int64
	ActorID   string
}

// WindowView returns the current window snapshot, or nil.
func (g *Game) WindowView() *WindowView {
	w := g.window
	if w == nil {
		return nil
	}
	responses := make(map[string]string, len(w.Responded))
	for id, r := range w.Responded {
		switch r.Kind {
		case RespCounterclaim:
			responses[id] = "counterclaim:" + r.Character.String()
		default:
			responses[id] = r.Kind.String()
		}
	}
	return &WindowView{
		ID:        w.ID,
		Kind:      w.Kind,
		TargetID:  w.TargetID,
		Settings:  w.Settings,
		Eligible:  append([]string{}, w.Eligible...),
		Responses: responses,
		ActionSeq: w.ActionSeq,
		ActorID:   w.ActorID,
	}
}

// AllowedResponsesFor returns the response grammar tokens for one seat in the
// open window (nil when no window is open or the seat is not eligible) — the
// public "what you MAY do" list, never advice (§2.2).
func (g *Game) AllowedResponsesFor(seatID string) []string {
	w := g.window
	if w == nil || !w.isEligible(seatID) {
		return nil
	}
	return w.responseTokensForSeat(seatID)
}

// DecisionView is a read-only snapshot of the pending single-seat decision.
// Options/DrawnCards are seat-scoped: only the addressed viewer's snapshot may
// include them (§8). Nil when no decision is pending.
type DecisionView struct {
	ID         string
	Kind       DecisionKind
	SeatID     string
	Options    []string
	DrawnCards []Card
	TimeoutMs  int
}

func (g *Game) DecisionView() *DecisionView {
	p := g.pending
	if p == nil {
		return nil
	}
	return &DecisionView{
		ID:         p.ID,
		Kind:       p.Kind,
		SeatID:     p.SeatID,
		Options:    append([]string{}, p.Options...),
		DrawnCards: append([]Card{}, p.DrawnCards...),
		TimeoutMs:  p.TimeoutMs,
	}
}

// VoteView is a read-only snapshot of the active forfeit vote (nil when none).
type VoteView struct {
	ID             string
	TargetID       string
	InitiatorID    string
	EligibleVoters []string
	MajorityNeeded int
	Yes            int
	No             int
}

func (g *Game) VoteView() *VoteView {
	v := g.vote
	if v == nil || v.Result != "" {
		return nil
	}
	view := &VoteView{
		ID:             v.ID,
		TargetID:       v.TargetID,
		InitiatorID:    v.InitiatorID,
		EligibleVoters: append([]string{}, v.EligibleVoters...),
		MajorityNeeded: v.MajorityNeeded,
	}
	for _, voted := range v.Cast {
		if voted {
			view.Yes++
		} else {
			view.No++
		}
	}
	return view
}

// CheckInvariants asserts every engine invariant of spec §3.2 plus the window/
// decision exclusivity. It is called internally after every mutation (panicking
// on violation) and is exported so tests can assert it after each transition.
func (g *Game) CheckInvariants() error {
	invariant := func(format string, args ...any) error {
		return fmt.Errorf("game %s: "+format, append([]any{g.ID}, args...)...)
	}

	// 1+2. Court deck size constant: |deck| = 15 − 2×seats, and total cards in
	// play is always 15. During a suspended exchange-finish decision the two
	// drawn cards are held by the pending decision (net-zero completes on the
	// answer), so they count toward both tallies while out of the deck.
	held := 0
	if g.pending != nil && g.pending.Kind == DecisionExchangeFinish {
		held = len(g.pending.DrawnCards)
	}
	if g.Deck.Size()+held != 15-2*len(g.Seats) {
		return invariant("deck size %d (+%d held) != 15-2*%d", g.Deck.Size(), held, len(g.Seats))
	}

	// 2. Total cards in play is always 15.
	total := g.Deck.Size() + held
	for _, s := range g.Seats {
		total += len(s.FaceDown) + len(s.FaceUp)
		if len(s.FaceDown)+len(s.FaceUp) != 2 {
			return invariant("seat %s holds %d cards, want 2", s.ID, len(s.FaceDown)+len(s.FaceUp))
		}
		if s.Coins < 0 {
			return invariant("seat %s has negative coins %d", s.ID, s.Coins)
		}
	}
	if total != 15 {
		return invariant("cards in play %d != 15", total)
	}

	// 3. Eliminated ⇔ no influence; eliminated seats are inert everywhere.
	for _, s := range g.Seats {
		if s.Forfeited && !s.Eliminated() {
			return invariant("seat %s forfeited but reports active", s.ID)
		}
		if !s.Forfeited && s.Eliminated() && len(s.FaceUp) != 2 {
			return invariant("seat %s eliminated with %d face-up cards, want 2", s.ID, len(s.FaceUp))
		}
	}

	// 6. Exactly one of {window, pending decision} at a time; forfeit vote may
	// coexist with any structure (out-of-band).
	switch {
	case g.window != nil && g.pending != nil:
		return invariant("both window and pending decision are open")
	case g.Status == Finished && (g.window != nil || g.pending != nil):
		return invariant("finished game still has an open structure")
	case g.Status == Finished && g.vote != nil:
		return invariant("finished game still has an active forfeit vote")
	case g.Status == InProgress && g.Phase() == PhaseTurnAction && !g.turnSeat().Active():
		return invariant("turn seat %s is not active with no open structure", g.turnSeat().ID)
	}

	if w := g.window; w != nil {
		if len(w.Eligible) == 0 {
			return invariant("open window has no eligible seats")
		}
		for _, id := range w.Eligible {
			s := g.findSeat(id)
			if s == nil || !s.Active() {
				return invariant("window %s lists ineligible responder %q", w.ID, id)
			}
		}
	}

	// §7: a pending vote must always target an active seat — elimination by
	// play voids it immediately (voidVoteIfTargetGone).
	if v := g.vote; v != nil && v.Result == "" {
		t := g.findSeat(v.TargetID)
		if t == nil || !t.Active() {
			return invariant("pending vote %s targets inactive seat %s", v.ID, v.TargetID)
		}
	}

	return nil
}

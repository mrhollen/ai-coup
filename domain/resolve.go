package domain

// This file holds the claim/challenge resolution cascade and sunk-cost
// handling (spec §2.1 resolve.go): window settlement, reveal-or-penalty
// (rules.md Ruling 7), the failed-offensive-void vs failed-defensive-"no
// block" asymmetry (§4.3), effect-time liveness re-checks, and the exchange
// finish. Everything here runs synchronously inside one engine call; it may
// suspend via g.pending (single-seat decisions) but never blocks.

// settleWindow closes the open window with the given reason and runs the
// RESOLVING phase: reveal-or-penalty, sunk costs (already paid), block/fizzle
// bookkeeping, then effects — in one synchronous pass. A counterclaim close
// re-enters WINDOW OPEN with the nested window as innermost (it never stacks);
// a flip choice suspends via g.pending and resumes on AnswerDecision/timeout.
func (g *Game) settleWindow(reason WindowCloseReason, deciderID string, triggerSeq int64, evs *[]Event) {
	w := g.window
	g.window = nil // exactly-one invariant: the nested window replaces before RESOLVING completes
	*evs = append(*evs, g.evWindowClosed(w.ID, reason, deciderID))

	if reason != CloseFirstResponse || deciderID == "" {
		// Termination (b) all-responded, (c) timeout, (d) auto-no-responders:
		// no winning response was ever registered — the pending claim survives.
		g.resolveClaimSurvived(w, evs)
		return
	}

	resp := w.Responded[deciderID]
	if resp.Kind == RespPass { // unreachable by construction; guard anyway
		g.resolveClaimSurvived(w, evs)
		return
	}
	switch w.Kind {
	case WindowChallenge:
		if resp.Kind == RespCounterclaim {
			blocker := g.findSeat(deciderID)
			nw := g.newBlockWindow(w.GuardAction, blocker, resp.Character, w)
			g.window = nw // re-enter WINDOW OPEN with W′ innermost (§5.3)
			*evs = append(*evs, g.evWindowOpened(nw, blocker.ID, triggerSeq))
			return
		}
		g.resolveOffensiveChallenge(w, deciderID, evs)
	case WindowCounterclaimChallenge:
		g.resolveDefensiveChallenge(w, deciderID, evs)
	}
}

// resolveClaimSurvived settles a window that closed without any winning
// response (b/c/d): the pending claim proceeds. An eliminated claimant's claim
// can no longer take effect (see [ASSUMPTION] register in the report); an
// eliminated blocker's block fizzles so the original attack proceeds on top of
// whatever already happened.
func (g *Game) resolveClaimSurvived(w *openWindow, evs *[]Event) {
	switch w.GuardRole {
	case ClaimOffensive:
		if !g.findSeat(w.ClaimantID).Active() {
			*evs = append(*evs, g.evActionFizzled(w.ActionSeq, w.ActorID, fizzleReasonForInactive(g.findSeat(w.ClaimantID))))
			g.finishCascade(evs)
			return
		}
		finishAfterEffect(g.applyActionEffect(w.GuardAction, w.ActionSeq, w.ActorID, evs), evs, g)
	case ClaimDefensive:
		if !g.findSeat(w.BlockerID).Active() {
			// The blocker is gone: there was never a block that could stand, so
			// the original attack proceeds as if unopposed (§4.3 spirit).
			finishAfterEffect(g.applyActionEffect(w.GuardAction, w.ActionSeq, w.ActorID, evs), evs, g)
			return
		}
		*evs = append(*evs, g.evActionFizzled(w.ActionSeq, w.ActorID, FizzleCounterclaimSuccess))
		g.finishCascade(evs)
	}
}

func fizzleReasonForInactive(s *Seat) string {
	if s != nil && s.Forfeited {
		return FizzleTargetForfeited
	}
	return FizzleTargetEliminated
}

// resolveOffensiveChallenge settles a challenge against a claimed turn action
// (§4.2). Ruling 7: revealing is mandatory — the branch depends only on
// whether the claimant actually holds the character; there is no refusal path.
func (g *Game) resolveOffensiveChallenge(w *openWindow, challengerID string, evs *[]Event) {
	claimantID, char := w.ClaimantID, w.ClaimChar
	if g.seatHolds(claimantID, char) {
		*evs = append(*evs, g.evClaimSucceeded(claimantID, char, challengerID))
		g.revealAndRedraw(claimantID, char, evs)
		// The (failed) challenger loses one influence; then the claim's effects
		// execute as normal.
		g.loseInfluence(challengerID, FlipPenaltyFailedChallenge, func(e *[]Event) {
			finishAfterEffect(g.applyActionEffect(w.GuardAction, w.ActionSeq, w.ActorID, e), e, g)
		}, evs)
		return
	}
	// claim_failed: the claimant is busted; penalty flips one of THEIR cards.
	*evs = append(*evs, g.evClaimFailed(claimantID, char, challengerID))
	// The claimed action is VOID (nothing was ever granted); any sunk cost is
	// lost. No effect runs — that is exactly what makes "failed offensive ⇒
	// void" mean something (§4.3).
	g.loseInfluence(claimantID, FlipPenaltyFailedClaim, func(e *[]Event) { g.finishCascade(e) }, evs)
}

// resolveDefensiveChallenge settles the nested window of a blocking
// counterclaim (§4.2/§4.3). Success blocks the original attack (fizzle);
// failure penalties the blocker and the attack PROCEEDS — the load-bearing
// asymmetry.
func (g *Game) resolveDefensiveChallenge(w *openWindow, challengerID string, evs *[]Event) {
	blockerID, char := w.BlockerID, w.ClaimChar
	attack := w.GuardAction // the original attack being blocked
	if g.seatHolds(blockerID, char) {
		*evs = append(*evs, g.evClaimSucceeded(blockerID, char, challengerID))
		g.revealAndRedraw(blockerID, char, evs)
		g.loseInfluence(challengerID, FlipPenaltyFailedChallenge, func(e *[]Event) {
			*e = append(*e, g.evActionFizzled(w.ActionSeq, w.ActorID, FizzleCounterclaimSuccess))
			g.finishCascade(e)
		}, evs)
		return
	}
	*evs = append(*evs, g.evClaimFailed(blockerID, char, challengerID))
	g.loseInfluence(blockerID, FlipPenaltyFailedClaim, func(e *[]Event) {
		// NO BLOCK: the original attack proceeds to full effect on top of the
		// penalty, with target liveness re-checked at effect time (§4.3).
		finishAfterEffect(g.applyActionEffect(attack, w.ActionSeq, w.ActorID, e), e, g)
	}, evs)
}

// revealAndRedraw implements the successful-reveal bookkeeping (rules.md
// Claims & challenges step 4): the revealed card is shuffled back into the
// Court Deck and its owner draws one replacement face-down card — net-zero deck
// (§3.2.1). The replacement itself is face-down and never appears in any event.
func (g *Game) revealAndRedraw(claimantID string, char Character, evs *[]Event) {
	s := g.findSeat(claimantID)
	idx := -1
	for i, c := range s.FaceDown {
		if c.Character == char {
			idx = i
			break
		}
	}
	card := s.FaceDown[idx]
	s.FaceDown = append(s.FaceDown[:idx], s.FaceDown[idx+1:]...)
	g.Deck.ReturnShuffled([]Card{card}, g.rng)
	repl := g.Deck.DrawTop(1)[0]
	s.FaceDown = append(s.FaceDown, repl)
	*evs = append(*evs, g.evCardFlipped(claimantID, char.String(), FlipSuccessfulReveal))
}

// loseInfluence makes a seat lose one influence (penalty, Coup or Assassinate
// victim). With two face-down cards the seat CHOOSES which flips — a single-
// seat decision suspends the cascade (§5.4); with exactly one the flip is
// automatic (no round-trip). `after` runs once the flip has landed and carries
// the rest of the cascade (typically ending in finishCascade). Returns whether
// the engine suspended on the flip-choice decision.
func (g *Game) loseInfluence(seatID, reason string, after func(evs *[]Event), evs *[]Event) bool {
	s := g.findSeat(seatID)
	if len(s.FaceDown) == 2 {
		d := g.newDecision(DecisionFlipChoice, seatID, []string{"face_down_0", "face_down_1"}, nil)
		d.resume = func(g *Game, c Choice) []Event {
			idx, _ := flipOptionIndex(c.FlipOption)
			return g.applyFlipAndContinue(seatID, idx, reason, after)
		}
		g.pending = d
		*evs = append(*evs, g.evFlipChoiceRequested(seatID, d))
		return true
	}
	var out []Event
	out = g.applyFlipAndContinue(seatID, 0, reason, func(e *[]Event) { after(e) })
	*evs = append(*evs, out...)
	return false
}

// applyFlipAndContinue flips one face-down card (positional index), emits the
// public card_flipped + any elimination events, then runs `after`.
func (g *Game) applyFlipAndContinue(seatID string, idx int, reason string, after func(evs *[]Event)) []Event {
	var evs []Event
	s := g.findSeat(seatID)
	card := s.FaceDown[idx]
	s.FaceDown = append(s.FaceDown[:idx], s.FaceDown[idx+1:]...)
	s.FaceUp = append(s.FaceUp, card)
	evs = append(evs, g.evCardFlipped(seatID, card.Character.String(), reason))
	if r := elimReasonFor(reason); r != "" {
		g.checkEliminationSeat(seatID, r, &evs)
	}
	if after != nil {
		after(&evs)
	}
	return evs
}

// effectOutcome says what the caller must do after applyActionEffect returns:
//   - effectApplied:     the effect ran (or fizzled) synchronously; the caller
//     must end the cascade with finishCascade.
//   - effectComplete:    a terminal flip ran inline AND already finished the
//     cascade (assassinate auto-flip); the caller does nothing.
//   - effectSuspended:   the engine suspended on a single-seat decision whose
//     stored resume owns the finish; the caller does nothing.
type effectOutcome int

const (
	effectApplied effectOutcome = iota
	effectComplete
	effectSuspended
)

// applyActionEffect executes a claimed action's effects at the moment its claim
// has survived (§4.1 timing). Target (and actor) liveness is re-checked HERE,
// not at declaration — the engine requirement of §4.3.
func (g *Game) applyActionEffect(x Action, actionSeq int64, actorID string, evs *[]Event) effectOutcome {
	actor := g.findSeat(actorID)
	switch x.Kind {
	case ForeignAid:
		if !actor.Active() {
			*evs = append(*evs, g.evActionFizzled(actionSeq, actorID, fizzleReasonForInactive(actor)))
			return effectApplied
		}
		g.addCoins(actor, 2, CoinsForeignAid, evs)
	case Tax:
		if !actor.Active() {
			*evs = append(*evs, g.evActionFizzled(actionSeq, actorID, fizzleReasonForInactive(actor)))
			return effectApplied
		}
		g.addCoins(actor, 3, CoinsTax, evs)
	case Steal:
		target := g.findSeat(x.TargetID)
		if !target.Active() {
			*evs = append(*evs, g.evActionFizzled(actionSeq, actorID, fizzleReasonForInactive(target)))
			return effectApplied
		}
		n := 2
		if target.Coins < n {
			n = target.Coins
		}
		if n < 1 { // unreachable by construction (target validated ≥ 1 coin at declaration)
			*evs = append(*evs, g.evActionFizzled(actionSeq, actorID, FizzleTargetEliminated))
			return effectApplied
		}
		g.addCoins(target, -n, CoinsStealGiven, evs)
		g.addCoins(actor, n, CoinsStealTook, evs)
	case Assassinate:
		target := g.findSeat(x.TargetID)
		if !target.Active() {
			*evs = append(*evs, g.evActionFizzled(actionSeq, actorID, fizzleReasonForInactive(target)))
			return effectApplied
		}
		// Terminal flip: the continuation (finishCascade) is carried by the
		// decision when suspended; on the inline auto-flip path the caller must
		// NOT finish again — signal effectComplete instead.
		if g.loseInfluence(target.ID, FlipAssassinate, func(e *[]Event) { g.finishCascade(e) }, evs) {
			return effectSuspended
		}
		return effectComplete
	case Exchange:
		if !actor.Active() {
			*evs = append(*evs, g.evActionFizzled(actionSeq, actorID, fizzleReasonForInactive(actor)))
			return effectApplied
		}
		drawn := g.Deck.DrawTop(2) // deck size invariant keeps this safe (§3.2.1)
		d := g.newDecision(DecisionExchangeFinish, actor.ID, nil, drawn)
		d.resume = func(g *Game, c Choice) []Event {
			var out []Event
			g.applyExchangeFinish(actorID, d.DrawnCards, c, &out)
			out = append(out, g.evExchangeCompleted(actorID))
			g.finishCascade(&out)
			return out
		}
		g.pending = d
		*evs = append(*evs, g.evExchangeRevealed(actor.ID, drawn, d))
		return effectSuspended
	}
	return effectApplied
}

// finishAfterEffect ends the cascade unless applyActionEffect already did (or
// will, via the suspended decision's resume). Exactly one finishCascade runs
// per cascade.
func finishAfterEffect(o effectOutcome, evs *[]Event, g *Game) {
	if o == effectApplied {
		g.finishCascade(evs)
	}
}

// applyExchangeFinish applies the actor's exchange-finish choice (§11.2):
// (a) no swap — both drawn cards return, hand unchanged; (b)/(c) keep one
// drawn card (drawn_index), and the OTHER drawn card plus the named face-down
// hand card return to the deck as an unordered set (net-zero).
func (g *Game) applyExchangeFinish(actorID string, drawn []Card, c Choice, evs *[]Event) {
	actor := g.findSeat(actorID)
	if c.ExchangeNoSwap || len(drawn) != 2 {
		g.Deck.ReturnShuffled(drawn, g.rng)
		return
	}
	kept := drawn[c.ExchangeDrawnIdx]
	var returned []Card
	for i, card := range drawn {
		if i != c.ExchangeDrawnIdx {
			returned = append(returned, card)
		}
	}
	handIdx, _ := flipOptionIndex(c.ExchangeHandOpt)
	returned = append(returned, actor.FaceDown[handIdx])
	actor.FaceDown[handIdx] = kept
	g.Deck.ReturnShuffled(returned, g.rng)
}

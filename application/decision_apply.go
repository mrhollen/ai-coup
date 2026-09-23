package application

import (
	"errors"
	"strconv"
	"strings"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// runDecision is the body of one LLM decision goroutine (spec §6.4.4): the
// driver does the re-prompting inside Decide; the application dispatches once
// per owed decision, validates the candidate through the engine, feeds any
// rejection back via DecisionInput.Feedback, bounded at K = maxDriverRetries,
// after which the kind-safe default applies so no decision can permadeath a
// game (A4). Cancellation of f.ctx discards late results without touching the
// game (A9/A12-style stale responses are rejected by normal validation for
// client submissions; driver results are simply dropped — there is no HTTP
// caller to reconcile against).
func (h *gameHandle) runDecision(f *flight, in DecisionInput) {
	defer func() {
		f.cancel() // idempotent; releases the decision context at completion
		h.mu.Lock()
		if cur, ok := h.inFlight[f.seatID]; ok && cur == f {
			delete(h.inFlight, f.seatID)
		}
		h.redispatchIfOwedLocked(h.g, f.seatID)
		h.mu.Unlock()
	}()

	feedback := ""
	attempt := 0
	for {
		if f.ctx.Err() != nil {
			h.logDiscard(f, in.Kind, "cancelled")
			return
		}
		t0 := h.app.deps.Clock.Now()
		cand, derr := h.app.driver.Decide(f.ctx, in)
		inference := h.app.deps.Clock.Now().Sub(t0)
		if derr != nil {
			// Terminal driver failure (e.g. scripted queue exhausted). The openai
			// driver blocks until it produces a candidate or its context is
			// cancelled (§6.4.6), so this path is mostly for the fake driver;
			// recovery is by design the response timeout or a forfeit vote (§7).
			h.app.log.Error("driver_error", []any{"game_id", h.id, "player_id", f.seatID, "kind", in.Kind.String(), "structure", f.structID, "err", derr}...)
			return
		}
		if f.ctx.Err() != nil {
			h.logDiscard(f, in.Kind, "cancelled")
			return
		}
		outcome := h.applyCandidate(f, cand)
		if outcome.applied {
			h.app.log.Info("llm_decision", []any{"game_id", h.id, "player_id", f.seatID, "kind", in.Kind.String(), "structure", f.structID, "attempts", attempt + 1, "inference_ms", inference.Milliseconds(), "total_ms", h.app.deps.Clock.Now().Sub(f.startedAt).Milliseconds()}...)
			return
		}
		if outcome.terminal || f.ctx.Err() != nil {
			h.logDiscard(f, in.Kind, discardReason(outcome))
			return
		}
		if attempt >= maxDriverRetries {
			h.applyKindDefault(f) // A4 kind-safe default; logs its own line
			return
		}
		feedback = outcome.feedback
		in.Feedback = feedback
		h.app.log.Warn("driver_retry", []any{"game_id", h.id, "player_id", f.seatID, "kind", in.Kind.String(), "structure", f.structID, "attempt", attempt + 1, "feedback", feedback}...)
		attempt++
	}
}

func discardReason(outcome applyOutcome) string {
	if outcome.terminal {
		return "structure_changed"
	}
	return "cancelled"
}

func (h *gameHandle) logDiscard(f *flight, kind DecisionKind, reason string) {
	h.app.log.Info("llm_discarded", []any{"game_id", h.id, "player_id", f.seatID, "kind", kind.String(), "structure", f.structID, "reason", reason}...)
}

// applyOutcome reports the fate of one candidate under engine validation.
type applyOutcome struct {
	applied  bool
	terminal bool   // structure changed or unrecoverable: stop retrying, discard
	feedback string // table-voice error for DecisionInput.Feedback
}

// applyCandidate submits one candidate through the appropriate engine entry
// point under the game lock and commits the resulting batch. The caller must
// NOT hold h.mu (it is taken here).
func (h *gameHandle) applyCandidate(f *flight, cand Candidate) applyOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.g
	var evs []domain.Event
	var err error
	switch f.kind {
	case KindTurnAction:
		a, ok := parseAction(cand)
		if !ok {
			return applyOutcome{feedback: "not a valid action"}
		}
		evs, err = g.DeclareTurnAction(f.seatID, a)
	case KindWindowResponse:
		r, ok := parseWindowResponse(cand)
		if !ok {
			return applyOutcome{feedback: "not a valid response"}
		}
		evs, err = g.SubmitWindowResponse(f.seatID, r)
	case KindSingleSeatChoice:
		dv := g.DecisionView()
		if dv == nil || dv.ID != f.structID {
			return applyOutcome{terminal: true} // the structure is gone; drop it
		}
		c, ok := parseChoice(cand, dv)
		if !ok {
			return applyOutcome{feedback: "that is not a valid option"}
		}
		evs, err = g.AnswerDecision(f.seatID, dv.ID, c)
	}
	if err != nil {
		var le *domain.LegacyError
		if errors.As(err, &le) {
			return applyOutcome{feedback: le.Message} // §6.4.4 feedback loop
		}
		h.app.log.Error("apply_unexpected_error", []any{"game_id", h.id, "player_id", f.seatID, "kind", f.kind.String(), "err", err}...)
		return applyOutcome{terminal: true}
	}
	if _, aerr := h.commitLocked(g, evs); aerr != nil {
		h.app.log.Error("apply_commit_failed", []any{"game_id", h.id, "player_id", f.seatID, "err", aerr}...)
		return applyOutcome{terminal: true}
	}
	return applyOutcome{applied: true}
}

// applyKindDefault applies the A4 kind-safe default after the driver retry
// budget is exhausted: turn ⇒ income, window ⇒ pass, choice ⇒ engine default
// (first flip option / no-swap). It never loops: one shot, log, move on.
func (h *gameHandle) applyKindDefault(f *flight) {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.g
	var evs []domain.Event
	var err error
	switch f.kind {
	case KindTurnAction:
		evs, err = g.DeclareTurnAction(f.seatID, domain.Action{Kind: domain.Income})
	case KindWindowResponse:
		evs, err = g.SubmitWindowResponse(f.seatID, domain.WindowResponse{Kind: domain.RespPass})
	case KindSingleSeatChoice:
		dv := g.DecisionView()
		if dv == nil || dv.ID != f.structID {
			h.app.log.Warn("decision_fallback_skipped", []any{"game_id", h.id, "player_id", f.seatID, "structure", f.structID, "reason", "structure_changed"}...)
			return
		}
		evs, err = g.AnswerDecision(f.seatID, dv.ID, dv.Kind.DefaultChoice())
	}
	if err != nil {
		h.app.log.Error("decision_fallback_failed", []any{"game_id", h.id, "player_id", f.seatID, "kind", f.kind.String(), "err", err}...)
		return
	}
	if _, aerr := h.commitLocked(g, evs); aerr != nil {
		h.app.log.Error("decision_fallback_commit_failed", []any{"game_id", h.id, "player_id", f.seatID, "err", aerr}...)
		return
	}
	h.app.log.Warn("decision_fallback", []any{"game_id", h.id, "player_id", f.seatID, "kind", f.kind.String(), "structure", f.structID, "default", kindDefaultName(f.kind)}...)
}

func kindDefaultName(k DecisionKind) string {
	switch k {
	case KindTurnAction:
		return "income"
	case KindWindowResponse:
		return "pass"
	default:
		return "engine_default"
	}
}

// parseAction converts a candidate into a domain action. Unparseable shapes
// are reported so the retry loop can feed them back (bounded by A4).
func parseAction(cand Candidate) (domain.Action, bool) {
	kind, ok := domain.ActionKindFromName(strings.ToLower(cand.Action))
	if !ok {
		return domain.Action{}, false
	}
	a := domain.Action{Kind: kind, TargetID: cand.TargetID}
	if kind.ClaimOptions() != nil {
		ch, ok := domain.CharacterFromName(strings.ToLower(cand.Character))
		if !ok {
			return domain.Action{}, false
		}
		a.Character = ch
	}
	return a, true
}

// parseWindowResponse converts a candidate response into the window grammar.
func parseWindowResponse(cand Candidate) (domain.WindowResponse, bool) {
	switch strings.ToLower(cand.Response) {
	case "pass":
		return domain.WindowResponse{Kind: domain.RespPass}, true
	case "challenge":
		return domain.WindowResponse{Kind: domain.RespChallenge}, true
	case "counterclaim":
		ch, ok := domain.CharacterFromName(strings.ToLower(cand.Character))
		if !ok {
			return domain.WindowResponse{}, false
		}
		return domain.WindowResponse{Kind: domain.RespCounterclaim, Character: ch}, true
	default:
		return domain.WindowResponse{}, false
	}
}

// parseChoice converts a candidate choice for a pending single-seat decision.
// Exchange-finish outcomes are one identifier each (see Request.Options):
// "no_swap" or "swap_drawn_<i>_<handOption>".
func parseChoice(cand Candidate, dv *domain.DecisionView) (domain.Choice, bool) {
	switch dv.Kind {
	case domain.DecisionFlipChoice:
		if cand.Choice == "" {
			return domain.Choice{}, false
		}
		return domain.Choice{FlipOption: cand.Choice}, true
	default: // DecisionExchangeFinish
		switch c := strings.ToLower(cand.Choice); {
		case c == "no_swap":
			return domain.Choice{ExchangeNoSwap: true}, true
		case strings.HasPrefix(c, "swap_drawn_"):
			parts := strings.Split(c, "_")
			// swap_drawn_<i>_<handOption-as-face_down_j...> →
			// [swap, drawn, <i>, face, down, <j>]
			if len(parts) != 6 || parts[3] != "face" || parts[4] != "down" {
				return domain.Choice{}, false
			}
			i, err1 := strconv.Atoi(parts[2])
			j, err2 := strconv.Atoi(parts[5])
			if err1 != nil || err2 != nil || i < 0 || i >= len(dv.DrawnCards) || j < 0 || j > 1 {
				return domain.Choice{}, false
			}
			return domain.Choice{ExchangeDrawnIdx: i, ExchangeHandOpt: "face_down_" + parts[5]}, true
		default:
			return domain.Choice{}, false
		}
	}
}

// cancelAll stops every in-flight LLM decision and structure watch of the
// game (spec §9: server shutdown / end-of-life of the handle).
func (h *gameHandle) cancelAll(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.inFlight {
		h.cancelFlightLocked(f, reason)
	}
	if h.meta != nil {
		close(h.meta.stop)
		h.meta = nil
	}
}

package application

import (
	"context"
	"strings"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// Decision kinds accepted by SubmitDecision (wire "kind" values, spec §11.2).
const (
	DecisionKindAct     = "act"     // declare the turn action
	DecisionKindRespond = "respond" // respond to the open window
	DecisionKindChoose  = "choose"  // answer a pending single-seat decision
)

// DecisionIn mirrors the wire body of POST /games/{id}/decisions (spec §11.2):
// one endpoint for every player-originated move, discriminated by Kind. The
// handler decodes JSON straight into this struct; validation below turns the
// payload checks into invalid_payload errors.
type DecisionIn struct {
	Kind       string      `json:"kind"` // "act" | "respond" | "choose"
	Action     *ActionIn   `json:"action,omitempty"`
	Response   *ResponseIn `json:"response,omitempty"`
	WindowID   string      `json:"window_id,omitempty"` // A16: implicit; a supplied mismatched id is rejected
	DecisionID string      `json:"decision_id,omitempty"`
	Choice     *ChoiceIn   `json:"choice,omitempty"`
}

// ActionIn is the wire shape of an act submission. Character is required for
// tax/steal/assassinate/exchange (the claim); optional-but-ignored for base
// actions. TargetID is required for coup/steal/assassinate.
type ActionIn struct {
	Type      string `json:"type"` // income|foreign_aid|coup|tax|steal|assassinate|exchange
	Character string `json:"character,omitempty"`
	TargetID  string `json:"target_id,omitempty"`
}

// ResponseIn is the wire shape of a respond submission: pass | counterclaim |
// challenge.
type ResponseIn struct {
	Type      string `json:"type"`
	Character string `json:"character,omitempty"` // counterclaims only
}

// ChoiceIn is the wire shape of a choose submission; exactly one of
// FlipChoice / ExchangeFinish must be set, matching the pending decision kind.
type ChoiceIn struct {
	FlipChoice     string            `json:"flip_choice,omitempty"`     // "face_down_0" | "face_down_1" (§5.4)
	ExchangeFinish *ExchangeFinishIn `json:"exchange_finish,omitempty"` // one of (a)/(b)/(c), §11.2
}

// ExchangeFinishIn covers all three exchange-finish outcomes: no_swap (both
// drawn cards return, hand unchanged), or keep drawn_index and return the
// named face-down hand_option to the Court Deck as an unordered set.
type ExchangeFinishIn struct {
	NoSwap     bool   `json:"no_swap"`
	DrawnIndex int    `json:"drawn_index"` // 0 = left, 1 = right; only when !NoSwap
	HandOption string `json:"hand_option,omitempty"`
}

// SubmitDecisionResult is the 202 body: accepted (queued for resolution), not
// final outcome — outcomes arrive as events on the poll stream.
type SubmitDecisionResult struct {
	Accepted bool  `json:"accepted"`
	EventSeq int64 `json:"event_seq"` // first seq of the resulting batch
}

// SubmitDecision implements POST /games/{id}/decisions (spec §11.2): a single
// endpoint for turn actions, window responses and single-seat choices, with
// A16 semantics — each seat responds at most once per window ("you have
// already responded"), stale or mismatched window ids hit the resolved state
// through normal validation ("the window has already been resolved", 409),
// and every semantic rejection is additionally recorded as a private
// rejection event on the offending seat's stream (A9).
func (a *Application) SubmitDecision(ctx context.Context, gameID, viewerSeatID string, in DecisionIn) (*SubmitDecisionResult, error) {
	h, err := a.game(gameID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	viewer, verr := h.humanSeat(viewerSeatID)
	finished := h.g.Status == domain.Finished
	h.mu.Unlock()
	if verr != nil {
		return nil, verr
	}

	firstSeq, engineErr := h.run(func(g *domain.Game) ([]domain.Event, error) {
		if finished {
			return nil, &domain.LegacyError{Code: domain.CodeGameOver, Message: "the game is over"}
		}
		switch in.Kind {
		case DecisionKindAct:
			action, perr := buildAction(in.Action)
			if perr != nil {
				return nil, perr
			}
			return g.DeclareTurnAction(viewer.ID, action)
		case DecisionKindRespond:
			resp, perr := buildResponse(in.Response, in.WindowID, g)
			if perr != nil {
				return nil, perr
			}
			return g.SubmitWindowResponse(viewer.ID, resp)
		case DecisionKindChoose:
			choice, perr := buildChoice(in.DecisionID, in.Choice)
			if perr != nil {
				return nil, perr
			}
			return g.AnswerDecision(viewer.ID, in.DecisionID, choice)
		default:
			return nil, NewAppError(ErrInvalidInput, `kind must be "act", "respond" or "choose"`, 400)
		}
	})
	if engineErr != nil {
		if ae, ok := AsAppError(engineErr); ok {
			h.recordRejection(viewer.ID, ae) // A9: private rejection event, own seq
		}
		return nil, engineErr
	}
	return &SubmitDecisionResult{Accepted: true, EventSeq: firstSeq}, nil
}

// buildAction validates the act payload (400 class errors) and maps it to a
// domain action; claim/target presence rules come from the action grammar.
func buildAction(in *ActionIn) (domain.Action, error) {
	if in == nil {
		return domain.Action{}, NewAppError(ErrInvalidInput, `an act decision requires "action"`, 400)
	}
	kind, ok := domain.ActionKindFromName(strings.ToLower(in.Type))
	if !ok {
		return domain.Action{}, NewAppError(ErrInvalidInput, `unknown action type `+quote(in.Type), 400)
	}
	a := domain.Action{Kind: kind, TargetID: in.TargetID}
	if opts := kind.ClaimOptions(); opts != nil {
		ch, ok := domain.CharacterFromName(strings.ToLower(in.Character))
		if !ok || !characterInList(opts, ch) {
			return domain.Action{}, NewAppError(ErrInvalidInput, "a valid claimed character is required for "+kind.String(), 400)
		}
		a.Character = ch
	}
	if kind.RequiresTarget() && in.TargetID == "" {
		return domain.Action{}, NewAppError(ErrInvalidInput, "target_id is required for "+kind.String(), 400)
	}
	return a, nil
}

// buildResponse validates the respond payload. A supplied window_id that does
// not match the one open window (or none) is rejected with the stale-window
// semantics of A16 — same message and code as a late response.
func buildResponse(in *ResponseIn, windowID string, g *domain.Game) (domain.WindowResponse, error) {
	if in == nil {
		return domain.WindowResponse{}, NewAppError(ErrInvalidInput, `a respond decision requires "response"`, 400)
	}
	wv := g.WindowView()
	if windowID != "" && (wv == nil || wv.ID != windowID) {
		return domain.WindowResponse{}, &domain.LegacyError{Code: domain.CodeWindowResolved, Message: "the window has already been resolved"} // A16
	}
	switch strings.ToLower(in.Type) {
	case "pass":
		return domain.WindowResponse{Kind: domain.RespPass}, nil
	case "challenge":
		return domain.WindowResponse{Kind: domain.RespChallenge}, nil
	case "counterclaim":
		ch, ok := domain.CharacterFromName(strings.ToLower(in.Character))
		if !ok {
			return domain.WindowResponse{}, NewAppError(ErrInvalidInput, "a counterclaim requires a claimed character", 400)
		}
		return domain.WindowResponse{Kind: domain.RespCounterclaim, Character: ch}, nil
	default:
		return domain.WindowResponse{}, NewAppError(ErrInvalidInput, `response type must be "pass", "counterclaim" or "challenge"`, 400)
	}
}

// buildChoice validates the choose payload shape (the engine re-validates the
// option values against the pending decision).
func buildChoice(decisionID string, in *ChoiceIn) (domain.Choice, error) {
	if in == nil {
		return domain.Choice{}, NewAppError(ErrInvalidInput, `a choose decision requires "choice" and "decision_id"`, 400)
	}
	if decisionID == "" {
		return domain.Choice{}, NewAppError(ErrInvalidInput, "decision_id is required", 400)
	}
	switch {
	case in.FlipChoice != "" && in.ExchangeFinish == nil:
		return domain.Choice{FlipOption: in.FlipChoice}, nil
	case in.FlipChoice == "" && in.ExchangeFinish != nil:
		e := in.ExchangeFinish
		if e.NoSwap {
			return domain.Choice{ExchangeNoSwap: true}, nil
		}
		if e.DrawnIndex < 0 || e.DrawnIndex > 1 || e.HandOption == "" {
			return domain.Choice{}, NewAppError(ErrInvalidInput, "exchange_finish requires drawn_index (0 or 1) and hand_option", 400)
		}
		return domain.Choice{ExchangeDrawnIdx: e.DrawnIndex, ExchangeHandOpt: e.HandOption}, nil
	default:
		return domain.Choice{}, NewAppError(ErrInvalidInput, "choice must set exactly one of flip_choice or exchange_finish", 400)
	}
}

func quote(s string) string { return `"` + s + `"` }

// characterInList reports whether ch is among the allowed claim characters.
func characterInList(opts []domain.Character, ch domain.Character) bool {
	for _, o := range opts {
		if o == ch {
			return true
		}
	}
	return false
}

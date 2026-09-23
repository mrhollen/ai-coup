package application

import (
	"context"
	"time"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// Snapshot is the per-viewer state of spec §11.2, already privacy-filtered:
// seat-scoped decision options appear ONLY in the addressed viewer's snapshot;
// face-down cards appear only for the viewer's own seat; LLM seats have no
// snapshots at all (no token exists for them).
type Snapshot struct {
	GameID         string            `json:"game_id"`
	Status         string            `json:"status"` // "in_progress" | "finished"
	WinnerID       *string           `json:"winner_id"`
	WindowSettings WindowSettingsOut `json:"window_settings"`
	CourtDeckSize  int               `json:"court_deck_size"`
	Seats          []SeatSnap        `json:"seats"`
	YourSeatID     string            `json:"your_seat_id"`
	YourFaceDown   []string          `json:"your_face_down_cards"`
	LastEventSeq   int64             `json:"last_event_seq"`
	Current        *CurrentSnap      `json:"current"`
}

// SeatSnap is one seat's public view (all seats' coins and face-up cards are
// public, §8).
type SeatSnap struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Coins       int      `json:"coins"`
	FaceUpCards []string `json:"face_up_cards"`
	Active      bool     `json:"active"`
}

// CurrentSnap is the phase-specific shape of "current" (§11.2): exactly one
// of turn_action / window_open / single_seat_decision / finished carries its
// fields.
type CurrentSnap struct {
	Phase      string        `json:"phase"` // turn_action|window_open|single_seat_decision|finished
	TurnID     int           `json:"turn_id,omitempty"`
	TurnSeatID string        `json:"turn_seat_id,omitempty"`
	Window     *WindowSnap   `json:"window,omitempty"`
	Decision   *DecisionSnap `json:"decision,omitempty"`
}

// WindowSnap is the open window as a viewer sees it. YourAllowedResponses is
// present only when the viewer is eligible (the response grammar — public
// table information, never advice).
type WindowSnap struct {
	WindowID             string            `json:"window_id"`
	Kind                 string            `json:"kind"` // challenge | counterclaim_challenge
	TargetSeatID         string            `json:"target_seat_id,omitempty"`
	RequireAllResponses  bool              `json:"require_all_responses"`
	TimeoutMs            int               `json:"timeout_ms"`
	DeadlineAt           *string           `json:"deadline_at,omitempty"`
	YourAllowedResponses []string          `json:"your_allowed_responses,omitempty"`
	ResponsesSoFar       map[string]string `json:"responses_so_far"`
}

// DecisionSnap is the pending single-seat decision. Options appears ONLY for
// the addressed viewer (seat-scoped, §8); every other viewer receives the same
// object with options omitted.
type DecisionSnap struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"` // flip_choice | exchange_finish
	SeatID     string  `json:"seat_id"`
	Options    any     `json:"options,omitempty"` // addressed viewer only
	TimeoutMs  int     `json:"timeout_ms"`
	DeadlineAt *string `json:"deadline_at,omitempty"`
}

// GetSnapshot implements GET /games/{id} for a human viewer (spec §11.2).
// The game lock is held only while reading the aggregate; event sequence
// numbers come from the store (append-only, so no lock is needed there).
func (a *Application) GetSnapshot(ctx context.Context, gameID, viewerSeatID string) (*Snapshot, error) {
	h, err := a.game(gameID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	viewer, verr := h.humanSeat(viewerSeatID)
	if verr != nil {
		h.mu.Unlock()
		return nil, verr
	}
	snap := snapshotFor(h.g, viewer, h.meta, a.deps.Clock.Now())
	h.mu.Unlock()

	lastSeq, err := a.deps.Events.LastSeq(ctx, gameID)
	if err != nil {
		return nil, FromDomain(err)
	}
	snap.LastEventSeq = lastSeq
	return snap, nil
}

func snapshotFor(g *domain.Game, viewer *domain.Seat, meta *structureMeta, now time.Time) *Snapshot {
	status := "in_progress"
	var winner *string
	if g.Status == domain.Finished {
		status = "finished"
		winner = &g.WinnerID
	}
	snap := &Snapshot{
		GameID:   g.ID,
		Status:   status,
		WinnerID: winner,
		WindowSettings: WindowSettingsOut{
			RequireAllResponses: g.Settings.RequireAllResponses,
			ResponseTimeoutMs:   g.Settings.TimeoutMs,
		},
		CourtDeckSize: g.Deck.Size(),
		Seats:         make([]SeatSnap, 0, len(g.Seats)),
		YourSeatID:    viewer.ID,
	}
	yours := []string{}
	for i := range g.Seats {
		s := &g.Seats[i]
		faceUp := make([]string, 0, len(s.FaceUp))
		for _, c := range s.FaceUp {
			faceUp = append(faceUp, c.Character.String())
		}
		snap.Seats = append(snap.Seats, SeatSnap{
			ID:          s.ID,
			Name:        s.Name,
			Type:        s.Type.String(),
			Coins:       s.Coins,
			FaceUpCards: faceUp,
			Active:      s.Active(),
		})
		if s.ID == viewer.ID {
			yours = make([]string, 0, len(s.FaceDown))
			for _, c := range s.FaceDown {
				yours = append(yours, c.Character.String())
			}
		}
	}
	snap.YourFaceDown = yours

	cur := &CurrentSnap{Phase: string(g.Phase())}
	switch g.Phase() {
	case domain.PhaseTurnAction:
		cur.TurnID = g.TurnID
		cur.TurnSeatID = g.Seats[g.TurnSeatIndex].ID
	case domain.PhaseWindowOpen:
		wv := g.WindowView()
		if wv != nil {
			ws := &WindowSnap{
				WindowID:            wv.ID,
				Kind:                wv.Kind.String(),
				TargetSeatID:        wv.TargetID,
				RequireAllResponses: wv.Settings.RequireAllResponses,
				TimeoutMs:           wv.Settings.TimeoutMs,
				ResponsesSoFar:      map[string]string{},
			}
			for k, v := range wv.Responses {
				ws.ResponsesSoFar[k] = v
			}
			if tokens := g.AllowedResponsesFor(viewer.ID); len(tokens) > 0 {
				ws.YourAllowedResponses = tokens
			}
			if meta != nil && meta.id == wv.ID && wv.Settings.TimeoutMs > 0 {
				dl := now.Add(time.Duration(wv.Settings.TimeoutMs) * time.Millisecond).UTC().Format(time.RFC3339)
				ws.DeadlineAt = &dl
			}
			cur.Window = ws
		}
	case domain.PhaseSingleSeatDecision:
		dv := g.DecisionView()
		if dv != nil {
			ds := &DecisionSnap{
				ID:        dv.ID,
				Kind:      dv.Kind.String(),
				SeatID:    dv.SeatID,
				TimeoutMs: dv.TimeoutMs,
			}
			if dv.SeatID == viewer.ID { // options are seat-scoped (§8)
				switch dv.Kind {
				case domain.DecisionFlipChoice:
					ds.Options = append([]string{}, dv.Options...)
				default: // DecisionExchangeFinish — same shape as the wire request
					ds.Options = map[string]any{
						"drawn_index":  []int{0, 1},
						"hand_options": []string{"face_down_0", "face_down_1"},
					}
				}
			}
			if meta != nil && meta.id == dv.ID && dv.TimeoutMs > 0 {
				dl := now.Add(time.Duration(dv.TimeoutMs) * time.Millisecond).UTC().Format(time.RFC3339)
				ds.DeadlineAt = &dl
			}
			cur.Decision = ds
		}
	case domain.PhaseFinished:
		// terminal state; nothing else set on "current"
	}
	snap.Current = cur
	return snap
}

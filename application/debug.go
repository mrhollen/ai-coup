package application

import (
	"strconv"
	"strings"
	"time"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// DebugState is the full, UNFILTERED state of one game as served by the
// COUP_DEBUG-gated /debug endpoint (§11.2): every seat's face-down cards are
// included — it is a local operator tool and must never be exposed without the
// gate (A10). It is built under the per-game lock, so concurrent engine
// mutation can never race it; the HTTP layer adds the unfiltered event
// history (from the EventStore) and build config around this state.
type DebugState struct {
	GameID            string            `json:"game_id"`
	Status            string            `json:"status"` // "in_progress" | "finished"
	WinnerID          *string           `json:"winner_id,omitempty"`
	TurnID            int               `json:"turn_id"`
	Phase             string            `json:"phase"`
	WindowSettings    WindowSettingsOut `json:"window_settings"`
	CourtDeck         []string          `json:"court_deck"` // internal card ids, top first — debug only
	Seats             []DebugSeat       `json:"seats"`
	OpenWindow        *DebugWindow      `json:"open_window"`
	PendingDecisions  []DebugDecision   `json:"pending_decisions"`
	ActiveForfeitVote *DebugVote        `json:"active_forfeit_vote"`
}

// DebugSeat is one seat with its FULL hand (every seat's face-down cards are
// visible here — debug scope only, §11.2).
type DebugSeat struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	Coins    int         `json:"coins"`
	FaceDown []string    `json:"face_down"`
	FaceUp   []string    `json:"face_up"`
	Active   bool        `json:"active"`
	Driver   DebugDriver `json:"driver"`
}

// DebugDriver describes the driver backing one seat. ConversationMessages
// reports the driver's stored per-seat conversation length (§6.4) — real data
// from drivers implementing ConversationReporter (the openai driver), 0 for
// drivers that keep no conversations (heuristic, fake, unavailable).
// InFlight reports a decision currently being produced for the seat, if any.
type DebugDriver struct {
	Mode                 string         `json:"mode"`
	ConversationMessages int            `json:"conversation_messages"`
	InFlight             *DebugInFlight `json:"in_flight_decision"` // nil when none
}

// DebugInFlight is one in-flight LLM decision (§11.2 debug shape); exactly one
// of WindowID/DecisionID/TurnID is set, mirroring the flight's structure id.
type DebugInFlight struct {
	Kind       string `json:"kind"` // "turn_action" | "window_response" | "single_seat_choice"
	WindowID   string `json:"window_id,omitempty"`
	DecisionID string `json:"decision_id,omitempty"`
	TurnID     int    `json:"turn_id,omitempty"`
	StartedAt  string `json:"started_at"`
}

// DebugWindow is the open response window, unfiltered (all responses so far).
type DebugWindow struct {
	WindowID            string            `json:"window_id"`
	Kind                string            `json:"kind"` // "challenge" | "counterclaim_challenge"
	TargetSeatID        string            `json:"target_seat_id,omitempty"`
	RequireAllResponses bool              `json:"require_all_responses"`
	TimeoutMs           int               `json:"timeout_ms"`
	Eligible            []string          `json:"eligible"`
	Responses           map[string]string `json:"responses"`
}

// DebugDecision is a pending single-seat decision (kind + addressed seat are
// public; the options themselves live only in the addressed seat's snapshot).
type DebugDecision struct {
	ID        string `json:"decision_id"`
	Kind      string `json:"kind"` // "flip_choice" | "exchange_finish"
	SeatID    string `json:"seat_id"`
	TimeoutMs int    `json:"timeout_ms"`
}

// DebugVote is the active forfeit ballot (nil when none is active — resolved
// votes leave the aggregate by construction).
type DebugVote struct {
	VoteID          string   `json:"vote_id"`
	TargetSeatID    string   `json:"target_seat_id"`
	InitiatorSeatID string   `json:"initiator_seat_id"`
	EligibleVoters  []string `json:"eligible_voters"`
	MajorityNeeded  int      `json:"majority_needed"`
	Yes             int      `json:"yes"`
	No              int      `json:"no"`
}

// DebugGame returns the full unfiltered state of one game for the §11.2 debug
// endpoint. The per-game lock is held while copying, so this is race-free
// against concurrent engine mutation (spec §9). Unknown games yield a 404
// AppError; the caller is expected to have gated the route on COUP_DEBUG (A10).
func (a *Application) DebugGame(gameID string) (*DebugState, error) {
	h, err := a.game(gameID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	st := &DebugState{
		GameID:           h.id,
		Status:           "in_progress",
		TurnID:           h.g.TurnID,
		Phase:            string(h.g.Phase()),
		CourtDeck:        []string{},
		Seats:            make([]DebugSeat, 0, len(h.g.Seats)),
		PendingDecisions: []DebugDecision{},
	}
	st.WindowSettings = WindowSettingsOut{
		RequireAllResponses: h.g.Settings.RequireAllResponses,
		ResponseTimeoutMs:   h.g.Settings.TimeoutMs,
	}
	if h.g.Status == domain.Finished {
		st.Status = "finished"
		w := h.g.WinnerID
		st.WinnerID = &w
	}
	for _, c := range h.g.Deck.Cards() { // top first (debug only)
		st.CourtDeck = append(st.CourtDeck, cardDebugID(c))
	}

	mode := ""
	var convReporter ConversationReporter
	if a.deps.Driver != nil {
		mode = a.deps.Driver.Name()
		if cr, ok := a.deps.Driver.(ConversationReporter); ok {
			convReporter = cr // §6.4 drivers report their stored conversation length
		}
	}
	for i := range h.g.Seats {
		s := &h.g.Seats[i]
		ds := DebugSeat{
			ID:       s.ID,
			Name:     s.Name,
			Type:     s.Type.String(),
			Coins:    s.Coins,
			FaceDown: []string{},
			FaceUp:   []string{},
			Active:   s.Active(),
			Driver:   DebugDriver{Mode: mode},
		}
		if convReporter != nil && s.Type == domain.SeatLLM {
			ds.Driver.ConversationMessages = convReporter.ConversationMessages(h.id, s.ID)
		}
		for _, c := range s.FaceDown {
			ds.FaceDown = append(ds.FaceDown, c.Character.String())
		}
		for _, c := range s.FaceUp {
			ds.FaceUp = append(ds.FaceUp, c.Character.String())
		}
		if f, ok := h.inFlight[s.ID]; ok && !f.cancelled {
			in := &DebugInFlight{Kind: f.kind.String(), StartedAt: f.startedAt.UTC().Format(time.RFC3339)}
			switch f.kind {
			case KindWindowResponse:
				in.WindowID = f.structID
			case KindSingleSeatChoice:
				in.DecisionID = f.structID
			default: // turn flights carry structID "turn:<n>"
				if n, perr := strconv.Atoi(strings.TrimPrefix(f.structID, "turn:")); perr == nil {
					in.TurnID = n
				}
			}
			ds.Driver.InFlight = in
		}
		st.Seats = append(st.Seats, ds)
	}

	if wv := h.g.WindowView(); wv != nil {
		resp := make(map[string]string, len(wv.Responses))
		for k, v := range wv.Responses {
			resp[k] = v
		}
		st.OpenWindow = &DebugWindow{
			WindowID:            wv.ID,
			Kind:                wv.Kind.String(),
			TargetSeatID:        wv.TargetID,
			RequireAllResponses: wv.Settings.RequireAllResponses,
			TimeoutMs:           wv.Settings.TimeoutMs,
			Eligible:            append([]string{}, wv.Eligible...),
			Responses:           resp,
		}
	}
	if dv := h.g.DecisionView(); dv != nil {
		st.PendingDecisions = append(st.PendingDecisions, DebugDecision{
			ID:        dv.ID,
			Kind:      dv.Kind.String(),
			SeatID:    dv.SeatID,
			TimeoutMs: dv.TimeoutMs,
		})
	}
	if vv := h.g.VoteView(); vv != nil {
		st.ActiveForfeitVote = &DebugVote{
			VoteID:          vv.ID,
			TargetSeatID:    vv.TargetID,
			InitiatorSeatID: vv.InitiatorID,
			EligibleVoters:  append([]string{}, vv.EligibleVoters...),
			MajorityNeeded:  vv.MajorityNeeded,
			Yes:             vv.Yes,
			No:              vv.No,
		}
	}
	return st, nil
}

// cardDebugID renders a card's internal identity (character, copy-index) for
// the debug dump only; the copy index never appears on any client wire (§3.1).
func cardDebugID(c domain.Card) string {
	return c.Character.String() + "-" + strconv.Itoa(c.Copy)
}

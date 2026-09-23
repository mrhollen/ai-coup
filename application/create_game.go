package application

import (
	"context"
	"strconv"
	"strings"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// PlayerSpec is one seat as supplied at creation (spec §6.1). Names are
// trimmed; blank names get the A14 defaults ("Vito", "Sonnie"... for LLM seats
// cycled in seat order, "Player N" for human seats).
type PlayerSpec struct {
	Name string
	Type string // "human" | "llm"
}

// CreateGameRequest is the use-case input. Window settings omitted ⇒ A8
// defaults: require_all_responses=false, response_timeout none (open-ended
// FCFS). The handler layer parses the wire duration string into milliseconds.
type CreateGameRequest struct {
	Players           []PlayerSpec
	RequireAll        bool // false when the window object is omitted (A8)
	ResponseTimeoutMs int  // 0 = no timeout (A8); negative rejected
}

// SeatOut is one seat in the create response. Token appears ONLY for human
// seats and only here — it is shown exactly once, never revoked (A2).
type SeatOut struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Token string `json:"token,omitempty"` // humans only
}

// WindowSettingsOut mirrors the wire shape of domain.WindowSettings (§10.1).
type WindowSettingsOut struct {
	RequireAllResponses bool `json:"require_all_responses"`
	ResponseTimeoutMs   int  `json:"response_timeout_ms"`
}

// CreateGameResult is the 201 response body (spec §11.2) — the only place
// tokens appear.
type CreateGameResult struct {
	GameID          string            `json:"game_id"`
	Seats           []SeatOut         `json:"seats"`
	WindowSettings  WindowSettingsOut `json:"window_settings"`
	FirstTurnSeatID string            `json:"first_turn_seat_id"`
}

// llmDefaultNames are the A14 blank-name defaults for LLM seats, cycled in
// seat order.
var llmDefaultNames = []string{"Vito", "Sonny", "Carmine", "Rocco", "Salvatore", "Bruno"}

// CreateGame implements POST /games (spec §6.1): it validates the request,
// mints ids and human bearer tokens, builds the game through the pure engine
// (which shuffles the deck, deals, picks the random start seat per rules.md
// Ruling 6) and commits the initial batch — which also starts driving any LLM
// seat holding the first turn.
func (a *Application) CreateGame(ctx context.Context, req CreateGameRequest) (*CreateGameResult, error) {
	specs, err := normalizePlayerSpecs(req.Players)
	if err != nil {
		return nil, err
	}
	settings := domain.WindowSettings{RequireAllResponses: req.RequireAll, TimeoutMs: req.ResponseTimeoutMs}

	gameID := a.deps.IDs.New("g")
	domainSpecs := make([]domain.SeatSpec, 0, len(specs))
	tokens := map[string]string{}
	for _, s := range specs {
		id := a.deps.IDs.New("s")
		s.ID = id
		domainSpecs = append(domainSpecs, s)
		if s.Type == domain.SeatHuman {
			tok, terr := a.deps.TokenSource()
			if terr != nil {
				return nil, NewAppError(ErrServer, "token generation failed", 500)
			}
			tokens[id] = tok
		}
	}

	g, evs, err := domain.NewGame(gameID, domainSpecs, settings, a.deps.IDs, a.deps.Rng)
	if err != nil {
		return nil, FromDomain(err)
	}
	h := &gameHandle{
		app:      a,
		id:       gameID,
		g:        g,
		tokens:   tokens,
		inFlight: map[string]*flight{},
		done:     map[string]map[string]bool{},
	}
	a.register(h)
	a.deps.Games.Put(g)

	if _, err := h.run(func(*domain.Game) ([]domain.Event, error) { return evs, nil }); err != nil {
		return nil, err
	}

	result := &CreateGameResult{
		GameID:          gameID,
		WindowSettings:  WindowSettingsOut{RequireAllResponses: settings.RequireAllResponses, ResponseTimeoutMs: settings.TimeoutMs},
		FirstTurnSeatID: g.Seats[g.TurnSeatIndex].ID,
	}
	for i := range specs {
		result.Seats = append(result.Seats, SeatOut{
			ID:    specs[i].ID,
			Name:  specs[i].Name,
			Type:  specs[i].Type.String(),
			Token: tokens[specs[i].ID], // "" for LLM seats (omitted by json)
		})
	}
	a.log.Info("game_created", []any{"game_id", gameID, "seats", len(specs), "require_all_responses", settings.RequireAllResponses, "response_timeout_ms", settings.TimeoutMs, "first_turn_seat_id", result.FirstTurnSeatID}...)
	return result, nil
}

// normalizePlayerSpecs validates the seat list and applies A14 name defaults.
func normalizePlayerSpecs(players []PlayerSpec) ([]domain.SeatSpec, error) {
	if len(players) < 2 || len(players) > 6 {
		return nil, NewAppError(ErrInvalidInput, "a game needs between two and six seats", 400)
	}
	humans := 0
	llmIdx := 0
	out := make([]domain.SeatSpec, 0, len(players))
	for i, p := range players {
		st := strings.ToLower(strings.TrimSpace(p.Type))
		var typ domain.SeatType
		switch st {
		case "human":
			typ = domain.SeatHuman
			humans++
		case "llm":
			typ = domain.SeatLLM
		default:
			return nil, NewAppError(ErrInvalidInput, `seat type must be "human" or "llm"`, 400)
		}
		name := strings.TrimSpace(p.Name)
		if len(name) > 32 {
			return nil, NewAppError(ErrInvalidInput, "player names are limited to 32 characters", 400)
		}
		if name == "" { // A14 defaults
			switch typ {
			case domain.SeatLLM:
				name = llmDefaultNames[llmIdx%len(llmDefaultNames)]
				llmIdx++
			default:
				name = "Player " + strconv.Itoa(i+1) // displayed seat number (1-based)
			}
		}
		out = append(out, domain.SeatSpec{Name: name, Type: typ})
	}
	if humans < 1 {
		return nil, NewAppError(ErrInvalidInput, "a game needs at least one human seat", 400)
	}
	return out, nil
}

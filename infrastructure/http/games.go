package http

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	app "forgejo.lan/mrhollen/ai-coup/application"
)

// createPlayerWire is one per-seat entry of POST /games (§11.2): name may be
// blank (A14 defaults apply in the application layer); type is "human"|"llm".
type createPlayerWire struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// windowSettingsWire is the optional `window` object of POST /games. Omitted
// ⇒ A8 defaults (require_all_responses=false, no timeout); response_timeout is
// a Go duration string ("45s") or null — parsed to milliseconds here because
// the application layer works in plain int milliseconds (§2: no time.Time).
type windowSettingsWire struct {
	RequireAllResponses bool    `json:"require_all_responses"`
	ResponseTimeout     *string `json:"response_timeout"`
}

// createGameWire is the POST /games request body (§11.2).
type createGameWire struct {
	Players []createPlayerWire  `json:"players"`
	Window  *windowSettingsWire `json:"window,omitempty"`
}

// handleCreateGame implements POST /games (§6.1/§11.2): no auth. Per-seat
// name+type parsing and window settings with A8 omitted⇒defaults happen here;
// the 2–6 seat count, ≥1 human, token direction (tokens only for humans) and
// the LLM-driver availability gate are enforced by the application layer. The
// 201 body is the ONLY place tokens appear (A2).
func (s *Server) handleCreateGame(rw http.ResponseWriter, r *http.Request) {
	var wire createGameWire
	if err := decodeJSON(r, &wire); err != nil {
		s.writeError(rw, r, app.NewAppError(app.ErrInvalidInput, "malformed JSON body", 400))
		return
	}

	req := app.CreateGameRequest{Players: make([]app.PlayerSpec, 0, len(wire.Players))}
	for _, p := range wire.Players {
		req.Players = append(req.Players, app.PlayerSpec{Name: p.Name, Type: p.Type})
	}
	if w := wire.Window; w != nil {
		req.RequireAll = w.RequireAllResponses
		if w.ResponseTimeout != nil && *w.ResponseTimeout != "" {
			d, err := time.ParseDuration(*w.ResponseTimeout)
			if err != nil {
				s.writeError(rw, r, app.NewAppError(app.ErrInvalidInput, `response_timeout must be a parseable duration (e.g. "45s")`, 400))
				return
			}
			req.ResponseTimeoutMs = int(d / time.Millisecond)
		}
	}

	res, err := s.App.CreateGame(r.Context(), req)
	if err != nil {
		s.writeError(rw, r, err)
		return
	}
	s.writeJSON(rw, r, http.StatusCreated, res)
}

// handleSnapshot implements GET /games/{id} (§11.2): per-viewer snapshot behind
// bearer auth (token → seat). Privacy is enforced by the application layer: the
// response carries only public info plus the viewer's own face-down cards and
// decision options. There is no unauthenticated view — the snapshot shape
// requires a seat (§11.2), so missing/invalid tokens answer 401 (§11.1).
func (s *Server) handleSnapshot(rw http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	seatID, ok := s.authorize(rw, r, id)
	if !ok {
		return
	}
	snap, err := s.App.GetSnapshot(r.Context(), id, seatID)
	if err != nil {
		s.writeError(rw, r, err)
		return
	}
	s.writeJSON(rw, r, http.StatusOK, snap)
}

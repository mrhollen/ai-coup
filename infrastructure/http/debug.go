package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	app "forgejo.lan/mrhollen/ai-coup/application"
)

// debugConfig is the build-config block of the §11 debug dump: which driver
// mode backs LLM seats, the configured poll hold (already clamped, A1), and the
// game's window settings (§1: these are the server-side knobs a running game
// was built with).
type debugConfig struct {
	DriverMode     string                `json:"driver_mode"`
	PollHoldMs     int                   `json:"poll_hold_ms"`
	WindowSettings app.WindowSettingsOut `json:"window_settings"`
}

// debugEvents is the FULL unfiltered event history split by audience (A9/A10):
// every public event plus every seat's private ones — no privacy filtering here
// at all (that is what distinguishes /debug from /events).
type debugEvents struct {
	Public  []app.EventOut `json:"public"`
	Private []app.EventOut `json:"private"`
}

// debugGameResponse assembles the §11.2 dump: Application.DebugGame state
// (promoted fields: game_id/status/winner_id/turn_id/seats/court_deck/… — ALL
// hands included, incl. LLM seats), plus created_at (the ts of the first
// event), config, and the unfiltered event history.
type debugGameResponse struct {
	CreatedAt string      `json:"created_at"`
	Config    debugConfig `json:"config"`
	*app.DebugState
	Events *debugEvents `json:"events"`
}

// handleDebugGame implements GET /debug/games/{id} (§11.2/A10): served ONLY
// when COUP_DEBUG=1 (the route is mounted conditionally — with the gate off this
// path 404s like any unknown route, not 403) and unauthenticated (the env gate
// is the security boundary). Data sources: Application.DebugGame for the locked
// state view and MemoryEventStore.All for the complete event history.
func (s *Server) handleDebugGame(rw http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.Debug { // defensive; the route only exists when the gate is on
		s.writeError(rw, r, app.NewAppError(app.ErrNotFound, "no such route", 404))
		return
	}

	st, err := s.App.DebugGame(id)
	if err != nil {
		s.writeError(rw, r, err) // unknown game → 404 not_found
		return
	}

	all := s.Events.All(id)
	pub := make([]app.EventOut, 0, len(all))
	priv := []app.EventOut{}
	created := ""
	for i, e := range all {
		if i == 0 {
			created = e.TS // game_created is always the first event of a game
		}
		if e.Audience.Public {
			pub = append(pub, e)
		} else {
			priv = append(priv, e)
		}
	}

	s.writeJSON(rw, r, http.StatusOK, &debugGameResponse{
		CreatedAt:  created,
		Config:     debugConfig{DriverMode: s.DriverMode, PollHoldMs: int(s.PollHold.Milliseconds()), WindowSettings: st.WindowSettings},
		DebugState: st,
		Events:     &debugEvents{Public: pub, Private: priv},
	})
}

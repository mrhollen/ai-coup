package http

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	app "forgejo.lan/mrhollen/ai-coup/application"
)

// handleEvents implements GET /games/{id}/events?after=N (§11.1): the long-poll
// stream, audience-filtered to public events plus the token's own seat. Semantics:
//   - after=0 or omitted ⇒ from the beginning; polls are idempotent by after.
//   - if new events exist they return immediately; otherwise the hold (clamped
//     per A1) is waited and expiry answers 200 with an EMPTY events array —
//     never a timeout error code (A1).
//   - after game_over, polls return empty arrays forever.
//
// The client's request context IS the wait context: a disconnect releases the
// hold early (§9).
func (s *Server) handleEvents(rw http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	seatID, ok := s.authorize(rw, r, id)
	if !ok {
		return
	}

	after := int64(0)
	if v := strings.TrimSpace(r.URL.Query().Get("after")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.writeError(rw, r, app.NewAppError(app.ErrInvalidInput, "after must be a non-negative integer event sequence", 400))
			return
		}
		after = n
	}

	res, err := s.App.PollEvents(r.Context(), id, after, seatID, s.PollHold)
	if err != nil {
		s.writeError(rw, r, err)
		return
	}
	s.writeJSON(rw, r, http.StatusOK, res)
}

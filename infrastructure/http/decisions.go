package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	app "forgejo.lan/mrhollen/ai-coup/application"
)

// decisionAccepted is the 202 body of POST /games/{id}/decisions (§11.2). A 202
// means *accepted*, not final outcome — outcomes arrive as events on the poll
// stream. request_id echoes the A17 id of this request.
type decisionAccepted struct {
	Accepted  bool   `json:"accepted"`
	EventSeq  int64  `json:"event_seq"` // first seq of the accepted batch (0 when none were appended)
	RequestID string `json:"request_id"`
}

// handleDecision implements POST /games/{id}/decisions (§11.2): the single
// decisions endpoint for turn actions (kind=act), window responses
// (kind=respond) and single-seat choices (kind=choose). The body decodes
// straight into application.DecisionIn; payload-shape validation errors surface
// as 400 invalid_payload, semantic rejections keep their §11.1 wire codes
// (not_your_turn, already_resolved 409 with table-voice messages verbatim, …).
// Rejections also land as private rejection events on the offender's stream —
// done by the application layer (A9), not here.
func (s *Server) handleDecision(rw http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	seatID, ok := s.authorize(rw, r, id)
	if !ok {
		return
	}

	var in app.DecisionIn
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(rw, r, app.NewAppError(app.ErrInvalidInput, "malformed JSON body", 400))
		return
	}

	res, err := s.App.SubmitDecision(r.Context(), id, seatID, in)
	if err != nil {
		s.writeError(rw, r, err)
		return
	}
	s.writeJSON(rw, r, http.StatusAccepted, decisionAccepted{
		Accepted:  true,
		EventSeq:  res.EventSeq,
		RequestID: requestIDFrom(r.Context()),
	})
}

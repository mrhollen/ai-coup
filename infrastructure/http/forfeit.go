package http

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	app "forgejo.lan/mrhollen/ai-coup/application"
)

// forfeitVoteTargetWire is the POST /games/{id}/forfeit-votes body (§11.2).
type forfeitVoteTargetWire struct {
	TargetSeatID string `json:"target_seat_id"`
}

// forfeitBallotWire is the POST …/ballots body (§7): "yes" or "no".
type forfeitBallotWire struct {
	Vote string `json:"vote"`
}

// handleInitiateForfeitVote implements POST /games/{id}/forfeit-votes (§7/§11.2):
// any active HUMAN seat may initiate against another ACTIVE seat. Errors flow
// from the application layer with their §11.1 codes: already_resolved (a vote
// is already active, A15), target_not_active, self_target ("you cannot target
// your own seat"), not_found (no such seat). Rejections are recorded on the
// initiator's private stream by the application layer (A9).
func (s *Server) handleInitiateForfeitVote(rw http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	initiator, ok := s.authorize(rw, r, id)
	if !ok {
		return
	}

	var wire forfeitVoteTargetWire
	if err := decodeJSON(r, &wire); err != nil {
		s.writeError(rw, r, app.NewAppError(app.ErrInvalidInput, "malformed JSON body", 400))
		return
	}
	wire.TargetSeatID = strings.TrimSpace(wire.TargetSeatID)
	if wire.TargetSeatID == "" {
		s.writeError(rw, r, app.NewAppError(app.ErrInvalidInput, "target_seat_id is required", 400))
		return
	}

	res, err := s.App.InitiateForfeitVote(r.Context(), id, initiator, wire.TargetSeatID)
	if err != nil {
		s.writeError(rw, r, err)
		return
	}
	s.writeJSON(rw, r, http.StatusCreated, res)
}

// handleCastForfeitVote implements POST /games/{id}/forfeit-votes/{vote_id}/ballots
// (§7/§11.2). Only the other active HUMAN seats may vote, once each; the ballot
// closes early on majority and the full cascade (elimination, structure settle)
// flows as events from the application layer. §11.2 ballot errors:
//   - vote_closed    409 — the named ballot is already resolved (or none is open
//     and the id is one of this game's closed ballots);
//   - vote_not_found 404 — the path names a ballot this game never had;
//   - not_a_voter    409 — the bearer seat is not an eligible voter.
func (s *Server) handleCastForfeitVote(rw http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	voteID := chi.URLParam(r, "vote_id")
	voter, ok := s.authorize(rw, r, id)
	if !ok {
		return
	}

	var wire forfeitBallotWire
	if err := decodeJSON(r, &wire); err != nil {
		s.writeError(rw, r, app.NewAppError(app.ErrInvalidInput, "malformed JSON body", 400))
		return
	}
	switch strings.ToLower(strings.TrimSpace(wire.Vote)) {
	case "yes":
	case "no":
	default:
		s.writeError(rw, r, app.NewAppError(app.ErrInvalidInput, `vote must be "yes" or "no"`, 400))
		return
	}

	// The engine keeps at most one active ballot per game (A15) and the use case
	// is addressed to the ACTIVE ballot, so the id in the URL must be validated
	// against it before delegating: a path naming a different (closed or made-up)
	// ballot must not cast on the live one. Unknown games fall through untouched —
	// the application layer answers its canonical 404 "no such game".
	st, gerr := s.App.DebugGame(id)
	if gerr == nil {
		var openID string
		if st.ActiveForfeitVote != nil {
			openID = st.ActiveForfeitVote.VoteID
		}
		switch {
		case openID != "" && openID != voteID:
			s.answerClosedOrUnknownBallot(rw, r, id, voteID)
			return
		case openID == "" && !s.ballotIDInHistory(r.Context(), id, voteID):
			s.writeError(rw, r, app.NewAppError("vote_not_found", "no such forfeit vote in this game", 404))
			return
		}
	}

	res, err := s.App.CastForfeitVote(r.Context(), id, voter, wire.Vote == "yes")
	if err != nil {
		s.writeError(rw, r, err)
		return
	}
	s.writeJSON(rw, r, http.StatusOK, res)
}

// answerClosedOrUnknownBallot distinguishes §11.2's two closed-ballout answers
// for a ballot id that is not the open one: 409 vote_closed when the id belongs
// to this game's history (a resolved ballot), 404 vote_not_found otherwise.
func (s *Server) answerClosedOrUnknownBallot(rw http.ResponseWriter, r *http.Request, gameID, voteID string) {
	if s.ballotIDInHistory(r.Context(), gameID, voteID) {
		s.writeError(rw, r, app.NewAppError("vote_closed", "the forfeit vote has already been resolved", 409))
		return
	}
	s.writeError(rw, r, app.NewAppError("vote_not_found", "no such forfeit vote in this game", 404))
}

// ballotIDInHistory reports whether any of this game's PUBLIC forfeit_vote_*
// events (opened/cast/resolved — all public, §10.1) carries the given vote id.
func (s *Server) ballotIDInHistory(ctx context.Context, gameID, voteID string) bool {
	if voteID == "" {
		return false
	}
	evts, err := s.Events.Range(ctx, gameID, 0, "") // public-only stream
	if err != nil {
		return false
	}
	for _, e := range evts {
		switch e.Type {
		case "forfeit_vote_opened", "forfeit_vote_cast", "forfeit_vote_resolved":
			if e.Payload["vote_id"] == voteID {
				return true
			}
		}
	}
	return false
}

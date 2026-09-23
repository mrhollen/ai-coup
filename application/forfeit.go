package application

import (
	"context"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// InitiateForfeitVoteResult is the 201 body of POST /games/{id}/forfeit-votes.
type InitiateForfeitVoteResult struct {
	VoteID         string `json:"vote_id"`
	MajorityNeeded int    `json:"majority_needed"`
}

// CastForfeitVoteResult is the 200 body of POST .../ballots: closed reports
// whether this ballot resolved the vote (majority reached or ballot
// exhausted).
type CastForfeitVoteResult struct {
	VoteID string `json:"vote_id"`
	Closed bool   `json:"closed"`
}

// InitiateForfeitVote implements §7 initiation: any active HUMAN seat may
// target any other ACTIVE seat (human or LLM — e.g. a wedged model server).
// One active vote per game (A15); self-targets rejected with "you cannot
// target your own seat"; eliminated targets with "you cannot target an
// eliminated player". Rejections are also recorded on the initiator's stream
// (A9).
func (a *Application) InitiateForfeitVote(ctx context.Context, gameID, initiatorSeatID, targetSeatID string) (*InitiateForfeitVoteResult, error) {
	h, err := a.game(gameID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	viewer, verr := h.humanSeat(initiatorSeatID)
	h.mu.Unlock()
	if verr != nil {
		return nil, verr
	}

	_, engineErr := h.run(func(g *domain.Game) ([]domain.Event, error) {
		return g.InitiateForfeitVote(viewer.ID, targetSeatID)
	})
	if engineErr != nil {
		if ae, ok := AsAppError(engineErr); ok {
			h.recordRejection(viewer.ID, ae)
		}
		return nil, engineErr
	}

	// The ballot is public information; read it from the aggregate (it exists
	// on success by construction).
	h.mu.Lock()
	vv := h.g.VoteView()
	h.mu.Unlock()
	if vv == nil { // unreachable unless the vote vanished between calls; defensive
		return nil, NewAppError(ErrServer, "vote disappeared after initiation", 500)
	}
	return &InitiateForfeitVoteResult{VoteID: vv.ID, MajorityNeeded: vv.MajorityNeeded}, nil
}

// CastForfeitVote implements §7 ballots: yes/no from the other active HUMAN
// seats only (LLM seats do not vote in v1), one ballot per voter. The batch
// returned by the engine already carries the full cascade — early majority
// close (forfeited ⇒ target eliminated, concurrent structures settled per
// §7), tie close (not_forfeited), and voids when the target dies by play
// while pending (A15).
func (a *Application) CastForfeitVote(ctx context.Context, gameID, voterSeatID string, yes bool) (*CastForfeitVoteResult, error) {
	h, err := a.game(gameID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	voter, verr := h.humanSeat(voterSeatID)
	h.mu.Unlock()
	if verr != nil {
		return nil, verr
	}

	var voteID string
	closed := false
	_, engineErr := h.run(func(g *domain.Game) ([]domain.Event, error) {
		evs, err := g.CastForfeitVote(voter.ID, yes)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			switch e.Type {
			case "forfeit_vote_cast":
				voteID = payloadString(e.Payload, "vote_id")
			case "forfeit_vote_resolved":
				voteID = payloadString(e.Payload, "vote_id")
				closed = true
			}
		}
		return evs, nil
	})
	if engineErr != nil {
		if ae, ok := AsAppError(engineErr); ok {
			h.recordRejection(voter.ID, ae)
		}
		return nil, engineErr
	}
	return &CastForfeitVoteResult{VoteID: voteID, Closed: closed}, nil
}

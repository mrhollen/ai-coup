package application_test

import (
	"testing"

	app "forgejo.lan/mrhollen/ai-coup/application"
	llm "forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestDriver_NeverDrivesHumanSingleSeatDecisions pins the §6.3 boundary at the
// single-seat decision seam (the turn and window seams already checked seat
// type): a flip choice / exchange finish addressed to a HUMAN seat must NOT
// be dispatched to the driver — the human client answers it, and driving it
// would race the client's own submission ("that decision was already made")
// and silently rob the player of the choice. Regression test for the A13
// stress suite catching exactly this in a 6-seat game.
func TestDriver_NeverDrivesHumanSingleSeatDecisions(t *testing.T) {
	// Find a seed where HUMAN seat 0 takes the first turn.
	h, res := createGameAt(t, []playerSpec{{"Ada", "human"}, {"Vito", "llm"}}, 0, true, 0, nil, nil)

	// Vito owes the exchange's challenge window once it opens: queue his pass.
	h.driver.Queue(res.Seats[1].ID, llm.CandidateStep(app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))

	// Ada declares an exchange (ambassador): the claim window opens for Vito,
	// and once it settles the EXCHANGE FINISH decision is addressed to ADA —
	// a human. The driver must not touch it.
	h.mustAct(t, res, res.Seats[0].ID, app.ActionIn{Type: "exchange", Character: "ambassador"})

	var decisionID string
	h.waitFor(t, "Ada's exchange-finish decision open (human-addressed)", func() bool {
		snap := h.snapshot(t, res.GameID, res.Seats[0].ID)
		if snap.Current != nil && snap.Current.Phase == "single_seat_decision" && snap.Current.Decision != nil {
			if snap.Current.Decision.Kind != "exchange_finish" || snap.Current.Decision.SeatID != res.Seats[0].ID {
				return false
			}
			decisionID = snap.Current.Decision.ID
			return true
		}
		return false
	})

	for _, in := range h.driver.Calls() {
		if in.Kind == app.KindSingleSeatChoice && in.SeatID == res.Seats[0].ID {
			t.Fatalf("the driver was dispatched for a human seat's decision %s: %+v — §6.3 forbids driving human seats", decisionID, in)
		}
	}

	// The human still answers her own decision through the public path.
	h.choose(t, res, res.Seats[0].ID, decisionID, app.ChoiceIn{ExchangeFinish: &app.ExchangeFinishIn{NoSwap: true}})
}

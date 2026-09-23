package application

import (
	"context"
	"time"
)

// PollResult is the 200 body of GET /games/{id}/events?after=N (spec §11.1).
// Events are audience-filtered to public + the viewer's own seat, seq
// ascending. When no new events arrive within the hold, Events is an empty
// slice and LastSeq reflects the store's position — never a timeout error
// (A1).
type PollResult struct {
	GameID  string     `json:"game_id"`
	Events  []EventOut `json:"events"`
	LastSeq int64      `json:"last_seq"`
}

// PollEvents implements the long-poll use case (spec §11.1): if events with
// seq > after exist they are returned immediately; otherwise the call blocks
// up to the configured hold — clamped to [1s,30s] per A1 via ClampPollHold —
// waiting on EventStore.WaitNew, and returns whatever exists at expiry (which
// may be empty). Idempotent by after; after=0 means from the beginning. Only
// human seats have a stream: LLM seats are driven internally and receive no
// polls (§6.1). The ctx is the caller's (request) context, so a client
// disconnect releases the hold early.
func (a *Application) PollEvents(ctx context.Context, gameID string, after int64, viewerSeatID string, hold time.Duration) (*PollResult, error) {
	h, err := a.game(gameID)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	viewer, verr := h.humanSeat(viewerSeatID)
	h.mu.Unlock()
	if verr != nil {
		return nil, verr
	}
	hold = ClampPollHold(hold)
	if after < 0 {
		after = 0
	}

	evts, err := a.deps.Events.Range(ctx, gameID, after, viewer.ID)
	if err != nil {
		return nil, FromDomain(err)
	}
	if len(evts) > 0 {
		return a.pollResult(gameID, evts)
	}

	// Bounded wait: the hold timer is armed on the Clock; whichever fires
	// first (new event or deadline) cancels the store wait and we re-read.
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-a.deps.Clock.After(hold):
			cancel()
		case <-waitCtx.Done():
		}
	}()
	a.deps.Events.WaitNew(waitCtx, gameID, after)

	evts, err = a.deps.Events.Range(context.Background(), gameID, after, viewer.ID)
	if err != nil {
		return nil, FromDomain(err)
	}
	return a.pollResult(gameID, evts)
}

func (a *Application) pollResult(gameID string, evts []EventOut) (*PollResult, error) {
	lastSeq, err := a.deps.Events.LastSeq(context.Background(), gameID)
	if err != nil {
		return nil, FromDomain(err)
	}
	if evts == nil {
		evts = []EventOut{} // JSON: "events":[] never null (A1)
	}
	return &PollResult{GameID: gameID, Events: evts, LastSeq: lastSeq}, nil
}

// WaitForEvent blocks until some event with seq > after is appended for the
// game, or ctx is done — whichever comes first. It is the unbounded wait
// primitive behind PollEvents, exposed for SCRIPTED/UNATTENDED clients (the
// A13 stress-suite auto client of spec §14.3): unlike PollEvents there is no
// artificial hold, so a scripted player reacts exactly when the table changes
// instead of on a 1-30s polling cadence. The check-then-register inside the
// store is atomic, so a wake-up can never be missed. Unknown games are an
// immediate 404 AppError; a cancelled ctx simply returns.
func (a *Application) WaitForEvent(ctx context.Context, gameID string, after int64) error {
	if _, err := a.game(gameID); err != nil {
		return err
	}
	a.deps.Events.WaitNew(ctx, gameID, after)
	return nil
}

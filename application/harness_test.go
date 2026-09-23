// Package application_test is the §14.2 application-layer suite: pure
// in-process composition tests with the scripted fake driver and a manual
// clock — no network, no listeners, no fixed ports.
package application_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/store"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/time"
)

func slogJSON(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// seatIDn is the deterministic seat id a seqIDs generator mints for the n-th
// seat (1-based) in this process/harness — used to pre-queue driver steps
// before CreateGame exists.
func seatIDn(n int) string { return fmt.Sprintf("s_%012d", n) }

// dealtNoCharacter is a createGameAt predicate: true when the 0-indexed seat
// was NOT dealt the named character — how tests make a busted-claim scenario
// deterministic (the bust then cannot depend on the random deal).
func dealtNoCharacter(n int, ch domain.Character) func(*harness, *app.CreateGameResult) bool {
	return func(h *harness, res *app.CreateGameResult) bool {
		g, ok := h.games.Get(res.GameID)
		if !ok {
			return false
		}
		for _, c := range g.Seats[n].FaceDown {
			if c.Character == ch {
				return false
			}
		}
		return true
	}
}

type playerSpec struct {
	name, typ string // typ: "human" | "llm"
}

// seqIDs is a deterministic IDGen for tests: per-kind counters, so in a fresh
// harness the first game is always g_…01 with seats s_…01..s_n — which makes
// seatIDn(n) addressable before CreateGame runs.
type seqIDs struct{ n map[string]*atomic.Int32 }

func (g *seqIDs) New(kind string) string {
	v, ok := g.n[kind]
	if !ok {
		v = &atomic.Int32{}
		g.n[kind] = v
	}
	return fmt.Sprintf("%s_%012d", kind, v.Add(1))
}

func newSeqIDs() *seqIDs { return &seqIDs{n: map[string]*atomic.Int32{}} }

// safeBuf is a mutex-guarded bytes.Buffer: the application logs asynchronously
// through slog while tests read the accumulated lines, and bytes.Buffer alone
// is not safe for that.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t      *testing.T
	app    *app.Application
	cl     *clock.ManualClock
	driver *llm.FakeDriver
	events *store.MemoryEventStore
	games  *store.MemoryGameStore
	logBuf *safeBuf
}

func newHarness(t *testing.T, seed int64) *harness {
	t.Helper()
	h := &harness{t: t}
	h.cl = clock.NewManualClock(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	h.driver = llm.NewFakeDriver()
	h.events = store.NewMemoryEventStore(h.cl)
	h.games = store.NewMemoryGameStore()
	h.logBuf = &safeBuf{}
	logger := slogJSON(h.logBuf)
	h.app = app.NewApplication(app.Dependencies{
		Events: h.events,
		Games:  h.games,
		Clock:  h.cl,
		IDs:    newSeqIDs(),
		Driver: h.driver,
		Log:    logger,
		Rng:    mathrand.New(mathrand.NewSource(seed)),
	}) // default TokenSource: real crypto tokens (format asserted per A2)
	t.Cleanup(func() {
		if err := h.app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return h
}

// createGameAt builds a game whose random start seat is players[wantStart]
// (rules.md Ruling 6, made deterministic by seed search) and whose post-
// creation state satisfies pred (nil = any). pre is run on the fresh harness
// BEFORE CreateGame — the place to queue driver steps for seats that owe a
// decision at birth. It returns the winning harness + result.
func createGameAt(t *testing.T, players []playerSpec, wantStart int, reqAll bool, timeoutMs int, pre func(*harness), pred func(*harness, *app.CreateGameResult) bool) (*harness, *app.CreateGameResult) {
	t.Helper()
	for seed := int64(1); seed < 20000; seed++ {
		h := newHarness(t, seed)
		if pre != nil {
			pre(h)
		}
		res := h.createGame(t, players, reqAll, timeoutMs)
		if res.FirstTurnSeatID != res.Seats[wantStart].ID {
			continue
		}
		if pred != nil && !pred(h, res) {
			continue
		}
		return h, res
	}
	t.Fatalf("no seed produced a game with start seat %d for %+v", wantStart, players)
	return nil, nil
}

func (h *harness) createGame(t *testing.T, players []playerSpec, reqAll bool, timeoutMs int) *app.CreateGameResult {
	t.Helper()
	req := app.CreateGameRequest{RequireAll: reqAll, ResponseTimeoutMs: timeoutMs}
	for _, p := range players {
		req.Players = append(req.Players, app.PlayerSpec{Name: p.name, Type: p.typ})
	}
	res, err := h.app.CreateGame(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	return res
}

// snapshot fetches the per-viewer snapshot (viewer must be a human seat).
func (h *harness) snapshot(t *testing.T, gameID, viewer string) *app.Snapshot {
	t.Helper()
	s, err := h.app.GetSnapshot(context.Background(), gameID, viewer)
	if err != nil {
		t.Fatalf("GetSnapshot(%s): %v", viewer, err)
	}
	return s
}

// humanViewer returns the first human seat id of a create result.
func humanViewer(res *app.CreateGameResult) string {
	for _, s := range res.Seats {
		if s.Type == "human" {
			return s.ID
		}
	}
	panic("no human seat in game")
}

// coinsOf reads a seat's public coin count via the snapshot surface.
func (h *harness) coinsOf(t *testing.T, res *app.CreateGameResult, seatID string) int {
	t.Helper()
	s := h.snapshot(t, res.GameID, humanViewer(res))
	for _, se := range s.Seats {
		if se.ID == seatID {
			return se.Coins
		}
	}
	t.Fatalf("seat %s not in snapshot", seatID)
	return 0
}

// stream returns the viewer's full event stream (public + own private).
func (h *harness) stream(t *testing.T, gameID, viewer string) []app.EventOut {
	t.Helper()
	evts, err := h.events.Range(context.Background(), gameID, 0, viewer)
	if err != nil {
		t.Fatalf("Range(%s): %v", viewer, err)
	}
	return evts
}

func findEvents(evts []app.EventOut, typ string) []app.EventOut {
	var out []app.EventOut
	for _, e := range evts {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// waitFor polls cond on the WALL clock (goroutine convergence only — all game
// time itself is driven by the manual clock).
func (h *harness) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// lastSeq of the game in the store.
func (h *harness) lastSeq(t *testing.T, gameID string) int64 {
	t.Helper()
	n, err := h.events.LastSeq(context.Background(), gameID)
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	return n
}

// submit helpers -----------------------------------------------------------

func (h *harness) act(t *testing.T, res *app.CreateGameResult, seat string, action app.ActionIn) (*app.SubmitDecisionResult, error) {
	t.Helper()
	return h.app.SubmitDecision(context.Background(), res.GameID, seat, app.DecisionIn{
		Kind:   app.DecisionKindAct,
		Action: &action,
	})
}

func (h *harness) mustAct(t *testing.T, res *app.CreateGameResult, seat string, action app.ActionIn) *app.SubmitDecisionResult {
	t.Helper()
	r, err := h.act(t, res, seat, action)
	if err != nil {
		t.Fatalf("act %s as %s: %v", action.Type, seat, err)
	}
	return r
}

func (h *harness) respond(t *testing.T, res *app.CreateGameResult, seat string, resp app.ResponseIn, windowID string) (*app.SubmitDecisionResult, error) {
	t.Helper()
	return h.app.SubmitDecision(context.Background(), res.GameID, seat, app.DecisionIn{
		Kind:     app.DecisionKindRespond,
		Response: &resp,
		WindowID: windowID,
	})
}

func (h *harness) choose(t *testing.T, res *app.CreateGameResult, seat, decisionID string, choice app.ChoiceIn) (*app.SubmitDecisionResult, error) {
	t.Helper()
	return h.app.SubmitDecision(context.Background(), res.GameID, seat, app.DecisionIn{
		Kind:       app.DecisionKindChoose,
		DecisionID: decisionID,
		Choice:     &choice,
	})
}

func (h *harness) initiateVote(t *testing.T, gameID, initiator, target string) (*app.InitiateForfeitVoteResult, error) {
	t.Helper()
	return h.app.InitiateForfeitVote(context.Background(), gameID, initiator, target)
}

// initiateVoteErr re-runs an initiation returning just the error (for message
// assertions after a code assertion).
func (h *harness) initiateVoteErr(t *testing.T, gameID, initiator, target string) error {
	t.Helper()
	_, err := h.app.InitiateForfeitVote(context.Background(), gameID, initiator, target)
	return err
}

func (h *harness) castVote(t *testing.T, gameID, voter string, yes bool) (*app.CastForfeitVoteResult, error) {
	t.Helper()
	return h.app.CastForfeitVote(context.Background(), gameID, voter, yes)
}

// pumpForeignAid plays foreign-aid turns by the human actor until the actor's
// coin count reaches targetCoins, cycling the table back to the actor each
// round. Seats start with 2 coins and Coup needs 7, so this is how tests get a
// banked player deterministically: LLM seats' window passes + turn incomes are
// queued up front, the manual clock advances past each window timeout (the
// game must have a response timeout), other HUMAN seats are driven by `drive`
// when their turn arrives. It returns once it is the actor's turn again.
func (h *harness) pumpForeignAid(t *testing.T, res *app.CreateGameResult, actor string, targetCoins int, hold time.Duration, drive func(h *harness, seatID string)) {
	t.Helper()
	// Each FA round nets the actor exactly +2 (responders only pass), so the
	// number of rounds is known up front — and therefore each LLM seat's exact
	// consumption: one window pass + one turn income per round. Queue precisely
	// that so later test scripts land at predictable queue positions.
	rounds := (targetCoins - h.coinsOf(t, res, actor) + 1) / 2
	if rounds < 0 {
		rounds = 0
	}
	for i := 0; i < rounds; i++ {
		for _, s := range res.Seats {
			if s.Type == "llm" && s.ID != actor {
				h.driver.Queue(s.ID, llm.CandidateStep(app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
				h.driver.Queue(s.ID, llm.CandidateStep(app.Candidate{Kind: app.KindTurnAction, Action: "income"}))
			}
		}
	}
	llmResponders := 0
	for _, s := range res.Seats {
		if s.Type == "llm" && s.ID != actor {
			llmResponders++
		}
	}
	for i := 0; i < rounds; i++ {
		h.mustAct(t, res, actor, app.ActionIn{Type: "foreign_aid"})
		// Let LLM responders land their passes BEFORE expiring the window, so
		// timeout auto-pass bookkeeping and driver applications never race.
		h.waitFor(t, "LLM window responses landed", func() bool {
			s := h.snapshot(t, res.GameID, humanViewer(res))
			if s.Current.Phase != "window_open" || s.Current.Window == nil {
				return true
			}
			return len(s.Current.Window.ResponsesSoFar) >= llmResponders
		})
		h.cl.Advance(hold) // expire the FA window ⇒ +2 coins on the actor
		// Walk the table back to the actor: LLM seats auto-act from their
		// queues; other human seats are driven by the test.
	walk:
		for {
			s := h.snapshot(t, res.GameID, humanViewer(res))
			if s.Current.Phase == "turn_action" && s.Current.TurnSeatID == actor {
				break walk
			}
			if s.Current.Phase == "turn_action" {
				for _, seat := range s.Seats {
					if seat.ID != s.Current.TurnSeatID {
						continue
					}
					if seat.Type == "human" {
						if drive == nil {
							t.Fatalf("pump reached human turn %s with no drive callback", seat.ID)
						}
						drive(h, seat.ID)
					}
					break
				}
			}
			time.Sleep(2 * time.Millisecond) // let in-flight LLM decisions apply
		}
	}
	if got := h.coinsOf(t, res, actor); got < targetCoins {
		t.Fatalf("pumpForeignAid: actor at %d coins, want ≥ %d", got, targetCoins)
	}
}

// forceFinished marks a game finished directly in the store. ONLY for
// quiescent games with no in-flight driver decisions (e.g. all-human games).
func (h *harness) forceFinished(t *testing.T, res *app.CreateGameResult, winner string) {
	t.Helper()
	g, ok := h.games.Get(res.GameID)
	if !ok {
		t.Fatalf("game %s not in store", res.GameID)
	}
	g.Status = domain.Finished
	g.WinnerID = winner
}

// errCode extracts the wire code from a use-case error.
func errCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	ae, ok := app.AsAppError(err)
	if !ok {
		t.Fatalf("expected *app.AppError, got %T: %v", err, err)
	}
	return ae.Code
}

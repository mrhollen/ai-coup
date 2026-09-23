package application_test

import (
	"context"
	mathrand "math/rand"
	"strings"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/store"
	clock "forgejo.lan/mrhollen/ai-coup/infrastructure/time"
)

// newHarnessWithDriver is newHarness with a chosen MoveDriver: the standard
// harness wires the fake, but the unavailable-stub tests need their own.
func newHarnessWithDriver(t *testing.T, seed int64, driver app.MoveDriver) *harness {
	t.Helper()
	h := &harness{t: t}
	h.cl = clock.NewManualClock(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	h.driver = llm.NewFakeDriver() // field parity with newHarness; tests use their own driver
	h.events = store.NewMemoryEventStore(h.cl)
	h.games = store.NewMemoryGameStore()
	h.logBuf = &safeBuf{}
	logger := slogJSON(h.logBuf)
	h.app = app.NewApplication(app.Dependencies{
		Events: h.events,
		Games:  h.games,
		Clock:  h.cl,
		IDs:    newSeqIDs(),
		Driver: driver,
		Log:    logger,
		Rng:    mathrand.New(mathrand.NewSource(seed)),
	}) // default TokenSource: real crypto tokens (A2)
	t.Cleanup(func() {
		if err := h.app.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return h
}

// TestCreateGame_UnavailableLLMDriver_FailsFastOnLLMSeats (spec §6.2, this
// slice's contract): a driver that does not claim LLMCapable must make any
// create request containing an llm seat fail at creation time with a clear 400
// invalid_payload — the game never exists — while the same request minus the
// llm seat succeeds and issues tokens for the human seats.
func TestCreateGame_UnavailableLLMDriver_FailsFastOnLLMSeats(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithDriver(t, 1, llm.NewUnavailableDriver("heuristic"))

	withLLM := app.CreateGameRequest{Players: []app.PlayerSpec{
		{Name: "Marta", Type: "human"},
		{Name: "", Type: "llm"},
	}}
	res, err := h.app.CreateGame(ctx, withLLM)
	if err == nil {
		t.Fatalf("create with llm seat succeeded against the unavailable driver: %+v", res)
	}
	ae, ok := app.AsAppError(err)
	if !ok || ae.Status != 400 || ae.Code != "invalid_payload" {
		t.Fatalf("unexpected error for llm-seat create: %v (want 400 invalid_payload)", err)
	}
	if !strings.Contains(ae.Message, "llm") {
		t.Fatalf("message is not clear about the LLM unavailability: %q", ae.Message)
	}

	humanOnly := app.CreateGameRequest{Players: []app.PlayerSpec{
		{Name: "Marta", Type: "human"},
		{Name: "Sonny", Type: "human"},
	}}
	res, err = h.app.CreateGame(ctx, humanOnly)
	if err != nil {
		t.Fatalf("human-only create failed against the unavailable driver: %v", err)
	}
	for i, s := range res.Seats {
		if len(s.Token) != 64 {
			t.Errorf("seat %d token is not 64 hex: %q", i, s.Token)
		}
	}
	if !strings.HasPrefix(res.GameID, "g_") || res.FirstTurnSeatID == "" {
		t.Fatalf("malformed create result: %+v", res)
	}
}

// TestCreateGame_FakeDriver_StillDrivesLLMSeats guards the capability marker in
// the positive direction: scripted test compositions keep creating games with
// llm seats (every other §14.2 test relies on this).
func TestCreateGame_FakeDriver_StillDrivesLLMSeats(t *testing.T) {
	h := newHarness(t, 3)
	res := h.createGame(t, []playerSpec{{"Marta", "human"}, {"Vito", "llm"}}, false, 0)
	if res.Seats[0].Token == "" || res.Seats[1].Token != "" {
		t.Fatalf("token direction wrong: %+v", res.Seats)
	}
}

// TestDebugGame_FullStateView asserts the §11.2 debug state view: ALL hands
// (every seat, including LLM seats), deck composition, the in-flight driver
// decision hook, and clean 404 mapping for unknown games.
func TestDebugGame_FullStateView(t *testing.T) {
	// Unknown game ⇒ 404 AppError.
	h0 := newHarness(t, 11)
	if _, err := h0.app.DebugGame("g_000000000000"); err == nil {
		t.Fatal("DebugGame on unknown game must fail")
	} else if ae, ok := app.AsAppError(err); !ok || ae.Status != 404 || ae.Code != "not_found" {
		t.Fatalf("unexpected unknown-game error: %v (want 404 not_found)", err)
	}

	gate := make(chan struct{})
	pre := func(h *harness) {
		// Vito holds his first-turn answer until the test opens the gate, so
		// the in-flight decision hook is observable deterministically.
		h.driver.Queue(seatIDn(2), llm.GatedStep(gate, app.Candidate{Kind: app.KindTurnAction, Action: "income"}))
	}
	h, res := createGameAt(t, []playerSpec{{"Marta", "human"}, {"Vito", "llm"}}, 1, false, 0, pre, nil)
	vitoID := res.Seats[1].ID

	st, err := h.app.DebugGame(res.GameID)
	if err != nil {
		t.Fatalf("DebugGame: %v", err)
	}
	assertDebugState(t, st, "in_progress", "turn_action", 1)
	if len(st.Seats) != 2 {
		t.Fatalf("expected 2 seats, got %d", len(st.Seats))
	}
	for _, s := range st.Seats {
		if len(s.FaceDown) != 2 {
			t.Errorf("seat %s: debug view must expose BOTH face-down cards, got %v", s.ID, s.FaceDown)
		}
		if s.Driver.Mode != "fake" {
			t.Errorf("seat %s: driver mode = %q, want fake", s.ID, s.Driver.Mode)
		}
	}
	if len(st.CourtDeck) != 15-4 { // |deck| = 15 − 2×seats (spec §3.2.1)
		t.Errorf("court deck size in debug view = %d, want 11", len(st.CourtDeck))
	}
	if st.OpenWindow != nil || len(st.PendingDecisions) != 0 || st.ActiveForfeitVote != nil {
		t.Errorf("unexpected structures: %+v", st)
	}
	if fl := inFlightOf(st, vitoID); fl == nil {
		t.Fatalf("Vito's turn decision should be in flight while gated")
	} else if fl.Kind != "turn_action" || fl.TurnID != 1 {
		t.Errorf("in-flight decision wrong: %+v", fl)
	}

	// Release the gate; Vito's income lands and the turn passes to Marta.
	close(gate)
	h.waitFor(t, "Vito's income batch (seq ≥ 3)", func() bool { return h.lastSeq(t, res.GameID) >= 3 })
	st, err = h.app.DebugGame(res.GameID)
	if err != nil {
		t.Fatalf("DebugGame after income: %v", err)
	}
	var vito *app.DebugSeat
	for i := range st.Seats {
		if st.Seats[i].ID == vitoID {
			vito = &st.Seats[i]
		}
	}
	if vito == nil || vito.Coins != 3 { // 2 starting coins + income
		t.Errorf("Vito coins after income = %+v, want 3", vito)
	}
	if fl := inFlightOf(st, vitoID); fl != nil {
		t.Errorf("in-flight decision should be gone after the action settled: %+v", fl)
	}
	assertDebugState(t, st, "in_progress", "turn_action", 2)
}

func assertDebugState(t *testing.T, st *app.DebugState, status, phase string, turnID int) {
	t.Helper()
	if st.Status != status || st.Phase != phase {
		t.Errorf("state: status=%s phase=%s (want %s/%s)", st.Status, st.Phase, status, phase)
	}
	if st.WinnerID != nil {
		t.Errorf("winner set on an in-progress game: %v", *st.WinnerID)
	}
	if turnID > 0 && st.TurnID != turnID {
		t.Errorf("turn_id = %d, want %d", st.TurnID, turnID)
	}
}

// inFlightOf finds one seat's in-flight decision in the debug view.
func inFlightOf(st *app.DebugState, seatID string) *app.DebugInFlight {
	for i := range st.Seats {
		if st.Seats[i].ID == seatID {
			return st.Seats[i].Driver.InFlight
		}
	}
	return nil
}

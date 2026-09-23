package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestE2E_TwoHumanBots_FullGameToGameOver is the §14.3 "UI-contract" test: two
// scripted human protocol clients play a full 2-seat game to completion over
// real HTTP (httptest, ephemeral port), consuming only the §10/§11 wire shapes.
// Window settings are require_all=true + short timeout so windows close
// deterministically. The bot policy (see e2e_helpers_test.go) guarantees
// eliminations and therefore `finished`. Assertions across the run:
//   - per-stream seq monotonicity and no cross-seat private events;
//   - snapshots stay privacy-correct (only the viewer's own face-down cards);
//   - long polls return promptly on new events;
//   - a late decision after game_over is rejected with code game_over and lands
//     as a private rejection event on the offender's stream only (A9);
//   - final snapshot status "finished" with winner_id, and game_over arrives in
//     both public streams;
//   - the COUP_DEBUG dump's unfiltered history equals the union of both seats'
//     filtered streams, seat by seat (§8/§10 privacy re-verified out-of-band).
func TestE2E_TwoHumanBots_FullGameToGameOver(t *testing.T) {
	// Hold requested below the A1 floor on purpose: the server must clamp it to
	// 1 s (A1) — long-poll promptness below is asserted against that effective hold.
	srv := newTestServer(t, 200*time.Millisecond, true /* debug on: union check */)

	res := srv.createGame(t, []seatInfo{
		{Name: "Marta", Type: "human"},
		{Name: "Sonny", Type: "human"},
	}, true, 1500)
	if len(res.Seats) != 2 {
		t.Fatalf("expected 2 seats, got %d", len(res.Seats))
	}
	for i, s := range res.Seats {
		if len(s.Token) != 64 || !isHexString(s.Token) {
			t.Fatalf("seat %d token is not 64 hex: %q", i, s.Token)
		}
		if res.WindowSettings.RequireAllResponses != true || res.WindowSettings.ResponseTimeoutMs != 1500 {
			t.Fatalf("window settings did not round-trip: %+v", res.WindowSettings)
		}
	}

	a := newBot(t, srv, res, 0, res.Seats[1].ID)
	b := newBot(t, srv, res, 1, res.Seats[0].ID)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second) // generous wall budget (§14.3)
	defer cancel()
	start := time.Now()
	go a.run(ctx)
	go b.run(ctx)
	select {
	case <-a.finished:
	case <-ctx.Done():
		t.Fatalf("game did not finish in the wall budget (bot A never observed status finished)")
	}
	select {
	case <-b.finished:
	case <-ctx.Done():
		t.Fatalf("game did not finish in the wall budget (bot B never observed status finished)")
	}
	elapsed := time.Since(start)
	t.Logf("full 2-human game finished over HTTP in %v", elapsed)
	if elapsed > 60*time.Second {
		t.Errorf("game took %v, expected well under the 60s budget", elapsed)
	}

	ea, eb := a.eventsSnapshot(), b.eventsSnapshot()
	assertStreamBasics(t, "A", res.Seats[0].ID, ea)
	assertStreamBasics(t, "B", res.Seats[1].ID, eb)

	// Privacy: no cross-seat private events on either stream (§8/§10).
	assertNoCrossSeatEvents(t, "A's stream", ea, res.Seats[1].ID)
	assertNoCrossSeatEvents(t, "B's stream", eb, res.Seats[0].ID)

	// Flip-choice requests are private to the losing seat and appear only there.
	flipChoices := countType(append(append([]wireEvent{}, ea...), eb...), "card_flip_choice_requested")
	if flipChoices == 0 {
		t.Error("expected at least one card_flip_choice_requested across the run (attacks hit 2-card hands)")
	}
	for name, evs := range map[string][]wireEvent{"A": ea, "B": eb} {
		for _, e := range evs {
			if e.Type == "card_flip_choice_requested" && (e.Audience.SeatID != seatOf(name, res) || e.str("seat_id") != seatOf(name, res)) {
				t.Errorf("stream %s carries a flip choice addressed to another seat: %+v", name, e)
			}
		}
	}

	// Snapshot privacy (§8): only the viewer's own face-down cards ever appear,
	// and the viewer id is always the token's seat.
	for name, bts := range map[string]*bot{"A": a, "B": b} {
		bts.mu.Lock()
		snaps := append([]wireSnapshot{}, bts.snaps...)
		bts.mu.Unlock()
		if len(snaps) == 0 {
			t.Fatalf("bot %s never saw a snapshot", name)
		}
		for i, s := range snaps {
			if s.YourSeatID != seatOf(name, res) {
				t.Fatalf("bot %s snapshot %d: your_seat_id = %q, want own seat", name, i, s.YourSeatID)
			}
			if len(s.YourFaceDown) > 2 {
				t.Errorf("bot %s snapshot %d: %d face-down cards visible (max 2)", name, i, len(s.YourFaceDown))
			}
		}
		if len(snaps[0].YourFaceDown) != 2 {
			t.Errorf("bot %s initial hand not fully visible to itself: %v", name, snaps[0].YourFaceDown)
		}
	}

	// Long-poll promptness: every poll that returned events answered well under
	// the effective (clamped, 1 s) hold — i.e. immediately on new events (§9/A1).
	for name, bts := range map[string]*bot{"A": a, "B": b} {
		bts.mu.Lock()
		lats := append([]time.Duration{}, bts.prompts...)
		bts.mu.Unlock()
		if len(lats) == 0 {
			t.Fatalf("bot %s never received events on its polls", name)
		}
		for _, d := range lats {
			if d > time.Second {
				t.Errorf("bot %s: event-bearing poll took %v (≥ the 1 s clamped hold)", name, d)
			}
		}
	}

	// Terminal state: finished + winner_id in the final snapshot of both bots,
	// and game_over (public, once) in both streams.
	for name, bts := range map[string]*bot{"A": a, "B": b} {
		bts.mu.Lock()
		snap := bts.snaps[len(bts.snaps)-1]
		bts.mu.Unlock()
		if snap.Status != "finished" || snap.WinnerID == nil || *snap.WinnerID == "" {
			t.Fatalf("bot %s final snapshot: status=%q winner_id=%v (want finished + set)", name, snap.Status, snap.WinnerID)
		}
		winner := *snap.WinnerID
		if winner != res.Seats[0].ID && winner != res.Seats[1].ID {
			t.Fatalf("winner %q is not one of the seats", winner)
		}
		bts.mu.Lock()
		gos := filterType(append([]wireEvent{}, bts.events...), "game_over")
		bts.mu.Unlock()
		if len(gos) != 1 {
			t.Fatalf("stream %s: %d game_over events (want exactly 1)", name, len(gos))
		}
		if gos[0].str("winner_seat_id") != winner {
			t.Errorf("stream %s: game_over winner mismatch: event=%q snapshot=%q", name, gos[0].str("winner_seat_id"), winner)
		}
		if snap.Current == nil || snap.Current.Phase != "finished" {
			t.Errorf("bot %s final snapshot current.phase = %+v (want finished)", name, snap.Current)
		}
	}

	assertDebugUnion(t, srv, res, ea, eb)

	// Late decision after game_over: rejected with the §11.2 code and recorded
	// as a private rejection on the offender's stream only (A9).
	status, _, raw := srv.do(t, http.MethodPost, "/games/"+res.GameID+"/decisions", res.Seats[0].Token,
		map[string]any{"kind": "act", "action": map[string]string{"type": "income"}})
	if status != http.StatusConflict {
		t.Fatalf("late act after game_over → %d (want 409): %s", status, raw)
	}
	assertError(t, status, raw, "game_over")

	evAfterA := pollStream(t, srv, res.GameID, res.Seats[0].Token, a.lastSeqAfter())
	if rej := findRejection(evAfterA); rej == nil {
		t.Fatalf("offender's stream lacks the private rejection event: %+v", evAfterA)
	} else if rej.str("code") != "game_over" || !strings.Contains(rej.str("message"), "the game is over") {
		t.Errorf("rejection payload wrong: code=%q message=%q", rej.str("code"), rej.str("message"))
	}
	evAfterB := pollStream(t, srv, res.GameID, res.Seats[1].Token, b.lastSeqAfter())
	if rej := findRejection(evAfterB); rej != nil {
		t.Errorf("other seat's stream must not carry the offender's rejection: %+v", evAfterB)
	}
}

// --- per-stream assertion helpers ------------------------------------------

func assertStreamBasics(t *testing.T, name, ownSeat string, evs []wireEvent) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatalf("stream %s is empty", name)
	}
	if evs[0].Seq != 1 || evs[0].Type != "game_created" {
		t.Fatalf("stream %s does not start with game_created at seq 1: %+v", name, evs[0])
	}
	prev := int64(0)
	for _, e := range evs {
		if e.Seq <= prev { // live-checked in poll(); belt and braces over the copy
			t.Fatalf("stream %s seq regression: %d after %d", name, e.Seq, prev)
		}
		prev = e.Seq
		if !e.Audience.Public && e.Audience.SeatID != ownSeat {
			t.Fatalf("stream %s carries a foreign private event (seq %d, seat %q)", name, e.Seq, e.Audience.SeatID)
		}
		if e.GameID == "" || e.TS == "" {
			t.Errorf("stream %s event %d missing game_id/ts", name, e.Seq)
		}
	}
}

func assertNoCrossSeatEvents(t *testing.T, streamName string, evs []wireEvent, otherSeatID string) {
	t.Helper()
	for _, e := range evs {
		if e.Audience.SeatID == otherSeatID {
			t.Fatalf("%s carries a private event of the OTHER seat (seq %d, type %s)", streamName, e.Seq, e.Type)
		}
	}
}

// assertDebugUnion verifies the §11 debug dump: public history equals the union
// of both seats' streams' public events, and every private event appears on
// exactly its own seat's stream (full privacy re-verification out-of-band).
func assertDebugUnion(t *testing.T, srv *testServer, res *createResponse, ea, eb []wireEvent) {
	t.Helper()
	status, _, raw := srv.do(t, http.MethodGet, "/debug/games/"+res.GameID, "", nil) // unauthenticated by design (A10)
	if status != http.StatusOK {
		t.Fatalf("GET /debug → %d (want 200): %s", status, raw)
	}
	var dump struct {
		GameID    string   `json:"game_id"`
		Status    string   `json:"status"`
		WinnerID  *string  `json:"winner_id"`
		CourtDeck []string `json:"court_deck"`
		Seats     []struct {
			ID       string   `json:"id"`
			FaceDown []string `json:"face_down"`
			FaceUp   []string `json:"face_up"`
			Driver   struct {
				Mode string `json:"mode"`
			} `json:"driver"`
		} `json:"seats"`
		Events struct {
			Public  []wireEvent `json:"public"`
			Private []wireEvent `json:"private"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatalf("decode debug dump: %v", err)
	}
	if dump.Status != "finished" || dump.WinnerID == nil {
		t.Fatalf("debug dump not finished: %+v", dump.Status)
	}
	if len(dump.CourtDeck) != 11 { // 2 seats ⇒ |deck| = 15 − 4 (spec §3.2.1)
		t.Errorf("debug court_deck = %d cards, want 11", len(dump.CourtDeck))
	}
	total := len(dump.CourtDeck)
	for _, s := range dump.Seats {
		if s.Driver.Mode != "heuristic" {
			t.Errorf("debug seat %s driver mode = %q, want heuristic (this build's stub)", s.ID, s.Driver.Mode)
		}
		total += len(s.FaceDown) + len(s.FaceUp)
	}
	if total != 15 { // invariant §3.2.2: Σ faceDown + Σ faceUp + |deck| = 15
		t.Errorf("card accounting across the dump = %d, want 15", total)
	}

	pubBySeq := map[int64]struct{}{}
	for _, e := range dump.Events.Public {
		pubBySeq[e.Seq] = struct{}{}
	}
	seenPublic := map[string]map[int64]bool{"A": {}, "B": {}}
	for name, evs := range map[string][]wireEvent{"A": ea, "B": eb} {
		for _, e := range evs {
			if e.Audience.Public {
				seenPublic[name][e.Seq] = true
				if _, ok := pubBySeq[e.Seq]; !ok {
					t.Errorf("stream %s public event seq %d missing from the debug dump", name, e.Seq)
				}
			}
		}
	}
	union := map[int64]struct{}{}
	for _, m := range seenPublic {
		for s := range m {
			union[s] = struct{}{}
		}
	}
	if len(union) != len(pubBySeq) {
		t.Errorf("union of both streams' public events (%d) ≠ debug dump public history (%d)", len(union), len(pubBySeq))
	}

	// Every private event in the dump belongs to exactly one stream — its own.
	streamOf := map[string][]wireEvent{res.Seats[0].ID: ea, res.Seats[1].ID: eb}
	for _, e := range dump.Events.Private {
		evs, ok := streamOf[e.Audience.SeatID]
		if !ok {
			t.Fatalf("private event seq %d addressed to unknown seat %q", e.Seq, e.Audience.SeatID)
		}
		found := false
		for _, o := range evs {
			if o.Seq == e.Seq {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("debug private event seq %d (type %s, seat %s) missing from its owner's stream", e.Seq, e.Type, e.Audience.SeatID)
			continue
		}
		// And it must NOT appear on the other seat's stream.
		for seatID, other := range streamOf {
			if seatID == e.Audience.SeatID {
				continue
			}
			for _, o := range other {
				if o.Seq == e.Seq {
					t.Errorf("private event seq %d leaked onto the other seat's stream", e.Seq)
				}
			}
		}
	}
}

// --- small lookups -----------------------------------------------------------

func seatOf(name string, res *createResponse) string {
	if name == "A" {
		return res.Seats[0].ID
	}
	return res.Seats[1].ID
}

func countType(evs []wireEvent, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func filterType(evs []wireEvent, typ string) []wireEvent {
	var out []wireEvent
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// lastSeqAfter returns the bot's current last-seen seq (its poll position).
func (b *bot) lastSeqAfter() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSeq
}

// pollStream performs one long-poll of a game from a given after position and
// returns what arrives (used for the A9 rejection assertions). It also
// advances the given bot's stream bookkeeping so later polls continue from
// there.
func pollStream(t *testing.T, srv *testServer, gameID, tok string, after int64) []wireEvent {
	t.Helper()
	status, _, raw := srv.do(t, http.MethodGet, fmt.Sprintf("/games/%s/events?after=%d", gameID, after), tok, nil)
	if status != http.StatusOK {
		t.Fatalf("poll → %d: %s", status, raw)
	}
	var pr struct {
		Events []wireEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode poll response: %v: %s", err, raw)
	}
	return pr.Events
}

// findRejection returns the first private §10 `rejection` event (A9) in evs.
func findRejection(evs []wireEvent) *wireEvent {
	for i := range evs {
		if evs[i].Type == "rejection" {
			return &evs[i]
		}
	}
	return nil
}

package http_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestE2E_LongPoll_A1HoldEmptyAndPrompt (§11.1): with the hold configured BELOW
// the A1 floor, an idle poll must hold for the clamped 1 s and then answer 200
// with an empty `events` array (never a timeout error); a poll with events
// available answers immediately; a malformed `after` is 400 invalid_payload.
func TestE2E_LongPoll_A1HoldEmptyAndPrompt(t *testing.T) {
	srv := newTestServer(t, 200*time.Millisecond /* requested sub-floor ⇒ clamped to 1 s (A1) */, false)
	res := srv.createGame(t, []seatInfo{{Name: "Marta", Type: "human"}, {Name: "Sonny", Type: "human"}}, false, 0)
	tok := res.Seats[0].Token

	// 1) Events exist ⇒ immediate answer (no hold), from the beginning.
	start := time.Now()
	status, _, raw := srv.do(t, http.MethodGet, "/games/"+res.GameID+"/events?after=0", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("poll after=0 → %d: %s", status, raw)
	}
	var pr struct {
		GameID  string      `json:"game_id"`
		Events  []wireEvent `json:"events"`
		LastSeq int64       `json:"last_seq"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode poll: %v: %s", err, raw)
	}
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Errorf("poll with events available took %v; want immediate (≪ the clamped hold)", d)
	}
	if pr.GameID != res.GameID || len(pr.Events) < 2 {
		t.Fatalf("poll after=0 returned %d events, want ≥2 (game_created, turn_start): %s", len(pr.Events), raw)
	}
	if pr.Events[0].Seq != 1 || pr.Events[0].Type != "game_created" || pr.Events[1].Type != "turn_start" {
		t.Fatalf("initial batch wrong: %+v", pr.Events[:2])
	}
	if pr.LastSeq < pr.Events[len(pr.Events)-1].Seq {
		t.Errorf("last_seq %d < final event seq %d", pr.LastSeq, pr.Events[len(pr.Events)-1].Seq)
	}

	// 2) Idle poll (both seats human ⇒ nothing moves on its own): it must hold
	// for the clamped 1 s and then answer 200 with an EMPTY events array — the
	// A1 empty-200 semantics, not a timeout error.
	start = time.Now()
	status, _, raw = srv.do(t, http.MethodGet, fmt.Sprintf("/games/%s/events?after=%d", res.GameID, pr.LastSeq), tok, nil)
	hold := time.Since(start)
	if status != http.StatusOK {
		t.Fatalf("idle poll → %d (want 200 per A1): %s", status, raw)
	}
	var pr2 struct {
		Events []wireEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &pr2); err != nil {
		t.Fatalf("decode idle poll: %v: %s", err, raw)
	}
	if len(pr2.Events) != 0 {
		t.Errorf("idle poll returned %d events (want 0): %s", len(pr2.Events), raw)
	}
	if !bytes.Contains(raw, []byte(`"events":[]`)) {
		t.Errorf("idle poll body does not carry an empty events ARRAY: %s", raw)
	}
	if hold < 950*time.Millisecond || hold > 3*time.Second {
		t.Errorf("idle poll held %v; want ≈1 s (requested 200 ms clamped up per A1), no early return, no overrun", hold)
	}

	// 3) Malformed after ⇒ 400 invalid_payload.
	status, _, raw = srv.do(t, http.MethodGet, "/games/"+res.GameID+"/events?after=abc", tok, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("poll with after=abc → %d (want 400): %s", status, raw)
	}
	assertError(t, status, raw, "invalid_payload")
}

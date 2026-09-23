package http_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestE2E_DebugGate_A10 (§11.2/A10): with COUP_DEBUG off the /debug routes do
// NOT exist — they answer 404 (NOT 403) like any unknown path; with it on the
// unauthenticated full-state dump serves every hand (LLM and human alike), the
// build config, and the complete unfiltered event history. The same probe set
// covers bearer authz: missing/garbage tokens → 401, a valid token of another
// game → 401.
func TestE2E_DebugGate_A10(t *testing.T) {
	off := newTestServer(t, time.Second, false /* COUP_DEBUG=0 */)
	on := newTestServer(t, time.Second, true /* COUP_DEBUG=1 */)

	resOff := off.createGame(t, []seatInfo{{Name: "Marta", Type: "human"}, {Name: "Sonny", Type: "human"}}, false, 0)
	resOn := on.createGame(t, []seatInfo{{Name: "Marta", Type: "human"}, {Name: "Sonny", Type: "human"}}, false, 0)

	// --- gate off: /debug is simply not a route → 404 envelope, not 403 ------
	status, _, raw := off.do(t, http.MethodGet, "/debug/games/"+resOff.GameID, "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("COUP_DEBUG=0: /debug → %d (want 404 per A10, never 403): %s", status, raw)
	}
	assertError(t, status, raw, "not_found")

	// --- auth probes ----------------------------------------------------------
	status, _, raw = off.do(t, http.MethodGet, "/games/"+resOff.GameID, "", nil) // no token
	if status != http.StatusUnauthorized {
		t.Fatalf("snapshot without token → %d (want 401): %s", status, raw)
	}
	assertError(t, status, raw, "unauthorized")
	assertContains(t, raw, `"message":"a valid player token is required"`)

	status, _, raw = off.do(t, http.MethodGet, "/games/"+resOff.GameID, "deadbeef"+repeat("0", 56), nil) // garbage token
	if status != http.StatusUnauthorized {
		t.Fatalf("snapshot with garbage token → %d (want 401): %s", status, raw)
	}
	assertError(t, status, raw, "unauthorized")

	status, _, raw = off.do(t, http.MethodGet, "/games/"+resOn.GameID, resOff.Seats[0].Token, nil) // valid token, other game
	if status != http.StatusUnauthorized {
		t.Fatalf("snapshot with another game's token → %d (want 401): %s", status, raw)
	}
	assertError(t, status, raw, "unauthorized")

	// Polls and decisions are seat-scoped too.
	status, _, raw = off.do(t, http.MethodGet, "/games/"+resOff.GameID+"/events?after=0", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("poll without token → %d (want 401): %s", status, raw)
	}
	status, _, raw = off.do(t, http.MethodPost, "/games/"+resOff.GameID+"/decisions", "", map[string]any{"kind": "act"})
	if status != http.StatusUnauthorized {
		t.Fatalf("decision without token → %d (want 401): %s", status, raw)
	}

	// --- gate on: unauthenticated full dump ----------------------------------
	status, _, raw = on.do(t, http.MethodGet, "/debug/games/"+resOn.GameID, "", nil)
	if status != http.StatusOK {
		t.Fatalf("COUP_DEBUG=1: /debug → %d (want 200): %s", status, raw)
	}
	var dump struct {
		GameID    string `json:"game_id"`
		CreatedAt string `json:"created_at"`
		Config    struct {
			DriverMode     string     `json:"driver_mode"`
			PollHoldMs     int        `json:"poll_hold_ms"`
			WindowSettings windowJSON `json:"window_settings"`
		} `json:"config"`
		Status    string   `json:"status"`
		TurnID    int      `json:"turn_id"`
		CourtDeck []string `json:"court_deck"`
		Seats     []struct {
			ID       string   `json:"id"`
			Name     string   `json:"name"`
			Type     string   `json:"type"`
			Coins    int      `json:"coins"`
			FaceDown []string `json:"face_down"`
			FaceUp   []string `json:"face_up"`
			Active   bool     `json:"active"`
			Driver   struct {
				Mode                 string `json:"mode"`
				ConversationMessages int    `json:"conversation_messages"`
				InFlight             *any   `json:"in_flight_decision"`
			} `json:"driver"`
		} `json:"seats"`
		OpenWindow        any   `json:"open_window"`
		PendingDecisions  []any `json:"pending_decisions"`
		ActiveForfeitVote any   `json:"active_forfeit_vote"`
		Events            struct {
			Public  []wireEvent `json:"public"`
			Private []wireEvent `json:"private"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatalf("decode debug dump: %v: %s", err, raw)
	}
	if dump.GameID != resOn.GameID || dump.Status != "in_progress" || dump.TurnID != 1 {
		t.Errorf("debug dump header wrong: game=%s status=%s turn_id=%d", dump.GameID, dump.Status, dump.TurnID)
	}
	if _, err := time.Parse(time.RFC3339, dump.CreatedAt); err != nil {
		t.Errorf("created_at %q is not RFC3339: %v", dump.CreatedAt, err)
	}
	if dump.Config.DriverMode != "heuristic" {
		t.Errorf("config.driver_mode = %q, want heuristic (this build's stub reports the configured mode)", dump.Config.DriverMode)
	}
	if dump.Config.PollHoldMs != 1000 {
		t.Errorf("config.poll_hold_ms = %d, want 1000", dump.Config.PollHoldMs)
	}
	if len(dump.Seats) != 2 || len(dump.CourtDeck) != 11 {
		t.Fatalf("debug dump seats=%d court_deck=%d (want 2 / 11)", len(dump.Seats), len(dump.CourtDeck))
	}
	for i, s := range dump.Seats {
		if len(s.FaceDown) != 2 {
			t.Errorf("seat %s (%s): debug view must expose BOTH face-down cards, got %v", s.ID, s.Type, s.FaceDown)
		}
		if !s.Active || s.Coins != 2 {
			t.Errorf("seat %s initial state wrong: active=%v coins=%d", s.ID, s.Active, s.Coins)
		}
		if s.Driver.Mode != "heuristic" {
			t.Errorf("seat %d driver mode = %q, want heuristic", i, s.Driver.Mode)
		}
	}
	if len(dump.Events.Public) < 2 { // game_created + turn_start at minimum
		t.Errorf("debug public history has %d events, want ≥2: %s", len(dump.Events.Public), raw)
	}
	if len(dump.Events.Private) != 0 {
		t.Errorf("idle game must have no private events yet: %+v", dump.Events.Private)
	}
	if dump.ActiveForfeitVote != nil || dump.OpenWindow != nil || len(dump.PendingDecisions) != 0 {
		t.Errorf("idle game has structures in the dump: window=%+v decisions=%d vote=%+v", dump.OpenWindow, dump.PendingDecisions, dump.ActiveForfeitVote)
	}

	// Gate on but unknown game ⇒ 404 not_found (same as any use case).
	status, _, raw = on.do(t, http.MethodGet, "/debug/games/g_000000000000", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("debug dump of unknown game → %d (want 404): %s", status, raw)
	}
	assertError(t, status, raw, "not_found")

	// Unknown route (the static mount point's placeholder behavior) ⇒ 404 envelope.
	status, _, raw = on.do(t, http.MethodGet, "/definitely/not/a/route", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown route → %d (want 404): %s", status, raw)
	}
	assertError(t, status, raw, "not_found")
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

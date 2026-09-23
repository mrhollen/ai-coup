package http_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestE2E_Rejections_A16A9_TableVoiceAndPrivacy (§5.2.6/§4.5/A16/A9): a scripted
// window choreography over real HTTP asserting, with the exact table-voice
// messages verbatim in the §11.1 envelope:
//   - the claimant is not eligible in their own window → 409 not_eligible;
//   - per-seat once-ness while open (A16) → 409 already_resolved "you have
//     already responded";
//   - stale response after close (§5.2.6) → 409 already_resolved "the window
//     has already been resolved";
//   - every rejection additionally lands as a PRIVATE `rejection` event on the
//     OFFENDER's stream only (A9), with its own seq.
//
// Three seats are used deliberately: in a 2-seat FCFS window there is only one
// eligible responder, and that seat's single allowed response (A16) is exactly
// what would close the window — so a 3-seat table keeps both rejection cases
// independently observable (B double-responds while open; C closes; B goes stale).
func TestE2E_Rejections_A16A9_TableVoiceAndPrivacy(t *testing.T) {
	srv := newTestServer(t, time.Second, false)
	// FCFS window (require_all=false, no timeout): passes never close it by
	// themselves — exactly what this choreography needs.
	res := srv.createGame(t, []seatInfo{
		{Name: "Marta", Type: "human"},
		{Name: "Sonny", Type: "human"},
		{Name: "Vito", Type: "human"},
	}, false, 0)

	actorSeat, bSeat, cSeat := "", "", ""
	if start, ok := firstTurnActor(t, srv, res); !ok {
		t.Fatal("no turn_start in the initial batch")
	} else {
		for _, s := range res.Seats {
			switch s.ID {
			case start:
				actorSeat = s.ID
			default:
				if bSeat == "" {
					bSeat = s.ID
				} else {
					cSeat = s.ID
				}
			}
		}
	}
	tokOf := func(seatID string) string {
		for _, s := range res.Seats {
			if s.ID == seatID {
				return s.Token
			}
		}
		t.Fatalf("no token for %s", seatID)
		return ""
	}

	// The actor declares a Tax (Duke claim) ⇒ a challenge window opens for the
	// two other seats at once (§5.1/§5.2.1: simultaneous open).
	if status, _, raw := doDecision(t, srv, res.GameID, tokOf(actorSeat), map[string]any{
		"kind":   "act",
		"action": map[string]string{"type": "tax", "character": "duke"},
	}); status != http.StatusAccepted {
		t.Fatalf("actor's tax → %d: %s (want 202)", status, raw)
	}

	// (1) The claimant cannot respond to their own window (§5.1): 409 with the
	// §4.5 table-voice message verbatim — while the window is still open.
	status, _, raw := doDecision(t, srv, res.GameID, tokOf(actorSeat), map[string]any{
		"kind": "respond", "response": map[string]string{"type": "pass"},
	})
	if status != http.StatusConflict {
		t.Fatalf("claimant's respond → %d (want 409): %s", status, raw)
	}
	assertError(t, status, raw, "not_eligible")
	assertContains(t, raw, `"message":"you cannot respond to that window"`)

	// (2) B passes. FCFS: the window stays open (require_all=false, no timeout).
	if status, _, raw := doDecision(t, srv, res.GameID, tokOf(bSeat), map[string]any{
		"kind": "respond", "response": map[string]string{"type": "pass"},
	}); status != http.StatusAccepted {
		t.Fatalf("B's pass → %d: %s (want 202)", status, raw)
	}

	// (3) Per-seat once-ness while the window is still open (A16).
	status, _, raw = doDecision(t, srv, res.GameID, tokOf(bSeat), map[string]any{
		"kind": "respond", "response": map[string]string{"type": "pass"},
	})
	if status != http.StatusConflict {
		t.Fatalf("B's second response while open → %d (want 409): %s", status, raw)
	}
	assertError(t, status, raw, "already_resolved")
	assertContains(t, raw, `"message":"you have already responded"`)

	// (4) C's challenge is the first non-PASS ⇒ it closes the window for ALL
	// seats (§5.2: first non-PASS wins).
	if status, _, raw := doDecision(t, srv, res.GameID, tokOf(cSeat), map[string]any{
		"kind": "respond", "response": map[string]string{"type": "challenge"},
	}); status != http.StatusAccepted {
		t.Fatalf("C's challenge → %d: %s (want 202)", status, raw)
	}

	// (5) B's stale response after close (§5.2.6): rejected with the §4.5
	// message — distinct from (3)'s open-window once-ness error.
	status, _, raw = doDecision(t, srv, res.GameID, tokOf(bSeat), map[string]any{
		"kind": "respond", "response": map[string]string{"type": "pass"},
	})
	if status != http.StatusConflict {
		t.Fatalf("B's stale respond after close → %d (want 409): %s", status, raw)
	}
	assertError(t, status, raw, "already_resolved")
	assertContains(t, raw, `"message":"the window has already been resolved"`)

	// A9: each rejection landed as a private `rejection` event — on the
	// offender's stream only, with its own seq.
	evActor := fullStream(t, srv, res.GameID, tokOf(actorSeat))
	evB := fullStream(t, srv, res.GameID, tokOf(bSeat))
	evC := fullStream(t, srv, res.GameID, tokOf(cSeat))

	rj := rejectionsOf(evActor, actorSeat)
	if len(rj) != 1 {
		t.Fatalf("actor stream: %d rejection events, want exactly 1 (not_eligible)", len(rj))
	}
	if rj[0].str("code") != "not_eligible" || rj[0].str("message") != "you cannot respond to that window" {
		t.Errorf("actor rejection payload wrong: code=%q message=%q", rj[0].str("code"), rj[0].str("message"))
	}

	rjb := rejectionsOf(evB, bSeat)
	if len(rjb) != 2 {
		t.Fatalf("B stream: %d rejection events, want exactly 2 (once-ness + stale)", len(rjb))
	}
	got := map[string]bool{}
	for _, e := range rjb {
		got[e.str("message")] = true
	}
	if !got["you have already responded"] || !got["the window has already been resolved"] {
		t.Errorf("B stream rejections = %v, want the two A16/stale table-voice messages", got)
	}

	rjc := rejectionsOf(evC, cSeat)
	if len(rjc) != 0 {
		t.Fatalf("C stream: %d rejection events, want 0 (C's challenge was accepted)", len(rjc))
	}

	// No cross-contamination: each offender's rejections stay on their own
	// private stream (§8/A9), with the event's seat_id naming its owner.
	streams := []struct {
		name string
		evs  []wireEvent
		seat string
	}{
		{"actor", evActor, actorSeat},
		{"B", evB, bSeat},
		{"C", evC, cSeat},
	}
	for _, st := range streams {
		for _, e := range st.evs {
			if e.Type == "rejection" && (e.Audience.SeatID != st.seat || e.str("seat_id") != st.seat) {
				t.Errorf("stream %s carries a rejection for someone else: %+v", st.name, e)
			}
		}
	}

	// The two rejections on B's stream consumed their own seqs (A9): they sit
	// strictly between/after the public events, never replacing any.
	prev := int64(0)
	var rjSeqs []int64
	for _, e := range evB {
		if e.Seq <= prev {
			t.Fatalf("B stream seq regression: %d after %d", e.Seq, prev)
		}
		prev = e.Seq
		if e.Type == "rejection" {
			rjSeqs = append(rjSeqs, e.Seq)
		}
	}
	if len(rjSeqs) == 2 && rjSeqs[0] >= rjSeqs[1] {
		t.Errorf("B's rejection seqs not increasing: %v", rjSeqs)
	}
}

// firstTurnActor finds the seat owed the first turn from the initial public
// batch (turn_start.seat_id).
func firstTurnActor(t *testing.T, srv *testServer, res *createResponse) (string, bool) {
	t.Helper()
	status, _, raw := srv.do(t, http.MethodGet, "/games/"+res.GameID+"/events?after=0", res.Seats[0].Token, nil)
	if status != http.StatusOK {
		t.Fatalf("initial poll → %d: %s", status, raw)
	}
	var pr struct {
		Events []wireEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode initial poll: %v", err)
	}
	for _, e := range pr.Events {
		if e.Type == "turn_start" {
			return e.str("seat_id"), true
		}
	}
	return "", false
}

// doDecision POSTs one decision and returns (status, requestID, body).
func doDecision(t *testing.T, srv *testServer, gameID, tok string, body any) (int, string, []byte) {
	t.Helper()
	return srv.do(t, http.MethodPost, "/games/"+gameID+"/decisions", tok, body)
}

// fullStream returns a seat's complete event stream from the beginning.
func fullStream(t *testing.T, srv *testServer, gameID, tok string) []wireEvent {
	t.Helper()
	status, _, raw := srv.do(t, http.MethodGet, "/games/"+gameID+"/events?after=0", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("full stream poll → %d: %s", status, raw)
	}
	var pr struct {
		Events []wireEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode full stream: %v", err)
	}
	return pr.Events
}

// rejectionsOf returns the private rejection events of one seat in evs.
func rejectionsOf(evs []wireEvent, seatID string) []wireEvent {
	var out []wireEvent
	for _, e := range evs {
		if e.Type == "rejection" && e.Audience.SeatID == seatID {
			out = append(out, e)
		}
	}
	return out
}

func assertContains(t *testing.T, raw []byte, sub string) {
	t.Helper()
	if !bytes.Contains(raw, []byte(sub)) {
		t.Errorf("response %s does not contain %s", raw, sub)
	}
}

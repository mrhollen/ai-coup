package application_test

import (
	"strings"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestStructureBookkeepingStableAcrossResponses is the regression test for the
// reconcile bug found in the §13 observability audit: every committed batch
// (including a plain window response) used to rebuild the open structure's
// meta with a fresh openedAt, a fresh stop channel and re-spawned watches. A
// window that stays open (row 1 of §5.2: no require-all, no timeout, everyone
// passed ⇒ open-ended FCFS) accumulated one ghost heartbeat per response —
// duplicated still_waiting lines per tick, elapsed baselines reset at each
// response, and a sliding deadline for timed windows. Now the meta (and its
// single watch set) is kept while the same structure stays open: exactly one
// still_waiting line per 30 s tick, elapsed measured from the true opening.
func TestStructureBookkeepingStableAcrossResponses(t *testing.T) {
	players := []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}, {name: "C", typ: "llm"}}
	h, res := createGameAt(t, players, 0 /* A first */, false, 0 /* no timeout: row-1 FCFS keeps the window open */, func(h *harness) {
		h.driver.Queue(seatIDn(2), llm.CandidateStep(app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
		h.driver.Queue(seatIDn(3), llm.CandidateStep(app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}))
	}, nil)
	aID := res.Seats[0].ID

	// A's foreign aid opens a challenge window eligible to B and C; both LLM
	// seats pass. Under §5.2 row 1 the window then STAYS OPEN (open-ended
	// FCFS), so its heartbeat is observable indefinitely.
	h.mustAct(t, res, aID, app.ActionIn{Type: "foreign_aid"})
	var wid string
	h.waitFor(t, "window open with both LLM responses in", func() bool {
		evts := h.stream(t, res.GameID, aID)
		opened := findEvents(evts, "window_opened")
		passes := findEvents(evts, "response_received")
		if len(opened) == 0 || len(passes) < 2 {
			return false
		}
		wid, _ = opened[0].Payload["window_id"].(string)
		return wid != ""
	})

	countWaiting := func() int { return strings.Count(h.logBuf.String(), `"msg":"still_waiting"`) }

	// Tick 1: exactly one heartbeat line, elapsed measured from the open.
	h.cl.Advance(30 * time.Second)
	h.waitFor(t, "first still_waiting tick", func() bool { return countWaiting() >= 1 })
	if n := countWaiting(); n != 1 {
		t.Fatalf("still_waiting lines after 1 tick = %d, want exactly 1 (one watch per structure)", n)
	}
	first := strings.LastIndex(h.logBuf.String(), `"msg":"still_waiting"`)
	if !strings.Contains(h.logBuf.String()[first:], `"id":"`+wid+`"`) || !strings.Contains(h.logBuf.String()[first:], `"elapsed_ms":30000`) {
		t.Fatalf("tick 1 line = %s, want id=%s elapsed from the true opening (30000ms)", h.logBuf.String()[first:], wid)
	}

	// Tick 2: one more line only, baseline still the open (not the last response).
	h.cl.Advance(30 * time.Second)
	h.waitFor(t, "second still_waiting tick", func() bool { return countWaiting() >= 2 })
	if n := countWaiting(); n != 2 {
		t.Fatalf("still_waiting lines after 2 ticks = %d, want exactly 2", n)
	}
	second := strings.LastIndex(h.logBuf.String(), `"msg":"still_waiting"`)
	if !strings.Contains(h.logBuf.String()[second:], `"elapsed_ms":60000`) {
		t.Fatalf("tick 2 line = %s, want elapsed_ms=60000 (baseline must not slide with responses)", h.logBuf.String()[second:])
	}

	// The window is still open — row-1 FCFS: passes never close it by themselves.
	if phase(h.snapshot(t, res.GameID, aID).Current) != "window_open" {
		t.Fatal("row-1 FCFS window must stay open after all passes")
	}
}

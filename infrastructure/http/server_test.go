package http_test

// Server-level observability test (§12): the access log must carry one INFO
// line per request with the request id, method, path, seat id ("-" when
// unauthenticated), the REAL status code (not a stale 200), and latency.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	couphttp "forgejo.lan/mrhollen/ai-coup/infrastructure/http"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/store"
	clock "forgejo.lan/mrhollen/ai-coup/infrastructure/time"
)

// recordingHandler collects slog text lines for assertion.
type recordingHandler struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle renders the record as text-style key=value fields (the same layout
// slog.NewTextHandler produces for our purposes) for substring assertions.
func (r *recordingHandler) Handle(ctx context.Context, rec slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "level=%s msg=%q", rec.Level.String(), rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	r.mu.Lock()
	r.lines = append(r.lines, b.String())
	r.mu.Unlock()
	return nil
}
func (r *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recordingHandler) WithGroup(string) slog.Handler      { return r }

func (r *recordingHandler) requestLines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, l := range r.lines {
		if strings.Contains(l, `msg="http_request"`) {
			out = append(out, l)
		}
	}
	return out
}

// TestAccessLog_StatusCodeAndFields (§12/§13): every request emits one
// http_request line whose status matches the actual response (201 create, 401
// missing token, 200 valid snapshot, 404 unknown route), with the seat id
// resolved from the token and "-" otherwise.
func TestAccessLog_StatusCodeAndFields(t *testing.T) {
	rec := &recordingHandler{}
	logger := slog.New(rec)
	events := store.NewMemoryEventStore(clock.NewWallClock())
	games := store.NewMemoryGameStore()
	a := app.NewApplication(app.Dependencies{
		Events: events, Games: games, Clock: clock.NewWallClock(),
		IDs: clock.NewRealIDGen(), Driver: llm.NewUnavailableDriver("heuristic"),
		Log: logger,
	})
	t.Cleanup(func() { a.Close() })
	c := couphttp.NewServer(a, events, time.Second, false, "heuristic", logger, os.DirFS("../../web"))
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)

	// 201: create a game (no auth; the line must carry seat_id=-).
	resp, err := http.Post(srv.URL+"/games", "application/json", strings.NewReader(
		`{"players":[{"name":"Marta","type":"human"},{"name":"Sonny","type":"human"}]}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create → %d: %s", resp.StatusCode, raw)
	}
	var create struct {
		GameID string `json:"game_id"`
		Seats  []struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		} `json:"seats"`
	}
	if err := json.Unmarshal(raw, &create); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// 401: no token on a seat-scoped resource.
	r401, err := http.Get(srv.URL + "/games/" + create.GameID)
	if err != nil {
		t.Fatalf("no-token snapshot: %v", err)
	}
	io.ReadAll(r401.Body)
	r401.Body.Close()
	if r401.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token snapshot → %d (want 401)", r401.StatusCode)
	}

	// 200: valid token.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/games/"+create.GameID, nil)
	req.Header.Set("Authorization", "Bearer "+create.Seats[0].Token)
	r200, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authed snapshot: %v", err)
	}
	io.ReadAll(r200.Body)
	r200.Body.Close()
	if r200.StatusCode != http.StatusOK {
		t.Fatalf("authed snapshot → %d (want 200)", r200.StatusCode)
	}

	// 404: unknown route.
	r404, err := http.Get(srv.URL + "/nope/nothing")
	if err != nil {
		t.Fatalf("unknown route: %v", err)
	}
	io.ReadAll(r404.Body)
	r404.Body.Close()
	if r404.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route → %d (want 404)", r404.StatusCode)
	}

	lines := rec.requestLines()
	if len(lines) != 4 {
		t.Fatalf("want exactly 4 access log lines, got %d: %v", len(lines), lines)
	}
	find := func(substrs ...string) (string, bool) {
		for _, l := range lines {
			ok := true
			for _, s := range substrs {
				if !strings.Contains(l, s) {
					ok = false
					break
				}
			}
			if ok {
				return l, true
			}
		}
		return "", false
	}

	createLine, ok := find("path=/games ", "status=201", "seat_id=-") // note: text handler pads the value
	if !ok {
		createLine, ok = find("path=/games", "status=201", "seat_id=-")
	}
	if !ok {
		t.Fatalf("missing create access line (want path=/games status=201 seat_id=-): %v", lines)
	}
	line401, ok := find("/games/"+create.GameID, "status=401", "seat_id=-")
	if !ok {
		t.Fatalf("missing 401 access line: %v", lines)
	}
	line200, ok := find("/games/"+create.GameID, "status=200")
	if !ok {
		t.Fatalf("missing 200 access line: %v", lines)
	} else if !strings.Contains(line200, create.Seats[0].ID) {
		t.Errorf("authed access line must carry the token's seat id: %s", line200)
	}
	line404, ok := find("path=/nope/nothing", "status=404")
	if !ok {
		t.Fatalf("missing 404 access line: %v", lines)
	}

	for name, l := range map[string]string{"create": createLine, "401": line401, "200": line200, "404": line404} {
		if !strings.Contains(l, "request_id=") || !strings.Contains(l, "method=") || !strings.Contains(l, "latency_ms=") {
			t.Errorf("%s access log line missing required fields: %s", name, l)
		}
	}
}

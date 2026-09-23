// Package http_test is the §14.3 end-to-end suite: two scripted "human"
// protocol clients playing full games over real HTTP through httptest —
// localhost ephemeral ports only, no fixed port is ever bound or dialed (§1).
// The clients consume ONLY the §10/§11 wire shapes (the UI contract).
package http_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// testServer bundles one httptest server with the composition behind it.
type testServer struct {
	ts     *httptest.Server
	events *store.MemoryEventStore
}

// discardHandler is a no-op slog.Handler: e2e assertions live in the HTTP wire,
// not in logs, and test output stays clean.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

// newTestServer composes a production-shaped server (main.go's wiring: wall
// clock, real ids, the unavailable LLM driver stub) behind httptest. The hold
// is passed through unclamped so A1 clamping itself can be asserted by the
// caller; debug toggles the /debug route gate (A10).
func newTestServer(t *testing.T, hold time.Duration, debug bool) *testServer {
	t.Helper()
	events := store.NewMemoryEventStore(clock.NewWallClock())
	games := store.NewMemoryGameStore()
	logger := slog.New(discardHandler{})
	a := app.NewApplication(app.Dependencies{
		Events: events,
		Games:  games,
		Clock:  clock.NewWallClock(),
		IDs:    clock.NewRealIDGen(),
		Driver: llm.NewUnavailableDriver("heuristic"), // same stub as main.go in this slice
		Log:    logger,
	})
	srv := couphttp.NewServer(a, events, hold, debug, "heuristic", logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		a.Close() // cancels any in-flight work (§9 shutdown point)
	})
	return &testServer{ts: ts, events: events}
}

// ---------------------------------------------------------------------------
// raw wire types — EXACTLY the §10/§11 shapes, nothing else (UI contract)
// ---------------------------------------------------------------------------

type seatInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Token string `json:"token,omitempty"`
}

type createResponse struct {
	GameID          string     `json:"game_id"`
	Seats           []seatInfo `json:"seats"`
	WindowSettings  windowJSON `json:"window_settings"`
	FirstTurnSeatID string     `json:"first_turn_seat_id"`
}

type windowJSON struct {
	RequireAllResponses bool `json:"require_all_responses"`
	ResponseTimeoutMs   int  `json:"response_timeout_ms"`
}

type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// wireAudience is the §10 audience tag: "public" or {"seat":"<id>"}.
type wireAudience struct {
	Public bool
	SeatID string
}

func (a *wireAudience) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == `"public"` {
		a.Public = true
		return nil
	}
	var o struct {
		Seat string `json:"seat"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	a.SeatID = o.Seat
	return nil
}

// wireEvent is one §10 event as carried on the poll stream / debug dump. Raw
// holds the FULL flattened object (seq/game_id/ts/type/audience plus every
// type-specific payload field, §10.1) so assertions can inspect any payload
// key the spec names without re-decoding.
type wireEvent struct {
	Seq      int64
	GameID   string
	TS       string
	Type     string
	Audience wireAudience
	Raw      map[string]any
}

func (e *wireEvent) UnmarshalJSON(b []byte) error {
	var plain struct {
		Seq      int64        `json:"seq"`
		GameID   string       `json:"game_id"`
		TS       string       `json:"ts"`
		Type     string       `json:"type"`
		Audience wireAudience `json:"audience"`
	}
	if err := json.Unmarshal(b, &plain); err != nil {
		return err
	}
	e.Seq, e.GameID, e.TS, e.Type = plain.Seq, plain.GameID, plain.TS, plain.Type
	e.Audience = plain.Audience
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	raw := make(map[string]any, len(m))
	for k, v := range m {
		var a any
		if err := json.Unmarshal(v, &a); err != nil {
			return err
		}
		raw[k] = a
	}
	e.Raw = raw
	return nil
}

// str reads a string payload key from the event ("" when absent).
func (e wireEvent) str(key string) string {
	s, _ := e.Raw[key].(string)
	return s
}

// wireSnapshot mirrors the §11.2 GET /games/{id} response (viewer-scoped).
type wireSnapshot struct {
	GameID         string       `json:"game_id"`
	Status         string       `json:"status"`
	WinnerID       *string      `json:"winner_id"`
	WindowSettings windowJSON   `json:"window_settings"`
	CourtDeckSize  int          `json:"court_deck_size"`
	Seats          []seatSnap   `json:"seats"`
	YourSeatID     string       `json:"your_seat_id"`
	YourFaceDown   []string     `json:"your_face_down_cards"`
	LastEventSeq   int64        `json:"last_event_seq"`
	Current        *currentJSON `json:"current"`
}

type seatSnap struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Coins       int      `json:"coins"`
	FaceUpCards []string `json:"face_up_cards"`
	Active      bool     `json:"active"`
}

type currentJSON struct {
	Phase      string        `json:"phase"`
	TurnID     int           `json:"turn_id,omitempty"`
	TurnSeatID string        `json:"turn_seat_id,omitempty"`
	Window     *windowSnap   `json:"window,omitempty"`
	Decision   *decisionSnap `json:"decision,omitempty"`
}

type windowSnap struct {
	WindowID             string            `json:"window_id"`
	Kind                 string            `json:"kind"`
	TargetSeatID         string            `json:"target_seat_id,omitempty"`
	RequireAllResponses  bool              `json:"require_all_responses"`
	TimeoutMs            int               `json:"timeout_ms"`
	YourAllowedResponses []string          `json:"your_allowed_responses,omitempty"`
	ResponsesSoFar       map[string]string `json:"responses_so_far"`
}

type decisionSnap struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"` // "flip_choice" | "exchange_finish"
	SeatID    string          `json:"seat_id"`
	Options   json.RawMessage `json:"options,omitempty"` // addressed viewer only (§8)
	TimeoutMs int             `json:"timeout_ms"`
}

// ---------------------------------------------------------------------------
// request helpers
// ---------------------------------------------------------------------------

// do performs one HTTP call against the test server and returns status, the
// X-Request-Id response header and the raw body. A nil body means no payload;
// tok is the bearer token ("" = unauthenticated).
func (s *testServer) do(t *testing.T, method, path, tok string, reqBody any) (int, string, []byte) {
	t.Helper()
	var rd io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("marshal %s body: %v", method, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.ts.URL+path, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("X-Request-Id", "e2e-"+strings.ToLower(t.Name())+"-"+randomHex(4))
	resp, err := s.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s %s: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header.Get("X-Request-Id"), body
}

// createGame POSTs /games with the given seats and window settings (A8 when
// omitting them) and returns the decoded 201 response.
func (s *testServer) createGame(t *testing.T, players []seatInfo, requireAll bool, timeoutMs int) *createResponse {
	t.Helper()
	body := map[string]any{"players": json.RawMessage("[]")}
	ppl := make([]map[string]string, len(players))
	for i, p := range players {
		ppl[i] = map[string]string{"name": p.Name, "type": p.Type}
	}
	rawPpl, err := json.Marshal(ppl)
	if err != nil {
		t.Fatalf("marshal players: %v", err)
	}
	body["players"] = json.RawMessage(rawPpl)
	if requireAll || timeoutMs > 0 {
		w := map[string]any{"require_all_responses": requireAll}
		if timeoutMs > 0 {
			w["response_timeout"] = fmt.Sprintf("%dms", timeoutMs)
		}
		rawW, err := json.Marshal(w)
		if err != nil {
			t.Fatalf("marshal window: %v", err)
		}
		body["window"] = json.RawMessage(rawW)
	}

	status, reqID, raw := s.do(t, http.MethodPost, "/games", "", body)
	if status != http.StatusCreated {
		t.Fatalf("POST /games → %d, want 201: %s", status, raw)
	}
	assertRequestIDEcho(t, t.Name(), reqID, raw)
	var res createResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode create response: %v: %s", err, raw)
	}
	return &res
}

// assertRequestIDEcho verifies the A17 echo: the client-supplied id (which do()
// always sends) comes back in both the header and — for error responses — the
// envelope's request_id.
func assertRequestIDEcho(t *testing.T, testName, reqID string, raw []byte) {
	t.Helper()
	if !strings.HasPrefix(reqID, "e2e-"+strings.ToLower(testName)) {
		t.Errorf("X-Request-Id not echoed: %q", reqID)
	}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		if !strings.Contains(env.Error.RequestID, strings.ToLower(testName)) {
			t.Errorf("error envelope request_id not echoed: %q (header %q)", env.Error.RequestID, reqID)
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return strings.Repeat("0", 2*n) // cosmetic header value only
	}
	return hex.EncodeToString(b)
}

// isHexString reports whether s is an even-length lowercase hex string (the
// A2/A3 token shape: 64 hex chars).
func isHexString(s string) bool {
	if len(s)%2 != 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// assertError decodes a §11.1 error envelope from raw and checks its wire code
// (and that request_id is present).
func assertError(t *testing.T, status int, raw []byte, wantCode string) {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("not a JSON error envelope (%d): %s", status, raw)
	}
	if env.Error.Code != wantCode {
		t.Fatalf("error code = %q (message %q), want %q", env.Error.Code, env.Error.Message, wantCode)
	}
	if env.Error.RequestID == "" {
		t.Error("error envelope missing request_id")
	}
}

// ---------------------------------------------------------------------------
// the scripted human bot (§14.3 protocol client)
// ---------------------------------------------------------------------------

// bot is one scripted "human": it polls its own event stream and, whenever the
// snapshot says it owes a decision, acts by policy so a 2-seat game provably
// terminates: ≥10 coins ⇒ coup (mandatory), assassin in hand + affordable ⇒
// assassinate the opponent, else income; windows always get a pass; single-
// seat choices take the first option (flip) / no-swap (exchange finish).
type bot struct {
	t        *testing.T
	srv      *testServer
	gameID   string
	seatID   string
	token    string
	opponent string // the other seat's id (2-player games)

	mu       sync.Mutex
	events   []wireEvent // every event observed, in arrival order
	lastSeq  int64
	prompts  []time.Duration // latency of polls that returned ≥1 event
	snaps    []wireSnapshot  // every snapshot taken (privacy assertions)
	finished chan struct{}   // closed when the bot observes status finished
}

func newBot(t *testing.T, srv *testServer, res *createResponse, idx int, others ...string) *bot {
	t.Helper()
	return &bot{
		t:        t,
		srv:      srv,
		gameID:   res.GameID,
		seatID:   res.Seats[idx].ID,
		token:    res.Seats[idx].Token,
		opponent: others[0],
		events:   []wireEvent{},
		finished: make(chan struct{}),
	}
}

// run polls and acts until the snapshot says finished (or ctx is done). It runs
// in its own goroutine; the finished channel closes when the bot stops.
func (b *bot) run(ctx context.Context) {
	defer close(b.finished)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		finished, err := b.pollAndAct(ctx)
		if err != nil {
			b.t.Errorf("bot %s loop: %v", b.seatID, err)
			return
		}
		if finished {
			// Final drain: the terminal batch may have been committed between our
			// last poll and the snapshot that showed "finished" — catch anything
			// we have not seen so the stream is complete (no event can follow
			// game_over, so one more poll suffices).
			if err := b.poll(ctx); err != nil {
				b.t.Errorf("bot %s final drain: %v", b.seatID, err)
			}
			return
		}
	}
}

func (b *bot) Join() <-chan struct{} { return b.finished }

// poll performs one long-poll at b.lastSeq and appends what arrives. It asserts
// per-stream seq monotonicity live: every new event's seq exceeds everything
// previously seen on THIS stream (gaps are legal — other seats' private events
// are invisible here — duplicates or regressions are not).
func (b *bot) poll(ctx context.Context) error {
	start := time.Now()
	status, _, raw := b.srv.do(b.t, http.MethodGet, fmt.Sprintf("/games/%s/events?after=%d", b.gameID, b.lastSeq), b.token, nil)
	elapsed := time.Since(start)
	if status != http.StatusOK {
		return fmt.Errorf("poll → %d: %s", status, raw)
	}
	var pr struct {
		GameID  string      `json:"game_id"`
		Events  []wireEvent `json:"events"`
		LastSeq int64       `json:"last_seq"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		return fmt.Errorf("decode poll response: %v: %s", err, raw)
	}
	if pr.GameID != b.gameID || pr.LastSeq < b.lastSeq {
		return fmt.Errorf("poll inconsistency: game_id=%s last_seq=%d (was %d)", pr.GameID, pr.LastSeq, b.lastSeq)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range pr.Events {
		if e.Seq <= b.lastSeq {
			return fmt.Errorf("non-monotonic seq on my stream: got %d after %d", e.Seq, b.lastSeq)
		}
		b.events = append(b.events, e)
		b.lastSeq = e.Seq
	}
	if len(pr.Events) > 0 {
		b.prompts = append(b.prompts, elapsed)
	}
	return nil
}

// pollAndAct does one poll then answers whatever the snapshot now owes this
// seat. It reports finished=true once the snapshot says status "finished".
func (b *bot) pollAndAct(ctx context.Context) (bool, error) {
	if err := b.poll(ctx); err != nil {
		return false, err
	}
	snap, err := b.snapshot(ctx)
	if err != nil {
		return false, err
	}
	b.mu.Lock()
	b.snaps = append(b.snaps, *snap)
	myTurn := snap.Current != nil && snap.Current.Phase == "turn_action" && snap.Current.TurnSeatID == b.seatID
	iAmEligible := false
	if w := windowOf(snap); w != nil {
		iAmEligible = len(w.YourAllowedResponses) > 0
	}
	decisionForMe := snap.Current != nil && snap.Current.Phase == "single_seat_decision" && snap.Current.Decision != nil && snap.Current.Decision.SeatID == b.seatID
	finished := snap.Status == "finished"
	b.mu.Unlock()

	if finished {
		return true, nil
	}
	switch {
	case myTurn:
		if err := b.actOnTurn(snap); err != nil {
			return false, err
		}
	case iAmEligible && alreadyResponded(snap, b.seatID):
		// my pass is recorded; nothing owed (the window may still be open for others)
	case iAmEligible:
		if err := b.respondPass(); err != nil {
			return false, err
		}
	case decisionForMe:
		if err := b.answerDecision(snap); err != nil {
			return false, err
		}
	}
	return false, nil
}

// respondPass passes in the open window (the §14.3 policy: never challenge).
func (b *bot) respondPass() error {
	status, _, raw := b.srv.do(b.t, http.MethodPost, "/games/"+b.gameID+"/decisions", b.token,
		map[string]any{"kind": "respond", "response": map[string]string{"type": "pass"}})
	if status != http.StatusAccepted {
		return fmt.Errorf("respond(pass) → %d: %s", status, raw)
	}
	return nil
}

func (b *bot) snapshot(ctx context.Context) (*wireSnapshot, error) {
	status, _, raw := b.srv.do(b.t, http.MethodGet, "/games/"+b.gameID, b.token, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("snapshot → %d: %s", status, raw)
	}
	var snap wireSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %v: %s", err, raw)
	}
	if snap.YourSeatID != b.seatID {
		return nil, fmt.Errorf("snapshot your_seat_id = %q, want my seat %q", snap.YourSeatID, b.seatID)
	}
	return &snap, nil
}

// actOnTurn plays the policy move for this bot's turn (see bot doc comment).
func (b *bot) actOnTurn(snap *wireSnapshot) error {
	var me *seatSnap
	for i := range snap.Seats {
		if snap.Seats[i].ID == b.seatID {
			me = &snap.Seats[i]
			break
		}
	}
	if me == nil {
		return fmt.Errorf("my seat not in snapshot")
	}
	action := map[string]string{"type": "income"}
	switch {
	case me.Coins >= 10: // mandatory coup (rules.md Ruling 1)
		action = map[string]string{"type": "coup", "target_id": b.opponent}
	case b.hasCard("assassin") && me.Coins >= 3:
		action = map[string]string{"type": "assassinate", "character": "assassin", "target_id": b.opponent}
	}
	status, _, raw := b.srv.do(b.t, http.MethodPost, "/games/"+b.gameID+"/decisions", b.token,
		map[string]any{"kind": "act", "action": action})
	if status != http.StatusAccepted {
		return fmt.Errorf("act(%v) → %d: %s", action, status, raw)
	}
	return nil
}

// hasCard reports whether the bot's own face-down hand (from its most recent
// snapshot — the ONLY place another seat's cards could leak to this client)
// contains the character.
func (b *bot) hasCard(card string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.snaps) == 0 {
		return false
	}
	for _, c := range b.snaps[len(b.snaps)-1].YourFaceDown {
		if c == card {
			return true
		}
	}
	return false
}

// answerDecision answers a pending single-seat decision addressed to this bot:
// flip choice ⇒ first option; exchange finish ⇒ no-swap (policy).
func (b *bot) answerDecision(snap *wireSnapshot) error {
	d := snap.Current.Decision
	choice := map[string]any{}
	switch d.Kind {
	case "flip_choice":
		var opts []string
		if err := json.Unmarshal(d.Options, &opts); err != nil || len(opts) == 0 {
			return fmt.Errorf("flip choice options missing/wrong shape: %s", d.Options)
		}
		choice["flip_choice"] = opts[0]
	case "exchange_finish":
		choice["exchange_finish"] = map[string]any{"no_swap": true}
	default:
		return fmt.Errorf("unknown decision kind %q", d.Kind)
	}
	status, _, raw := b.srv.do(b.t, http.MethodPost, "/games/"+b.gameID+"/decisions", b.token,
		map[string]any{"kind": "choose", "decision_id": d.ID, "choice": choice})
	if status != http.StatusAccepted {
		return fmt.Errorf("choose(%s) → %d: %s", d.Kind, status, raw)
	}
	return nil
}

// events returns the bot's full observed stream (copy).
func (b *bot) eventsSnapshot() []wireEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]wireEvent, len(b.events))
	copy(out, b.events)
	return out
}

func windowOf(snap *wireSnapshot) *windowSnap {
	if snap.Current == nil || snap.Current.Phase != "window_open" {
		return nil
	}
	return snap.Current.Window
}

// alreadyResponded reports whether this seat's response is recorded on the open
// window (so a require-all window that is still open does not re-trigger).
func alreadyResponded(snap *wireSnapshot, seatID string) bool {
	w := windowOf(snap)
	return w != nil && w.ResponsesSoFar != nil && w.ResponsesSoFar[seatID] != ""
}

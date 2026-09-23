package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
)

// --- shared test plumbing ----------------------------------------------------

func newDriverWithLog(t *testing.T, baseURL string, debug bool) (*OpenAIDriver, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := NewOpenAIDriver(baseURL, "test-model", log, debug)
	return d, buf
}

// recordedSleeps is a no-op sleep seam that records every requested backoff
// delay — tests never wait out real intervals.
type recordedSleeps struct {
	mu     sync.Mutex
	dels   []time.Duration
	cancel func() // optional: fires when enough delays have been recorded
	every  int
}

// install wires the recording sleep seam into the driver. r.cancel/r.every are
// set by the caller BEFORE install (zero cancel / every ≤ 0 = never fire).
func (r *recordedSleeps) install(d *OpenAIDriver) {
	d.sleep = func(ctx context.Context, delay time.Duration) error {
		r.mu.Lock()
		r.dels = append(r.dels, delay)
		n := len(r.dels)
		c := r.cancel
		fire := c != nil && r.every > 0 && n%r.every == 0
		r.mu.Unlock()
		if fire {
			c()
		}
		return ctx.Err()
	}
}

// forceWorstCaseJitter pins the jitter draw to its cap so backoff sequences are
// exactly [500ms, 1s, 2s, …, 30s, 30s…].
func forceWorstCaseJitter(d *OpenAIDriver) {
	d.jitter = func(max time.Duration) time.Duration { return max }
}

type scriptedChatServer struct {
	ts     *httptest.Server
	mu     sync.Mutex
	bodies []chatRequest // every request body, decoded
	calls  int
	script func(n int, req chatRequest) (status int, body []byte)
}

func newScriptedChatServer(t *testing.T, script func(int, chatRequest) (int, []byte)) *scriptedChatServer {
	t.Helper()
	s := &scriptedChatServer{script: script}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req chatRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			w.WriteHeader(400)
			return
		}
		s.mu.Lock()
		s.calls++
		n := s.calls
		s.bodies = append(s.bodies, req)
		s.mu.Unlock()
		status, body := s.script(n, req)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.ts.Close)
	return s
}

func (s *scriptedChatServer) lastBody(t *testing.T) chatRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		t.Fatal("the mock server saw no requests")
	}
	return s.bodies[len(s.bodies)-1]
}

func (s *scriptedChatServer) bodyAt(t *testing.T, i int) chatRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		t.Fatalf("the mock server saw %d request(s), want at least %d", len(s.bodies), i+1)
	}
	return s.bodies[i]
}

func (s *scriptedChatServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// --- response body builders (OpenAI wire shape) ------------------------------

func respJSON(t *testing.T, msg map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": msg}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func toolCallMsg(id, name, args string) map[string]any {
	m := map[string]any{"role": "assistant"}
	if args == "" {
		args = "{}"
	}
	m["tool_calls"] = []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}
	return m
}

func contentMsg(text string) map[string]any {
	return map[string]any{"role": "assistant", "content": text}
}

func openaiIn(kind app.DecisionKind, youCoins int) app.DecisionInput {
	seats := []app.SeatView{
		{ID: "s_1", Name: "You", Type: "llm", Coins: youCoins, Active: true},
		{ID: "s_2", Name: "Opp", Type: "human", Coins: 3, Active: true},
	}
	return app.DecisionInput{
		GameID: "g_1", SeatID: "s_1", Kind: kind,
		World: app.WorldView{
			TurnID: 7, DeckSize: 9, You: seats[0],
			YourCards: []domain.Character{domain.Assassin, domain.Captain},
			Seats:     seats,
		},
	}
}

func alwaysRespond(t *testing.T, msg func() map[string]any) func(int, chatRequest) (int, []byte) {
	return func(int, chatRequest) (int, []byte) { return 200, respJSON(t, msg()) }
}

// --- tests -------------------------------------------------------------------

func TestOpenAI_NameAndCapability(t *testing.T) {
	// Never dialed here (Name/capability only); no fixed host or port is
	// referenced from test code (§14.3 port discipline).
	d := NewOpenAIDriver("http://unit-test.invalid/v1", "m", nil, false)
	if got := d.Name(); got != "openai" {
		t.Fatalf("Name() = %q, want openai", got)
	}
	var lc app.LLMCapable = d
	if !lc.CanDriveLLM() {
		t.Fatal("CanDriveLLM() = false, want true")
	}
}

// TestOpenAI_TurnActionToolCallHappyPath is the §6.4 happy path: persistent
// conversation (system prompt once + one user world render), the static seven
// tool shapes, and a parseable candidate back.
func TestOpenAI_TurnActionToolCallHappyPath(t *testing.T) {
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		return 200, respJSON(t, toolCallMsg("call_1", "tax", "{}"))
	})
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)

	cand, err := d.Decide(context.Background(), openaiIn(app.KindTurnAction, 4))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if cand.Action != "tax" || cand.Character != "duke" {
		t.Fatalf("candidate = %+v, want tax claiming duke (shape-derived claim)", cand)
	}

	body := srv.lastBody(t)
	if body.Model != "test-model" {
		t.Errorf("model = %q, want test-model", body.Model)
	}
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want [system user]", body.Messages)
	}
	user := body.Messages[1].Content
	for _, want := range []string{"Turn 7", "Court deck: 9 cards", "You: You (llm): 4 coin(s)", "face-down hand: assassin, captain", "It is your turn"} {
		if !strings.Contains(user, want) {
			t.Errorf("user message missing %q:\n%s", want, user)
		}
	}
	var names []string
	for _, tool := range body.Tools {
		names = append(names, tool.Function.Name)
	}
	wantTools := map[string]bool{"income": true, "foreign_aid": true, "tax": true, "coup": true, "steal": true, "assassinate": true, "exchange": true}
	if len(names) != len(wantTools) {
		t.Fatalf("tools = %v, want the seven action shapes", names)
	}
	for _, n := range names {
		if !wantTools[n] {
			t.Errorf("unexpected tool %q in turn toolset: %v", n, names)
		}
	}
	if got := d.ConversationMessages("g_1", "s_1"); got != 3 { // system + user + assistant
		t.Errorf("ConversationMessages = %d, want 3", got)
	}
}

// TestOpenAI_TurnToolCallShapes pins the tool→candidate mapping for every turn
// shape (including the shape-derived claimed characters).
func TestOpenAI_TurnToolCallShapes(t *testing.T) {
	cases := []struct {
		name, args string
		want       app.Candidate
	}{
		{"income", "", app.Candidate{Kind: app.KindTurnAction, Action: "income"}},
		{"foreign_aid", "", app.Candidate{Kind: app.KindTurnAction, Action: "foreign_aid"}},
		{"coup", `{"target":"s_2"}`, app.Candidate{Kind: app.KindTurnAction, Action: "coup", TargetID: "s_2"}},
		{"steal", `{"target":"s_2"}`, app.Candidate{Kind: app.KindTurnAction, Action: "steal", TargetID: "s_2", Character: "captain"}},
		{"assassinate", `{"target":"s_2"}`, app.Candidate{Kind: app.KindTurnAction, Action: "assassinate", TargetID: "s_2", Character: "assassin"}},
		{"exchange", "", app.Candidate{Kind: app.KindTurnAction, Action: "exchange", Character: "ambassador"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
				return 200, respJSON(t, toolCallMsg("call_1", tc.name, tc.args))
			})
			d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)
			got, err := d.Decide(context.Background(), openaiIn(app.KindTurnAction, 4))
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if got != tc.want {
				t.Fatalf("candidate = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestOpenAI_WindowToolsAndMapping pins the decision-specific window toolset
// (one tool per eligible token) and the candidate mapping.
func TestOpenAI_WindowToolsAndMapping(t *testing.T) {
	// Cases follow the SAME order as EligibleActions below: the mock answers
	// call n with token n, so each mapping is exercised once.
	cases := []struct {
		token string
		want  app.Candidate
	}{
		{"pass", app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}},
		{"counterclaim_captain", app.Candidate{Kind: app.KindWindowResponse, Response: "counterclaim", Character: "captain"}},
		{"challenge", app.Candidate{Kind: app.KindWindowResponse, Response: "challenge"}},
	}
	in := openaiIn(app.KindWindowResponse, 4)
	in.Request = app.Request{WindowID: "w_1", TriggerSummary: "Sonny claims Assassin against you", EligibleActions: []string{"pass", "counterclaim_captain", "challenge"}}

	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		// answer with the nth eligible token so every mapping is exercised
		return 200, respJSON(t, toolCallMsg(fmt.Sprintf("call_%d", n), in.Request.EligibleActions[n-1], "{}"))
	})
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)

	for i, tc := range cases {
		got, err := d.Decide(context.Background(), in)
		if err != nil {
			t.Fatalf("Decide %d: %v", i, err)
		}
		if got != tc.want {
			t.Fatalf("case %q: candidate = %+v, want %+v", tc.token, got, tc.want)
		}
	}

	var names []string
	for _, tool := range srv.lastBody(t).Tools {
		names = append(names, tool.Function.Name)
	}
	if strings.Join(names, ",") != "pass,counterclaim_captain,challenge" {
		t.Fatalf("window tools = %v, want the seat's eligible grammar verbatim", names)
	}
	firstMsgs := srv.bodyAt(t, 0).Messages
	if !strings.Contains(firstMsgs[len(firstMsgs)-1].Content, "Sonny claims Assassin against you") {
		t.Error("window user message must carry the public trigger summary")
	}
}

// TestOpenAI_ChoiceToolsAndMapping pins the flip / exchange-finish toolsets.
func TestOpenAI_ChoiceToolsAndMapping(t *testing.T) {
	flipIn := openaiIn(app.KindSingleSeatChoice, 4)
	flipIn.Request = app.Request{ChoiceKind: "flip_choice", Options: []string{"face_down_0", "face_down_1"}}
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		return 200, respJSON(t, toolCallMsg("call_1", "flip_card", `{"option":"face_down_1"}`))
	})
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)
	got, err := d.Decide(context.Background(), flipIn)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Choice != "face_down_1" {
		t.Fatalf("flip choice = %+v, want face_down_1", got)
	}
	if names := toolNames(srv.lastBody(t)); strings.Join(names, ",") != "flip_card" {
		t.Fatalf("flip tools = %v, want [flip_card]", names)
	}

	exchIn := openaiIn(app.KindSingleSeatChoice, 4)
	exchIn.Request = app.Request{ChoiceKind: "exchange_finish", TriggerSummary: "Finish your exchange: keep neither drawn card, or swap one drawn card for one of yours."}
	srv2 := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		if n == 1 {
			return 200, respJSON(t, toolCallMsg("call_1", "swap_drawn_option", `{"drawn_index":1,"hand_option":"face_down_0"}`))
		}
		return 200, respJSON(t, toolCallMsg("call_2", "keep_drawn_cards", ""))
	})
	d2, _ := newDriverWithLog(t, srv2.ts.URL+"/v1", false)
	got, err = d2.Decide(context.Background(), exchIn)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Choice != "swap_drawn_1_face_down_0" {
		t.Fatalf("exchange swap = %+v, want swap_drawn_1_face_down_0", got)
	}
	got, err = d2.Decide(context.Background(), exchIn) // second decision: keep both
	if err != nil {
		t.Fatalf("Decide 2: %v", err)
	}
	if got.Choice != "no_swap" {
		t.Fatalf("exchange keep = %+v, want no_swap", got)
	}
	if names := toolNames(srv2.bodyAt(t, 0)); strings.Join(names, ",") != "keep_drawn_cards,swap_drawn_option" {
		t.Fatalf("exchange-finish tools = %v, want the two §6.4.2 shapes", names)
	}
}

func toolNames(req chatRequest) []string {
	var out []string
	for _, tool := range req.Tools {
		out = append(out, tool.Function.Name)
	}
	return out
}

// TestOpenAI_FeedbackRetryAppendsToolResult pins §6.4.4: an application-level
// illegal-candidate retry (in.Feedback set) appends a tool-result message that
// references the seat's own last tool call — no new world render.
func TestOpenAI_FeedbackRetryAppendsToolResult(t *testing.T) {
	in := openaiIn(app.KindTurnAction, 4)
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		if n == 1 {
			return 200, respJSON(t, toolCallMsg("call_A", "steal", `{"target":"s_1"}`)) // self-target: engine rejects
		}
		return 200, respJSON(t, toolCallMsg("call_B", "income", "{}"))
	})
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)

	cand, err := d.Decide(context.Background(), in)
	if err != nil {
		t.Fatalf("Decide 1: %v", err)
	}
	if cand.Action != "steal" || cand.TargetID != "s_1" {
		t.Fatalf("first candidate = %+v, want the (to-be-rejected) self-steal", cand)
	}

	in.Feedback = "you cannot target your own seat"
	cand, err = d.Decide(context.Background(), in)
	if err != nil {
		t.Fatalf("Decide 2: %v", err)
	}
	if cand.Action != "income" {
		t.Fatalf("retry candidate = %+v, want income", cand)
	}

	msgs := srv.bodyAt(t, 1).Messages
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool" {
		t.Fatalf("retry conversation roles = %v, want system,user,assistant,tool (no new world render)", roles)
	}
	tool := msgs[3]
	if tool.ToolCallID != "call_A" {
		t.Errorf("tool message references call id %q, want the seat's own last call call_A", tool.ToolCallID)
	}
	if !strings.Contains(tool.Content, "you cannot target your own seat") {
		t.Errorf("tool result content = %q, must carry the server's descriptive error", tool.Content)
	}
}

// TestOpenAI_EmptyResponsesThenKindSafeDefault pins the A4 re-prompt ladder:
// up to N=3 re-prompts after a no-tool-call response, then the kind-safe
// default — and the mandatory-coup extension of it (income is illegal at ≥10
// coins, so the turn default becomes a coup there).
func TestOpenAI_EmptyResponsesThenKindSafeDefault(t *testing.T) {
	cases := []struct {
		name string
		in   app.DecisionInput
		want app.Candidate
	}{
		{"turn_income", openaiIn(app.KindTurnAction, 3), app.Candidate{Kind: app.KindTurnAction, Action: "income"}},
		{"turn_coup_mandatory_at_10", openaiIn(app.KindTurnAction, 12), app.Candidate{Kind: app.KindTurnAction, Action: "coup", TargetID: "s_2"}},
	}
	winIn := openaiIn(app.KindWindowResponse, 3)
	winIn.Request = app.Request{WindowID: "w_9", EligibleActions: []string{"pass", "challenge"}}
	cases = append(cases, struct {
		name string
		in   app.DecisionInput
		want app.Candidate
	}{"window_pass", winIn, app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}})

	flipIn := openaiIn(app.KindSingleSeatChoice, 3)
	flipIn.Request = app.Request{ChoiceKind: "flip_choice", Options: []string{"face_down_0", "face_down_1"}}
	cases = append(cases, struct {
		name string
		in   app.DecisionInput
		want app.Candidate
	}{"flip_first_option", flipIn, app.Candidate{Kind: app.KindSingleSeatChoice, Choice: "face_down_0"}})

	exchIn := openaiIn(app.KindSingleSeatChoice, 3)
	exchIn.Request = app.Request{ChoiceKind: "exchange_finish"}
	cases = append(cases, struct {
		name string
		in   app.DecisionInput
		want app.Candidate
	}{"exchange_no_swap", exchIn, app.Candidate{Kind: app.KindSingleSeatChoice, Choice: "no_swap"}})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScriptedChatServer(t, alwaysRespond(t, func() map[string]any { return contentMsg("…thinking out loud, no tool…") }))
			d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)

			got, err := d.Decide(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if got != tc.want {
				t.Fatalf("default candidate = %+v, want %+v (A4 kind-safe default)", got, tc.want)
			}
			if n := srv.callCount(); n != 1+d.maxEmptyReprompts {
				t.Fatalf("requests = %d, want %d (initial + N=%d re-prompts)", n, 1+d.maxEmptyReprompts, d.maxEmptyReprompts)
			}
			// The STORED conversation must end with the model's last (empty)
			// assistant message — the exhausted ladder leaves no dangling
			// re-prompt behind.
			d.mu.Lock()
			stored := append([]chatMessage(nil), d.convs[convKey(tc.in.GameID, tc.in.SeatID)].msgs...)
			d.mu.Unlock()
			if stored[len(stored)-1].Role != "assistant" {
				t.Fatalf("stored conversation ends with a %q message — the exhausted ladder must not leave a dangling re-prompt", stored[len(stored)-1].Role)
			}
			reprompts := 0
			for _, m := range stored {
				if m.Role == "user" && strings.Contains(m.Content, "Call a tool now") {
					reprompts++
				}
			}
			if reprompts != d.maxEmptyReprompts {
				t.Fatalf("re-prompt messages = %d, want exactly N=%d", reprompts, d.maxEmptyReprompts)
			}
		})
	}
}

// TestOpenAI_5xxBackoffThenSuccess pins §6.4.6: one WARN per retry, full-jitter
// delay inside min(30s, 500ms×2^attempt), and success on the next attempt.
func TestOpenAI_5xxBackoffThenSuccess(t *testing.T) {
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		if n == 1 {
			return 503, []byte(`{"error":"model server is restarting"}`)
		}
		return 200, respJSON(t, toolCallMsg("call_1", "income", "{}"))
	})
	d, logBuf := newDriverWithLog(t, srv.ts.URL+"/v1", false)
	sleeps := &recordedSleeps{}
	sleeps.install(d)
	forceWorstCaseJitter(d)

	cand, err := d.Decide(context.Background(), openaiIn(app.KindTurnAction, 4))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if cand.Action != "income" {
		t.Fatalf("candidate = %+v, want income after the retry", cand)
	}
	sleeps.mu.Lock()
	defer sleeps.mu.Unlock()
	if len(sleeps.dels) != 1 {
		t.Fatalf("backoff sleeps = %v, want exactly one (attempt 0)", sleeps.dels)
	}
	if sleeps.dels[0] < 0 || sleeps.dels[0] > d.backoffBase { // full jitter over cap 500ms; worst case pinned to the cap
		t.Fatalf("first backoff delay = %v, want within [0, %v]", sleeps.dels[0], d.backoffBase)
	}
	if !strings.Contains(logBuf.String(), `msg=llm_request_retry`) {
		t.Error("missing the per-retry WARN line llm_request_retry")
	}
}

// TestOpenAI_BackoffGrowsExponentiallyWithCap pins the delay ladder with worst-
// case jitter: 500ms, 1s, 2s, 4s, 8s, 16s, then capped at 30s — and that the
// loop is INDEFINITELY long (escaped here only by ctx cancellation).
func TestOpenAI_BackoffGrowsExponentiallyWithCap(t *testing.T) {
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) { return 502, []byte("bad gateway") })
	ctx, cancel := context.WithCancel(context.Background())
	d, logBuf := newDriverWithLog(t, srv.ts.URL+"/v1", false)
	sleeps := &recordedSleeps{cancel: cancel, every: 8}
	sleeps.install(d)
	forceWorstCaseJitter(d)

	_, err := d.Decide(ctx, openaiIn(app.KindTurnAction, 4))
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("Decide error = %v, want context cancellation (the only escape from indefinite retries)", err)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	sleeps.mu.Lock()
	defer sleeps.mu.Unlock()
	if len(sleeps.dels) != len(want) {
		t.Fatalf("recorded delays = %v, want %v", sleeps.dels, want)
	}
	for i := range want {
		if sleeps.dels[i] != want[i] {
			t.Fatalf("delay[%d] = %v, want %v (min(30s, 500ms×2^attempt))", i, sleeps.dels[i], want[i])
		}
	}
	if got := strings.Count(logBuf.String(), "llm_request_retry"); got != len(want) {
		t.Errorf("llm_request_retry lines = %d, want one per retry (%d)", got, len(want))
	}
}

// TestOpenAI_FullJitterStaysBounded runs the REAL (random) jitter and asserts
// every drawn delay stays inside its cap — full jitter, never negative, never
// above min(cap, base×2^n).
func TestOpenAI_FullJitterStaysBounded(t *testing.T) {
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) { return 503, nil })
	ctx, cancel := context.WithCancel(context.Background())
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false) // default random jitter
	sleeps := &recordedSleeps{cancel: cancel, every: 24}
	sleeps.install(d)

	if _, err := d.Decide(ctx, openaiIn(app.KindTurnAction, 4)); !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("Decide error = %v, want context cancellation", err)
	}
	capFor := func(n int) time.Duration {
		c := d.backoffBase
		for i := 0; i < n && c < d.backoffCap; i++ {
			c *= 2
		}
		if c > d.backoffCap {
			c = d.backoffCap
		}
		return c
	}
	sleeps.mu.Lock()
	defer sleeps.mu.Unlock()
	for i, delay := range sleeps.dels {
		if delay < 0 || delay > capFor(i) {
			t.Fatalf("delay[%d] = %v outside [0, %v]", i, delay, capFor(i))
		}
	}
}

// TestOpenAI_ConnectionRefusedRetriesIndefinitelyUntilCancelled pins the
// "connection refused ⇒ retry" branch with a real dead port (no httptest server
// — the listener is bound then closed).
func TestOpenAI_ConnectionRefusedRetriesIndefinitelyUntilCancelled(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if cerr := l.Close(); cerr != nil {
		t.Fatalf("close listener: %v", cerr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d, _ := newDriverWithLog(t, "http://"+addr+"/v1", false)
	sleeps := &recordedSleeps{cancel: cancel, every: 3}
	sleeps.install(d)

	start := time.Now()
	_, err = d.Decide(ctx, openaiIn(app.KindTurnAction, 4))
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("Decide error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Decide took %s — the backoff loop must not sleep real intervals in tests", elapsed)
	}
	sleeps.mu.Lock()
	defer sleeps.mu.Unlock()
	if len(sleeps.dels) < 3 {
		t.Fatalf("recorded retries = %d, want ≥3 (indefinite retry until cancelled)", len(sleeps.dels))
	}
}

// TestOpenAI_MalformedBodiesRetry pins A5's malformed-body class: non-JSON
// bodies and empty choices are retried like transport failures.
func TestOpenAI_MalformedBodiesRetry(t *testing.T) {
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		switch n {
		case 1:
			return 200, []byte(`this is not json`)
		case 2:
			return 200, []byte(`{"choices":[]}`)
		default:
			return 200, respJSON(t, toolCallMsg("call_3", "income", "{}"))
		}
	})
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)
	sleeps := &recordedSleeps{}
	sleeps.install(d)

	cand, err := d.Decide(context.Background(), openaiIn(app.KindTurnAction, 4))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if cand.Action != "income" {
		t.Fatalf("candidate = %+v, want income after two malformed-body retries", cand)
	}
	if n := srv.callCount(); n != 3 {
		t.Fatalf("requests = %d, want 3 (two malformed + one good)", n)
	}
}

// TestOpenAI_NonRetryable4xxIsTerminal pins that config-class failures (other
// than 5xx/429) are NOT retried indefinitely: Decide returns a terminal error
// without a single backoff sleep.
func TestOpenAI_NonRetryable4xxIsTerminal(t *testing.T) {
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) { return 404, []byte(`{"error":"no such model"}`) })
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)
	sleeps := &recordedSleeps{}
	sleeps.install(d)

	_, err := d.Decide(context.Background(), openaiIn(app.KindTurnAction, 4))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("Decide error = %v, want a terminal HTTP 404", err)
	}
	sleeps.mu.Lock()
	defer sleeps.mu.Unlock()
	if len(sleeps.dels) != 0 {
		t.Fatalf("backoff sleeps = %d, want none for a non-retryable 4xx", len(sleeps.dels))
	}
}

// TestOpenAI_CancellationAbortsInFlightRequest pins §5.2.6/§6.4.7: context
// cancellation aborts the in-flight HTTP request immediately — Decide returns
// at once with the ctx error, makes NO backoff retry, and the mock observed
// the request land before being abandoned. (Asserting the server-side
// disconnect too is not reliable: on HTTP/1.1 a handler that neither reads nor
// writes observes r.Context() stay live after a client FIN, a stdlib behaviour
// of no interest to this driver's contract.)
func TestOpenAI_CancellationAbortsInFlightRequest(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{}) // closed once the test has finished with the request
	defer close(release)
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(10 * time.Second): // last-resort valve in case the test dies mid-flight
		}
	}))
	t.Cleanup(blocking.Close)

	ctx, cancel := context.WithCancel(context.Background())
	d, logBuf := newDriverWithLog(t, blocking.URL+"/v1", false)

	go func() {
		select {
		case <-arrived:
			time.Sleep(100 * time.Millisecond) // let the request hang server-side
			cancel()
		case <-time.After(3 * time.Second):
			t.Error("the mock never received the request")
			cancel()
		}
	}()

	start := time.Now()
	_, err := d.Decide(ctx, openaiIn(app.KindTurnAction, 4))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Decide took %s after cancellation — the in-flight request must abort immediately", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("Decide error = %v, want context cancellation (a cancelled flight is terminal, never a backoff)", err)
	}
	if strings.Contains(logBuf.String(), "llm_request_retry") {
		t.Error("a cancellation must return at once — no backoff retry may follow it")
	}
}

// TestOpenAI_ReasoningContentNotStoredLoggedOnlyWhenDebug pins A12/§6.4.5:
// reasoning_content is logged only under COUP_DEBUG=1, truncated to 4KB, and
// NEVER appended to the stored conversation (asserted on the wire of the NEXT
// request, which resends the whole conversation).
func TestOpenAI_ReasoningContentNotStoredLoggedOnlyWhenDebug(t *testing.T) {
	const marker = "ENDMARKER"                      // must not survive truncation nor storage
	reasoning := strings.Repeat("R", 5000) + marker // > 4KB

	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		if n == 1 {
			msg := toolCallMsg("call_1", "steal", `{"target":"s_2"}`)
			msg["reasoning_content"] = reasoning
			return 200, respJSON(t, msg)
		}
		return 200, respJSON(t, toolCallMsg("call_2", "income", "{}"))
	})
	d, logBuf := newDriverWithLog(t, srv.ts.URL+"/v1", true) // COUP_DEBUG=1

	if _, err := d.Decide(context.Background(), openaiIn(app.KindTurnAction, 4)); err != nil {
		t.Fatalf("Decide 1: %v", err)
	}
	if _, err := d.Decide(context.Background(), openaiIn(app.KindTurnAction, 4)); err != nil {
		t.Fatalf("Decide 2: %v", err)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "msg=llm_reasoning") {
		t.Error("debug mode must log llm_reasoning when reasoning_content is present")
	}
	if strings.Contains(logs, marker) {
		t.Error("logged reasoning_content exceeds the A12 4KB truncation (marker survived)")
	}

	second := srv.bodyAt(t, 1) // resends the stored conversation
	for i, m := range second.Messages {
		blob := m.Content + strings.Join(toolCallIDs(m), " ")
		if strings.Contains(blob, "RRR") || strings.Contains(blob, marker) {
			t.Fatalf("stored message %d (role %s) leaked reasoning_content into the conversation", i, m.Role)
		}
	}

	// Debug OFF: no llm_reasoning line at all.
	srv2 := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		msg := toolCallMsg("call_1", "income", "{}")
		if n == 1 {
			msg["reasoning_content"] = reasoning
		}
		return 200, respJSON(t, msg)
	})
	d2, logBuf2 := newDriverWithLog(t, srv2.ts.URL+"/v1", false)
	if _, err := d2.Decide(context.Background(), openaiIn(app.KindTurnAction, 4)); err != nil {
		t.Fatalf("Decide (debug off): %v", err)
	}
	if strings.Contains(logBuf2.String(), "llm_reasoning") {
		t.Error("reasoning_content must NOT be logged when COUP_DEBUG is off")
	}
}

func toolCallIDs(m chatMessage) []string {
	var out []string
	for _, tc := range m.ToolCalls {
		out = append(out, tc.ID+tc.Function.Name+tc.Function.Arguments)
	}
	return out
}

// TestOpenAI_UnparseableToolCallRepromptsThenSucceeds pins that a hallucinated
// tool name consumes re-prompt budget like an empty response, and the next
// usable call is honoured (the stored conversation shows the bad call followed
// by the re-prompt).
func TestOpenAI_UnparseableToolCallRepromptsThenSucceeds(t *testing.T) {
	srv := newScriptedChatServer(t, func(n int, req chatRequest) (int, []byte) {
		if n == 1 {
			return 200, respJSON(t, toolCallMsg("call_bad", "self_destruct", "{}"))
		}
		return 200, respJSON(t, toolCallMsg("call_ok", "income", "{}"))
	})
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)
	cand, err := d.Decide(context.Background(), openaiIn(app.KindTurnAction, 4))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if cand.Action != "income" {
		t.Fatalf("candidate = %+v, want income from the second call", cand)
	}
	msgs := srv.bodyAt(t, 1).Messages
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,user" {
		t.Fatalf("conversation after a hallucinated call = %v, want the bad assistant call followed by a re-prompt", roles)
	}

	// A model that only ever hallucinates falls through to the kind-safe default.
	srv2 := newScriptedChatServer(t, alwaysRespond(t, func() map[string]any { return toolCallMsg("call_bad", "self_destruct", "{}") }))
	d2, _ := newDriverWithLog(t, srv2.ts.URL+"/v1", false)
	got, err := d2.Decide(context.Background(), openaiIn(app.KindTurnAction, 4))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Action != "income" {
		t.Fatalf("exhausted-hallucination candidate = %+v, want the A4 default income", got)
	}
	if n := srv2.callCount(); n != 1+d2.maxEmptyReprompts {
		t.Fatalf("requests = %d, want %d (hallucinated calls burn the re-prompt budget)", n, 1+d2.maxEmptyReprompts)
	}
}

// TestOpenAI_ConversationPersistsAcrossDecisions pins §6.4.1: the per-seat
// conversation accumulates across the whole game — decision two's request
// carries decision one's world render and answer.
func TestOpenAI_ConversationPersistsAcrossDecisions(t *testing.T) {
	srv := newScriptedChatServer(t, alwaysRespond(t, func() map[string]any { return toolCallMsg("call_n", "income", "{}") }))
	d, _ := newDriverWithLog(t, srv.ts.URL+"/v1", false)

	in1 := openaiIn(app.KindTurnAction, 4)
	in2 := in1
	in2.World.TurnID = 8
	in2.World.History = []app.EventOut{{Seq: 100, GameID: "g_1", Type: "action_declared", Payload: map[string]any{"text": "Vito declared income"}}}

	if _, err := d.Decide(context.Background(), in1); err != nil {
		t.Fatalf("Decide 1: %v", err)
	}
	if _, err := d.Decide(context.Background(), in2); err != nil {
		t.Fatalf("Decide 2: %v", err)
	}

	second := srv.bodyAt(t, 1).Messages
	var roles []string
	for _, m := range second {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,user" {
		t.Fatalf("conversation on the second decision = %v, want system,user,assistant,user", roles)
	}
	last := second[len(second)-1].Content
	for _, want := range []string{"Turn 8", "Vito declared income"} {
		if !strings.Contains(last, want) {
			t.Errorf("second user render missing %q — the world render must update per decision", want)
		}
	}
	if got := d.ConversationMessages("g_1", "s_1"); got != 5 { // system + (user, assistant) × 2
		t.Fatalf("ConversationMessages = %d, want 5", got)
	}
	if got := d.ConversationMessages("g_other", "s_1"); got != 0 {
		t.Fatalf("ConversationMessages for another game = %d, want 0 (conversations are per-game)", got)
	}
}

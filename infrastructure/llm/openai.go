package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
)

// OpenAIDriver is the production MoveDriver of spec §6.4: an
// OpenAI-compatible chat-completions + tool-calling client with a persistent
// per-seat conversation for the ENTIRE game. Behaviour summary (the spec is
// normative; this comment is the map):
//
//   - [§6.4.1] One conversation per (game, seat) for the whole game: system
//     prompt once, then per decision one user message (compact world render of
//     exactly what this player can know + "what is being asked"), and per model
//     answer one assistant message. On application-level retries the server's
//     descriptive error is appended as a tool-result message (§6.4.4).
//   - [§6.4.2] Static, decision-specific toolsets: one function per action
//     shape on turns; pass/counterclaim-<char>/challenge for windows; flip or
//     exchange-finish shapes for choices.
//   - [§6.4.3] Response without a usable tool call ⇒ re-prompt up to
//     maxEmptyReprompts (N=3) times, then the kind-safe default (turn ⇒
//     income — coup when a coup is mandatory, window ⇒ pass, choice ⇒ engine
//     default), so no decision can ever permadeath or stall a game (A4).
//   - [§6.4.5] reasoning_content, when present in the response, is logged ONLY
//     under COUP_DEBUG=1 (the driver's Debug flag), truncated to 4 KB (A12),
//     and is NEVER appended to the stored conversation.
//   - [§6.4.6] Transport: per-request timeout 120 s (A5); connection errors,
//     request timeouts, HTTP 5xx/429 and malformed bodies are retried
//     INDEFINITELY with full-jitter exponential backoff min(30s, 500ms×2^n),
//     one WARN log per retry — a flapping model server must never kill a game.
//     Decide blocks until it produces some candidate or its context is
//     cancelled; any other HTTP status (e.g. 4xx config errors) is terminal.
//   - [§6.4.7/§5.2.6] Context cancellation from the application aborts the
//     in-flight request immediately (the request carries the decision context).
type OpenAIDriver struct {
	baseURL string // e.g. http://localhost:8080/v1 — production default is llama-server; tests use httptest
	model   string
	log     *slog.Logger
	debug   bool // COUP_DEBUG=1 (A12 reasoning logs)

	// Tunables — defaults are the spec values; tests shorten/replace them.
	requestTimeout    time.Duration // A5: per-LLM-request timeout
	backoffBase       time.Duration // 500ms, doubled per attempt up to backoffCap
	backoffCap        time.Duration // 30s full-jitter ceiling
	maxEmptyReprompts int           // A4: N = 3

	// Injectable seams (defaults set by NewOpenAIDriver): ctx-aware sleep for
	// the backoff loop, and the jitter draw over a cap. Tests record instead of
	// sleeping so no test ever waits out a real backoff interval.
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(max time.Duration) time.Duration

	httpClient *http.Client // Timeout 0: per-request deadline comes from the context

	mu    sync.Mutex
	convs map[string]*seatConv // gameID+"\x1f"+seatID → conversation (whole game, §6.4.1)
}

// seatConv is one seat's private head (§6.4.1/§8): the message list plus the
// id of its most recent assistant tool call, which a retry's tool-result
// message must reference. It never stores reasoning_content (A12).
type seatConv struct {
	msgs         []chatMessage
	lastToolCall string
}

// NewOpenAIDriver builds the production driver: base URL (trailing slashes
// trimmed) + model from configuration, slog logger (nil → default), and the
// COUP_DEBUG flag. All §6.4 tunables take their spec defaults.
func NewOpenAIDriver(baseURL, model string, log *slog.Logger, debug bool) *OpenAIDriver {
	if log == nil {
		log = slog.Default()
	}
	return &OpenAIDriver{
		baseURL:           strings.TrimRight(baseURL, "/"),
		model:             model,
		log:               log,
		debug:             debug,
		requestTimeout:    120 * time.Second, // A5
		backoffBase:       500 * time.Millisecond,
		backoffCap:        30 * time.Second,
		maxEmptyReprompts: 3, // A4 (N)
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		jitter: func(max time.Duration) time.Duration { // full jitter: uniform [0, max]
			if max <= 0 {
				return 0
			}
			return time.Duration(rand.Float64() * float64(max))
		},
		httpClient: &http.Client{}, // Timeout 0: the per-request deadline is the context (A5 120s + cancellation)
		convs:      map[string]*seatConv{},
	}
}

var _ app.MoveDriver = (*OpenAIDriver)(nil)
var _ app.LLMCapable = (*OpenAIDriver)(nil)
var _ app.ConversationReporter = (*OpenAIDriver)(nil)

// Name implements app.MoveDriver.
func (o *OpenAIDriver) Name() string { return "openai" }

// CanDriveLLM implements app.LLMCapable (§6.2).
func (o *OpenAIDriver) CanDriveLLM() bool { return true }

// ConversationMessages implements app.ConversationReporter for the §11.2 debug
// hook: length of this seat's stored conversation (0 when the seat has never
// decided yet or belongs to another game).
func (o *OpenAIDriver) ConversationMessages(gameID, seatID string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c, ok := o.convs[convKey(gameID, seatID)]; ok {
		return len(c.msgs)
	}
	return 0
}

func convKey(gameID, seatID string) string { return gameID + "\x1f" + seatID }

// Decide produces one candidate for the decision described by in, blocking
// until it does or ctx is cancelled (§6.4.6). It implements the agentic loop:
// request → (tool call ⇒ candidate) | (no usable tool call ⇒ re-prompt, up to
// N times, then kind-safe default). Application-level illegal-candidate
// retries arrive as further Decide calls with in.Feedback set (§6.4.4); they
// append a tool-result message instead of a fresh world render, so the model
// sees exactly its own failed call and the server's explanation.
func (o *OpenAIDriver) Decide(ctx context.Context, in app.DecisionInput) (app.Candidate, error) {
	tools := toolsFor(in)

	o.mu.Lock()
	conv := o.conversationLocked(in.GameID, in.SeatID)
	if in.Feedback == "" {
		conv.msgs = append(conv.msgs, chatMessage{Role: "user", Content: renderDecision(in)})
	} else if tcid := conv.lastToolCall; tcid != "" {
		// Retry of the seat's own last call: the server rejected that tool
		// result — feed the descriptive error back as a tool message (§6.4.4).
		conv.msgs = append(conv.msgs, chatMessage{Role: "tool", ToolCallID: tcid, Content: "Error: " + in.Feedback})
	} else {
		// Defensive fallback (no prior tool call to attach to): re-render with
		// the error inline so nothing is ever silently lost.
		conv.msgs = append(conv.msgs, chatMessage{Role: "user", Content: renderDecision(in) + "\n\nError from your previous call: " + in.Feedback})
	}
	o.mu.Unlock()

	empty := 0
	for {
		if cerr := ctx.Err(); cerr != nil {
			return app.Candidate{}, cerr
		}
		msg, err := o.requestWithBackoff(ctx, conv, tools)
		if err != nil {
			return app.Candidate{}, err // ctx cancelled or a non-retryable model-server error
		}
		o.logReasoning(in.GameID, in.SeatID, msg.ReasoningContent) // A12: log-only, never stored

		tcs := msg.ToolCalls
		storedCall := false
		if len(tcs) > 1 {
			o.log.Warn("llm_multiple_tool_calls", "game_id", in.GameID, "seat_id", in.SeatID, "count", len(tcs))
			tcs = tcs[:1] // one answer per decision; the first call is honoured
		}
		if len(tcs) == 1 {
			tc := tcs[0]
			o.mu.Lock()
			conv.msgs = append(conv.msgs, chatMessage{Role: "assistant", Content: msg.Content, ToolCalls: []chatToolCall{tc}})
			conv.lastToolCall = tc.ID
			o.mu.Unlock()
			storedCall = true
			cand, ok := parseToolCall(in.Kind, tc.Function.Name, tc.Function.Arguments)
			if !ok {
				// A hallucinated tool name / malformed arguments behaves like an
				// empty response (the call itself stays stored): the re-prompt
				// budget applies.
				o.log.Warn("llm_unparseable_tool_call", "game_id", in.GameID, "seat_id", in.SeatID, "kind", in.Kind.String(), "tool", tc.Function.Name)
			} else {
				return cand, nil
			}
		}

		// No usable tool call: store the assistant message (unless the unparseable
		// one is already stored), then re-prompt up to N times before returning
		// the kind-safe default (A4).
		if !storedCall {
			o.mu.Lock()
			conv.msgs = append(conv.msgs, chatMessage{Role: "assistant", Content: msg.Content})
			o.mu.Unlock()
		}
		o.mu.Lock()
		canReprompt := empty < o.maxEmptyReprompts
		if canReprompt {
			conv.msgs = append(conv.msgs, chatMessage{Role: "user", Content: "You must answer by calling exactly one of the provided tools. Call a tool now."})
			empty++
		}
		o.mu.Unlock()
		if !canReprompt {
			return kindSafeDefault(in), nil // A4: no decision may permadeath a game
		}
	}
}

// conversationLocked returns (creating on first use) the seat's whole-game
// conversation, seeded with the system prompt exactly once (§6.4.1). The
// caller holds o.mu.
func (o *OpenAIDriver) conversationLocked(gameID, seatID string) *seatConv {
	key := convKey(gameID, seatID)
	c, ok := o.convs[key]
	if !ok {
		c = &seatConv{msgs: []chatMessage{{Role: "system", Content: systemPrompt}}}
		o.convs[key] = c
	}
	return c
}

// requestWithBackoff marshals the current conversation + tools and performs
// the chat-completions request with the §6.4.6 transport policy: retry
// connection failures, request timeouts, 5xx/429 and malformed bodies
// indefinitely with full-jitter backoff (one WARN per retry); return ctx
// errors and other HTTP errors as terminal.
func (o *OpenAIDriver) requestWithBackoff(ctx context.Context, conv *seatConv, tools []chatTool) (*chatRespMsg, error) {
	o.mu.Lock()
	body, err := json.Marshal(chatRequest{Model: o.model, Messages: conv.msgs, Tools: tools})
	o.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	var attempt int
	for {
		msg, termErr, retryErr := o.requestOnce(ctx, body)
		if msg != nil {
			return msg, nil
		}
		if retryErr == nil {
			return nil, termErr // terminal: ctx cancelled or a non-retryable status/parse class
		}
		cap := o.backoffCap
		d := o.backoffBase
		for i := 0; i < attempt && d < cap; i++ {
			d *= 2
		}
		if d > cap {
			d = cap
		}
		delay := o.jitter(d)
		o.log.Warn("llm_request_retry", "model", o.model, "url", o.baseURL+"/chat/completions",
			"attempt", attempt+1, "next_delay_ms", delay.Milliseconds(), "err", retryableErrString(retryErr))
		if serr := o.sleep(ctx, delay); serr != nil { // ctx cancelled during the backoff
			return nil, serr
		}
		attempt++
	}
}

// requestOnce performs a single chat-completions round trip. On success msg is
// the first choice's assistant message; on failure exactly one of termErr
// (terminal: decision cancelled or non-retryable status/read/parse class) and
// retryable (connection failure, 120s request timeout, 5xx/429, malformed
// body — §6.4.6) is set.
func (o *OpenAIDriver) requestOnce(ctx context.Context, body []byte) (msg *chatRespMsg, termErr error, retryable error) {
	reqCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err), nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, derr := o.httpClient.Do(req)
	if derr != nil {
		if cerr := ctx.Err(); cerr != nil { // decision cancelled (window closed, seat gone…): abort NOW (§5.2.6)
			return nil, cerr, nil
		}
		return nil, nil, derr // connection refused / per-request 120s timeout: retryable
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		snap, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, nil, fmt.Errorf("model server error: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snap)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snap, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("model server rejected the request: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snap))), nil
	}

	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB guard against pathological bodies
	if rerr != nil {
		return nil, fmt.Errorf("read chat response: %w", rerr), nil
	}
	var parsed chatResponse
	if jerr := json.Unmarshal(raw, &parsed); jerr != nil || len(parsed.Choices) == 0 {
		return nil, nil, fmt.Errorf("malformed chat response (%d bytes)", len(raw)) // A5: malformed body ⇒ retry
	}
	m := &parsed.Choices[0].Message
	return m, nil, nil
}

// logReasoning captures the provider-specific reasoning_content field [A12]:
// it is logged ONLY when COUP_DEBUG=1 (the driver's Debug flag), truncated to
// 4 KB, and is never appended to the stored conversation.
const maxReasoningLogRunes = 4 * 1024 // A12

func (o *OpenAIDriver) logReasoning(gameID, seatID, reasoning string) {
	if reasoning == "" || !o.debug {
		return
	}
	o.log.Debug("llm_reasoning", "game_id", gameID, "seat_id", seatID, "reasoning_bytes", len(reasoning), "content", truncateRunes(reasoning, maxReasoningLogRunes))
}

// kindSafeDefault is the A4 fallback returned when the model produces no
// usable tool call after N re-prompts (and mirrored by the application after
// K illegal candidates): turn ⇒ income — except while a coup is MANDATORY at
// ≥10 coins, where income would fail engine validation and stall the game, so
// the default is a coup against the first active opponent instead; window ⇒
// pass; choice ⇒ the engine default (first flip option / no-swap). By
// construction every kind-safe default this function returns is accepted by
// the engine on a live structure.
func kindSafeDefault(in app.DecisionInput) app.Candidate {
	switch in.Kind {
	case app.KindTurnAction:
		if in.World.You.Coins >= domain.MandatoryCoupThreshold {
			for _, s := range in.World.Seats { // deterministic first active opponent
				if s.Active && s.ID != in.World.You.ID {
					return app.Candidate{Kind: app.KindTurnAction, Action: "coup", TargetID: s.ID}
				}
			}
		}
		return app.Candidate{Kind: app.KindTurnAction, Action: "income"}
	case app.KindWindowResponse:
		return app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}
	default: // KindSingleSeatChoice — the A4 engine defaults (§5.4)
		if in.Request.ChoiceKind == "flip_choice" && len(in.Request.Options) > 0 {
			return app.Candidate{Kind: app.KindSingleSeatChoice, Choice: in.Request.Options[0]}
		}
		if in.Request.ChoiceKind == "flip_choice" {
			return app.Candidate{Kind: app.KindSingleSeatChoice, Choice: "face_down_0"}
		}
		return app.Candidate{Kind: app.KindSingleSeatChoice, Choice: "no_swap"}
	}
}

// truncateRunes cuts s to at most n runes (never splitting a rune) and marks
// the cut with an ellipsis.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// retryableErrString renders a retryable error for the WARN log line.
func retryableErrString(e error) string {
	if e == nil {
		return "unknown"
	}
	return e.Error()
}

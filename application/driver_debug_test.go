package application_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	app "forgejo.lan/mrhollen/ai-coup/application"
	llm "forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestDebugDriverConversationMessages_RealData pins the §11.2 debug hook: with
// the REAL openai driver wired in, DebugGame reports the per-seat stored
// conversation length (system + user + assistant after one applied decision),
// not the slice's placeholder zero. The model server is an httptest mock on an
// ephemeral port (§14.3 discipline).
func TestDebugDriverConversationMessages_RealData(t *testing.T) {
	for seed := int64(1); seed < 200; seed++ { // find a seed whose LLM seat takes the first turn
		h := newHarnessWithDriver(t, seed, newMockModelDriver(t))
		res := h.createGame(t, []playerSpec{{"Ada", "human"}, {"Vito", "llm"}}, false, 0)
		if res.FirstTurnSeatID != res.Seats[1].ID {
			continue // the human holds the first turn: no driver dispatch yet
		}

		st := waitForDebugConversation(t, h.app, res.GameID)
		var vito *app.DebugSeat
		for i := range st.Seats {
			if st.Seats[i].ID == res.Seats[1].ID {
				vito = &st.Seats[i]
			}
		}
		if vito == nil {
			t.Fatalf("seat %s missing from debug state", res.Seats[1].ID)
		}
		if vito.Driver.Mode != "openai" {
			t.Errorf("driver mode = %q, want openai", vito.Driver.Mode)
		}
		if vito.Driver.ConversationMessages < 3 { // system + user world render + assistant tool call
			t.Errorf("conversation_messages = %d, want >= 3 (the real per-seat conversation)", vito.Driver.ConversationMessages)
		}

		var ada *app.DebugSeat
		for i := range st.Seats {
			if st.Seats[i].ID == res.Seats[0].ID {
				ada = &st.Seats[i]
			}
		}
		if ada == nil || ada.Driver.ConversationMessages != 0 {
			t.Errorf("human seat conversation_messages = %+v, want 0 (the driver does not drive human seats)", ada)
		}
		return
	}
	t.Fatal("no seed under 200 put the LLM seat on the first turn")
}

// waitForDebugConversation polls DebugGame until the LLM seat's stored
// conversation has grown past dispatch (system + user + assistant).
func waitForDebugConversation(t *testing.T, a *app.Application, gameID string) *app.DebugState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := a.DebugGame(gameID)
		if err != nil {
			t.Fatalf("DebugGame: %v", err)
		}
		for i := range st.Seats {
			if st.Seats[i].Type == "llm" && st.Seats[i].Driver.ConversationMessages >= 3 {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the openai driver never grew a stored conversation within 5s")
	return nil
}

// newMockModelDriver builds an openai driver pointed at an ephemeral httptest
// server that always answers with an income tool call (always legal while the
// acting seat holds fewer than 10 coins — which it does on its first turn).
func newMockModelDriver(t *testing.T) *llm.OpenAIDriver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var name string
		for _, tool := range req.Tools { // income exists on turn decisions; pass on windows; defaults cover the rest
			if tool.Function.Name == "income" {
				name = "income"
				break
			}
			if tool.Function.Name == "pass" {
				name = "pass"
			}
			if tool.Function.Name == "flip_card" {
				name = "flip_card"
			}
		}
		args := map[string]any{}
		if name == "flip_card" {
			args = map[string]any{"option": "face_down_0"}
		}
		argsJSON, _ := json.Marshal(args)
		resp := map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []any{map[string]any{
						"id":   "call_dbg_1",
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": string(argsJSON),
						},
					}},
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			io.WriteString(w, "")
		}
	}))
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return llm.NewOpenAIDriver(srv.URL+"/v1", "mock-model", log, false)
}

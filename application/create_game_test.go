package application_test

import (
	"context"
	"testing"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/infrastructure/llm"
)

// TestCreateGameTokenIssuance_A2: every human seat gets a 64-hex bearer token
// shown exactly once in the create response; LLM seats get none; tokens map
// back to (game, seat) for auth.
func TestCreateGameTokenIssuance_A2(t *testing.T) {
	h, res := createGameAt(t, []playerSpec{{name: "M", typ: "human"}, {name: "P", typ: "human"}, {name: "", typ: "llm"}}, 0, false, 0, nil, nil)

	for i, s := range res.Seats {
		if i == 2 {
			if s.Token != "" {
				t.Fatalf("LLM seat carries a token: %q", s.Token)
			}
			continue
		}
		if !is64Hex(s.Token) {
			t.Fatalf("human token %q is not 64 hex chars (A2)", s.Token)
		}
		gotGame, gotSeat, ok := h.app.ResolveToken(s.Token)
		if !ok || gotGame != res.GameID || gotSeat != s.ID {
			t.Fatalf("token does not resolve to its seat: %q → (%s,%s,%v)", s.Token, gotGame, gotSeat, ok)
		}
	}
	if _, _, ok := h.app.ResolveToken(res.Seats[0].Token + "0"); ok {
		t.Fatal("a modified token must not resolve")
	}
	if _, _, ok := h.app.ResolveToken(""); ok {
		t.Fatal("empty token must not resolve")
	}
}

func is64Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// TestCreateGameDefaults_A14_A8: blank names get the A14 defaults ("Vito" for
// LLM seats cycled in seat order, "Player N" for humans) and an omitted window
// object yields the A8 open-ended FCFS settings.
func TestCreateGameDefaults_A14_A8(t *testing.T) {
	_, res := createGameAt(t, []playerSpec{{name: "", typ: "human"}, {name: "", typ: "llm"}, {name: "", typ: "llm"}}, 0, false, 0, nil, nil)

	if res.Seats[0].Name != "Player 1" {
		t.Fatalf("blank human name = %q, want \"Player 1\"", res.Seats[0].Name)
	}
	if res.Seats[1].Name != "Vito" || res.Seats[2].Name != "Sonny" {
		t.Fatalf("blank LLM names = %q,%q want Vito,Sonny", res.Seats[1].Name, res.Seats[2].Name)
	}
	ws := res.WindowSettings
	if ws.RequireAllResponses || ws.ResponseTimeoutMs != 0 {
		t.Fatalf("omitted window must default to require_all=false timeout=none (A8): %+v", ws)
	}

	// Explicit settings are carried through.
	h2, res2 := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}, 0, true, 45000, nil, nil)
	ws2 := res2.WindowSettings
	if !ws2.RequireAllResponses || ws2.ResponseTimeoutMs != 45000 {
		t.Fatalf("explicit window settings not honored: %+v", ws2)
	}
	_ = h2
}

// TestCreateGameValidation: the §11.2 create-payload validation table.
func TestCreateGameValidation(t *testing.T) {
	h := newHarness(t, 1)
	cases := []struct {
		name string
		req  app.CreateGameRequest
	}{
		{"one seat", app.CreateGameRequest{Players: []app.PlayerSpec{{Name: "A", Type: "human"}}}},
		{"seven seats", app.CreateGameRequest{Players: seven()}},
		{"no humans", app.CreateGameRequest{Players: []app.PlayerSpec{{Name: "", Type: "llm"}, {Name: "", Type: "llm"}}}},
		{"bad seat type", app.CreateGameRequest{Players: []app.PlayerSpec{{Name: "A", Type: "robot"}, {Name: "B", Type: "human"}}}},
		{"name too long", app.CreateGameRequest{Players: []app.PlayerSpec{{Name: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", Type: "human"}, {Name: "B", Type: "llm"}}}},
		{"negative timeout", app.CreateGameRequest{Players: two(), ResponseTimeoutMs: -5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.app.CreateGame(context.Background(), tc.req)
			if got := errCode(t, err); got != "invalid_payload" {
				t.Fatalf("code = %q, want invalid_payload", got)
			}
		})
	}
	// A failed creation must leave no store entry behind.
	if n := len(h.games.All()); n != 0 {
		t.Fatalf("failed creations stored games: %d", n)
	}
}

func two() []app.PlayerSpec {
	return []app.PlayerSpec{{Name: "A", Type: "human"}, {Name: "", Type: "llm"}}
}
func seven() []app.PlayerSpec {
	out := make([]app.PlayerSpec, 0, 7)
	for i := 0; i < 6; i++ {
		out = append(out, app.PlayerSpec{Name: "", Type: "llm"})
	}
	return append(out, app.PlayerSpec{Name: "A", Type: "human"})
}

// TestCreateGameStartsDrivingFirstLLMTurn: when the random start seat is an
// LLM, its turn action is dispatched immediately at creation (no idle table).
func TestCreateGameStartsDrivingFirstLLMTurn(t *testing.T) {
	gate := make(chan struct{})
	h, res := createGameAt(t, []playerSpec{{name: "A", typ: "human"}, {name: "B", typ: "llm"}}, 1 /* B first */, false, 0, func(h *harness) {
		h.driver.Queue(seatIDn(2), llm.GatedStep(gate, app.Candidate{Kind: app.KindTurnAction, Action: "income"}))
	}, nil)

	h.waitFor(t, "B's turn action dispatched", func() bool { return len(h.driver.Calls()) == 1 })
	if in := h.driver.Calls()[0]; in.Kind != app.KindTurnAction || in.Request.WindowID != "" || len(in.Request.EligibleActions) != 0 {
		t.Fatalf("turn-action request not minimal: %+v", in.Request)
	}
	// The world view must contain exactly what B may know (§8).
	if in := h.driver.Calls()[0]; in.World.TurnID != 1 || len(in.World.Seats) != 2 || len(in.World.YourCards) != 2 {
		t.Fatalf("world view at creation wrong: %+v", in.World)
	}
	close(gate)
	h.waitFor(t, "income applied", func() bool { return len(findEvents(h.stream(t, res.GameID, res.Seats[0].ID), "action_declared")) == 1 })
}

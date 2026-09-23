package llm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	app "forgejo.lan/mrhollen/ai-coup/application"
	"forgejo.lan/mrhollen/ai-coup/domain"
)

// --- OpenAI-compatible chat-completions wire types (spec §6.4) --------------
//
// Only the fields this driver sends or reads are modeled; unknown response
// fields (including provider-specific ones) are ignored by json.Unmarshal.

// chatMessage is one conversation message in the request grammar. Role is
// "system" | "user" | "assistant" | "tool"; Content is omitted when empty
// (assistant messages that carry only a tool call), ToolCalls on assistant
// messages, ToolCallID on tool-result messages referencing that call.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// chatToolCall is one function call: an id (correlation token for the later
// tool-result message), type "function", and the function name + arguments.
// Arguments travel JSON-encoded inside a STRING per the OpenAI grammar.
type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatTool is one tool definition in the static per-decision toolset (§6.4.2).
type chatTool struct {
	Type     string `json:"type"` // always "function"
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters,omitempty"` // JSON-Schema object
	} `json:"function"`
}

// chatRequest is the POST /chat/completions body.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
}

// chatRespMsg is the assistant/tool message of a completion response.
// ReasoningContent is the provider-specific chain-of-thought field [A12]: it
// is logged only under COUP_DEBUG=1 and never stored in the conversation.
type chatRespMsg struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ToolCalls        []chatToolCall `json:"tool_calls"`
	ReasoningContent string         `json:"reasoning_content,omitempty"` // [A12]
}

// chatResponse is the part of the completion response this driver consumes:
// the finish reason and message of the first choice.
type chatResponse struct {
	Choices []struct {
		FinishReason string      `json:"finish_reason"`
		Message      chatRespMsg `json:"message"`
	} `json:"choices"`
}

// objectSchema builds a minimal JSON-Schema object type for tool parameters.
func objectSchema(props map[string]any, required ...string) any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func stringProp(desc string) any { return map[string]any{"type": "string", "description": desc} }

// --- The decision-specific toolsets (spec §6.4.2) ---------------------------
//
// One function per ACTION SHAPE — presenting the grammar is not revealing
// legality; the server enforces it and feeds rejections back (§6.4.4). Claimed
// characters are implied by the action shape rather than carried as arguments
// (the spec's turn tool list has no claim parameter): tax claims duke, steal
// claims captain, assassinate claims assassin, exchange claims ambassador.

// turnTools is the static turn-action toolset: one function per action kind.
func turnTools() []chatTool {
	mk := func(name, desc string, params any) chatTool {
		var t chatTool
		t.Type = "function"
		t.Function.Name = name
		t.Function.Description = desc
		t.Function.Parameters = params
		return t
	}
	target := objectSchema(map[string]any{"target": stringProp("seat id of the target player")}, "target")
	return []chatTool{
		mk("income", "Earn 1 coin. No claim, no window.", objectSchema(nil)),
		mk("foreign_aid", "Earn 2 coins, claiming to be the Duke (a live duke counterclaim can block it).", objectSchema(nil)),
		mk("tax", "Collect 3 coins, claiming to be the Duke (can be challenged).", objectSchema(nil)),
		mk("coup", "Pay 7 coins; the target loses one influence. Mandatory while you hold ten or more coins.", target),
		mk("steal", "Steal up to 2 of the target's coins, claiming Captain or Ambassador.", target),
		mk("assassinate", "Pay 3 coins (sunk); the target loses one influence, claiming Assassin. Can be countered by a Contessa or challenged.", target),
		mk("exchange", "Claim Ambassador: draw two cards face-down, then keep them all or swap one for one of yours.", objectSchema(nil)),
	}
}

// windowTools builds the response toolset from this seat's eligible-action
// grammar (public table information — never advice): pass(), challenge(), and
// counterclaim_<character>() per allowed counterclaim.
func windowTools(eligible []string) []chatTool {
	var out []chatTool
	for _, tok := range eligible {
		switch {
		case tok == "pass":
			out = append(out, fnTool("pass", "Say nothing; let the claim stand."))
		case tok == "challenge":
			out = append(out, fnTool("challenge", "Force the claimant to reveal a card matching the claim (they fail if they cannot)."))
		case strings.HasPrefix(tok, "counterclaim_"):
			char := strings.TrimPrefix(tok, "counterclaim_")
			out = append(out, fnTool(tok, "Counterclaim that you are the "+char+" and would block this attack."))
		default: // unknown grammar token: expose it verbatim so the model can use it
			out = append(out, fnTool(tok, tok))
		}
	}
	return out
}

// choiceTools builds the single-seat decision toolset: flip_card(option) with
// the positional option identifiers, or keep_drawn_cards()/swap_drawn_option()
// for an exchange finish (§6.4.2).
func choiceTools(req app.Request) []chatTool {
	switch req.ChoiceKind {
	case "flip_choice":
		var t chatTool
		t.Type = "function"
		t.Function.Name = "flip_card"
		t.Function.Description = "Choose which of your face-down cards flips face-up (positional option identifier)."
		t.Function.Parameters = objectSchema(map[string]any{"option": stringProp("one of the offered option identifiers")}, "option")
		return []chatTool{t}
	default: // exchange_finish
		var keep, swap chatTool
		keep.Type = "function"
		keep.Function.Name = "keep_drawn_cards"
		keep.Function.Description = "Keep both drawn cards; they return to the Court Deck and your hand is unchanged."
		keep.Function.Parameters = objectSchema(nil)
		swap.Type = "function"
		swap.Function.Name = "swap_drawn_option"
		swap.Function.Description = "Keep one drawn card in hand and return one of your face-down cards to the Court Deck."
		swap.Function.Parameters = objectSchema(map[string]any{
			"drawn_index": map[string]any{"type": "integer", "description": "which drawn card you keep: 0 = left, 1 = right"},
			"hand_option": stringProp("which face-down hand card returns (positional identifier)"),
		}, "drawn_index", "hand_option")
		return []chatTool{keep, swap}
	}
}

func fnTool(name, desc string) chatTool {
	var t chatTool
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = desc
	t.Function.Parameters = objectSchema(nil)
	return t
}

// toolsFor assembles the decision-specific toolset (§6.4.2).
func toolsFor(in app.DecisionInput) []chatTool {
	switch in.Kind {
	case app.KindTurnAction:
		return turnTools()
	case app.KindWindowResponse:
		return windowTools(in.Request.EligibleActions)
	default: // KindSingleSeatChoice
		return choiceTools(in.Request)
	}
}

// --- Tool call → candidate (the inverse of the toolsets) --------------------

// parseToolCall converts one model tool call into a Candidate. ok=false means
// the name is not part of this decision's grammar or its arguments are
// malformed; the caller treats that like an empty response (re-prompt budget).
func parseToolCall(kind app.DecisionKind, name, argsJSON string) (app.Candidate, bool) {
	var args map[string]any
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return app.Candidate{}, false
		}
	}
	argStr := func(k string) (string, bool) {
		s, _ := args[k].(string)
		return s, s != ""
	}
	switch kind {
	case app.KindTurnAction:
		c := app.Candidate{Kind: app.KindTurnAction}
		switch name {
		case "income", "foreign_aid":
			c.Action = name
		case "tax":
			c.Action, c.Character = "tax", "duke"
		case "exchange":
			c.Action, c.Character = "exchange", "ambassador"
		case "coup", "steal", "assassinate":
			t, ok := argStr("target")
			if !ok {
				return app.Candidate{}, false
			}
			c.Action = name
			c.TargetID = t
			if name == "steal" {
				c.Character = "captain"
			}
			if name == "assassinate" {
				c.Character = "assassin"
			}
		default:
			return app.Candidate{}, false
		}
		return c, true
	case app.KindWindowResponse:
		switch {
		case name == "pass":
			return app.Candidate{Kind: app.KindWindowResponse, Response: "pass"}, true
		case name == "challenge":
			return app.Candidate{Kind: app.KindWindowResponse, Response: "challenge"}, true
		case strings.HasPrefix(name, "counterclaim_"):
			charName := strings.TrimPrefix(name, "counterclaim_")
			if _, ok := domain.CharacterFromName(charName); !ok {
				return app.Candidate{}, false
			}
			return app.Candidate{Kind: app.KindWindowResponse, Response: "counterclaim", Character: charName}, true
		default:
			return app.Candidate{}, false
		}
	default: // KindSingleSeatChoice
		switch name {
		case "flip_card":
			opt, ok := argStr("option")
			if !ok {
				return app.Candidate{}, false
			}
			return app.Candidate{Kind: app.KindSingleSeatChoice, Choice: opt}, true
		case "keep_drawn_cards":
			return app.Candidate{Kind: app.KindSingleSeatChoice, Choice: "no_swap"}, true
		case "swap_drawn_option":
			idx, ok := args["drawn_index"].(float64) // JSON numbers decode as float64
			if !ok || idx != 0 && idx != 1 {
				return app.Candidate{}, false
			}
			hand, ok := argStr("hand_option")
			if !ok || hand != "face_down_0" && hand != "face_down_1" {
				return app.Candidate{}, false
			}
			return app.Candidate{Kind: app.KindSingleSeatChoice, Choice: fmt.Sprintf("swap_drawn_%d_%s", int(idx), hand)}, true
		default:
			return app.Candidate{}, false
		}
	}
}

// --- Compact world render (spec §6.3/§8) ------------------------------------
//
// Exactly what this seat may know, rendered into one block: own hand and coins,
// every seat's public info, Court Deck size, and the seat-visible event
// history (the application already filtered it per audience). No menu of legal
// moves, no hints — state plus "what is being asked".

const systemPrompt = `You play Coup at a table with the other players. You control exactly one seat and you see only what a player sitting at that seat would see: your own face-down cards, every seat's coins and revealed (face-up) cards, the Court Deck size, and the table's public event history — never another player's hidden cards or private information.
On each of your turns the table asks exactly one thing — declare an action, respond to a claim window, or make a card choice — and you answer by calling EXACTLY ONE of the provided tools. Do not narrate, do not ask questions, do not output text instead of a tool call.
Bluffing is allowed: any player may claim any character on the listed actions; the server validates every call and, when one is rejected, tells you why in a follow-up message so you can try again.`

// renderDecision builds the user-message body for one decision: the compact
// world state followed by an explicit statement of what is being asked.
func renderDecision(in app.DecisionInput) string {
	var b strings.Builder
	b.WriteString(renderWorld(in.World))
	b.WriteString("\n\nWhat you are being asked right now:\n")
	switch in.Kind {
	case app.KindTurnAction:
		fmt.Fprintf(&b, "It is your turn (turn %d). Declare exactly one action using the provided tools.\n", in.World.TurnID)
	case app.KindWindowResponse:
		if in.Request.TriggerSummary != "" {
			b.WriteString(in.Request.TriggerSummary + "\n")
		}
		b.WriteString("A response window is open and you are eligible to respond. Respond using exactly one of the provided tools (pass if you say nothing).")
	default: // KindSingleSeatChoice
		switch in.Request.ChoiceKind {
		case "flip_choice":
			b.WriteString("You must choose which of your face-down cards flips face-up. Use the provided tool with one of the offered option identifiers.")
		default: // exchange_finish
			if in.Request.TriggerSummary != "" {
				b.WriteString(in.Request.TriggerSummary + "\n")
			}
			b.WriteString("Finish your exchange using the provided tools (keep both drawn cards, or swap one drawn card for one of yours).")
		}
	}
	return b.String()
}

// renderWorld renders exactly the WorldView fields a driver may know (§8).
func renderWorld(w app.WorldView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Turn %d | Court deck: %d cards\n", w.TurnID, w.DeckSize)
	b.WriteString("You: ")
	writeSeatLine(&b, w.You, w.YourCards, true)
	for _, s := range w.Seats {
		if s.ID == w.You.ID {
			continue
		}
		b.WriteString("- ")
		writeSeatLine(&b, s, nil, false)
	}
	if len(w.History) > 0 {
		fmt.Fprintf(&b, "Table history (last %d visible to you):\n", len(w.History))
		for _, e := range w.History {
			b.WriteString(renderEvent(e))
		}
	}
	return b.String()
}

func writeSeatLine(b *strings.Builder, s app.SeatView, faceDown []domain.Character, isYou bool) {
	fmt.Fprintf(b, "%s (%s): %d coin(s)", s.Name, s.Type, s.Coins)
	if !s.Active {
		b.WriteString(" [eliminated]")
	}
	if len(s.FaceUp) > 0 {
		b.WriteString(", face-up: " + joinChars(s.FaceUp))
	}
	if isYou && len(faceDown) > 0 {
		b.WriteString(", your face-down hand: " + joinChars(faceDown))
	} else if isYou {
		b.WriteString(" (no cards left in hand)")
	}
	b.WriteString("\n")
}

func joinChars(cs []domain.Character) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.String()
	}
	return strings.Join(parts, ", ")
}

// renderEvent renders one materialized event compactly: "#seq type k=v …" with
// the payload's scalar fields sorted by key (deterministic prompt). The event
// stream is already privacy-filtered for this seat by the application.
func renderEvent(e app.EventOut) string {
	keys := make([]string, 0, len(e.Payload))
	for k := range e.Payload {
		switch e.Payload[k].(type) {
		case string, int, bool:
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, e.Payload[k]))
	}
	line := fmt.Sprintf("  #%d %s", e.Seq, e.Type)
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	return line + "\n"
}

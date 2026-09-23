// Package application implements the Coup use cases and game orchestration
// (spec §2): per-game serialization, LLM driver fan-out with cancellation,
// timeout scheduling, long-poll event delivery, snapshot filtering, forfeit
// votes, and structured observability (§13). It depends only on domain and
// its own port interfaces; concrete adapters live in infrastructure and are
// bound in the composition root.
package application

import (
	"context"
	"time"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// IDGen is the identifier port reused verbatim from domain: kind-prefixed
// opaque ids (§13, A3). The composition root binds one implementation for both
// layers.
type IDGen = domain.IDGen

// DecisionInput is everything a driver receives for one decision: exactly the
// world view the seat may know (§8), the structured request, and — non-empty
// on retry — the descriptive table-voice error from a rejected candidate
// (spec §2.2, §6.4.4).
type DecisionInput struct {
	GameID   string
	SeatID   string
	Kind     DecisionKind
	World    WorldView // exactly what this seat may know (§8), rendered-free
	Request  Request   // structured "what is being asked"
	Feedback string    // non-empty on retry: descriptive error from a rejected candidate
}

// MoveDriver produces one candidate decision for a seat. It receives exactly
// the world view the seat may know (§8) plus a description of what is being
// asked. It NEVER receives a menu of legal moves or hints; legality is enforced
// by domain code in the application layer, and rejection errors are fed back
// via DecisionInput.Feedback (spec §2.2, §6.4.4).
type MoveDriver interface {
	Name() string // "fake" | "heuristic" | "openai"
	Decide(ctx context.Context, in DecisionInput) (Candidate, error)
}

// DecisionKind names what is being asked of a seat (spec §2.2).
type DecisionKind int

const (
	// KindTurnAction asks the seat holding the turn to declare an action.
	KindTurnAction DecisionKind = iota
	// KindWindowResponse asks an eligible seat to respond to the open window.
	KindWindowResponse
	// KindSingleSeatChoice asks one addressed seat for a flip choice or
	// exchange finish.
	KindSingleSeatChoice
)

// String returns the wire name of the kind ("turn_action", "window_response",
// "single_seat_choice").
func (k DecisionKind) String() string {
	switch k {
	case KindTurnAction:
		return "turn_action"
	case KindWindowResponse:
		return "window_response"
	default:
		return "single_seat_choice"
	}
}

// SeatView is one seat's table-visible state (spec §8 item 2).
type SeatView struct {
	ID     string
	Name   string
	Type   string // "human" | "llm"
	Coins  int
	FaceUp []domain.Character
	Active bool
}

// WorldView is the exact, render-free world a driver's seat may know (spec
// §2.2/§6.3): own hand and coins, every seat's public info, Court Deck size,
// and the seat-visible event history. It never contains another seat's
// face-down cards or private events.
type WorldView struct {
	TurnID    int
	DeckSize  int
	You       SeatView           // the driver's own seat (public fields)
	YourCards []domain.Character // your own face-down hand, positional per §5.4
	Seats     []SeatView         // all seats in seat order, including You
	History   []EventOut         // events visible to this seat, ascending seq
}

// Request is the structured "what is being asked" (spec §2.2). Zero value for
// turn actions; WindowID/TriggerSummary/EligibleActions for window responses;
// ChoiceKind/Options for single-seat choices. EligibleActions is the response
// grammar of the window mechanics — public table information, never advice.
type Request struct {
	// turn action: zero value.
	WindowID       string
	TriggerSummary string // public facts only, e.g. "Vito claims Assassin against you"
	// EligibleActions is the response vocabulary for this seat in the window:
	//   ["pass","counterclaim_contessa","challenge"] etc.
	EligibleActions []string

	// single-seat choice: ChoiceKind ("flip_choice" | "exchange_finish") +
	// option identifiers only.
	ChoiceKind string
	Options    []string // positional, never names cards
}

// Candidate is one proposed decision (spec §2.2). Field usage depends on Kind.
type Candidate struct {
	Kind      DecisionKind
	Action    string // "income"|"foreign_aid"|"coup"|"tax"|"steal"|"assassinate"|"exchange"
	TargetID  string // where applicable
	Response  string // "pass"|"counterclaim"|"challenge"
	Character string // claimed character for counterclaims / claims
	// Choice: for single-seat choices, the option identifier. Flip choice:
	// "face_down_0" | "face_down_1". Exchange finish: "no_swap" or
	// "swap_drawn_<drawn_index>_face_down_<hand_index>" (see Request.Options
	// for exchange finish — one identifier per outcome).
	Choice string
}

// EventOut is a materialized wire event (spec §10): the domain Event plus the
// RFC3339 timestamp assigned by the application layer from its Clock when the
// batch is appended. JSON encoding flattens Payload into the event object,
// exactly as stored (§10.2).
type EventOut struct {
	Seq      int64
	GameID   string
	TS       string // RFC3339, from the Clock port at append time (§10)
	Type     string
	Audience domain.Audience
	Payload  map[string]any
}

// EventStore is append-only, per-game, with audience-tagged events (spec §2.2,
// §8). Privacy filtering happens here: a seat's stream is public events plus
// that seat's own private events, in one monotonic sequence.
type EventStore interface {
	Append(ctx context.Context, gameID string, evts []domain.Event) (firstSeq int64, err error)
	// Range returns events with seq > after whose audience is public or the
	// viewer's seat.
	Range(ctx context.Context, gameID string, after int64, viewerSeatID string) ([]EventOut, error)
	LastSeq(ctx context.Context, gameID string) (int64, error)
	// WaitNew blocks until some event with seq > after is appended for the
	// game, or ctx is done.
	WaitNew(ctx context.Context, gameID string, after int64)
}

// GameStore holds the live game aggregates (spec §2.2). In v1: a memory map
// guarded by a mutex; All() backs the future debug endpoint.
type GameStore interface {
	Put(game *domain.Game)
	Get(gameID string) (*domain.Game, bool)
	All() []*domain.Game
}

// Clock provides wall time to the application layer (spec §2.2). The domain
// stores plain durations; deadlines are computed here. Test fakes drive After
// manually.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

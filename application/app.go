package application

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// DefaultPollHold is the long-poll hold of spec A1: 25 s, clamped to [1s,30s].
const (
	DefaultPollHold = 25 * time.Second
	MinPollHold     = 1 * time.Second
	MaxPollHold     = 30 * time.Second

	// heartbeatInterval is the still-waiting lifecycle log cadence (A11).
	heartbeatInterval = 30 * time.Second

	// maxDriverRetries is K, the application-level retry budget after illegal
	// LLM candidates before the kind-safe default applies (A4).
	maxDriverRetries = 3
)

// ClampPollHold clamps a configured long-poll hold to [1s,30s] (A1); non-
// positive values fall back to the default.
func ClampPollHold(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultPollHold
	}
	if d < MinPollHold {
		return MinPollHold
	}
	if d > MaxPollHold {
		return MaxPollHold
	}
	return d
}

// Dependencies are the ports bound by the composition root (spec §2). All are
// required except where noted: a nil Log falls back to slog.Default(), a nil
// Rng is seeded from the wall clock, a nil TokenSource draws 32 crypto-random
// bytes.
type Dependencies struct {
	Events  EventStore
	Games   GameStore
	Clock   Clock
	IDs     IDGen
	Driver  MoveDriver
	Log     *slog.Logger        // optional; see above
	Rng     *rand.Rand          // optional; deck shuffle + random start seat (rules.md Ruling 6)
	TokenSource func() (string, error) // optional; human bearer tokens (A2)
}

// Application is the container of all use cases. It owns one handle per game:
// a sync.Mutex that serializes every engine call for that game (§9 — the
// domain itself is lock-free), plus the bookkeeping for LLM fan-out, timeout
// timers and lifecycle logging.
type Application struct {
	deps     Dependencies
	log      *slog.Logger
	driver   MoveDriver
	mu       sync.Mutex // guards handles
	handles  map[string]*gameHandle
	closed   bool
	closeErr error
}

// NewApplication wires the ports into a ready-to-serve application. It does
// not start any listeners; use cases are invoked directly (by handlers or
// tests).
func NewApplication(d Dependencies) *Application {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Rng == nil {
		d.Rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if d.TokenSource == nil {
		d.TokenSource = randomToken
	}
	return &Application{deps: d, log: d.Log, driver: d.Driver, handles: map[string]*gameHandle{}}
}

// Logger exposes the application's logger (used by tests and composition).
func (a *Application) Logger() *slog.Logger { return a.log }

// randomToken is the default A2/A3 token source: 64 hex chars from 32
// crypto-random bytes.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Close cancels every in-flight LLM decision and stops all timers across all
// games (spec §9 cancellation point "server shutdown"). It is safe to call
// once from the composition root at shutdown.
func (a *Application) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	hs := make([]*gameHandle, 0, len(a.handles))
	for _, h := range a.handles {
		hs = append(hs, h)
	}
	a.mu.Unlock()
	for _, h := range hs {
		h.cancelAll("shutdown")
	}
	return nil
}

// ResolveToken maps a human bearer token to the (game, seat) it was issued
// for. Support query for the HTTP auth middleware of the next slice: tokens
// are issued exactly once at creation and never revoked (A2).
func (a *Application) ResolveToken(token string) (gameID, seatID string, ok bool) {
	if token == "" {
		return "", "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for gid, h := range a.handles {
		if sid, found := h.tokens[token]; found {
			return gid, sid, true
		}
	}
	return "", "", false
}

// gameHandle is the per-game orchestration state. mu serializes ALL access to
// the domain aggregate for this game (spec §2.2/§9); nothing but engine calls
// and bookkeeping happens under it, and no blocked driver call ever runs while
// holding it.
type gameHandle struct {
	app    *Application
	id     string
	g      *domain.Game // same pointer the GameStore holds
	mu     sync.Mutex   // per-game serialization lock

	tokens   map[string]string // seatID → bearer token (humans only, A2)
	inFlight map[string]*flight // one in-flight LLM decision per seat (§9)
	done     map[string]map[string]bool // structureID → seats already dispatched
	meta     *structureMeta             // the currently open window/decision
}

type flight struct {
	seatID    string
	structID  string // window id, decision id or "turn:<n>"
	kind      DecisionKind
	ctx       context.Context
	cancel    context.CancelFunc
	startedAt time.Time
	cancelled bool // guarded by h.mu; llm_cancelled is logged once per flight
}

// structureMeta tracks the single open window or pending decision for timer,
// heartbeat and trigger-summary purposes.
type structureMeta struct {
	id        string // window id or decision id
	kind      string // "window" | "decision"
	openedAt  time.Time
	timeoutMs int
	trigger   string // public summary of what opened the structure
	stop      chan struct{} // closed when the structure no longer exists
}

func (a *Application) game(gameID string) (*gameHandle, error) {
	a.mu.Lock()
	h := a.handles[gameID]
	a.mu.Unlock()
	if h == nil {
		return nil, NewAppError(ErrNotFound, "no such game", 404)
	}
	return h, nil
}

// register stores a freshly created game handle.
func (a *Application) register(h *gameHandle) {
	a.mu.Lock()
	a.handles[h.id] = h
	a.mu.Unlock()
}

// run executes one engine interaction for the game: fn mutates through the
// aggregate under the game lock, and on success the returned batch is logged,
// appended to the event store and fanned out to pending LLM decisions. On
// engine error the game is unchanged (domain guarantee) and no events are
// appended; the error is mapped to wire form.
func (h *gameHandle) run(fn func(*domain.Game) ([]domain.Event, error)) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	evs, err := fn(h.g)
	if err != nil {
		return 0, FromDomain(err)
	}
	if len(evs) == 0 {
		return 0, nil
	}
	firstSeq, aerr := h.commitLocked(h.g, evs)
	if aerr != nil {
		return 0, aerr
	}
	return firstSeq, nil
}

// recordRejection appends a private rejection event for the offending seat
// (A9): rejected submissions get their own seq so clients can reconcile them
// from history alone. The wire code stored matches the code returned to the
// client by the use case.
func (h *gameHandle) recordRejection(seatID string, ae *AppError) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := h.g.MakeRejectionEvent(seatID, ae.Code, ae.Message)
	if _, err := h.commitLocked(h.g, []domain.Event{ev}); err != nil {
		h.app.log.Error("rejection_event_append_failed", "game_id", h.id, "seat_id", seatID, "err", err)
	}
	h.app.log.Warn("rejection", append(logGameContext(h.id, h.g.TurnID, ev.Seq), "seat_id", seatID, "code", ae.Code, "message", ae.Message)...)
}

// commitLocked appends a batch and runs the post-batch orchestration. The
// caller holds h.mu. Appends use a fresh context: an in-flight mutation must
// never be abandoned because a client connection went away.
func (h *gameHandle) commitLocked(g *domain.Game, evs []domain.Event) (int64, error) {
	firstSeq, err := h.app.deps.Events.Append(context.Background(), h.id, evs)
	if err != nil {
		return 0, FromDomain(err)
	}
	h.afterBatch(g, evs)
	return firstSeq, nil
}

// logGameContext builds the consistent §13 context fields.
func logGameContext(gameID string, turnID int, eventSeq int64) []any {
	return []any{"game_id", gameID, "turn_id", turnID, "event_seq", eventSeq}
}

// humanSeat validates that seatID names an existing human seat of the game.
// The caller holds h.mu.
func (h *gameHandle) humanSeat(seatID string) (*domain.Seat, error) {
	for i := range h.g.Seats {
		s := &h.g.Seats[i]
		if s.ID == seatID {
			if s.Type != domain.SeatHuman {
				return nil, NewAppError(ErrUnauthorized, "server-driven seats have no client stream", 401)
			}
			return s, nil
		}
	}
	return nil, NewAppError(ErrNotFound, "no such seat", 404)
}

# ai-coup — Technical Specification

Status: **normative, implementation-ready.** This spec covers the server-side system for playing
**Coup (2012)** with human and server-managed LLM players over a plain HTTP API. Game rules are
authoritative in [`../rules.md`](../rules.md); where this spec re-states them, `rules.md` wins on any
conflict. Every `[ASSUMPTION]` is collected in §16 (Assumptions register).

Non-goals for this document: UI implementation (the UI will be built strictly from the event
catalogue in §10 and the API in §11), persistence beyond in-memory, multi-node deployment.

---

## 1. Stack & runtime environment

- **Language/framework:** Go + `github.com/go-chi/chi` router over stdlib `net/http`. Exactly one
  third-party dependency (chi). No ORM, no gRPC, no SSE.
- **Transport to human clients: long polling** (not SSE). A poll blocks for a bounded hold
  (≤ ~30 s, server-configurable) and returns whatever new events exist; the client immediately
  re-polls. Long polling is idempotent (keyed by sequence number) and works through any proxy.
- **Default listen port: `:8090`** (`COUP_PORT`, overridable by env). **Port 8080 is forbidden to
  this system as a *bind* target and to tests as a dial target** — a `llama-server` runs there in
  the dev environment and must never be disturbed (no bind, no test traffic, no killing of its
  process). The *outbound* default LLM base URL `http://localhost:8080/v1` is different: it is an
  HTTP **client** connection from the `openai` driver to the model server in dev, and is exactly
  what `COUP_LLM_BASE_URL` points at. In any non-dev environment set it elsewhere.
- **Network policy for tests:** no test may make a network call anywhere except through
  `httptest` or `net` dialing localhost ephemeral ports (port 0). No test may dial port 8080, an
  external host, or any fixed port.

### 1.1 Configuration surface (complete — there are no other knobs)

| Env var | Meaning | Default | Notes |
|---|---|---|---|
| `COUP_PORT` | TCP listen port for the game server | `8090` | Must never be set to `8080` by tooling; startup fails fast if it is (defense in depth). |
| `COUP_POLL_HOLD` | Long-poll hold, duration string (`25s`) | `25s` `[ASSUMPTION A1]` | Clamped to `[1s, 30s]`. |
| `COUP_LLM_BASE_URL` | OpenAI-compatible API base for the `openai` driver | `http://localhost:8080/v1` | Client-side URL only; see port policy above. |
| `COUP_LLM_MODEL` | Model name sent in chat-completion requests | *(empty)* | **Required when `COUP_DRIVER_MODE=openai`**; startup fails with a clear error if empty `[ASSUMPTION A6]`. No other LLM client knobs (temperature etc.) exist in v1 — API defaults apply. |
| `COUP_DRIVER_MODE` | Which `MoveDriver` backs LLM seats: `heuristic` \| `openai` | `heuristic` | The `fake` driver is **not** a runtime mode; it exists only for test composition. |
| `COUP_DEBUG` | Debug mode: `0`/`1` (also accepts `true`/`false`) | `0` | Gates the `/debug/*` endpoints, LLM reasoning-content logging, and verbose driver logs. |
| `COUP_LOG_FORMAT` | `slog` handler: `text` \| `json` | `text` | |

Flag parity: each env var also has a matching command-line flag on `main.go` (flag wins over env)
so operators can run one-shot configurations without env files. The set above is closed; adding a
knob requires updating this table.

---

## 2. Architecture (clean/onion)

```
┌────────────────────────────────────────────────────────────────────┐
│ main.go — composition root: parse config, build slog, wire ports   │
├────────────────────────────────────────────────────────────────────┤
│ infrastructure/                                                   │
│   http/      chi router + handlers (games, events, decisions,     │
│              forfeit votes, debug) over net/http                  │
│   store/     in-memory GameStore + EventStore                     │
│   llm/       openai client driver, heuristic driver, fake driver  │
│   time/      wall Clock, real IDGen                               │
├────────────────────────────────────────────────────────────────────┤
│ application/                                                      │
│   use cases: CreateGame, PollEvents, GetSnapshot, SubmitDecision, │
│   InitiateForfeitVote, CastForfeitVote                            │
│   window manager (fan-out/cancel), driver loop, forfeiture logic  │
│   ports.go — interfaces only                                      │
├────────────────────────────────────────────────────────────────────┤
│ domain/ — PURE. No I/O, no time.Now(), no net, no sync package.   │
│   cards, deck, players, actions, legality, claim/challenge        │
│   resolution, elimination, game state machine                     │
└────────────────────────────────────────────────────────────────────┘
```

Dependency rule: `application` imports `domain` and its own port interfaces only;
`infrastructure` implements ports; `main.go` is the only place where a concrete adapter is bound to
a port. `domain` imports nothing outside stdlib pure types (no `time.Time` — it stores durations/
deadlines as values passed in by callers). Tests swap adapters at the composition level (fake
driver, fake clock, scripted event store) without touching domain or application code.

### 2.1 Package layout

```
main.go                      # composition root + flag/env parsing + server lifecycle
domain/
  card.go                    # Character enum (5 values), Card identity
  deck.go                    # CourtDeck: size invariant, shuffle/draw/return ops
  player.go                  # Seat state: coins, face-down (2), face-up, eliminated
  action.go                  # ActionKind, Claim, legality checks + descriptive errors
  window.go                  # Window spec: kind, eligibility, settings, state
  resolve.go                 # claim/challenge resolution cascade, sunk costs
  game.go                    # Game aggregate: seats, turn order, single-window invariant,
                             #   elimination, game-over detection
  *_test.go
application/
  ports.go                   # MoveDriver, EventStore, GameStore, Clock, IDGen
  create_game.go  poll_events.go  snapshot.go  submit_decision.go
  window_manager.go          # open/close/cancel windows, fan-out to drivers
  driver_loop.go             # turn-action + response decision loop with retry/fallback
  forfeit.go                 # vote lifecycle
  *_test.go                  # fake driver + fake clock compositions
infrastructure/
  http/server.go             # chi router, middleware (request id, access log, auth)
  http/games.go  events.go  decisions.go  forfeit.go  debug.go
  store/memory_game.go  memory_event.go
  llm/openai.go              # chat-completions + tool-calling client, retry/backoff
  llm/heuristic.go           # built-in strategy driver (default dev mode)
  llm/fake.go                # scripted queue driver (tests only)
  time/wall.go               # Clock impl; ids.go: IDGen impl
```

### 2.2 Ports (Go signatures, normative)

```go
package application

// MoveDriver produces one candidate decision for a seat. It receives exactly the world view the
// seat may know (§9) plus a description of what is being asked. It NEVER receives a menu of legal
// moves or hints; legality is enforced by domain code in the application layer, and rejection
// errors are fed back via input.Feedback.
type MoveDriver interface {
    Name() string // "fake" | "heuristic" | "openai"
    Decide(ctx context.Context, in DecisionInput) (Candidate, error)
}

type DecisionKind int // KindTurnAction | KindWindowResponse | KindSingleSeatChoice

type DecisionInput struct {
    GameID   string
    SeatID   string
    Kind     DecisionKind
    World    WorldView        // exactly what this seat may know (§9), rendered-free struct
    Request  Request          // structured "what is being asked" (see below)
    Feedback string           // non-empty on retry: descriptive error from a rejected candidate
}

type Request struct {
    // turn action: zero value.
    // window response: WindowID, trigger action summary (who claimed what, at whom).
    WindowID        string
    TriggerSummary  string    // public facts only, e.g. "Vito claims Assassin against you"
    EligibleActions []string  // the *grammar* of responses for this seat:
                              //   ["pass","counterclaim_contessa","challenge"] etc.
                              // This is the response vocabulary of the window mechanics, not a
                              // hint about what is wise; it mirrors what a human sees at the table.

    // single-seat choice: ChoiceKind (FlipChoice | ExchangeFinish) + identifiers only.
    ChoiceKind string         // "flip_choice" | "exchange_finish"
    Options    []string       // e.g. ["face_down_0","face_down_1"] — positional, never names cards
}

type Candidate struct {
    Kind     DecisionKind
    Action   string   // "income"|"foreign_aid"|"coup"|"tax"|"steal"|"assassinate"|"exchange"
    TargetID string   // where applicable
    Response string   // "pass"|"counterclaim"|"challenge"
    Character string  // claimed character for counterclaims / tax/steal/etc.
    Choice   string   // for single-seat choices: option identifier or "no_swap"
}

// EventStore is append-only, per-game, with audience-tagged events (§10).
type EventStore interface {
    Append(ctx context.Context, gameID string, evts []domain.Event) (firstSeq int64, err error)
    // Range returns events with seq > after whose audience is public or the viewer's seat.
    Range(ctx context.Context, gameID string, after int64, viewerSeatID string) ([]EventOut, error)
    LastSeq(ctx context.Context, gameID string) (int64, error)
    WaitNew(ctx context.Context, gameID string, after int64) // blocks until seq > after or ctx done
}

type GameStore interface {
    Put(game *domain.Game)               // in v1: memory map guarded by mutex
    Get(gameID string) (*domain.Game, bool)
    All() []*domain.Game                  // debug facility
}

type Clock interface {
    Now() time.Time
    After(d time.Duration) <-chan time.Time // test fakes drive these manually
}

type IDGen interface {
    New(kind string) string // "g", "s", "w", "d", "v" → kind-prefixed id (§13, A3)
}
```

Per-game concurrency: all domain state mutation for a game happens under one `sync.Mutex` owned by
the application (never inside `domain`). Long-poll waits use `EventStore.WaitNew`; LLM fan-outs are
goroutines with cancellable contexts. The engine is therefore always in a single, well-defined
state; "exactly one window open" is an invariant the mutex protects (§5.3).

---

## 3. Domain model & invariants

### 3.1 Core types (normative semantics)

- `Character` ∈ {`duke`, `assassin`, `contessa`, `captain`, `ambassador`}. Deck = **three copies
  of each** = 15 cards. Card *identity* for internal tracking is `(character, copy-index)`; the
  copy-index never leaves the server and is irrelevant to rules (cards of a character are
  interchangeable).
- `Seat`: `{ id, name, type: human|llm, coins ≥ 0, faceDown [≤2 cards], faceUp [cards] }`.
  `faceDown` has length exactly 2 while active; it shrinks only via flips.
- `CourtDeck`: the remaining cards, size invariant below. Only its top is ever drawn; revealed
  cards are shuffled back in; order of returned exchange cards is not visible to anyone (they are
  returned as a set).
- `Game`: ordered seat list (seat order = turn order), `courtDeck`, `turnSeatIndex`, `turnID`
  (monotonic from 1, assigned when a turn starts), exactly one of {no open structure, open window,
  pending single-seat decision, open forfeit vote bookkeeping*}, game status.

\* A forfeit vote may coexist with any in-game structure; it is an out-of-band ballot, not part of
the turn/window state machine (§7).

### 3.2 Invariants (engine must maintain; tests assert)

1. **Court Deck size is constant for the whole game:** `|deck| = 15 − 2×(seat count)`. Exchange
   draws 2 and returns 2 (net zero); a successful challenge resolution shuffles the revealed card
   back in while its owner draws one replacement (net zero). Consequences: min deck size is 3
   (6 players), **Exchange is always legal**, and Court Deck exhaustion cannot occur. No code path
   may check for deck exhaustion.
2. Total cards in play is always 15: `Σ faceDown + Σ faceUp + |deck| = 15`.
3. A seat is *eliminated* ⇔ both its cards are face-up. Eliminated seats: no turns, not eligible
   for any window response, not a valid target for any action or vote; their coins remain but are
   inert (cannot be stolen — they have no valid "target-ness", and in particular Steal requires a
   *non-eliminated* target with ≥ 1 coin). Eliminated seats' cards are fully public.
4. Face-up cards never leave the game: unusable, publicly known, counted in invariant 2.
5. `coins ≥ 0` for all seats; coins are unbounded on the high side — there is **no treasury cap**
   and no enforced supply (the physical 50-coin tray is not modeled); counts are tracked for
   display and legality only.
6. Exactly one global structure of {window, single-seat decision} at a time; forfeit voting aside,
   no two windows ever overlap even momentarily (§5.3).

---

## 4. Rules engine (exact encoding)

### 4.1 Actions

| Action | Claim? | Cost | Effect | Response window |
|---|---|---|---|---|
| `income` | no | — | actor +1 coin | none |
| `foreign_aid` | no (base action) | — | actor +2 coins, **unless** blocked by a surviving Duke counterclaim | FA window: any active seat except the aid-taker may `counterclaim(duke)` or `pass`. A *surviving* Duke counterclaim opens its **own nested challenge window** (anyone except the blocker). Direct challenges of Foreign Aid itself are impossible — it claims no character. |
| `coup` | no | pay 7, immediately | victim loses one influence: **the victim chooses** which face-down card flips (auto-flip if only one remains) | none. **Mandatory:** while the actor has ≥ 10 coins, Coup is the *only* legal action on their turn ("≥10" superset reading — owner-confirmed, rules.md Ruling 1). |
| `tax` (Duke) | yes — claim `duke` | — | +3 coins **if the claim survives** (§4.2 timing) | challenge window: any active seat except the claimant may challenge or pass |
| `steal` (Captain / Ambassador) | yes — claim either | — | actor takes `min(2, target's coins)` **if the claim survives**. Target validity: non-eliminated, **≥ 1 coin** (so min() always pays ≥ 1) | attack window: the *target* may `counterclaim(captain|ambassador)` or challenge; every other active seat may challenge only |
| `assassinate` (Assassin) | yes — claim `assassin` | **pay 3, immediately — sunk** | victim loses one influence (victim chooses which card flips) **if the attack survives** | attack window: the *target* may `counterclaim(contessa)` or challenge; others challenge only |
| `exchange` (Ambassador) | yes — claim `ambassador` | — | if the claim survives: draw top 2 of Court Deck (**privately revealed to the actor**, §10 event `exchange_revealed`), the actor may swap one drawn card with one of their face-down cards, then return 2 cards to the deck (order hidden from all) | challenge window: any active seat except the claimant may challenge or pass |

**Timing rule (normative):** base actions (`income`, `foreign_aid` pre-window outcome, `coup`)
apply their effects immediately — there is no window for coup/income at all. For claimed actions,
upfront **costs** are paid at declaration and are sunk; the action's **effects execute only after
the claim's window has resolved in favor of the claim** (survived unchallenged, or challenge failed
against a challenger). This is what makes "failed offensive claim ⇒ void" meaningful: nothing to
revert was ever granted.

### 4.2 Claim & challenge resolution (the core bluff mechanic)

A window closes via a winning **challenge** against a claim by seat A (character C):

1. `claim_succeeded` if and only if A holds C among face-down cards:
   - C is revealed publicly (`card_flipped`, reason = successful reveal), shuffled into the Court
     Deck, and A draws one replacement face-down card (net-zero deck, invariant 3.2.1).
   - The **challenger** loses one influence (their choice of which card flips; auto if one left) →
     `card_flipped` (penalty), possibly `eliminated`.
   - The original claim's effects then execute as normal.
2. Otherwise `claim_failed`:
    - A loses one influence (victim-of-penalty chooses which of their remaining face-down cards
      flips; auto if one left). This branch exists ONLY because C is absent from A's hand: revealing
      is mandatory — a challenged claimant holding C MUST reveal it (rules.md Ruling 7); refusing or
      "choosing not to reveal" is not an option, and the penalty is the only consequence of a busted claim.
   - The claimed action is **VOID**. Any cost already paid (Assassinate's 3 coins) is sunk.

A window closes via a winning **counterclaim** by seat B blocking attack X:

1. `claim_succeeded` iff B holds the claimed blocking character (Contessa vs Assassinate;
   Captain or Ambassador vs Steal; Duke vs Foreign Aid): original action X is **blocked — it
   fizzles with no effect**; any sunk cost of X is lost (`action_fizzled` event). The counterclaim
   itself has opened its nested challenge window first (§5.2) — it can only "succeed" if that
   nested window resolves in the blocker's favor, so the order is: attack window closes → nested
   window for the blocking claim → only after it settles does block/fizzle (or no-block) apply.
2. `claim_failed`: **B suffers the penalty and there is NO BLOCK** — the original attack proceeds
   to full effect. This is the load-bearing asymmetry, worked out in §4.3.

### 4.3 ⚠ The failed-defensive-claim asymmetry (load-bearing — call out in every review)

- **Failed offensive claim** → action VOID; cost sunk. The world state changes by exactly:
  claimant −1 influence (+ their flip choice), nothing else.
- **Failed defensive claim** → there was never "a block" that could be voided, so the attack
  simply **proceeds as if unopposed**, on top of the defender's penalty.

Worked example — busted Contessa bluff against an Assassinate (per rules.md + §11 edge list):

```
A declares assassinate(B), pays 3 (sunk). B counterclaims Contessa. C challenges B.
B does not hold Contessa ⇒ claim_failed:

• B had TWO face-down cards at the moment of the penalty:
    – B flips one (penalty, their choice) and the assassination PROCEEDS against B's remaining
      card ⇒ B's second card flips ⇒ B ELIMINATED in this single sequence.
      Net: B loses both cards; A's 3 coins are gone; A survives with ≥… whatever A had left.

• B had ONE face-down card at the moment of the penalty (already lost one earlier):
    – the penalty flips it ⇒ B is eliminated BY THE PENALTY ALONE, and the original assassination
      FIZZLES at effect time because its target has no influence when the attack resolves
      (`action_fizzled`, reason target_eliminated). A's 3 coins are gone; B's single card flips.
```

Engine requirement: effect application of any targeted action must re-check target liveness at the
moment effects run, not at declaration.

### 4.4 Elimination & game end

- Any influence loss (penalty, failed challenge, Coup, Assassinate) → the affected seat chooses
  which face-down card flips; with exactly one remaining it is automatic (`domain` resolves, no
  decision round-trip). With two, a **single-seat decision** round-trip occurs (§5.4).
- Both cards face-up ⇒ `eliminated` event (public, lists both cards); seat drops out of turn order
  and all future eligibility sets; coins retained but inert.
- Game ends immediately when exactly one active seat remains: `game_over` event with the winner.

### 4.5 Legality checks & descriptive errors

`domain.ValidateAction(game, seat, candidate) (err *domain.LegacyError)` returns **descriptive,
table-voice error strings** — these exact strings are what LLM drivers receive as tool-result
feedback and what human clients see:

| Situation | Message (normative string) |
|---|---|
| Not the seat's turn / no open structure addressed to them | `"it is not your turn"` |
| Seat already eliminated | `"you are eliminated — you cannot act"` |
| Insufficient coins for cost | `"not enough coins"` |
| Coup not taken while holding ≥ 10 | `"you must take a coup while you have ten or more coins"` |
| Steal target has 0 coins / is eliminated / missing | `"that player has no coins to steal from"` / `"that player is eliminated"` / `"no such seat"` |
| Coup/assassinate target eliminated/missing | `"you cannot target an eliminated player"` / `"no such seat"` |
| Claiming a character action without… (nothing) — claims are always allowed; the window decides. *(No error here by design.)* | — |
| Response not permitted for this seat in this window | `"you cannot respond to that window"` |
| Seat already responded in this window | `"you have already responded"` `[ASSUMPTION A16]` |
| Window closed / stale response | `"the window has already been resolved"` |
| Single-seat decision already answered / wrong seat | `"that decision was already made"` / `"this decision is not addressed to you"` |

The server **never** returns a menu of legal moves, probabilities, or hints. The vocabulary in
`DecisionInput.Request.EligibleActions` (§2.2) is the window grammar (pass/block-with-X/challenge),
which is public table information, not advice.

---

## 5. Response windows — the core interaction model

### 5.1 Eligibility matrix (per situation; all windows open SIMULTANEOUSLY to every eligible seat)

| Triggering action | Target's options | Every other active non-claimant's options |
|---|---|---|
| `steal` on T | `pass` \| `counterclaim(captain)` \| `counterclaim(ambassador)` \| `challenge` | `pass` \| `challenge` |
| `assassinate` on T | `pass` \| `counterclaim(contessa)` \| `challenge` | `pass` \| `challenge` |
| `foreign_aid` by A | *(A excluded from its own window)* any other seat: `pass` \| `counterclaim(duke)` | same — **no direct challenge exists** (FA claims no character) |
| surviving Duke counterclaim on FA (by B) | nested window: all active seats except B: `pass` \| `challenge` | — |
| `tax`, `exchange` by A | all active seats except A: `pass` \| `challenge` | — |

Eligibility = **active** (not eliminated) and not the claimant; the *shape* of allowed responses is
per-seat as above. "First non-PASS response wins" (§5.2) applies across these heterogeneous
vocabularies uniformly: the first seat whose response is not `pass` determines the resolution path.

### 5.2 Window rules (normative)

1. **Simultaneous open.** The moment a claimable action is declared (or a blocking claim is made),
   the window opens for all eligible seats at once — no ordering of "target first, then others".
2. **First non-PASS wins**, regardless of seat type (human or LLM). It resolves under normal rules
   (§4.2); a successful counterclaim proceeds as usual; a successful challenge forces
   reveal-or-penalty; a failed claim applies its penalty and cascade.
3. **Exactly one window open at a time, globally per game** — the innermost active one. A nested
   block-challenge window *replaces* the current one in the state machine; **no superseded claim
   ever reopens a new window after a cascade settles.** (The original action's fate is decided
   once, at the end of the cascade, §4.2.)
4. **Passes never close a window by themselves** — only via termination (b)/(c) below.
5. **Termination**, with game-creator settings `require_all_responses: bool` and
   `response_timeout: duration|null` chosen at creation (defaults `[ASSUMPTION A8]`:
   `false`, `null`):

| # | require_all | timeout | Termination semantics | Liveness |
|---|---|---|---|---|
| 1 | false | none | (a) first non-PASS only. | **Open-ended FCFS.** If everyone passes, the window stays open until someone acts or a human forfeit-votes the stall out of existence. Deliberate — consistent with the project's "no hidden timeouts" philosophy. Unattended simulations must NOT use this combination (§14). |
| 2 | false | T | (a) first non-PASS, else (c) at T: unresponded seats auto-pass; if nobody acted non-PASS the action proceeds unopposed. | Bounded by T. |
| 3 | true | none | (a) first non-PASS, else (b) when every eligible seat has responded (passes count). | Bounded by human/LLM responsiveness of all seats; LLM seats always respond (fallbacks, §6), humans may stall it. |
| 4 | true | T | Earliest of (a), (b), (c); at T unresponded seats auto-pass and the window closes even if not all responded. | Bounded by T. |

   Closure reason is recorded on `window_closed` (`first_response` \| `all_responded` \|
   `timeout` \| `auto_no_responders`, the last used when elimination/forfeit leaves a window with
   no eligible responders — §7).
6. **Cancellation on close.** When a window closes by a winning response, **every in-flight LLM
   inference for other seats still awaiting that window is cancelled immediately** via its context;
   each cancellation is logged (INFO, with `game_id`, `window_id`, seat, driver). Late/stale
   responses from *any* client (human or otherwise) arriving after close are rejected with 409 and
   `"the window has already been resolved"` (§4.5), recorded as a private `rejection` event on that
   seat's stream `[ASSUMPTION A9]`, and discarded. Human clients learn about closure through the
   public `window_closed` event, which names the decider seat.
7. **Per-seat once-ness.** Each seat may respond at most once per window; a second attempt while
   the window is still open is rejected `[ASSUMPTION A16]`.

### 5.3 Window lifecycle state machine (normative)

```
                     ┌───────────────────────────────────────────────────────────┐
                     │                                                          │
                     ▼                                                          │
        ┌─────────────────────┐   claimed action declared, or          nested challenge window for a
        │   NO WINDOW OPEN    │   blocking claim made                  surviving counterclaim REPLACES
        │  (turn/decision/idle│◄──────────────────────────────────────┤ the current one — it never stacks
         └─────────┬─────────┘                                         │
                   │ window opens (all eligible seats at once)         │
                   ▼                                                   │
        ┌─────────────────────┐  close: first non-PASS (a)             │
        │     WINDOW OPEN     │      all-responded (b, req-all)        │
        │  W = innermost,     │      timeout (c)                       │
        │  exactly ONE per    │──────────────────────┐                 │
        │  game               │                      │                 │
        │  passes are logged, │   (elimination/forfeit empties          │
        │  never close W      │    eligible set → auto_no_responders) ──┤
        └─────────────────────┘                      │                 │
                                                     ▼                 │
                                     ┌──────────────────────────┐      │
                                     │   RESOLVING W (cascade,  │◄─────┘
                                     │   synchronous under lock)│
                                     │  reveal-or-penalty, sunk │
                                     │  costs, block/fizzle     │
                                     │  bookkeeping             │
                                     └────────────┬─────────────┘
                                                  │ cascade settles
                                                  ▼
                              ┌───────────────────────────────────────┐
                              │ EFFECTS & CASCES: coin moves, flips,  │
                              │ elimination checks (may spawn a       │
                              │ single-seat flip-choice decision),    │
                              │ game-over check                       │
                              └────────────┬──────────────────────────┘
                                           │ nothing further open
                                           ▼
                          back to NO WINDOW OPEN → next seat's turn
                          (turn_id advances) or game_over
```

Notes: the RESOLVING phase is synchronous engine work (no wall-clock waiting), except it may emit a
single-seat decision (§5.4) which *is* a wait; nested windows re-enter WINDOW OPEN as drawn. "No
superseded claim ever reopens" means there is no edge from RESOLVING back to WINDOW OPEN for the
*original* claim — only the counterclaim's own challenge window may open, and it does so before
RESOLVING completes (i.e., the state machine re-enters WINDOW OPEN with W′ as innermost while the
original resolution is suspended; after W′ settles, the original cascade finishes in one RESOLVING
pass).

### 5.4 Single-seat decisions

Two kinds: **flip choice** (a seat with exactly two face-down cards loses influence → choose which
flips) and **exchange finish** (actor chooses keep-vs-swap with their two privately-drawn cards).
Semantics:

- Addressed to exactly one seat; the engine suspends until that seat answers.
- They honor the same `response_timeout` setting as windows (`require_all_responses` is not
  applicable — there is one respondent). With timeout = none they wait indefinitely (an LLM seat
  will always answer: driver fallbacks, §6.4; a human's stall can be resolved by forfeit vote, §7).
- Auto-resolution without any round-trip when no choice exists (one card left ⇒ auto-flip) or for
  the engine-safe default used only as the *driver-fallback* answer (`[ASSUMPTION A4]`: flip
  choice defaults to the first face-down card; exchange finish defaults to no-swap).
- Events: private `card_flip_choice_requested` (flip choices enumerate options positionally —
  never by card name, §10); resolution is visible publicly only through the resulting
  `card_flipped` / `exchange_completed` events.
- **Flip-choice index semantics (normative):** when a seat is asked which of its cards flips face-up
  (Coup/Assassinate victim or penalty claimer with two face-down cards), option identifier `face_down_i`
  refers to the i-th element (0-based) of that seat's current face-down hand — exactly as ordered in
  `your_face_down_cards` in its own snapshot and mirrored positionally in the `options` of
  `card_flip_choice_requested`; a seat cannot change its hand while a single-seat decision is pending
  for it, so the request-time and answer-time orderings coincide.

---

## 6. Seats, players & LLM drivers

### 6.1 Game creation contract

- `POST /games` takes per-seat entries: `name` (player-chosen) + `type`: `human` | `llm`, plus the
  creator window settings (`require_all_responses`, `response_timeout`). 2–6 seats, at least one
  human. Blank names get sensible defaults `[ASSUMPTION A14]`: LLM seats cycle through
  `"Vito"`, `"Sonny"`, `"Carmine"`, `"Rocco"`, `"Salvatore"`, `"Bruno"` in seat order; human
  seats become `"Player N"` (their displayed seat number).
- **Every human seat is issued a bearer token at creation** (64 hex chars = 32 random bytes,
  shown exactly once in the create response; not revocable in v1 `[ASSUMPTION A2]`). State is fully
  server-side: closing a browser loses nothing — resuming means polling again with `after=seq`,
  which is idempotent. The first listed seat is treated as the *creator* only for provenance
  logging; all human seats have identical API rights.
- LLM seats get no token and never poll; the server drives them internally (§6.4). Their private
  events still land in the event store and are visible through `/debug` (`COUP_DEBUG=1`).
- Turn order: **random starting seat** (rules.md Ruling 6), then fixed seat order skipping
  eliminated seats. The chosen first seat is reported in the create response for UI display.

### 6.2 Driver implementations behind `MoveDriver`

| Mode | Implementation | Role |
|---|---|---|
| `fake` | scripted queue of pre-canned `Candidate`s (configurable per test); returns a terminal error if the queue is empty | tests only; not selectable at runtime |
| `heuristic` | built-in deterministic strategy (below) | **DEFAULT** dev mode — humans can play the full loop with zero model server running |
| `openai` | real OpenAI-compatible chat-completions + tool-calling client (§6.4) | production LLM play |

**Heuristic driver behavior `[ASSUMPTION A7]`** (kept deliberately simple and *deterministic per
game*: its randomness is seeded from a hash of the game id, so simulated games are replayable):

- Turn action: if coins ≥ 10 ⇒ `coup` against a random active target (mandatory anyway). Else with
  weight 60% take free money (`income`, `foreign_aid`, or `tax` when claiming is "cheap" — i.e. the
  driver always claims tax and lets windows decide), weight 40% one of the other legal character
  actions (`steal` at a seat with ≥ 1 coin, `exchange`); targets chosen uniformly at random among
  valid seats. (Claimed characters are asserted without checking hand contents — bluffs included.)
- Response windows: **always `pass`** (the heuristic never counterclaims or challenges). This is
  what makes mode-4 games (require-all) terminate under unattended simulation (§14); it is also
  the documented reason the FCFS no-timeout combination must not be used for unattended play.
- Single-seat choices: first option / no-swap (the same defaults as the driver fallbacks, §6.4).

### 6.3 Information given to drivers (visibility contract)

A driver receives **exactly** what its seat may know (§9): own face-down cards, own coins, all
seats' names/types/coins/face-up+forfeited-cards, court deck size, and the full public event
history — rendered compactly into one user message — plus "what is being asked" (the `Request`,
§2.2). It receives **no** menu of legal moves, no hints, no other players' hidden cards, no
private events of other seats. The per-seat conversation history (§6.4) is the model's private head;
it stores nothing the human at that seat would not have seen.

### 6.4 `openai` driver — normative behavior per decision

1. **Persistent per-seat conversation for the entire game.** A new message pair is appended *every*
   decision (turn actions, window responses, single-seat choices alike): one **user** message =
   compact render of exactly what this player can know right now (§6.3) + an explicit statement of
   what is being asked ("It is your turn — choose an action" / "An assassination is targeting you —
   respond to the window" / "Choose which card flips"); one **assistant** message = the model's
   tool call (or re-prompt text, §6.4.3). This persistence is deliberate: the model must remember
   what it knew on previous turns and its own prior bluffs.
2. **Tools.** The request carries a fixed, static tool schema — one function per legal *action
   shape*, always present (presenting the grammar is not revealing legality; the server enforces):
   `income()`, `foreign_aid()`, `coup(target)`, `tax()`, `steal(target)`, `assassinate(target)`,
   `exchange()` — plus, for the decision at hand, a decision-specific toolset:
   - window response (per-seat grammar from §5.1): `pass()`, `counterclaim(character)` (only the
     characters this situation allows for this seat), `challenge()`;
    - flip choice: `flip_card(option)` with positional options (§5.4 index semantics);
   - exchange finish: `keep_drawn_cards()`, `swap_drawn_option(drawn_index, hand_option)`.
3. **Agentic loop inside one `Decide` call:** model response with a tool call ⇒ candidate returned.
   Response *without* a tool call ⇒ re-prompt ("You must choose using the provided tools") up to
   N times `[ASSUMPTION A4]`: **N = 3**; after the third empty response the driver returns the
   kind-safe default (turn ⇒ `income`; window ⇒ `pass`; choice ⇒ engine default) so **no decision
   can ever permadeath the game**.
4. **Illegal-candidate retry across `Decide` calls.** The application validates each candidate with
   domain rules; on failure it re-invokes `Decide` with the descriptive error string in
   `input.Feedback`, which the driver appends as a **tool-result message** ("Error: not enough
   coins") and lets the model retry. Bounded at K = 3 such application-level retries
   `[ASSUMPTION A4]`, after which the same kind-safe default applies (turn ⇒ `income`; window ⇒
   `pass`; choice ⇒ engine default). Every rejection-and-retry is logged.
5. **`reasoning_content`:** if present in the model's response it is captured and logged **only
   when `COUP_DEBUG=1`**, truncated `[ASSUMPTION A12]`, and is **never stored in the conversation
   history**. (Implementation note: verify field name/availability per-model during implementation —
   it is provider-specific.)
6. **Transport failures.** Connection errors, timeouts, 5xx, malformed JSON ⇒ retry with
   exponential backoff **indefinitely** `[ASSUMPTION A5]`: delay = full-jitter(`min(30s, 500ms ×
   2^attempt)`); one WARN log per retry (with attempt number and next delay). A flapping model
   server must never kill or stall-kill a game — `Decide` blocks until it produces *some* candidate
   or its context is cancelled (§5.2.6). Per-LLM-request client timeout: 120 s `[ASSUMPTION A5]`.
7. **Cancellation.** When a window closes or the seat is forfeited/eliminated, the in-flight HTTP
   request for that seat's decision is aborted (context cancellation) and logged with reason.

**Compact world render (exact format used in the user message):**

```
COUP — your table view (turn 12)
You: Marta (human), 6 coins, hand: [duke, ambassador]
Vito (llm): 3 coins, face-up: [captain]
Salvatore (human): 9 coins, face-up: []
Bruno (llm): 0 coins, face-up: [assassin, contessa] — ELIMINATED
Court deck: 5 cards.
Recent events:
 12 Vito declares Assassinate against Marta (paid 3)
 13 Window opened: you may pass, counterclaim (contessa), or challenge; others may pass or challenge
Question: An assassination is targeting you. Respond to the open window.
```

(Seat order in the render = seat order; eliminated seats are listed with their full face-up cards —
public knowledge. For LLM *turn* decisions the "Recent events" tail is the last 10 public+own
events to bound prompt size `[ASSUMPTION A5]` — full history remains accessible to the model via its
conversation memory.)

---

## 7. Forfeit votes (dropped-player recovery)

- **Initiation:** any **active human** player may initiate a forfeit vote against any seat that is
  stuck (human or LLM — e.g. `llama-server` wedged). No automatic detection; it is human-judged.
  `POST /games/{id}/forfeit-votes {target_seat_id}` → 409 if the target is already eliminated, not
  active, or an active vote already exists `[ASSUMPTION A15]`: **one active forfeit vote per game
  at a time** (simplest consistent rule). A seat may NOT initiate a vote against **its own** seat —
  rejected with code `self_target` and message `"you cannot target your own seat"` (table-voice per
  §4.5; part of A15's invalid-target rules) — even though human clients omit their own seat from the
  target list, the server remains authoritative.
- **Ballot:** yes/no from the **other active HUMAN seats only** — LLM seats do not vote in v1.
  The ballot is open-ended (no timeout) and **closes early the moment a majority of eligible
  voters has decided** (majority = strictly more than half of the eligible-voter count at vote
  opening; e.g. 3 eligible ⇒ closes on 2 same-direction votes). If every eligible voter has cast
  without either side reaching a majority (a tie), the ballot closes with result `not_forfeited`;
  the complete `result` vocabulary lives on `forfeit_vote_resolved` (§10.1). Ballots for an
  already-closed vote → 409.
- **Majority yes ⇒ target is instantly eliminated:** turns skip it, it becomes untargetable and
  ineligible for everything (invariant §3.2.3). Its unrevealed cards are logged publicly as
  *forfeited — never revealed*: they appear in the `eliminated` event's card list only as
  `{"card":"forfeited"}` placeholders — **they remain unknowns for belief tracking** (both humans'
  and LLMs' models of what was dealt) and are **not removed from deck-composition reasoning**.
- **Pending structures resolve per normal elimination rules:** an attack whose target just
  forfeited fizzles (`action_fizzled`, reason `target_forfeited` — a special case of the §4.3
  liveness re-check); any open window with no remaining eligible responders auto-closes
  (`window_closed`, reason `auto_no_responders`) ⇒ the original action proceeds unopposed; a
  pending single-seat decision addressed to the forfeited seat is voided and its safe default
  applied `[ASSUMPTION A15]`; if the vote's target itself ceases to be eligible while the vote is
  pending (eliminated by play), the vote voids with no effect.
- **2-player edge:** forfeiting your only opponent is simply winning — `game_over` follows the
  elimination. Fine by design; no special handling beyond the normal cascade.
- Events: `forfeit_vote_opened`, `forfeit_vote_cast`, `forfeit_vote_resolved` (§10).

---

## 8. Visibility & "state only, no advice"

**Every player (human or LLM) sees exactly:**

1. their own two face-down cards (or fewer) and their own coins;
2. public info = all seats' names + types, every seat's coin count, every seat's face-up and
   forfeited cards, Court Deck size, and the full public action/event history.

**Private (seat-scoped):** Exchange reveals (`exchange_revealed` is emitted only on the exchanging
player's stream) and `card_flip_choice_requested` options. No other event is seat-scoped.

**Server enforcement posture:**

- The server validates every submission with domain rules and returns **descriptive errors that
  read like table feedback** ("not enough coins", "that player has no coins to steal from") — the
  catalogue in §4.5. It **NEVER** sends menus of legal moves, suggested plays, or hints. Error
  strings must not reveal hidden information (e.g., never "you don't hold a captain").
- LLM prompts contain exactly the same world information a human has at the table (§6.3/§9 render).
  The per-seat conversation history is *just* the model's private head — it may contain its own
  prior reasoning and private events (its exchange draws), never another player's.
- Privacy filtering is enforced **at the store** (`EventStore.Range` with `viewerSeatID`, §2.2): a
  seat's poll stream = public events + its own private events, in one monotonic sequence. Snapshots
  are filtered the same way at read time.

---

## 9. Concurrency & liveness notes

- One mutex per game serializes all state mutation; `domain` is pure and re-entrant-safe for
  validation outside the lock (validation takes a consistent copy).
- LLM decision goroutines: one in flight per seat at a time (a seat never has two concurrent
  `Decide` calls — its next request only starts after the previous candidate was accepted,
  rejected-with-feedback, or cancelled).
- Cancellation points: window close (§5.2.6), seat elimination/forfeit, game over, server
  shutdown. Every cancel is logged with reason.
- Bounded-wait long polls must survive handler timeouts on proxied deployments: the server's hold
  (`COUP_POLL_HOLD`) always starts below typical proxy read-timeouts (30 s cap) and every poll
  returns a full 200 with JSON — never an empty body, never a hang past the hold.

---

## 10. Event model (normative catalogue — the UI is built strictly from this)

- Each game has a **monotonic `event_seq`** starting at 1. Events are appended in batches
  (one resolution step = one batch); within a batch seqs increase by 1 each.
- Every event carries: `seq`, `game_id`, `ts` (RFC3339 from the Clock port), `type`, an
  `audience` (`"public"` or `{"seat": "<seat_id>"}`), and type-specific fields.
- Storage is append-only; there are no updates. Rejections get their own seq, private to the seat
  `[ASSUMPTION A9]`.

### 10.1 Event types with JSON shapes

Common envelope (abridged below for brevity — `seq`/`game_id`/`ts` always present):

```jsonc
// game_created — public. Emitted as the first batch after a successful POST /games.
{"type":"game_created",
 "seats":[{"id":"s_7f3a9c2e1b4d","name":"Marta","type":"human"},
          {"id":"s_c2e5a1b8f0d3","name":"Vito","type":"llm"}],
 "window_settings":{"require_all_responses":true,"response_timeout_ms":45000},
 "first_turn_seat_id":"s_c2e5a1b8f0d3"}

// turn_start — public. New turn_id; seat to act (random first, fixed order thereafter).
{"type":"turn_start","turn_id":7,"seat_id":"s_7f3a9c2e1b4d"}

// action_declared — public. The claim/declaration; cost fields are what was PAID immediately.
{"type":"action_declared","turn_id":7,"actor_id":"s_c2e5a1b8f0d3",
 "action":{"type":"assassinate","character":"assassin","target_id":"s_7f3a9c2e1b4d","coins_paid":3},
 "text":"Vito claims Assassin and declares an Assassinate against Marta (paid 3)"}

// window_opened — public.
{"type":"window_opened","window_id":"w_1e0d…",
 "kind":"challenge",                       // challenge | counterclaim_challenge
 "trigger":{"action_seq":42,"actor_id":"s_c2e5a1b8f0d3"},
 "eligible":["s_7f3a9c2e1b4d","s_9aa…"],   // seat ids, in seat order
 "target_seat_id":"s_7f3a9c2e1b4d",        // present for attack windows
 "require_all_responses":true,"timeout_ms":45000,"deadline_at":"2026-09-22T12:00:45Z"}

// response_received — public. Passes are events too (they feed require-all bookkeeping).
{"type":"response_received","window_id":"w_1e0d…","seat_id":"s_7f3a9c2e1b4d",
 "response":{"type":"counterclaim","character":"contessa"}}   // pass | counterclaim | challenge

// window_closed — public. Names the decider (locked requirement for human clients).
{"type":"window_closed","window_id":"w_1e0d…",
 "reason":"first_response",                // first_response|all_responded|timeout|auto_no_responders
 "decider_seat_id":"s_7f3a9c2e1b4d"}

// claim_succeeded — public. Claimant revealed the character; deck shuffled+redraw happened
// (the replacement card itself is face-down and NEVER appears in any event).
{"type":"claim_succeeded","seat_id":"s_c2e5a1b8f0d3","character":"assassin",
 "challenger_seat_id":"s_9aa…"}

// claim_failed — public. Claimant busted; penalty follows (or flip-choice decision if 2 cards left).
{"type":"claim_failed","seat_id":"s_c2e5a1b8f0d3","claimed_character":"assassin",
 "challenger_seat_id":"s_9aa…"}

// card_flipped — public. One influence lost; the card is now face-up and publicly known.
{"type":"card_flipped","seat_id":"s_7f3a9c2e1b4d","card":"contessa",
 "reason":"assassinate"}   // penalty_failed_claim | penalty_failed_challenge | coup | assassinate |
                           // successful_reveal — the last one ONLY for a card revealed by a
                           // successful challenge: it becomes public knowledge but is shuffled back
                           // into the Court Deck (its owner draws a replacement), so it does NOT
                           // join the seat's face-up list.

// coins_changed — public. Before/after in one event (delta + new total), per state transition.
{"type":"coins_changed","seat_id":"s_c2e5a1b8f0d3","delta":-3,"total":0,
 "reason":"assassination_paid"}   // income|foreign_aid|tax|coup_paid|assassination_paid
                                   // |steal_given|steal_took

// action_fizzled — public. A previously declared action took no effect.
{"type":"action_fizzled","action_seq":42,"actor_id":"s_c2e5a1b8f0d3",
 "reason":"counterclaim_success"}   // counterclaim_success | target_eliminated | target_forfeited

// eliminated — public. Both cards face-up (or forfeited placeholders, §7).
{"type":"eliminated","seat_id":"s_9aa…","reason":"forfeit",   // coup|assassinate|penalty|failed_challenge|forfeit
 "cards":[{"card":"captain"},{"card":"forfeited"}]}

// exchange_revealed — PRIVATE to the exchanging seat (audience {"seat": …}).
{"type":"exchange_revealed","seat_id":"s_7f3a9c2e1b4d",
 "drawn":["duke","ambassador"],
 "decision":{"id":"d_5c…","kind":"exchange_finish",
             "options":{"drawn_index":[0,1],"hand_options":["face_down_0","face_down_1"]},
             "timeout_ms":45000}}

// card_flip_choice_requested — PRIVATE to the losing seat. Options positional, never card names.
{"type":"card_flip_choice_requested","seat_id":"s_7f3a9c2e1b4d",
 "decision":{"id":"d_6b…","kind":"flip_choice",
             "options":["face_down_0","face_down_1"],"timeout_ms":null}}

// exchange_completed — public. No card identities (they were private).
{"type":"exchange_completed","seat_id":"s_7f3a9c2e1b4d"}

// forfeit_vote_opened — public.
{"type":"forfeit_vote_opened","vote_id":"v_8d…","target_seat_id":"s_c2e5a1b8f0d3",
 "initiator_seat_id":"s_7f3a9c2e1b4d",
 "eligible_voters":["s_7f3a9c2e1b4d","s_9aa…"],"majority_needed":2}

// forfeit_vote_cast — public.
{"type":"forfeit_vote_cast","vote_id":"v_8d…","voter_seat_id":"s_9aa…","vote":"yes"}

// forfeit_vote_resolved — public. `result` vocabulary (complete; see §7):
//   "forfeited"     — yes-majority reached; an `eliminated` event follows in the same batch.
//   "not_forfeited" — no-majority: the no side reached majority first, or every eligible voter cast
//                     and neither side did (tie). Target unaffected.
//   "voided"        — the target was eliminated by play while the vote was pending (A15): no effect.
{"type":"forfeit_vote_resolved","vote_id":"v_8d…","result":"forfeited",
 "tally":{"yes":2,"no":0,"eligible":3}}

// game_over — public. Terminal event; polls afterwards return no new events.
{"type":"game_over","winner_seat_id":"s_7f3a9c2e1b4d"}

// rejection — PRIVATE to the offending seat (audience {"seat": …}). Own seq [A9].
{"type":"rejection","seat_id":"s_c2e5a1b8f0d3",
 "code":"window_already_resolved","message":"the window has already been resolved"}
```

### 10.2 Emission discipline

- One append batch per atomic resolution step (declaration; each response; closure + full cascade
  in one batch — so human clients never observe a partially-resolved world).
- `coins_changed` is emitted for **every** coin movement, including the sunk payment at
  declaration (delta −3 on `assassinate` declaration) — this is what makes logs and UI reconcile
  exactly.
- The public stream of a finished game contains everything needed to replay the game with perfect
  information about the *public* line; private events are never reconstructed for other seats.

---

## 11. HTTP API (normative surface)

### 11.1 Conventions

- JSON bodies, UTF-8. Content type `application/json` on all endpoints except 405 responses.
- **Auth:** human seats present `Authorization: Bearer <token>` (issued at creation, §6.1). LLM
  seats have no endpoint of their own — the server drives them internally. Requests without a valid
  token to seat-scoped resources → 401 `{"error":{"code":"unauthorized","message":"…"}}`.
- **Request IDs:** client may send `X-Request-Id`; if absent the server generates one (32 hex,
  `[ASSUMPTION A17]`); it is echoed in responses and appears in access logs.
- **Error envelope (all 4xx/5xx):**
  ```json
  {"error":{"code":"not_enough_coins","message":"not enough coins","request_id":"9f2c…"}}
  ```
  Codes: `unauthorized`, `not_found`, `invalid_payload`, `not_your_turn`, `not_eligible`,
  `already_resolved` (window, or an already-active forfeit vote), `vote_not_found`, `vote_closed`,
  `not_a_voter`, `target_not_active`, `self_target` (forfeit-vote initiation, §7),
  `game_over`, `server_error`. Messages follow the §4.5 table-voice catalogue where applicable.
- **Long-poll semantics** (`GET …/events?after=N`): if events with seq > N exist they are returned
  immediately; otherwise the handler holds up to `COUP_POLL_HOLD` (clamped [1s,30 s], A1) waiting on
  `EventStore.WaitNew`; on hold expiry it returns **200 with an empty `events` array** — never a
  timeout error code `[ASSUMPTION A1]`. After `game_over`, polls return empty arrays forever; the
  client detects finished games via `game_over` in the event stream or `status` in the snapshot.
  Polls are idempotent by `after`; `after=0` (or omitted) means "from the beginning".

### 11.2 Endpoints

#### `POST /games` — create game (no auth)

```jsonc
// request
{"players":[{"name":"Marta","type":"human"},{"name":"","type":"llm"}],
 "window":{"require_all_responses":true,"response_timeout":"45s"}}   // window may be omitted [A8]
// 201 response — the ONLY place tokens appear
{"game_id":"g_0a1b2c3d4e5f6789",
 "seats":[{"id":"s_7f3a9c2e1b4d","name":"Marta","type":"human",
           "token":"9f86d0ab…(64 hex)"},
          {"id":"s_c2e5a1b8f0d3","name":"Vito","type":"llm"}],
 "window_settings":{"require_all_responses":true,"response_timeout_ms":45000},
 "first_turn_seat_id":"s_c2e5a1b8f0d3"}
```
Validation: 2–6 players, ≥1 human, `type` ∈ {human,llm}, names ≤ 32 chars (trimmed; blank ⇒
defaults A14), timeout parseable duration or null. Errors: `invalid_payload`.

#### `GET /games/{id}` — per-viewer snapshot (bearer)

```jsonc
// 200, as seen by Marta (token of s_7f3a9c2e1b4d)
{"game_id":"g_0a1b2c3d4e5f6789","status":"in_progress","winner_id":null,
 "window_settings":{"require_all_responses":true,"response_timeout_ms":45000},
 "court_deck_size":11,
 "seats":[{"id":"s_7f3a9c2e1b4d","name":"Marta","type":"human","coins":6,
           "face_up_cards":[],"active":true},
          {"id":"s_c2e5a1b8f0d3","name":"Vito","type":"llm","coins":3,
           "face_up_cards":["captain"],"active":true}],
 "your_seat_id":"s_7f3a9c2e1b4d",
 "your_face_down_cards":["duke","ambassador"],        // viewer's own only — never anyone else's
 "last_event_seq":42,
 "current":{"phase":"window_open",                 // turn_action | window_open | single_seat_decision | finished
   "window":{"window_id":"w_1e0d…","kind":"challenge","target_seat_id":"s_7f3a9c2e1b4d",
              "require_all_responses":true,"timeout_ms":45000,
              "your_allowed_responses":["pass","counterclaim_contessa","challenge"],
               "responses_so_far":{"s_7f3a9c2e1b4d":"counterclaim:contessa"}}}}
```

Snapshot vocabulary (normative):

- `status`: exactly `"in_progress"` | `"finished"`. At `finished`, `winner_id` is the winner's
  seat id and remains set on every subsequent snapshot; no other values exist, and there is no
  reason field (the win condition is singular — last seat with influence, §4.4). Clients may treat
  any non-`"in_progress"` status as terminal; only these two occur.
- `current.phase`: exactly `turn_action` | `window_open` | `single_seat_decision` | `finished`, and
  `current` carries the phase-specific shape below (one shape per phase — no others exist):

```jsonc
// an active seat's turn awaits its action:
"current":{"phase":"turn_action","turn_id":7,"turn_seat_id":"s_c2e5a1b8f0d3"}

// a response window is open — full shape in the 200 example above (`your_allowed_responses`
// present only if the viewer is eligible):
"current":{"phase":"window_open","window":{…}}

// a pending single-seat decision (flip choice or exchange finish). The kind and the addressed seat
// are public; `options` appears ONLY in the snapshot of the addressed viewer (seat-scoped, §8) —
// every other viewer receives the same object with `options` omitted. For kind "flip_choice" the
// options are the flat array ["face_down_0","face_down_1"] (§5.4 index semantics):
"current":{"phase":"single_seat_decision",
  "decision":{"id":"d_5c…","kind":"exchange_finish","seat_id":"s_7f3a9c2e1b4d",
              "options":{"drawn_index":[0,1],"hand_options":["face_down_0","face_down_1"]},
              "timeout_ms":45000,"deadline_at":"2026-09-22T12:01:30Z"}}

// the terminal game-over state (`status` = "finished", `winner_id` set):
"current":{"phase":"finished"}
```

#### `GET /games/{id}/events?after={seq}` — long-poll (bearer)

```jsonc
// 200 (immediate or after hold)
{"game_id":"g_…","events":[ /* EventOut objects, seq ascending */ ],"last_seq":47}
// EventOut = the JSON from §10.1, exactly as stored (audience already filtered to public+viewer).
```

#### `POST /games/{id}/decisions` — submit any seat decision (bearer)

One endpoint for every player-originated move, discriminated by `kind`:

```jsonc
// 1) turn action (your turn; "claim-and-act" included via character)
{"kind":"act","action":{"type":"assassinate","character":"assassin","target_id":"s_c2e5a1b8f0d3"}}
//    type ∈ income|foreign_aid|coup|tax|steal|assassinate|exchange ; character required for
//    tax/steal/assassinate/exchange (the claim), optional-but-ignored for base actions; target_id
//    required for coup/steal/assassinate.

// 2) response to the currently open window (window_id is implicit — exactly one is open; a
//    supplied mismatched window_id is rejected) [A16 applies]
{"kind":"respond","response":{"type":"counterclaim","character":"contessa"}}
//    type ∈ pass|counterclaim|challenge

// 3) answer to a pending single-seat decision addressed to you. Field names match the request
//    events' / snapshot's `current.decision` options (§10.1); flip-choice index semantics in §5.4.
//    flip choice — one of the option identifiers from `card_flip_choice_requested.options`:
{"kind":"choose","decision_id":"d_6b…","choice":{"flip_choice":"face_down_0"}}
//    exchange finish — complete wire shape for ALL three outcomes:
//    (a) keep neither: both drawn cards return to the Court Deck, hand unchanged:
{"kind":"choose","decision_id":"d_5c…","choice":{"exchange_finish":{"no_swap":true}}}
//    (b)/(c) keep one of the drawn cards (`drawn_index`: 0 = left, 1 = right): that card joins your
//    hand; the OTHER drawn card and exactly one of your face-down hand cards — the one named by
//    `hand_option` (e.g. "face_down_1", positional per §5.4) — return to the Court Deck as an
//    unordered set (their order is never revealed):
{"kind":"choose","decision_id":"d_5c…","choice":{"exchange_finish":{"drawn_index":0,"hand_option":"face_down_1"}}}
//    All three are accepted with the 202 shape below; other seats learn of the outcome only via
//    the public `exchange_completed` event (which hand cards changed is private, §8).

// 202 response (accepted, queued for resolution)
{"accepted":true,"event_seq":48,"request_id":"9f2c…"}
```
Errors: `not_your_turn`, `already_resolved` (`"the window has already been resolved"`),
`not_eligible`, `invalid_payload`, `game_over`. A 202 for `respond`/`choose` means *accepted*, not
*final outcome* — the outcome arrives as events on the poll stream.

#### Forfeit votes (bearer, human seats only)

```jsonc
// POST /games/{id}/forfeit-votes        → 201 {"vote_id":"v_8d…","majority_needed":2}
{"target_seat_id":"s_c2e5a1b8f0d3"}
//   errors: not_found (no such seat), invalid_payload, vote_closed→ code "already_resolved" if a
//   vote is already active [A15], target_not_active ("you cannot target an eliminated player"),
//   self_target ("you cannot target your own seat", §7).

// POST /games/{id}/forfeit-votes/{vote_id}/ballots   → 200 {"vote_id":"v_8d…","closed":false}
{"vote":"yes"}   // or "no"; errors: vote_not_found, vote_closed (already resolved — 409), not_a_voter
```

#### `GET /debug/games/{id}` — full state dump (COUP_DEBUG-gated)

Served **only when `COUP_DEBUG=1`**; otherwise **404** `[ASSUMPTION A10]`, and unauthenticated (it
is a local operator tool; the gate is the feature). Returns:

```jsonc
{"game_id":"g_…","created_at":"…",
 "config":{"driver_mode":"heuristic","poll_hold_ms":25000,"window_settings":{…}},
 "status":"in_progress","turn_id":7,
 "seats":[{"id":"s_7f3a9c2e1b4d","name":"Marta","type":"human","coins":6,
           "face_down":["duke","ambassador"],          // ALL hands — every seat
           "face_up":[],"active":true,
           "driver":{"mode":"heuristic","conversation_messages":0,"in_flight_decision":null}},
          {"id":"s_c2e5a1b8f0d3","name":"Vito","type":"llm","coins":3,
           "face_down":["contessa","duke"],"face_up":["captain"],"active":true,
           "driver":{"mode":"openai","conversation_messages":14,"in_flight_decision":
                     {"kind":"window_response","window_id":"w_1e0d…","started_at":"…"}}}],
 "court_deck":["tax-card-internal-ids…"],              // actual deck composition (deck is public in size only; composition here for debugging)
 "open_window":{"window_id":"w_1e0d…","opened_at":"…","responses":{…},"deadline_at":"…"},
 "pending_decisions":[{"decision_id":"d_5c…","kind":"exchange_finish","seat_id":"s_…"}],
 "active_forfeit_vote":null,
 "events":{"public":[ …all public events… ],
           "private":[{"seat_id":"s_7f3a9c2e1b4d",…}, …]} }   // full private history, unfiltered
```

---

## 12. Observability (day one — the owner debugs from logs alone)

- **`slog` structured logging**, handler chosen by `COUP_LOG_FORMAT` (`text` default, or `json`).
  One logger per component; **every log line carries the consistent context fields**
  `game_id`, `turn_id`, `event_seq`, `player_id`, `driver` (each present when applicable — e.g. a
  `window_opened` line has game/turn/seq + player=null until responses; access logs carry request id
  instead of turn data).
- **Every state transition is logged with before/after deltas** of the affected fields (coins,
  cards, eligibility). Canonical examples (JSON handler):

```json
{"level":"INFO","msg":"state","game_id":"g_…","turn_id":7,"event_seq":44,"player_id":"s_c2e5a1b8f0d3","driver":"openai",
 "transition":"action_declared","action":"assassinate","target":"s_7f3a9c2e1b4d",
 "coins_before":{"s_c2e5a1b8f0d3":3},"coins_after":{"s_c2e5a1b8f0d3":0}}

{"level":"INFO","msg":"state","game_id":"g_…","turn_id":7,"event_seq":49,
 "transition":"card_flipped","player_id":"s_7f3a9c2e1b4d","card":"contessa","reason":"assassinate",
 "face_down_before":2,"face_down_after":1}
```

- **Full decision lifecycle logging** for every window and single-seat decision:
  1. issued — `window_opened` / decision requested: eligible seats, settings, deadline;
  2. each response — seat, kind, elapsed since window open (LLM responses also log total inference
     wall-time); heartbeats while still waiting `[ASSUMPTION A11]`: one **INFO** line every 30 s
     listing which eligible seats have not yet responded;
  3. resolution — close reason, decider seat, total elapsed, cascade effects (each effect logged as
     its own `state` transition), and for cancellations: one INFO line per cancelled in-flight LLM
     inference (`msg":"llm_cancelled"` with seat, window, driver, reason).
- **HTTP access log** (one line per request): request ID, method, path, player seat (from token;
  `-` for unauthenticated), status, latency_ms. Emitted at INFO via a chi middleware.
- **LLM client logging** per call: `model`, `latency_ms`, `attempt`, tool calls issued (names +
  args), outcome (`candidate`|`re_prompted`|`fallback`|`infra_error_retry`); backoff entries
  (`msg":"llm_backoff"`, with next delay); cancellations; and — **only when `COUP_DEBUG=1`** — the
  model's `reasoning_content` (truncated, A12) as a dedicated field. No log line ever contains
  another seat's face-down cards except the debug endpoint's scope: driver logs may contain their
  OWN seat's hand (the prompt includes it) but never other seats'.
- Log levels: INFO = all of the above in normal operation; DEBUG = per-event batch contents, full
  tool payloads, heuristic driver's random draws.

---

## 13. Identifiers & misc conventions `[ASSUMPTION A3]`

| Kind | Prefix | Format | Used for |
|---|---|---|---|
| game | `g_` | 12 hex chars (48-bit crypto-random) | games, URLs |
| seat | `s_` | 12 hex | seats within a game (globally unique enough) |
| window | `w_` | 12 hex | response windows |
| decision | `d_` | 12 hex | single-seat decisions |
| vote | `v_` | 12 hex | forfeit votes |
| token | — | 64 hex (32 bytes, crypto-random) | human seat bearer tokens |

IDs are opaque; no information may be encoded in or derived from them by clients.

---

## 14. Test plan (normative — the suite mirrors this section)

### 14.1 Domain unit tests (`domain/*_test.go`, plain Go, no I/O) — keyed to the edge-case list (§15)

Each numbered edge case below becomes at least one named test; invariants §3.2 are asserted by a
shared table-driven invariant checker run after every transition in all of these tests:

| # | Edge case (from §15) | Test name (normative) | Asserts |
|---|---|---|---|
| 1 | Failed Contessa bluff vs Assassinate, defender had 2 cards | `TestBustedContessaBluff_TwoCards_EliminatedInOneSequence` | defender loses both cards in one cascade; attack proceeds; defender eliminated; attacker's 3 coins sunk; exactly one window ever open |
| 2 | Same, defender had 1 card | `TestBustedContessaBluff_OneCard_PenaltyEliminates_AssassinationFizzles` | penalty flips last card ⇒ eliminated by penalty; `action_fizzled(target_eliminated)`; no second flip exists to hit |
| 3 | Mandatory Coup at exactly 10 coins | `TestMandatoryCoup_AtExactlyTen` / `_AtEleven` / `_NotAtNine` | at 10/11 every non-coup action rejected with the ruling-mandated message (rules.md Ruling 1; §4.5 string); coup legal (has target); at 9 any action legal |
| 4 | Steal from a seat with 0 / 1 coin | `TestSteal_ZeroCoins_InvalidTarget` / `_OneCoin_TakesOne` | 0-coin seat not targetable (`not enough`/no-coins error, §4.5); 1-coin target yields exactly 1 (min(2,1)) |
| 5 | One non-PASS response closes the window for all | `TestWindow_FirstNonPass_ClosesForAll_SeatsCancelled_LateRejected` | late second responses → rejection; decider recorded; no window reopen |
| 6 | Nested block-challenge cascade, no re-opened windows | `TestNestedBlockChallengeCascade_NoReopen` | exactly one innermost window at all times (invariant checker); original claim never re-windowed after settle |
| 7 | Elimination reveals both cards publicly | `TestElimination_PublishesBothCards` | `eliminated` event carries full card list; face-up lists consistent in subsequent snapshots |
| 8 | Exchange at minimum deck size (6 players, deck = 3) | `TestExchange_MinDeckSize_ThreeCards` | legal; after exchange deck back to 3 (net-zero invariant); deck never reaches 0 in any 6-player scenario |
| 9 | Forfeit vote resolves a stuck LLM seat mid-window | `TestForfeitVote_StuckLLM_MidWindow` | window auto-closes (`auto_no_responders`) or re-scopes; in-flight driver ctx cancelled (fake driver asserts cancel); game continues with remaining seats |
| 10 | Two-player forfeit = win | `TestForfeitVote_TwoPlayers_Win` | yes-majority ⇒ opponent eliminated ⇒ `game_over` for initiator |
| 11 | Coins unbounded, no treasury cap | `TestCoins_Unbounded_NoTreasuryCap` | repeated income/steals beyond 50 coins (physical tray size) succeed; counts exact |
| 12 | Mandatory reveal — no refusal path (rules.md Ruling 7) | `TestChallenge_MandatoryReveal_PenaltyOnlyWhenAbsent` | claimant holding C ⇒ `claim_succeeded` + `card_flipped(successful_reveal)` + shuffle/redraw; claimant not holding C ⇒ `claim_failed` + penalty flip; no event or state admits a refusal outcome |

Plus the standing rule tests: challenge reveal/shuffle/redraw net-zero; sunk costs on voided
offensive claims; "no block" semantics on failed defensive claims generally (parametrized over
all three blockable situations); elimination liveness re-checks at effect time.

### 14.2 Application-layer tests (fake driver + fake clock, composition-level)

- **Window termination matrix — all four combinations** (`require_all` × timeout ∈ {none, T}) as a
  table-driven suite with the fake clock driving deadlines; asserts close reason, auto-pass bookkeeping,
  and that LLM seats always get answered (fallbacks). `[ASSUMPTION A7]`'s always-pass heuristic is
  exercised here via the fake driver's pass-forever script.
- Stale-response rejection after each close reason; per-seat once-ness (A16); cancellation counts on
  window close (fake driver records ctx cancels — exactly one cancel per non-deciding LLM seat).
- Driver retry/fallback path: fake driver scripted to return illegal candidates K+1 times ⇒ safe
  default applied, game never stalls (this is the **no-permadeath property** at app level).
- Forfeit vote lifecycle incl. majority-close-early, non-voter rejection, target-already-gone void.
- Long-poll behavior against the in-memory EventStore with fake clock: immediate return, hold
  expiry empty-200 (A1), privacy filtering (private events never leak across seats).

### 14.3 End-to-end tests over real HTTP (`httptest` — localhost ephemeral ports only)

- **Two scripted "human" protocol clients** play a full game to completion against each other
  (both human seats, window settings `require_all=true` + short timeout so windows deterministically
  close): asserts the full event catalogue appears in a coherent sequence, both tokens work,
  snapshots stay privacy-correct, and `game_over` arrives. This is the UI-contract test: it consumes
  only §10/§11 shapes.
- **Unattended heuristic-vs-heuristic simulated full games** as the stress / no-permadeath test
  `[ASSUMPTION A13]`: 50 games, 4–6 seats with mixed human (scripted auto-client) and `heuristic`
  LLM seats, window settings `require_all=true`, timeout=null (heuristics always respond ⇒
  termination guaranteed without fake-clock dependence); each game must reach `game_over` within a
  2-minute wall budget; suite asserts zero hung windows, zero panics, invariants hold per transition.
- **Port discipline:** no test dials any fixed port; the only listener is `httptest`. (CI guard: a
  grep-based check that test code references neither `8080` nor hardcoded hostnames.)

---

## 15. Edge-case list (normative input to §14)

Re-stated as the canonical checklist (each item ⇒ ≥ 1 named test in §14.1):

1. Failed Contessa bluff on an Assassinate with **2 cards** ⇒ defender eliminated in one sequence
   (penalty + proceeding attack).
2. Same bluff with **1 card** ⇒ bluff fails, defender eliminated **by the penalty**, and the
   original assassination **fizzles** (target has no influence when it resolves).
3. **Mandatory Coup at exactly 10** coins (superset reading: ≥10; 9 is free choice).
4. **Steal from a seat with 0 or 1 coin**: 0-coin seat not targetable; 1-coin yields `min(2,coins)=1`.
5. **One non-PASS response closes the window for all** — stale late responses (human or LLM) are
   rejected and discarded; in-flight LLM inference cancelled + logged.
6. **Nested block-challenge cascade with no re-opened windows** — exactly one innermost window at
   any moment; a settled claim never reopens.
7. **Elimination reveals both cards publicly** (face-up lists, `eliminated` event).
8. **Exchange at minimum deck size** — 6 players, deck of 3: legal, net-zero, deck stays ≥ 3 always.
9. **Forfeit vote resolving a stuck LLM seat mid-window** — window re-scopes/auto-closes per §5.2.5;
   driver cancelled; game continues.
10. **Two-player forfeit = win.**
11. **Coins unbounded** — no treasury cap enforced; counts tracked for display only.
12. **Mandatory reveal (rules.md Ruling 7)** — a challenged claimant who holds the claimed character
    MUST reveal it; the penalty branch fires only when the character is absent from the hand. There
    is no refusal path anywhere in the state machine or event stream.

---

## 16. Assumptions register

Every `[ASSUMPTION]` marker in this spec, consolidated:

| ID | Assumption (simplest option consistent with locked decisions) |
|---|---|
| **A1** | Long-poll hold default `25s`, clamped to `[1s, 30s]`; on hold expiry the endpoint returns **200 with an empty `events` array** (no timeout error code); finished games signal only via `game_over`/snapshot — no special terminal status code. |
| **A2** | Human bearer tokens are 64 hex chars (32 crypto-random bytes), shown exactly once in the create response, not revocable in v1; no auth needed for LLM seats (none of their traffic is client-initiated). |
| **A3** | ID scheme: `g_`/`s_`/`w_`/`d_`/`v_` + 12 hex chars; token = 64 hex (§13). |
| **A4** | LLM decision budgets: N = 3 internal re-prompts on tool-call-less responses (openai driver), then kind-safe default; K = 3 application-level retries after illegal candidates, then the same kind-safe defaults (turn ⇒ `income()`, window ⇒ `pass`, single-seat choice ⇒ engine default: first flip option / no-swap). No decision can ever permadeath a game. |
| **A5** | LLM transport: per-request client timeout 120 s; backoff delay = full-jitter(`min(30s, 500ms × 2^attempt)`), retried indefinitely with one WARN per retry; prompt "recent events" tail capped at the last 10 public+own events to bound prompt size. |
| **A6** | `COUP_LLM_MODEL` is required when `COUP_DRIVER_MODE=openai` (startup fails fast if empty); no other LLM client knobs exist in v1 (API defaults for temperature etc.). |
| **A7** | Heuristic driver: deterministic per game (randomness seeded from a hash of the game id), weighted greedy+random turn play as detailed in §6.2, and **always `pass`** in response windows; engine-safe defaults for single-seat choices. |
| **A8** | Game-creator window settings omitted at creation ⇒ `require_all_responses=false`, `response_timeout=null` (open-ended FCFS — the documented philosophy default). |
| **A9** | Rejection outcomes of HTTP submissions are stored as private `rejection` events with their own `event_seq` on the offending seat's stream (and logged), so a client can reconcile its rejected POSTs from the event history alone. |
| **A10** | `GET /debug/games/{id}` returns **404** (not 403) when `COUP_DEBUG≠1` and requires no authentication; the env gate is the security boundary (local operator tool). |
| **A11** | Still-waiting window heartbeats are logged at **INFO every 30 s** per open window, listing unresponsive eligible seats. |
| **A12** | `reasoning_content`, when present and `COUP_DEBUG=1`, is logged truncated to the first 4 KB; never stored in conversation history. (Field availability to be verified per-model at implementation time.) |
| **A13** | Unattended stress suite: 50 heuristic-participating games with `require_all=true` and no timeout, each required to reach `game_over` within a 2-minute wall budget. |
| **A14** | Blank seat names default to `"Vito"`, `"Sonny"`, `"Carmine"`, `"Rocco"`, `"Salvatore"`, `"Bruno"` (LLM seats, cycled in seat order) and `"Player N"` (human seats). |
| **A15** | Forfeit voting: at most one active vote per game; votes against eliminated/invalid targets rejected; if the vote's target is eliminated by play while a vote is pending, the vote voids with no effect; a single-seat decision addressed to a just-forfeited seat is voided and resolved with its engine default. |
| **A16** | Each seat may respond at most once per open window; later responses (open or closed) are rejected (`"you have already responded"` / `"the window has already been resolved"`). `window_id` in `POST /decisions` is implicit (exactly one window open); a supplied mismatched id is rejected. |
| **A17** | If the client omits `X-Request-Id`, the server generates 32 hex chars; always echoed back and logged. |
| **A18** | **RESOLVED by owner ruling (2026-09-22) — no longer an assumption.** Reveal-or-penalty confirmed as *mandatory reveal*: a challenged claimant who holds the claimed character MUST reveal it; "choose not to reveal" does not exist as an option, and the penalty applies only when the character is absent from the hand. Now normative as [rules.md Ruling 7](../rules.md#rulings). (A18's Coup half — the ≥10 superset reading — was already confirmed by rules.md Ruling 1; this row's earlier pointer to "Ruling 2" was stale under the current rules.md numbering.) |

---

*End of specification. Implementation starts from §2 package layout; anything not specified here
and not derivable from `rules.md` must be resolved as an `[ASSUMPTION]` entry (extend this table)
rather than decided silently in code.*

# ai-coup

Coup — the five-character bluffing card game — recreated as a live server you play
alongside AI agents. Humans and LLM-driven seats sit at the same table: humans act
through a long-polling HTTP API and a vanilla-JS UI (no build step, no framework),
while server-managed seats are driven by a pluggable `MoveDriver` — a built-in
deterministic heuristic or an OpenAI-compatible tool-calling client pointed at your
own model server. The rules engine is pure Go (cards, claim/challenge resolution,
response windows, forfeit votes); every decision a driver produces is validated by
the engine, and bounded retries plus kind-safe defaults guarantee no game can be
permadeath'd by a bad model (§6.4).

## Build / run / test

```sh
go build ./...                 # compile everything (one third-party dep: chi)
COUP_PORT=8090 COUP_DRIVER_MODE=heuristic go run .   # start the server
go test ./...                  # full suite (add -race for the final gate)
```

The server binds `COUP_PORT` (default `:8090`) and serves both the API and the UI
from the same origin. **Port 8080 is forbidden to this system as a bind target** —
a `llama-server` owns it in dev and must never be disturbed; startup fails fast if
you try (and no test may dial it either). The *outbound* default LLM base URL
`http://localhost:8080/v1` is a different thing: it is the HTTP **client** address
the `openai` driver dials.

### Configuration (closed set — there are no other knobs)

Each env var has a matching command-line flag (`-port`, `-poll-hold`, `-llm-base-url`,
`-llm-model`, `-driver-mode`, `-debug`, `-log-format`); **the flag wins over env**.

| Env var | Meaning | Default | Notes |
|---|---|---|---|
| `COUP_PORT` | TCP listen port | `8090` | Must never be set to `8080`; startup fails fast if it is. |
| `COUP_POLL_HOLD` | Long-poll hold (duration string) | `25s` | Clamped to `[1s, 30s]`. |
| `COUP_LLM_BASE_URL` | OpenAI-compatible API base for the `openai` driver | `http://localhost:8080/v1` | Client-side URL only (see port policy above). |
| `COUP_LLM_MODEL` | Model name sent in chat-completion requests | *(empty)* | **Required when `COUP_DRIVER_MODE=openai`**; startup fails with a clear error if empty. No other LLM knobs exist — API defaults apply. |
| `COUP_DRIVER_MODE` | `MoveDriver` backing LLM seats: `heuristic` \| `openai` | `heuristic` | The `fake` driver is test-only, not a runtime mode. |
| `COUP_DEBUG` | Debug mode (`0`/`1`, also `true`/`false`) | `0` | Gates `/debug/*`, LLM reasoning-content logging, DEBUG-level logs. |
| `COUP_LOG_FORMAT` | `slog` handler: `text` \| `json` | `text` | |

## 60-second quickstart

1. **Humans vs humans.** Run `go run .` and open http://localhost:8090 in a browser.
   In the lobby, set every seat to *Human* (row 1 is locked human) and create the
   game. Tokens appear exactly once at creation — open a second tab, choose *Join*,
   paste the game id plus your one-time seat token, and play: you long-poll events
   and submit decisions (income, claims, challenges, counterclaims, flip choices).
2. **LLM seats in heuristic mode.** Restart with
   `COUP_DRIVER_MODE=heuristic go run .`, create a mixed table from the lobby
   (Human/AI toggles per seat) and watch the AI seats play unattended — income,
   foreign aid, bluffed claims and windows resolve on their own while you respond.
3. **LLM seats with your own model.** Start your `llama-server` (the dev default is
   port 8080, which this system may *dial* but never bind), then run
   `COUP_DRIVER_MODE=openai COUP_LLM_BASE_URL=http://localhost:8080/v1 \
   COUP_LLM_MODEL=<your model> go run .`. Each LLM decision is one tool-calling
   chat completion over the seat's legal-world view; illegal candidates get a
   table-voice rejection fed back as feedback for bounded retries, after which a
   kind-safe default applies. Seats are server-managed — an LLM seat that stalls
   is recovered by forfeit vote, not by a human taking over. Play quality depends
   entirely on the model (small models bluff badly).

## Specs & tests

- `docs/technical-spec.md` — the contract: stack/config (§1), architecture (§2),
  rules engine (§4), response windows (§5), drivers (§6), forfeit votes (§7),
  visibility (§8), events (§10), HTTP API (§11), observability (§12).
- `docs/ui-spec.md` — the UI contract (built strictly from the §10 event model).
- `rules.md` — house rulings, including the load-bearing mandatory-coup rule.

`go test ./...` covers all four layers: domain edge cases (the normative §15 list —
claim/challenge asymmetries, eliminations, coin accounting), application-layer
scenarios against a fake driver + manual clock (the full §5.2 window-termination
matrix, A4 fallbacks, forfeit voting), end-to-end games over real `httptest` HTTP
(two scripted human clients playing to completion, privacy, long-poll semantics),
and a 50-game unattended stress suite with real driver wiring against a scripted
model server.

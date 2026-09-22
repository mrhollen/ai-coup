# ai-coup — Web Client UI Specification

Status: **normative for the frontend.** This document is a complete, implementable design for the
browser client. It is built strictly from `technical-spec.md` (the "tech spec"): every screen,
state, and interaction derives from the endpoint surface in tech-spec §11, the event catalogue in
§10, the window model in §5, forfeit voting in §7, visibility rules in §8, and the descriptive
error catalogue in §4.5/§11.1. **The client invents no server behavior.** Where this document had
to decide something the tech spec leaves open, it is marked `[ASSUMPTION U#]` and collected in
§17 (Assumptions register). The eventual client is a single-page vanilla HTML/CSS/JS app: one
`index.html`, one `app.css`, a small number of flat JS modules, **no frameworks, no build step**.

Companion file: [`mockup.html`](./mockup.html) — a self-contained static render of the game view
mid-action. It is the visual source of truth for look & feel; where this document and the mockup
disagree on pixels, **the mockup wins** (it is what the owner approves).

---

## 1. Purpose, scope, delivery

### 1.1 What the client does

- Create a game (`POST /games`) or join an existing one with a seat token.
- Render the table: 2–6 seats in a ring around a center board; eliminated seats dimmed with their
  face-up cards public; LLM seats badged everywhere a name appears.
- Show the viewer's own hand (two face-down cards, face-up to the viewer only).
- Run one long-poll loop: `GET /games/{id}/events?after={seq}`, apply each batch to local state,
  re-render; immediate re-poll. Handle empty holds, stale/late submissions, and disconnects (§7).
- Present the four decision surfaces — turn action, window response, flip choice, exchange finish —
  plus forfeit-vote initiation and ballot UI, a scrolling table log, and game-over state (§8, §9,
  §10, §11).

### 1.2 Delivery `[ASSUMPTION U1]`

**Recommended: serve the client from the Go server's static file handler at `/`, same origin as
the API.** The client then uses relative URLs (`/games/{id}/…`) — no CORS, no base-URL config, no
mixed-host cookie/auth questions. This is the only delivery mode this spec designs against.

Alternatives (standalone `file://` open, or a separate static host) are out of scope: `file://`
breaks same-origin fetch semantics for the API, and a separate host would require a CORS decision
the tech spec does not make. **Open question for owner/backend:** if a separate host is ever
wanted, the backend must confirm CORS headers; nothing in this spec may change until then.

### 1.3 Client file inventory (for implementation, not part of this deliverable)

| File | Contents |
|---|---|
| `index.html` | Static skeleton: topbar shell, table region, hand strip, log panel, modal root, toast root. All dynamic content rendered by JS. |
| `app.css` | Design tokens (§3) + all component styles. No preprocessor. |
| `state.js` | Local state object, snapshot seeding, event-batch reducer (§7). |
| `poll.js` | Long-poll loop, watchdog, backoff resync (§7.2–7.5). |
| `render.js` | Table/seat/log/hand rendering, LLM badge helper, turn pointer. |
| `decide.js` | The four decision surfaces + forfeit UI (§8–§11), all POST /decisions and /forfeit-votes traffic. |
| `lobby.js` | Create/join forms (§6). |

Module style: ES modules (`<script type="module">`), no bundler, no dependencies.

---

## 2. Screens (inventory)

| # | Screen / route | Entered from | Exits to |
|---|---|---|---|
| S1 | **Lobby — New game** `#/` | fresh load, "New game" button | S3 on 201 |
| S2 | **Lobby — Join** `#/?join` | join tab / pasted pair | S3 after snapshot OK |
| S3 | **Game view** `#/g/{gameId}` | lobby success | S1 via game-over overlay or "Leave" |
| S4 | **Auth-error state** (overlay on S3) | 401 on any seat-scoped call | S2 (rejoin) |

Routing is hash-based (`[ASSUMPTION U15]`): `#/`, `#/?join`, `#/g/{gameId}`. Hash routing keeps
the static handler simple (no server-side route config) and survives full page reloads with zero
backend involvement. The game id in the hash is only a *label* — every call authenticates via the
bearer token; the hash is never trust data.

**Session storage `[ASSUMPTION U2]`:** `(gameId, seatId, token)` per human seat is kept in
`sessionStorage` under key `coup.session.{gameId}`. Rationale: a token grants full rights to a
seat; scoping it to the tab means opening two tabs can play two different seats of the same game
(each tab gets its own seat), and closing the browser drops the credential. Nothing persists
across sessions — resuming always re-bootstraps from snapshot + event replay, which is idempotent
(tech spec §6.1: "closing a browser loses nothing").

---

## 3. Design system (tokens)

### 3.1 Direction

A smoky Italian parlor at night: near-black warm charcoal surfaces, felt-toned table, candle-gold
accents, ivory type. Serif display for anything that should read like the table *speaking* (log
action lines, modal headers, game over); neutral sans for chrome and controls. Two accents only —
gold (agency, money, "your" affordances) and oxblood red (penalties, challenges, danger). LLM seats
are the one cool note: slate-blue badges, deliberately against the warm field so "this seat is a
machine" reads at a glance.

### 3.2 Tokens (drop-in CSS custom properties)

```css
:root {
  /* --- surfaces --- */
  --bg:            #14100e;   /* page: smoky black-brown */
  --bg-panel:      #1d1712;   /* panels: topbar, log, lobby cards */
  --bg-elev:       #251d16;   /* modal panel, tiles, raised */
  --bg-inset:      #0f0c0a;   /* recessed wells (log rows alt, deck circle floor) */

  /* --- text --- */
  --ink:           #ede3d0;   /* primary: warm ivory */
  --ink-dim:       #9c8f7a;   /* secondary / captions (≥4.6:1 on --bg) */
  --ink-faint:     #6e6355;   /* disabled, watermark text only (not body copy) */

  /* --- accents --- */
  --gold:          #c9a227;   /* primary accent: turn pointer, money, dividers */
  --gold-bright:   #e8c15a;   /* focus ring, active states, small gold text */
  --gold-ink:      #1a130c;   /* text ON gold fills (≥7:1 contrast) */
  --red:           #b3402f;   /* danger, challenge, penalty */
  --red-bright:    #d8694f;   /* red on dark for small text (≥5:1) */
  --green:         #4d7c53;   /* success, claim-succeeded, voted-yes fill */
  --green-bright:  #7fae88;
  --llm-bg:        #223041;   /* LLM badge fill (cool slate) */
  --llm-ink:       #a8c3dd;
  --llm-line:      #35495f;

  /* --- lines & fills --- */
  --line:          #3a2f24;   /* hairlines on dark panels */
  --line-strong:   #55452f;   /* borders on raised elements */
  --card-face:     #f3ead6;   /* playing-card face */
  --card-face-line:#d8c9a8;
  --card-back:     #6e2020;   /* card back base (oxblood) */
  --card-back-hi:  #873030;

  /* --- character colors (top stripe / underline accent, one per character) --- */
  --char-duke:        #c9a227;
  --char-assassin:    #b3402f;
  --char-contessa:    #4d7c53;
  --char-captain:     #3f6ea5;
  --char-ambassador:  #7b5ea7;

  /* --- type scale (rem, root = 16px) --- */
  --text-xs:   0.75rem;    /* 12 — badges, kicker labels */
  --text-sm:   0.875rem;   /* 14 — log rows, chips, table chrome */
  --text-md:   1rem;       /* 16 — body, buttons */
  --text-lg:   1.125rem;   /* 18 — modal subheads, seat names in modals */
  --text-xl:   1.5rem;     /* 24 — lobby headings, winner name */
  --text-2xl:  2.25rem;    /* 36 — "GAME OVER" display */

  --font-display: Palatino Linotype, Palatino, "Book Antiqua", Georgia, serif;
  --font-ui: system-ui, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  --font-mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;

  /* --- spacing (4px grid) --- */
  --sp-1: 0.25rem;  /* 4  */
  --sp-2: 0.5rem;   /* 8  */
  --sp-3: 0.75rem;  /* 12 */
  --sp-4: 1rem;     /* 16 */
  --sp-5: 1.5rem;   /* 24 */
  --sp-6: 2rem;     /* 32 */

  /* --- radii --- */
  --r-card: 8px;      /* playing cards */
  --r-chip: 999px;    /* pills, chips */
  --r-panel: 10px;    /* panels, tiles */
  --r-modal: 14px;

  /* --- elevation --- */
  --shadow-1: 0 1px 2px rgba(0,0,0,.4);
  --shadow-2: 0 6px 18px rgba(0,0,0,.45);
  --shadow-modal: 0 24px 64px rgba(0,0,0,.55), 0 0 0 1px var(--line-strong);

  /* --- motion --- */
  --t-fast:   150ms cubic-bezier(.2,0,0,1);   /* hover, press, chips */
  --t-med:    220ms cubic-bezier(.2,0,0,1);   /* modal in/out, panels */
  --t-card:   450ms cubic-bezier(.3,.7,.2,1); /* card flip (face-down → face-up) */
}
```

### 3.3 Global surface treatment

- `body`: background `var(--bg)` with a faint vignette:
  `background-image: radial-gradient(120% 90% at 50% 0%, #1a1410 0%, var(--bg) 60%);`
- Table felt (the game board panel): `radial-gradient(closest-side, #2b2118, #1c1510 78%)`,
  border `1px solid var(--line-strong)`, radius `var(--r-panel)`, inset shadow
  `inset 0 2px 18px rgba(0,0,0,.55)`; plus a 3%-opacity diagonal texture layer
  (`repeating-linear-gradient(45deg, transparent 0 3px, rgba(255,255,255,.03) 3px 4px)`).
- Focus (everywhere): `:focus-visible { outline: 2px solid var(--gold-bright); outline-offset: 2px; }`
- Selection: `::selection { background: #4a3a18; color: var(--ink); }`
- Scrollbars (WebKit, cosmetic only): 10px thumb `var(--line-strong)` on `var(--bg-inset)`.

### 3.4 Button styles (four states of the house style)

| Class | Fill / border | Text | Use |
|---|---|---|---|
| `.btn-primary` | bg `var(--gold)`; hover `var(--gold-bright)`; press: translateY(1px) | `var(--gold-ink)`, 600 weight | the "do it" action of any surface (e.g., Create game, Vote YES, keep-left confirm) |
| `.btn-outline` | transparent; border `1px solid var(--line-strong)`; hover border `var(--gold)`, text → `var(--gold-bright)` | `var(--ink)` | secondary actions (Challenge, Pass, Cancel, Vote NO) |
| `.btn-danger` | bg `transparent`; border `1px solid rgba(179,64,47,.55)`; hover bg `rgba(179,64,47,.15)` | `var(--red-bright)` | explicitly hostile (Challenge in window modal uses this) |
| `.btn-ghost` | no border; text only, `var(--ink-dim)`; hover `var(--ink)` | — | topbar utilities (Forfeit vote, Leave) |

Common: height 40px, radius 8px, padding `0 var(--sp-4)`, font `--font-ui` 15px/600, letter-spacing
`.02em`; icon+label gap 8px. Disabled: opacity `.45`, `cursor: not-allowed`, no hover styles, and a
`title` attribute explaining *why* (table voice where the server already speaks: e.g. coup-mandatory
disables carry `title="you must take a coup while you have ten or more coins"`).

---

## 4. Global components

### 4.1 Topbar (S3 only)

Height 56px, bg `var(--bg-panel)`, bottom border `1px solid var(--line)`. Left→right:

1. Wordmark: "COUP" in `--font-display`, small-caps via `font-variant: small-caps`, color
   `var(--gold-bright)`, 20px; hairline divider `1px × 24px var(--line-strong)`.
2. Game chip: game id truncated to 8 hex chars (`g_0a1b2c`), `--font-mono` 12px, color
   `var(--ink-dim)`, pill bg `var(--bg-inset)`, copies full id on click (info toast "id copied").
3. Connection dot: 8px circle; live = `var(--green)` with 2s soft pulse animation, reconnecting =
   `var(--red)` solid + label "reconnecting…" in `--text-xs`. `aria-hidden` dot + visually-hidden
   text mirroring state (`role="status"`).
4. Spacer.
5. "Forfeit vote" `.btn-ghost` (opens §10.5) — hidden/disabled when `forfeit_vote` is active, you
   are eliminated, or game is over.
6. Viewer chip: "MARTA" with LLM badge rules of §4.2 (never LLM for the viewer), gold underline
   accent 2px, `--text-sm`.

### 4.2 LLM badge — one helper, every name

Every rendered seat name goes through `renderName(seat)`: `<span class="seat-name">Vito
<span class="llm-badge">AI</span></span>`. Badge: inline-block pill, font 10px/700 letter-spacing
`.08em`, color `var(--llm-ink)` on `var(--llm-bg)`, border `1px solid var(--llm-line)`, radius
`var(--r-chip)`, padding `1px 6px`, margin-left 6px, vertically centered. It appears **everywhere a
name appears**: seat tiles, log rows (inline after the name), target pickers, response chips,
forfeit voter rows, game-over standings, "decided by X" lines, and the winner line. Human seats get
no badge; the viewer additionally gets a gold `YOU` tag on their tile only (§4.3) — tags never mix:
a seat is human or LLM per `seat.type`, full stop.

### 4.3 Seat tile

Container `.seat`: width 168px, padding 10px 12px, bg `var(--bg-elev)`, border
`1px solid var(--line-strong)`, radius `var(--r-panel)`, shadow `--shadow-1`. Positioned by §5.3
ring geometry (centered on its anchor via `transform: translate(-50%,-50%)`).

Interior, top→bottom (gap 6px):

1. **Name row**: name (`--font-display` 15px, small-caps, `var(--ink)`; LLM badge per §4.2) +
   right-aligned `YOU` tag for the viewer: gold text 10px/700 letter-spacing `.1em`.
2. **Coins row**: coin glyph (CSS circle 12px, radial-gradient gold: `#e8c15a → #8a6d1a`, border
   `#5e4a12`) + count in `--font-mono` 14px, color `var(--gold-bright)`. Eliminated seats render
   the same but dimmed and with `title="coins are inert — this seat is out"`.
3. **Card row**: gap 6px, min-height 52px.
   - Face-down card: back design §4.5 at 34×48. Count = face-down count (1 or 2 while active).
   - Face-up card: chip 34×48, bg `var(--card-face)`, radius 6px, border-bottom stripe 3px in the
     character color (§3.2), center monogram letter in `--font-display` 16px
     (D / A / C / C / A) — full name lives in `title` and `aria-label` ("Marta's face-up card:
     Captain"). The two Cs are disambiguated by stripe color (red = Assassin, green = Contessa).
   - Eliminated tile: both cards rendered face-up; whole tile gets class `.eliminated`:
     `opacity .55; filter: saturate(.6)`, plus a small "OUT" tag (10px, letter-spacing `.12em`,
     color `var(--ink-dim)`) replacing the `YOU` slot. Cards remain fully legible (invariant: they
     are public knowledge).

**Turn pointer:** the seat whose turn it is gets `.acting`: box-shadow
`0 0 0 2px var(--gold), 0 0 18px rgba(201,162,39,.35)`, and its name row appends a 2px gold left
rule. While you are eligible in an open window your tile gets `.decision` (dashed 1px
`var(--gold-bright)` ring, subtle 2s opacity pulse) — distinct from the turn pointer so "the table
is waiting on someone" reads separately from "it is someone's turn."

### 4.4 Cards — shared design language

- **Hand cards** (viewer only): width `min(24vw, 96px)`, aspect-ratio `5/7`, bg `var(--card-face)`,
  radius `var(--r-card)`, border `1px solid var(--card-face-line)`, inner keyline
  (`box-shadow: inset 0 0 0 4px var(--card-face), inset 0 0 0 5px rgba(0,0,0,.08)`), top stripe 6px
  character color. Character name centered, `--font-display` 13px small-caps, ink `#241c14`. A
  position numeral ("1"/"2") in the top-right corner: 11px `--font-mono`, `rgba(0,0,0,.35)` — this
  is the index into `your_face_down_cards` that flip-choice and exchange-finish decisions reference
  (§9.2/§9.3). `title="Your card 1 (face_down_0)"`.
- **Card back** (seat face-down cards, any size): bg `var(--card-back)`; pattern: two crossed
  `repeating-linear-gradient`s at ±45°, lines `rgba(255,255,255,.06)`, 7px period; inner border
  `inset 0 0 0 3px var(--card-back-hi), inset 0 0 0 4px rgba(0,0,0,.35)`; radius 6px.
- **Drawn-card reveal** (exchange modal §9.3): hand-card design but 72×101, same stripes/numerals
  ("L"/"R" instead of numerals).
- **Flip animation:** when a `card_flipped` event moves one of *your* face-down cards to face-up,
  the affected tile card animates `transform: rotateY(90deg→0)` over `--t-card`, back → face. No
  animation for other seats (their chips simply appear) — keeps the log-to-state mapping obvious.
  Suppressed entirely under `prefers-reduced-motion` (§15).

### 4.5 Table center board

Center of the felt: 168px circle, `background: var(--bg-inset)`, double ring: outer border
`1px solid var(--line-strong)`, inner ring via `box-shadow: inset 0 0 0 6px var(--bg-inset),
inset 0 0 0 7px var(--gold)` at 45% alpha (use `rgba(201,162,39,.4)`). Content is phase-driven
(`.center` text block, centered, max-width 150px):

| Phase (`snapshot.current.phase`) | Main line (`--font-display` small-caps) | Sub line (`--text-xs`, `var(--ink-dim)`) |
|---|---|---|
| `turn_action` | "{NAME}'S TURN" (name colored gold if it's you) | "court deck · N cards" |
| `window_open` | "WINDOW OPEN" in `var(--gold-bright)` | "{kind word}" — `challenge` → "a claim hangs in the air"; `counterclaim_challenge` → "the block itself is challenged"; plus "court deck · N cards" |
| `single_seat_decision` | addressed to you: "CHOOSE A CARD" / else "WAITING ON {NAME}" | "court deck · N cards" |
| `finished` (or status ≠ in-progress) | "{WINNER} WINS" gold | final deck count, muted |

The turn pointer ring on the tile (§4.3) and this center block are driven by the same state field;
they never disagree.

### 4.6 Table log panel

Region `.log`: header row ("TABLE LOG" kicker, `--text-xs` letter-spacing `.14em`, `var(--ink-dim)`;
right side: a "follow" toggle — default ON) + scrolling list (`max-height` per §5.2 layout), bottom-
anchored auto-scroll (auto-scroll only while the follow toggle is on *and* the user's scroll position
is within 40px of the bottom — standard feed behavior; new entries otherwise show a "new entries"
jump pill at the panel's bottom edge).

Row anatomy: `[time] [icon] [text]`. Time: `HH:MM` from event `ts`, `--font-mono` 11px,
`var(--ink-faint)`, fixed 44px slot. Icon: a 20×20 inline-SVG-less glyph box — one of: none, ◆
(declaration), → (coins), ✕ (fizzled/failed), ⬛ face-up chip (flipped/eliminated), ⚖ (forfeit),
⚠ (rejection). Text per §12's per-event templates. Row padding 6px 8px; alternate rows
`var(--bg-inset)` at 40% alpha. Special row classes:

- `.row-divider` — turn dividers and game-created line: centered, small-caps, letter-spacing
  `.18em`, `--text-xs`, flanked by 1px rules (`flex` with two `var(--line)` stretches).
- `.row-private` (only ever on the viewer's own stream): left border `2px solid var(--gold)`, bg
  `rgba(201,162,39,.07)`; prefix word "PRIVATE ·" in gold `--text-xs`.
- `.row-warning` (`rejection`): left border `2px solid var(--red)`, bg `rgba(179,64,47,.08)`, text
  in `var(--ink)`.
- `.row-banquet` (`game_over`): centered display line, `--text-lg` small-caps gold, rule above/below.

**Log retention:** cap the DOM at 400 rendered rows; on overflow, drop oldest and insert a single
static dim row "… earlier entries trimmed …" `[ASSUMPTION U13]`. The full history is always
recoverable by re-running `events?after=0` (e.g., via "Leave & rejoin").

### 4.7 Toasts

Stack, bottom-center above the hand strip (or bottom of viewport in lobby), column-reverse, gap 8px,
max 3 visible (older ones are discarded, not queued — toasts are ephemeral feedback; durable facts
live in the log). Width `min(92vw, 400px)`. Anatomy: label line + message line.

- **Error toast** (all server rejections): bg `#2a1512`, border-left `3px solid var(--red)`,
  radius 8px, shadow `--shadow-2`. Label line: code-mapped small-caps kicker, 10px letter-spacing
  `.14em`, color `var(--red-bright)` (mapping in §13). Message line: **the server's `error.message`
  verbatim**, `--font-display` italic 15px, `var(--ink)`. The strings from tech spec §4.5 are
  written to read like the table talking; the UI must not paraphrase, prefix, or translate them —
  they are the copy. Auto-dismiss 8s, manual ✕ always. `role="alert"`, announced once on entry.
- **Info toast**: bg `var(--bg-elev)`, border-left `3px solid var(--gold)`; kicker "TABLE", gold;
  message regular UI font. Uses: connection restored, id copied, vote tallied results are *not*
  toasted (the log + any open modal carry that). Auto-dismiss 5s.

### 4.8 Modal shell

All modals share one shell `.modal` inside `#modal-root` (a full-viewport fixed layer): backdrop
`rgba(10,8,6,.72)` + `backdrop-filter: blur(2px)`; panel centered, width per §5.2, max-width
94vw, bg `var(--bg-elev)`, border `1px solid var(--line-strong)`, radius `var(--r-modal)`, shadow
`--shadow-modal`; padding 0 (each modal supplies its own sections: header / body / footer, each
padding `var(--sp-5)`; hairlines between). Entry: opacity 0→1 + scale .97→1 over `--t-med`; exit
reverses. Panel structure contract:

```
.modal-panel
  .modal-kicker      (small-caps gold label, e.g. "THE WINDOW", "SINGLE CHOICE")
  .modal-title       (font-display 18px; the situation's headline)
  .modal-body        (variable per modal, §9/§10)
  .modal-footer      (button row, right-aligned, gap 8px; or a status line when locked)
```

---

## 5. Game view layout (S3)

### 5.1 Grid by breakpoint

| Viewport | Layout |
|---|---|
| ≥ 1120px | `display:grid; grid-template-columns: minmax(0,1fr) 400px; grid-template-rows: auto 1fr auto; gap: var(--sp-4); padding: var(--sp-4);` → topbar spans both columns; felt panel left; log right (full column height, internal scroll); hand strip spans bottom-left cell above the fold? No — hand strip sits inside the left column under the felt. See 5.2. |
| 720–1119px | single column: topbar / felt (max-width 640px, centered) / hand strip / log (max-height 38vh). |
| < 720px | single column; ring geometry replaced by **strip mode** (§5.3); modal width `calc(100% − 24px)`. |
| 380–479px (floor supported down to ~380px) | hand cards shrink (`min(22vw, 88px)`), action console buttons wrap onto two rows, seat strip tiles become 140px wide single row of scrollable chips if needed. All text scales only via the fixed rem scale — no font-size changes; density drops instead (log shows time + text, icon column hidden below 560px). |

### 5.2 Felt panel internals

`.felt` is `position: relative`, square: `width: min(100%, 640px); aspect-ratio: 1/1; margin-inline:
auto;` (in the ≥1120 layout it may stretch to column width, capped at 720px). Children: `.center`
(circle, absolute-centered), six slot positions for seat tiles (§5.3), and — absolutely positioned
bottom-center *inside* the felt, offset up so it never collides with a bottom-anchored tile on ≥720
screens — nothing else: **the hand strip lives outside/below the felt**, as a sibling row
(`.hand-row`): centered flex, gap 12px; caption above it "YOUR HAND" (`--text-xs`, letter-spacing
`.14em`, `var(--ink-dim)`, centered). The two (or one) hand cards sit here per §4.4. When you hold
one card (one already flipped face-up), the single card is centered and its position numeral is
suppressed (no choice exists; auto-flip applies).

### 5.3 Ring geometry (the "seats around a center")

Seats are placed in seat order = turn order on a regular N-gon, first seat at 12 o'clock:

```
R = 41%          /* radius as % of felt box; tiles centered on anchor */
θ_i = i · (360°/N)          for N ≥ 3
anchor_x(i) = 50 + R·sin(θ_i)      (%)
anchor_y(i) = 50 − R·cos(θ_i)      (%)   (screen y grows downward; slot 0 at top)

N = 2: anchors are (18%, 50%) and (82%, 50%).
```

Slot styles are generated once in JS as CSS custom properties on the felt
(`--s{i}x`, `--s{i}y`) consumed by `.seat[data-slot="i"] { left: var(--sx); top: var(--sy); }` —
with **inset clamping** so a tile is never clipped at any felt size
(`m = half tile width + 6px ≈ 90px`, same for height with the taller vertical value):

```css
.seat { position:absolute; transform: translate(-50%,-50%);
        left: clamp(84px, var(--sx), calc(100% - 84px));
        top:  clamp(72px, var(--sy), calc(100% - 72px)); }
```

On a wide felt the anchors sit on the pure polygon; on a tight one (or many seats) tiles tuck
inward instead of spilling off the felt. Tiles are `position:absolute` with
`transform: translate(-50%,-50%)`.

**Strip mode (< 720px):** the media query overrides all slots to
`position: relative; transform:none; inset:auto`, and the felt loses its square aspect (becomes
`aspect-ratio: auto; min-height: 240px`; `.center` circle shrinks to 128px and re-centers). The
seat tiles render in document order = seat order as a centered flex-wrap row above the center board:
`.seats { display:flex; flex-wrap:wrap; justify-content:center; gap: var(--sp-3); }`, tile width
140px. The turn pointer / decision rings work unchanged (box-shadow only). **Tile content never
changes between modes** — only position.

### 5.4 Turn-action console (your turn)

Not a modal — a docked strip between the felt and the hand row (it is not interruptive: while it is
shown, no window can be open for anyone, by invariant §3.2.6 of the tech spec). `.console`:
bg `var(--bg-panel)`, border-top `1px solid var(--line-strong)`, padding 12px 16px; content: a
kicker line ("YOUR MOVE — TURN {n}" small-caps gold; when coup is mandatory append " · COUP IS
MANDATORY" in `var(--red-bright)`) + button row (flex-wrap, gap 8px):

| Button | Label (exact) | Sub-caption (10px, dim, below label) | Requires target picker | Disabled states & reasons (button `title` = table-voice string where one exists) |
|---|---|---|---|---|
| Income | "Income" | "+1 coin" | no | only when not your turn / game over |
| Foreign Aid | "Foreign Aid" | "+2 coins" | no | as above |
| Coup | "Coup" | "−7 now · target loses a card" | yes | as above; when you have < 7 coins the picker shows but the POST surfaces `not enough coins` (server authoritative) |
| Tax | "Tax" | "claim Duke · +3 if it holds" | no | as above |
| Steal | "Steal" | "claim Captain or Ambassador" | yes (with claim picker) | as above; zero-coin targets are disabled in the picker with `title="that player has no coins to steal from"` |
| Assassinate | "Assassinate" | "−3 now, sunk · claim Assassin" | yes | as above |
| Exchange | "Exchange" | "claim Ambassador" | no | as above |

- **Mandatory coup (you have ≥ 10 coins):** all buttons except Coup get `disabled` +
  `title="you must take a coup while you have ten or more coins"` (verbatim §4.5 string — the
  server will say exactly this if the client is out of sync). The kicker line carries the warning.
- **Steal claim picker:** after choosing Steal, the target list row expands to two mini-buttons per
  candidate target: "as Captain" / "as Ambassador" (the `character` field of the POST). Assassinate
  sends `character:"assassassin"`; Tax sends `character:"duke"`; Exchange sends
  `character:"ambassador"` — these are claims, always allowed, the window decides.
- **Target picker:** inline expansion below the button row (no nested modal): a horizontal list of
  chip-buttons for every *active* seat except you — name + badge + coin count; coup/assassinate may
  target any active non-self; steal additionally requires ≥ 1 coin (disabled otherwise per above).
  Selecting submits `{"kind":"act","action":{…}}` and collapses the console into a locked status
  line: "Waiting on the table…" (gold, small-caps) with the row's buttons hidden. The console
  reappears only if a new `turn_start` names you again (i.e., after a full round) — otherwise the
  next seat acts and it disappears with their `action_declared`.
- **Rejection while submitting:** any 4xx → error toast (§13), console stays interactive (your turn
  still belongs to you unless the message says otherwise; if `code` is `game_over`, tear down into
  the game-over overlay from the poll stream).

**State table — turn console:**

| State | Trigger | Rendering | Buttons |
|---|---|---|---|
| hidden | phase ≠ `turn_action` or acting seat ≠ you | not rendered | — |
| active | your `turn_start` arrived / snapshot says so | strip visible | all enabled except per coup-mandatory / coin notes |
| locked-awaiting | your POST got 202, no window events yet | status line only | none (re-locks automatically on any later event batch) |
| mandatory | active + your coins ≥ 10 | kicker warning | only Coup enabled |

---

## 6. Lobby (S1 / S2)

One screen, two stacked cards (both visible; join card collapsible below the fold). Page bg per
§3.3; content column max-width 560px centered, top padding 48px.

### 6.1 New game card

- Kicker "SEAT THE TABLE" + heading "New game" (`--text-xl`, display).
- **Seat rows (2–6):** each row: number badge (circle 24px, gold outline), name input
  (bg `var(--bg-inset)`, border `1px solid var(--line)`, focus per §3.3; placeholder defaults per
  tech spec A14 — "Vito" for LLM rows cycling in order, "Player N" for human rows), and a segmented
  toggle [Human | AI] (two joined buttons, active = gold fill/`--gold-ink`). Row 1 is locked Human
  with `title="the first seat must be human"` (tech spec: ≥ 1 human; row 1 is the only enforced one
  — other rows may go either way, min 1 human validated client-side before POST and re-validated by
  the server). Add-seat "+" button up to 6, remove "×" down to 2. **Default on load: 4 rows — you +
  three AI `[ASSUMPTION U16]`.**
- **Window settings (details/summary "Window settings", open by default):**
  - checkbox "Wait for every eligible seat to answer (otherwise the first real answer ends the
    window)" → `require_all_responses` (default off).
  - text input "Auto-pass unresponded seats after (blank = never)", placeholder "e.g. 45s, 2m" →
    `response_timeout`; empty ⇒ `null`. A small hint line: default is open-ended — a stalled window
    can be resolved by a forfeit vote.
- **Create button** (`.btn-primary`, full width). On 201: store session (§2), navigate to S3, and
  show the one-time token notice modal (§6.3).
- Client-side validation mirrors the server's (`invalid_payload` otherwise): names trimmed ≤ 32
  chars; ≥ 1 human; timeout parseable or blank. Errors toast with the verbatim server message.

### 6.2 Join card

Two inputs: "Game id" (e.g., `g_0a1b2c3d4e5f`) and "Your seat token" (password-style input,
`autocomplete="off"`). Button "Enter the table". On submit → bootstrap S3 (§7.1); on 401 → inline
error under the card: "That token doesn't open this game." + toast. Sharing is out-of-band: whoever
created the game sends each human seat its `(game id, token)` pair `[ASSUMPTION U15]` — there is no
unauthenticated lookup in the API, and none should be added.

### 6.3 Token notice modal (one-time, after creation)

Shown over S3's first paint. Kicker "YOUR KEYS", title "Keep these to yourself." Body: the viewer's
token in a mono block with a copy button; one line: "Closing this tab loses nothing — rejoin any
time with your game id and token." Footer: single `.btn-primary` "Deal me in". Dismissible only by
that button (Esc copies-then-closes? No — Esc does nothing; it is not blocking long). `[ASSUMPTION
U2]` storage still applies.

### 6.4 Lobby state table

| Element | States |
|---|---|
| Seat row toggle | human-active / llm-active (exactly one) |
| Add-seat button | enabled ≤5 rows, disabled at 6 |
| Remove button | hidden on rows that would drop below 2 humans? No — below **2 total**; the ≥1-human rule is checked at submit and on every toggle: toggling the last human to AI disables Create with `title="at least one seat must be human"` |
| Timeout input | free text; client regex `^\d+(ms\|s\|m\|h)?$` → if unparseable, Create disabled with hint (server would return `invalid_payload`) |
| Create / Enter buttons | idle / submitting (label → "Seating…", disabled) / error (toast only, re-enable) |

---

## 7. State model & event loop

### 7.1 Bootstrap sequence (S3 entry)

```
1. token = sessionStorage[coup.session.{gameId}]; if absent → S2 with pre-filled game id.
2. GET /games/{id}            → seed: seats[], your_seat_id, your_face_down_cards,
                                court_deck_size, status, winner_id, window_settings, last_event_seq,
                                current{phase,…}, (turn seat — see U8)
3. if status != in_progress  → render table (final) + game-over overlay; start no polling loop at
                               all (polls would just return empty forever); stop here.
4. GET /games/{id}/events?after=0   → replay the FULL stream into the log (§12) — this is also where
                               private events of your seat (exchange reveals, flip requests,
                               rejections) are recovered for display. Event-derived *seat state*
                               (coins/cards) is NOT applied here: the snapshot from step 2 is the
                               seed truth; replay exists for the log and for reconstructing the
                               current window's deadline / responses if the snapshot is minimal.
5. Start poll loop with after = max(snapshot.last_event_seq, last replayed seq).
6. Render everything once; open any decision surface implied by current.phase (§8–§10) — a modal
   must appear even when arriving mid-window (the viewer just reloaded while it was open).
```

**State-sync policy `[ASSUMPTION U7]`:** seat state = snapshot seed + *incremental* event deltas
applied in step 5 onward. No periodic re-snapshotting (the monotonic poll stream cannot miss
anything while connected). Re-snapshot exactly when: a poll or POST returns 4xx/5xx/network error,
or the client reconnects after > 1 missed watchdog cycle — then re-render from the fresh snapshot
and *do not* clear the log (it is append-only truth on its own).

Local state object (single source of truth; render functions are pure over it):

```js
state = {
  gameId, mySeatId, token,
  seats:      [{id, name, type, coins, faceUp:[], active}],   // snapshot §11.2 shape
  myHand:     ["duke","ambassador"],                          // your_face_down_cards
  deckSize:   5, status: "in_progress", winnerId: null,
  windowSettings: {requireAllResponses, responseTimeoutMs},
  phase:      "turn_action" | "window_open" | "single_seat_decision" | "finished",
  turnSeatId: null,                                            // U8
  window: {                                                    // null unless phase == window_open
    id, kind, targetSeatId|null, eligible:[seatIds], actorId,
    deadlineAt: Date|null, requireAll, timeoutMs,
    myAllowed: ["pass","counterclaim_contessa","challenge"],   // snapshot current.window.your_allowed_responses
    responses: { [seatId]: "pass"|"challenge"|"counterclaim:contessa"|… },
    iResponded: false, myResponseLabel: null,
    triggerText: ""                                            // action_declared.text of the triggering action (U9)
  } | null,
  decision:   { id, kind: "flip_choice"|"exchange_finish",
                options:[], drawn:["duke","ambassador"], deadlineAt } | null,
  vote:       { id, targetSeatId, initiatorSeatId, eligibleVoters:[ids], majorityNeeded,
                votes:{ [seatId]:"yes"|"no" }, resolved:null|{result,tally} } | null,
  logRows:    […], lastSeq: n,
  conn: "live" | "reconnecting"
}
```

### 7.2 The long-poll loop (normative client behavior)

- **Single-flight:** exactly one in-flight `GET …/events?after={seq}` at a time; the next poll is
  issued only after the previous settles (empty or not). Overlapping polls are never made —
  idempotency makes overlap safe, but single-flight keeps `lastSeq` trivially correct.
- **Apply-then-repoll:** on 200: for each event in ascending seq order, run the reducer (§7.3) —
  state deltas first, log row second (order matters: rows reference state). Set
  `after = max(events[].seq)`; immediately issue the next poll. An empty `events` array is a
  normal hold-expiry return (tech spec A1) — just re-poll; no UI change, no log entry.
- **Watchdog `[ASSUMPTION U6]`:** abort the in-flight fetch after 70s without a response
  (`AbortController`) and re-issue with the *same* `after`. Hold is server-capped at 30s, so this
  only fires on genuine network hangs; re-issuing is safe because polling is idempotent.
- **Errors:**
  - transport failure / watchdog abort → `conn:"reconnecting"`, backoff retry at 1s, 2s, 4s, 8s
    (capped), same `after`; topbar dot + banner text via the connection status (§4.1). On success:
    re-snapshot once (§7.1 policy) then clear "reconnecting". If > 3 consecutive failures, also
    show info toast "Connection lost — retrying…".
  - **401** on any call → stop everything, S4 overlay (auth error): title "Your key no longer opens
    this table", body: verbatim `error.message` if meaningful, else "Check your token.", button
    "Rejoin" → S2. Tokens are not revocable in v1 (A2), so 401 means wrong token for the id.
  - **404** → one-shot modal "That game is gone (the server keeps games in memory — a restart drops
    them)." Button: lobby. `[ASSUMPTION U17]`
- **Game over:** on `game_over` event *or* `status != in_progress` from any snapshot: stop the poll
  loop, set `phase:"finished"`, render the overlay (§10.6). No further polling (the server returns
  empty arrays forever anyway).

### 7.3 Event reducer — per-type state effects (beyond log rendering, §12)

| Event | State effect |
|---|---|
| `turn_start` | `phase="turn_action"`, `turnSeatId=seat_id`, clear stale console/window UI; if it's you → show console (§5.4) |
| `action_declared` | remember `actionsBySeq[turn_id/action] = {type, actor, target, text}` (used by window headers, fizzle lines); if actor is you and console was locked → keep locked |
| `coins_changed` | `seat.coins = total` (trust the absolute `total`; log the delta) |
| `window_opened` | `phase="window_open"`, populate `state.window` (eligible, kind, target, settings, `deadlineAt = ts + timeout_ms` when `timeout_ms` present else null); if you ∈ eligible → open response modal (§8.1); else show `.decision` ring on *no* tile and a passive topbar banner "A window is open — {N} seats may respond" (dismissable) |
| `response_received` | `window.responses[seat] = <canonical string>`; if seat is you → set `iResponded`, lock modal footer (§8.1); chip re-render |
| `window_closed` | close/cancel: `phase="turn_action"` *tentatively* (the batch will carry the rest); `window=null` after cascade events in the same batch settle state; if a response modal is open → transition it to closed-state (§8.1 S3/S4). **Any decision POST in flight at this moment must be treated as stale on arrival** (its 202 is impossible — you get 409 instead, §7.4) |
| `claim_succeeded` / `claim_failed` | no seat state change beyond what the accompanying `card_flipped`/`coins_changed` events carry; mark trigger action outcome in `actionsBySeq` (affects subsequent fizzle wording) |
| `card_flipped` | move card from that seat's face-down count to `faceUp[]`; if seat is you → flip animation §4.4, remove the card from `myHand` at its position index |
| `action_fizzled` | annotate `actionsBySeq[action_seq].fizzled = reason` |
| `eliminated` | `seat.active=false`, `faceUp = cards` (forfeited placeholders render as "?" chips, §4.3); strip seat from all pickers/chips; if the eliminated seat is you → enter **spectator mode**: hand strip hidden, all decision UIs permanently unavailable, topbar chip gains "OUT", a one-time info toast "You are out of the game — spectating." The poll loop keeps running (the game continues around you) |
| `exchange_revealed` (private, yours) | store `drawn`; set `state.decision` when the accompanying decision is requested; open exchange-finish modal (§9.3) |
| `card_flip_choice_requested` (private, yours) | `state.decision = {kind:"flip_choice", options}`; open flip modal (§9.2) |
| `exchange_completed` | clear `state.decision`; if your exchange just finished: refresh hand from next snapshot-free knowledge — **no card identities are public here**, so do NOT mutate `myHand` from this event; the viewer's own drawn/hand changes were visible only in `exchange_revealed`, and the engine default/your choice is already applied to *your* hand knowledge at the moment you chose (the client applies your own choice locally at submit: swap per §9.3). This keeps `myHand` correct without any new server data |
| `forfeit_vote_opened` / `_cast` / `_resolved` | populate/update/clear `state.vote`; if you ∈ eligible_voters → ballot modal (§10.5); passive banner otherwise |
| `game_over` | §7.2 terminal handling |
| `rejection` (private, yours) | log row only (§12). **No second toast** — the HTTP 4xx that caused it already toasted (§7.4); the event exists for history reconciliation |

### 7.4 Decision submission (all POSTs)

```
async function post(path, body):
   try:    res = await fetch(path, {method:"POST", headers:{Authorization, Content-Type}, body})
           j   = await res.json()
           if (!res.ok) → handleRejection(j.error)     // §13 toast + per-code recovery
           return j
   catch (network)      → conn reconnect flow (§7.2); throw to caller's "stayed interactive" state
handleRejection(err):
   error toast: kicker(code) + err.message verbatim (§13)
   code already_resolved → the surface that posted gets its stale-treatment (§8.1 S4, §9.2/9.3 S3):
                           "the moment has passed" inline notice + close; ALSO force-re-snapshot to
                           resync phase (a window may have closed and another opened in the gap).
   code not_your_turn / game_over → re-snapshot (§7.1 policy); render whatever is now true.
   code not_eligible              → same as above (defensive; should not happen if UI eligibility
                                    mirrors your_allowed_responses).
   vote_closed / not_a_voter      → close ballot modal from the resolved events that are already on
                                    the stream (or arrive milliseconds later); re-snapshot.
```

**202 semantics:** `{"accepted":true,…}` means *queued*, not *resolved* — never toast success,
never render an outcome; outcomes arrive exclusively as events (§10.1 emission discipline: one
batch per atomic step). The submitting surface enters its locked state and waits on the stream.

### 7.5 "Stale response" UX (the moment-has-passed path)

Because windows close on *any* seat's first non-pass, a human may always click after an LLM has
already answered. Three layers must agree:

1. **Pre-click:** if `state.window` already shows a resolving state (impossible pre-close — closes
   are instantaneous server-side), buttons stay enabled until the close event lands; races are
   expected and handled at layer 2.
2. **On 409** (`already_resolved`): error toast verbatim (§13) + the open modal switches to
   `.stale` state: footer buttons disabled, replaced by a centered notice line — display italic,
   gold-dim: *"The moment has passed — {DECIDER} answered first."* (decider from the
   `window_closed` event if it is already in state; otherwise just "…already been resolved."). Modal
   auto-closes 3.5s later `[ASSUMPTION U10]`.
3. **On `window_closed` while modal open** (no POST involved): same `.stale`-style closed-state —
   but neutral copy, no toast: *"Window closed — {DECIDER} moved first."* / per-reason line (§8.1).

---

## 8. Decision surface — WINDOW RESPONSE (S3's modal)

The single most important screen of the client. It renders from `state.window` + your
`your_allowed_responses` and never invents options: **the button set is exactly the snapshot's list,
mapped by §8.1's table.**

### 8.1 Anatomy & state

Kicker "THE WINDOW". Title = the triggering `action_declared.text` verbatim (U9) — e.g., *"Vito
claims Assassin and declares an Assassinate against Marta (paid 3)."* Below it, when the window is
an attack, a sub-line in `--ink-dim`: "*You are the target.*" (target windows only; bystanders get
"*The table is weighing {TARGET}'s fate?*" — no: keep it factual: bystander sub-line = none).

**Body:** response-chip row — one chip per seat in `window.eligible` (seat order): name + badge,
then a state word: pending → "…" (`var(--ink-faint)`, pulse); `pass` → "passed" (dim);
`counterclaim:x` → "blocks as {X}" (green text); `challenge` → "challenged!" (`var(--red-bright)`,
600 weight). Your own chip is gold-bordered and reads "deciding…" until you answer. Live-updates
on every `response_received`.

**Settings line** (small, dim): from window settings — if `timeout_ms`: countdown
`"closes in 0:45"` ticking each second (`deadlineAt`, §7.3); last 10s turns `var(--red-bright)`.
If no timeout and `requireAll`: "waits for every seat to answer · {k} of {n} answered". If neither:
"first real answer ends it." (These describe mechanics already shown at creation; they are
informational, not advice.)

**Footer buttons**, mapped from `your_allowed_responses` exactly (pattern per U3 — the spec's own
example shows `"counterclaim_contessa"`):

| Grammar value | Button | Style | POST body (`kind:"respond"`) |
|---|---|---|---|
| `pass` | "Pass" | `.btn-outline` | `{"response":{"type":"pass"}}` |
| `challenge` | "Challenge" | `.btn-danger` | `{"response":{"type":"challenge"}}` |
| `counterclaim_contessa` | "Block — I'm Contessa" | `.btn-primary` | `{"response":{"type":"counterclaim","character":"contessa"}}` |
| `counterclaim_captain` | "Block — I'm Captain" | `.btn-primary` | `{"response":{"type":"counterclaim","character":"captain"}}` |
| `counterclaim_ambassador` | "Block — I'm Ambassador" | `.btn-primary` | `{"response":{"type":"counterclaim","character":"ambassador"}}` |
| `counterclaim_duke` | "Block — I'm Duke" | `.btn-primary` | `{"response":{"type":"counterclaim","character":"duke"}}` |

(FA windows therefore show **only** Block-as-Duke + Pass for eligible seats — no Challenge button
exists because the grammar omits it; nested Duke-counterclaim and tax/exchange windows show
Challenge + Pass. This matrix is the UI expression of tech spec §5.1; implement it data-driven, not
hard-coded per window kind.) Button order left→right: blocks (in the listed order), Challenge,
Pass — escalation outward, surrender on the right. An unknown future grammar value renders a plain
`.btn-outline` with its raw string as label and fails closed to a toast if the server rejects it.

**Close affordance:** ✕ top-right + **Esc = "hold"** (close without responding). Closing does not
submit anything; while the window stays open and you remain eligible, your `.decision` tile ring +
a persistent pill button docked to the modal's usual position ("Respond — a window is open") reopens
it on click. This never traps the player: passing is optional until the window ends.

**State table:**

| State | Trigger | Rendering | Buttons |
|---|---|---|---|
| S0 closed | `state.window` null, or you ∉ eligible (and no passive banner chosen) | not present (banner per §7.3 may be) | — |
| S1 open-deciding | window opened / reopened; you eligible; `iResponded=false` | full modal per above; chips live; countdown live | all grammar buttons enabled |
| S2 locked-awaiting | your respond POST got 202 | footer → status line "You answered: {LABEL} — the table is deciding…" (small-caps gold); chips keep updating; settings line keeps counting | none |
| S3 closed-by-event | `window_closed` arrived (you had not acted) | footer → neutral notice per §7.5(3); panel border dims to `var(--line)` | none; auto-close 3.5s |
| S4 stale (late POST 409) | your POST rejected `already_resolved` after S1/S2 attempt | error toast + gold-dim italic notice "The moment has passed — {decider} answered first." | none; auto-close 3.5s `[ASSUMPTION U10]` |

While S2, if *another* seat somehow acts (server allows the first non-pass only — a second is
rejected; your state simply waits until `window_closed` names a decider). The modal must never show
two different "you answered" lines: `iResponded` latches.

---

## 9. Decision surfaces — SINGLE-SEAT (modals, not dismissable)

Both are addressed to exactly one seat (tech spec §5.4) and render only when the *private* request
event arrives for your seat. They honor `response_timeout` like windows: countdown line in the
footer area if a deadline is set; if you do nothing and it expires, the engine applies its default —
your modal, on `window_closed`-equivalent settlement (the resulting public events), switches to a
closed state: "Time passed — the table chose for you." `[ASSUMPTION U14]` **These modals have no ✕
and no Esc** — there is no legal "pass" in a flip choice; a stalled human seat is exactly what
forfeit voting exists for (a hint line in the footer says so: "Stuck? The other humans can call a
forfeit vote.").

### 9.2 Flip-choice modal

- Trigger: private `card_flip_choice_requested` (you are losing one card to a penalty/coup/assassinate,
  and you still hold exactly two). Kicker "SINGLE CHOICE", title **"Which card falls?"**
- Body: your two hand cards side by side at 72×101 (§4.4), each with its position numeral ("1"/"2")
  enlarged to 14px, labels under them "Card 1 — face_down_0" / "Card 2 — face_down_1". The event's
  `options` are positional identifiers; the *names* on the faces come from **your own snapshot hand**
  (`your_face_down_cards[i]`) `[ASSUMPTION U5-part-b: option index i maps to your_face_down_cards[i]]`.
- Footer: two `.btn-outline` buttons "Flip card 1" / "Flip card 2". Submit →
  `{"kind":"choose","decision_id":id,"choice":{"flip_choice":"face_down_0"|…}}` → locked line
  "Chosen — waiting for the flip." (The public `card_flipped` event then plays your §4.4 animation.)
- State: S1 deciding / S2 locked-awaiting / S3 settled-by-event ("{CARD} is face-up now." dim) /
  S4 expired (engine default applied; copy per above). A second `choose` for the same id → 409
  "that decision was already made" → verbatim toast, no state change.

### 9.3 Exchange-finish modal

- Trigger: your claim on Exchange survived — you see it as a batch: (private) `exchange_revealed`
  with the two drawn cards + the pending decision; public `exchange_completed` follows only *after*
  you decide. So the modal is open from `exchange_revealed` until your choice settles. Kicker
  "SINGLE CHOICE", title **"Your Exchange held — choose what to keep."**
- Body, two labeled rows:
  - "YOU DREW (only you can see these)" — the two drawn cards at 72×101, labeled **L** and **R**.
  - "YOUR HAND" — your current face-down cards as §4.4 hand cards (numerals 1/2).
- **Choice flow (two steps in one panel):**
  1. Row of three `.btn-outline`: **"Keep neither"** / **"Keep left"** / **"Keep right"**. Selecting
     Keep-left/Right highlights that drawn card (gold ring) and reveals a second row: "Return which
     hand card?" with two mini-buttons "Card 1" / "Card 2". "Keep neither" skips straight to confirm.
  2. Confirm button `.btn-primary` "Seal it" submits the mapping:

     | UI choice | POST `choice.exchange_finish` |
     |---|---|
     | keep left, return card k | `{"drawn_index":0,"hand_option":"face_down_{k-1}"}` |
     | keep right, return card k | `{"drawn_index":1,"hand_option":"face_down_{k-1}"}` |
     | keep neither | `{"no_swap":true}` `[ASSUMPTION U4]` — confirm the wire shape with the backend; tech spec §2.2 names a `no_swap` choice value but §11.2 only shows the swap shape |

  - On 202: locked line "Sealed — the deck took two back." Locally apply your own hand change now
    (you know both sides of the swap; §7.3): e.g., keep-left-return-1 → `myHand = [drawn[0],
    myHand[1]]`. The subsequent public `exchange_completed` must not double-apply.
- State: S1 choosing / S2 locked-awaiting (POST sent, awaiting batch) / S3 settled (`exchange_completed`
  seen — panel fades with one line "Done.") / S4 expired (engine's no-swap default; copy per §9).

---

## 10. Forfeit-vote UI & game over

### 10.1–10.4 Passive visibility

- A **passive banner** (topbar area, gold-outline pill, dismissable ×) whenever a vote is open and
  you are *not* an eligible voter — or you *are* one but the modal was closed (it reopens on any
  new `forfeit_vote_cast` while unresolved, because the tally moved). Text: "Forfeit vote against
  {TARGET} — {yes}–{no}, needs {majority}." Public information; all humans may watch the tally.
- The log carries every step verbatim per §12 (the ballot is public by design).

### 10.5 Ballot modal (event-driven)

Opens automatically on `forfeit_vote_opened` **iff you ∈ `eligible_voters`** (tech spec §7: other
active human seats — the initiator included, per the spec's own example where
`initiator_seat_id` ∈ `eligible_voters`). Kicker "THE TABLE ASKS", title **"Forfeit vote — {TARGET}"**
(badge rendered if target is LLM). Sub-line: "Called by {INITIATOR}. The other human seats decide;
a majority ends it early."

Body: **live tally block**:
- Big count line: `YES {y}   NO {n}` (display font, yes in `var(--green-bright)`, no in
  `var(--red-bright)`), right-aligned "majority: {M} of {E}".
- Progress bar: height 6px, track `var(--bg-inset)`, fill split green/red proportional to y/(y+n);
  a tick at the majority fraction.
- Voter list (seat order): each row name + badge? (voters are human — no badges by definition; keep
  the renderer uniform anyway) + status chip: undecided "…" / "VOTED YES" green chip / "VOTED NO"
  red chip, updating on `forfeit_vote_cast`.

Footer: while unresolved and you haven't voted: **"Vote YES"** `.btn-primary` (green tint variant:
bg `var(--green)`, text `#f2f7f0`) + **"Vote NO"** `.btn-danger`. Submit → 200/409 path; on accept
the footer becomes "You voted {X}." and the list shows your chip. On `forfeit_vote_resolved`:
modal switches to result state — `forfeited` → "{TARGET} is forfeited. Their cards are never shown."
(other results: "The vote fails {y}–{n}. {TARGET} stays." `[ASSUMPTION U12]` for any other result
value, e.g. a void) — auto-close 4s. A POST to a closed vote → 409 `vote_closed` → verbatim toast +
the resolved state above (the `_resolved` event is in the same/matching batch).

**Initiation modal** (from topbar button): title "Call a forfeit vote", body: "Use it when a seat —
human or AI — is stuck and the table can't move on. The other human seats vote; nothing happens
unless a majority agrees." Target list = every **active seat except you** `[ASSUMPTION U18]` (the
spec says "any seat that is stuck"; self-forfeit is not a meaningful use case and stays out of the
UI — the server will reject it anyway if it even accepts such a target). Each row: name + badge +
coins; click → confirm step in-place ("Really call {NAME}? The other humans get asked.") with
Confirm/Cancel. POST `{"target_seat_id":…}`; 201 → close, banner appears (you are an eligible voter
→ the ballot modal opens a moment later via the event). Errors: 409 (vote already active / target
invalid) and 404s → verbatim toast, modal stays open.

### 10.6 Game-over overlay

Full-viewport layer above everything (backdrop `.78` + blur), centered panel 360px. Kicker "THE
TABLE CLOSES". Title **"GAME OVER"** (`--text-2xl`, display, gold). Winner line: "{NAME}" `--text-xl`
display + LLM badge if applicable + one dim sub-line "wins the table." Standings list (active-first
then eliminated, each: name+badge, coins in mono gold; winner's row gold-bordered). Footer:
".btn-primary" "Back to lobby" (clears `sessionStorage` for this game → S1) — that is the only
action; polling has already stopped (§7.2). If *you* are the winner, the title line reads
"GAME OVER — YOU WIN THE TABLE."

---

## 11. Spectator & eliminated-viewer handling

When your seat is eliminated (§7.3 `eliminated`): hand strip unmounts; console never shows; window
modals never open (you are out of every eligibility set by invariant §3.2.3); flip/exchange modals
impossible (no cards). The topbar chip gains an "OUT" tag (same style as seat tiles). Everything
else renders identically — the table log is where the endgame is watched. Rejoining after
elimination works normally (snapshot shows you inactive; client enters spectator mode immediately at
bootstrap step 6).

---

## 12. Event log rendering rules (per event type)

Rendering contract: one row per event, in seq order; names via `renderName` (§4.2); time from `ts`;
**text is composed client-side except where the server supplies its own string** — `action_declared.text`
(U9) and `rejection.message` are used verbatim. Character words are capitalized first letter
("Contessa"); seats' names as-given. Templates (icon column per §4.6):

| Event | Row template / treatment |
|---|---|
| `game_created` | `.row-divider`: "THE TABLE SEATS {names joined ' · '} — {FIRST} OPENS" |
| `turn_start` | `.row-divider`: "TURN {turn_id} — {NAME}" (name in gold when it's you) |
| `action_declared` | ◆ **{ev.text verbatim}** |
| `window_opened` | dim: "Responses open — {eligible names} may act." (+ if kind == counterclaim_challenge: "— {blocker}'s block is itself challenged.") |
| `response_received` | pass → "{NAME} passed" (dim); challenge → "{NAME} **challenges!**" (`var(--red-bright)`); counterclaim → "{NAME} blocks as {CHARACTER}" (`var(--green-bright)`) |
| `window_closed` | reason map: `first_response` → "The window closes — {DECIDER} moved first."; `all_responded` → "…— every seat has answered."; `timeout` → "…— the window timed out; silent seats pass."; `auto_no_responders` → "…— no one is left to answer." (dim, 600 weight) |
| `claim_succeeded` | "{NAME} was holding the {CHARACTER}. {CHALLENGER}'s challenge fails." (gold-dim) + next row will carry the flip |
| `claim_failed` | "✕ {NAME} did not hold the {CHARACTER}. The play is void; {NAME} loses a card." (`var(--red-bright)` accent on "void") |
| `card_flipped` | face-up chip icon (character color). Text by reason: `penalty_failed_claim` → "{NAME}'s {CARD} flips face-up — busted."; `penalty_failed_challenge` → "{NAME}'s {CARD} flips face-up — the challenge failed."; `coup` → "A coup lands on {NAME} — {CARD} flips face-up."; `assassinate` → "The assassin's strike lands — {NAME}'s {CARD} flips face-up."; `successful_reveal` → "{NAME} was called out — {CARD} is revealed in public, then straight back to the Court Deck ({NAME} draws a replacement; this card never joins their face-up list)." (transient by design, tech spec §4.2: the reveal makes it known, not kept) |
| `coins_changed` | reason map (delta signed, total in parens): `income` → "{NAME} takes Income (+1) — now {t}"; `foreign_aid` → "{NAME} takes Foreign Aid (+2) — now {t}"; `tax` → "{NAME} taxes the treasury (+3) — now {t}"; `coup_paid` → "{NAME} pays 7 for a Coup — now {t}"; `assassination_paid` → "{NAME} pays 3 — sunk — for an assassination, now {t}"; `steal_given` → "{NAME} is robbed (−{d}) — now {t}"; `steal_took` → "{NAME} steals +{d} — now {t}" |
| `action_fizzled` | lookup the declared action via `actionsBySeq[action_seq]`: reason map: `counterclaim_success` → "✕ The {ACTION-WORD} fizzles — blocked. (Sunk costs stay sunk.)"; `target_eliminated` → "✕ …fizzles — its target is already gone."; `target_forfeited` → "✕ …fizzles — its target has been forfeited." |
| `eliminated` | bold: "{NAME} is eliminated." + inline face-up chips (monogram+stripe per §4.3; `forfeited` placeholders render as "?" chips, explicitly *not* revealed) |
| `exchange_revealed` | `.row-private`: "You drew {D0} and {D1}. Only you see this." |
| `card_flip_choice_requested` | `.row-private`: "You must choose which card falls." (modal is the primary surface; row is its history) |
| `exchange_completed` | dim: "{NAME} completes an exchange. (Two cards return to the deck, order unknown.)" |
| `forfeit_vote_opened` | ⚖ bold: "FORFEIT VOTE opened against {TARGET} by {INITIATOR} — majority {M} of {E}." |
| `forfeit_vote_cast` | "{VOTER} votes **YES** / **NO** on the forfeit vote." (word colored per side) |
| `forfeit_vote_resolved` | `forfeited` → "⚖ Vote passes ({yes}–{no}). {TARGET} is eliminated — their cards are forfeited, never revealed."; other values → "⚖ The vote fails ({yes}–{no}). {TARGET} stays at the table." (U12) |
| `game_over` | `.row-banquet`: "GAME OVER — {WINNER} WINS THE TABLE" (+ badge in the name span) |
| `rejection` (private) | `.row-warning` ⚠: "The table rejects yours: "{message verbatim}"" |

**Batching note:** resolution cascades arrive as one batch (§10.2 emission discipline) — render rows
in seq order within the batch before any state-triggered re-render, so a full bust (claim_failed →
flip → eliminate) reads top-to-bottom in one pass. Auto-scroll honors §4.6.

---

## 13. Error toasts & copy conventions

- **Verbatim rule:** every server `error.message` is displayed unchanged — tech spec §4.5 strings
  are authored to read like table feedback ("not enough coins", "that player has no coins to steal
  from") and the UI's italic-serif treatment (§4.7) leans into that voice. No capitalization fixes,
  no prefixes inside the quote, no localization layer in v1.
- **Kicker mapping** (from `error.code`; unknown codes render upper-cased code):

  | code | kicker |
  |---|---|
  | `not_your_turn` | NOT YOUR TURN |
  | `already_resolved` | TOO LATE |
  | `not_eligible` | NO SAY SO |
  | `invalid_payload` | ILLEGAL MOVE |
  | `game_over` | GAME OVER |
  | `unauthorized` | ACCESS DENIED |
  | `not_found` | NOT FOUND |
  | `vote_not_found` | NO SUCH VOTE |
  | `vote_closed` | VOTE CLOSED |
   | `not_a_voter` | NOT A VOTER |
   | `self_target` | CAN'T TARGET YOURSELF |
   | `target_not_active` | ALREADY ELIMINATED |
   | `server_error` / other | THE TABLE HICCUPS |

- Forfeit-initiation rejections (initiation modal, §10.5): the two verbatim strings are
  `self_target` → "you cannot target your own seat" and `target_not_active` → "you cannot
  target an eliminated player" (tech spec §4.5); display is covered by the verbatim rule above.

- **Moment-has-passed notices** are *separate from* the verbatim toast: they live inside the stale
  modal state (§7.5) in display italic, e.g., "The moment has passed — Vito answered first." The
  two may appear together (toast + modal notice); that is intentional: toast = the server's words,
  notice = the UI's framing.

---

## 14. Responsive behavior summary (re-check at 380px)

| Concern | ≥1120 | 720–1119 | <720 → 380 |
|---|---|---|---|
| Log | right column, full height | below hand, max-height 38vh | below hand, max-height 45vh; icon column hidden <560px |
| Seats | ring geometry §5.3 | ring, felt max 640px | **strip mode** §5.3 (tiles wrap above center) |
| Center circle | 168px | 168px | 128px, re-centered |
| Hand cards | `min(24vw,96px)` | same | `min(22vw,88px)`; at 380px ≈ 83.6px each — two fit with 12px gap in ~180px ✓ |
| Console buttons | one row | wrap 2 rows | wrap 2 rows, labels only (sub-captions hidden <560px) |
| Modals | fixed width per modal (response 480px, others 420px, token notice 380px) | same | `calc(100% − 24px)`; button rows wrap full-width stacked (<480px buttons become `width:100%`, column layout, primary last) |
| Topbar | full | game chip hidden <900px, forfeit button icon-only? No — keep text, hide game chip; at 380px keep wordmark + dot + viewer chip only | as left cell |

Everything above has been sanity-checked against a 380×760 viewport with the token scale unchanged:
no horizontal overflow sources remain (log rows `overflow-wrap:anywhere`, log names never truncate —
they wrap).

---

## 15. Accessibility baseline

- **Keyboard:** every interactive element reachable & operable by keyboard (real `<button>`s,
  inputs; no div-clicks without `role`/`tabindex`). Modals: `role="dialog" aria-modal="true"`,
  labelled by their `.modal-title` (`aria-labelledby`); focus moves to the panel on open
  (initial focus: window-response modals → "Pass" [the safe default]; flip/exchange → first choice
  button; ballot → "Vote NO"? No — neutral: first listed = "Vote YES"; token notice → "Deal me in");
  **focus trap** cycles Tab/Shift-Tab within the panel; focus returns to the trigger on close. Esc:
  window-response = hold (§8.1); flip/exchange = does nothing (U14, hint line explains why); ballot
  & token notice = close without voting / close after copy.
- **Focus visibility:** §3.3 gold ring everywhere; never `outline:none` without replacement.
- **Live regions:** log list container `role="log"` (rows appended, no per-row live announcements —
  the container is enough); toasts `role="alert"` (error) / `role="status"` (info); connection
  status `role="status"`. Countdown text updates are `aria-hidden` with a static accessible
  alternative "window closes in about 45 seconds" refreshed per minute, not per second.
- **Contrast:** ivory on bg ≈ 13:1; dim ink ≈ 5.6:1 (body-copy minimum 4.5 ✓); gold-bright on bg
  ≈ 9:1; `--gold-ink` on gold fill ≈ 7:1; red-bright on bg ≈ 5.2:1; LLM badge ≈ 7:1. Disabled
  states may drop to 3:1 (they are non-interactive). Vetoed anywhere: `var(--ink-faint)` for body
  copy (watermarks/decorations only).
- **Reduced motion:** `@media (prefers-reduced-motion: reduce)` → all animations/transitions off
  (cards appear instantly; pulses removed; modal fades become instant opacity).
- **Semantics of game state:** your-turn console has `aria-live="polite"` on its kicker so screen
  readers hear "YOUR MOVE — TURN 7" without hunting; window modals announce their title on open;
  the eliminated toast announces spectator mode once.

---

## 16. Mockup cross-reference

`docs/mockup.html` renders: S3 mid-window — five seats in ring geometry (Vito LLM-badged top-left
as the acting LLM, Bruno eliminated bottom-left with two face-up chips), court deck "5", center
"WINDOW OPEN", your hand [Ambassador, Captain] labeled 1/2, the response modal for an Assassinate
against you (Block — I'm Contessa / Challenge / Pass) with live chips (Carmine passed, Sonny
pending, You deciding), settings line with countdown + require-all progress, and a table log of the
preceding two turns including the declaration, sunk-cost coin row, window open, and one pass. If you
change tokens in §3.2, update the mockup first (it is the approval artifact).

---

## 17. Assumptions register (UI-side)

| ID | Assumption | Where it bites / what to confirm |
|---|---|---|
| **U1** | Client is served same-origin by the Go server's static handler at `/`; all API calls use relative URLs. `file://` and separate-host deployments are unsupported until a CORS decision exists. | Delivery/deploy choice for owner; backend to confirm static handler path |
| **U2** | Session `(gameId, seatId, token)` lives in `sessionStorage`, per tab; nothing persists across sessions; rejoin always re-bootstraps (idempotent by design). | Privacy posture — acceptable? |
| **U3** | `your_allowed_responses` grammar strings follow the spec example's pattern: `pass`, `challenge`, `counterclaim_<character>` (`contessa/captain/ambassador/duke`). Client maps buttons by exact match; unknown values render raw and fail closed. | Confirm exact string set with backend (it is the snapshot contract) |
| **U4** | Exchange "keep neither" submits `{"choice":{"exchange_finish":{"no_swap":true}}}` — §2.2 names a `no_swap` choice value but §11.2 only shows the swap shape. | **Backend to confirm wire shape before implementation of §9.3** |
| **U5** | Exchange-finish semantics as implemented by the UI: keep-left/right = one drawn card enters the hand at `drawn_index`, the chosen `hand_option` face-down card is returned; keep-neither = no swap. Also: flip-choice option index *i* maps to `your_face_down_cards[i]` from your own snapshot (private events are positional by design). | Confirm both mappings with backend (§6.4 tool names `keep_drawn_cards()`/`swap_drawn_option()` suggest the same model) |
| **U6** | Client-side poll watchdog: abort + re-issue identical `after` after 70s (server hold is ≤30s; idempotent). | None — pure client robustness |
| **U7** | State = snapshot seed + incremental event deltas; re-snapshot only on 4xx/5xx/network errors and reconnects; log is never cleared by a resnapshot. | Design call for consistency vs. traffic (one snapshot per incident) |
| **U8** | `GET /games/{id}`'s `current` includes the acting seat during `phase:"turn_action"` (e.g., `turn_seat_id`). Fallback if absent: derive from the last `turn_start` in the replayed log (always available after bootstrap step 4). | **Confirm field name with backend** |
| **U9** | `action_declared` events always carry a human-readable `text` field (as in the §10.1 example), used verbatim for log rows and window-modal titles. Fallback if ever absent: client composes "{actor} declares {ACTION}{ against {target}}" from structured fields. | Confirm `text` is guaranteed by backend emission |
| **U10** | Auto-dismiss timing for closed modal states: response modal (event-closed or stale) 3.5s; forfeit ballot result 4s. "Hold"-closed response modals re-open via the docked "Respond — a window is open" pill while you remain eligible. | Cosmetic; tune on approval of the mockup flow |
| **U11** | Snapshot `status`: exactly `"in_progress"` \| `"finished"` — no other values exist (tech spec §11.2). At `"finished"`, the snapshot carries a top-level `winner_id` (the winner's seat id; it stays set on every subsequent snapshot). The defensive rule stands: *any* non-`"in_progress"` status is terminal (stop polling, show game-over overlay) — and now there is exactly one such value. | Resolved against tech spec §11.2 (the old `"over"` guess is withdrawn); nothing left to confirm |
| **U12** | `forfeit_vote_resolved.result` — full vocabulary per tech spec §10.1: `"forfeited"` (yes-majority reached; an `eliminated` event follows in the same batch), `"not_forfeited"` (the no side reached majority first, **or** every eligible voter has cast and neither side did — a tie closes it as not-forfeited), `"voided"` (the target was eliminated by play while the vote was pending; the ballot closes with no effect). Rendering: `forfeited` gets its line (§10.5/§12); the other two render the generic "vote fails {yes}–{no}" line with the tally. | Resolved against tech spec §10.1 (vocabulary completed after this entry was written); nothing left to confirm |
| **U13** | Log DOM cap: 400 rendered rows, oldest dropped behind a single "… earlier entries trimmed …" marker; full history recoverable by rejoin/replay. | Cosmetic |
| **U14** | Flip-choice and exchange-finish modals are **not dismissable** (no ✕, no Esc): there is no legal pass in those decisions, and a stalling human seat is precisely what forfeit voting exists for; the modal says so in its footer. Window-response modals *are* closable (Esc = hold, §8.1). | Design call — flag to owner if they want an explicit "pass" there (the server would not accept one) |
| **U15** | Hash routing (`#/`, `#/?join`, `#/g/{id}`); joining requires out-of-band sharing of the `(game id, token)` pair — the API has no unauthenticated game lookup and none should be added. | Owner to confirm sharing model for v1 (chat/message with token) |
| **U16** | Lobby defaults: 4 seat rows pre-filled — row 1 human ("you"), rows 2–4 AI; window settings at tech-spec A8 defaults (`require_all_responses=false`, timeout blank). | Cosmetic |
| **U17** | A `404` from any seat-scoped call renders a one-shot modal "That game is gone." (games are in-memory per the tech spec's non-goals; a server restart drops them) with a single action back to the lobby. | Wording only |
| **U18** | The forfeit-initiation target list excludes the viewer's own seat ("any active human may initiate … against any stuck seat" — self-forfeit is not a meaningful flow); the server remains authoritative either way. | Edge case; confirm backend rejects self-targets if we're wrong |

---

*End of UI specification. Implementation order for the frontend developer: §3 tokens → §4 global
components → §7 state+poll → §12 log renderer → §9/§8 decision surfaces (in that order: log first,
because every screen is judged against a live table) → §6 lobby → §10 forfeit/game-over. Everything
not specified here and not derivable from `technical-spec.md` must be raised as an open question to
the team lead before being decided silently in code.*

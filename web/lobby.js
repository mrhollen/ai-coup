// ===========================================================================
// lobby.js — S1/S2 (ui-spec §6): new-game card (2–6 seats, per-seat name +
// human/llm picker, window settings) and the join card. POST /games is the
// only unauthenticated call; tokens appear exactly once in the 201 body (A2).
// ===========================================================================

import { esc } from "./state.js";
import { apiPost, ApiError } from "./poll.js";
import { rejectToast, infoToast } from "./decide.js";

export const LLM_FAST_FAIL_HINT = // verbatim server message (this build) — shown as a toast (§13).
  "llm seats cannot join in this build: the LLM driver is not available yet — create a human-only game";

const DEFAULT_LLM_NAMES = ["Vito", "Sonny", "Carmine", "Rocco", "Salvatore", "Bruno"]; // A14 placeholders
const TIMEOUT_RE = /^\d+(ms|s|m|h)?$/; // §6.4 state table

// The lobby model is module-local until POST succeeds (then it's discarded).
let model = null;

function freshModel() {
  return {
    seats: [
      { name: "", type: "human", lockedHuman: true }, // row 1 locked human (§6.1)
      { name: "", type: "llm" },
      { name: "", type: "llm" },
      { name: "", type: "llm" },                       // U16: default 4 rows — you + three AI
    ],
    requireAll: false,
    timeout: "",                                       // blank ⇒ null ⇒ server defaults (A8)
    submitting: false,
  };
}

export function placeholderFor(seat, idx, llmSeen) {
  if (seat.name) return seat.name;
  return seat.type === "llm" ? DEFAULT_LLM_NAMES[llmSeen % DEFAULT_LLM_NAMES.length] : `Player ${idx + 1}`;
}

function humansCount(m) { return m.seats.filter((s) => s.type === "human").length; }
function llmSeenAt(m, i) { let n = 0; for (let j = 0; j < i; j++) if (m.seats[j].type === "llm") n++; return n; }

/** Client-side validation mirroring the server's (§6.1). Returns an error string or null. */
function validate(m) {
  if (m.seats.length < 2 || m.seats.length > 6) return "a game needs between two and six seats";
  if (humansCount(m) < 1) return "a game needs at least one human seat";
  for (const s of m.seats) {
    const name = s.name.trim();
    if (name.length > 32) return "player names are limited to 32 characters";
  }
  if (m.timeout !== "" && !TIMEOUT_RE.test(m.timeout.trim())) {
    return "response_timeout must be a duration like 45s or 2m (or left blank)";
  }
  return null;
}

/**
 * Render the lobby into #lobby-root. `joinPrefill` carries a game id when we
 * bounced here from a failed S3 bootstrap (ui-spec §7.1 step 1 / S4 rejoin).
 */
export function renderLobby(rootEl, { joinPrefill = null } = {}) {
  if (!model || model.destroyed) model = freshModel();
  rootEl.innerHTML = `
    <div class="lobby-brand">Coup<small>the table is set for two to six — claims are cheap, challenges cost.</small></div>

    <section class="lobby-card" aria-labelledby="newgame-h">
      <span class="kicker">SEAT THE TABLE</span>
      <h1 id="newgame-h">New game</h1>
      <div class="seat-rows" id="seat-rows"></div>
      <details class="win-settings" open>
        <summary>Window settings</summary>
        <div class="ws-row">
          <input type="checkbox" id="ws-reqall">
          <label for="ws-reqall">Wait for every eligible seat to answer (otherwise the first real answer ends the window)</label>
        </div>
        <div class="ws-row">
          <label for="ws-timeout" style="flex:1">Auto-pass unresponded seats after <span style="color:var(--ink-faint)">(blank = never)</span></label>
          <input type="text" class="timeout-in" id="ws-timeout" placeholder="e.g. 45s, 2m" autocomplete="off">
        </div>
        <div class="ws-hint" id="ws-hint">Default is open-ended — a stalled window can be resolved by a forfeit vote.</div>
      </details>
      <button type="button" class="btn btn-primary" id="create-btn" style="width:100%">Create game</button>
    </section>

    <form class="lobby-card" aria-labelledby="join-h" id="join-form">
      <span class="kicker">ALREADY SEATED?</span>
      <h1 id="join-h">Join a game</h1>
      <input type="text" class="field-in" id="join-id" placeholder="Game id (e.g. g_0a1b2c3d4e5f)" autocomplete="off" spellcheck="false">
      <input type="password" class="field-in" id="join-token" placeholder="Your seat token" autocomplete="off" spellcheck="false">
      <button type="submit" class="btn btn-outline" id="join-btn" style="width:100%">Enter the table</button>
      <div class="join-inline-err" id="join-err"></div>
    </form>`;

  // --- seat rows -------------------------------------------------------------
  const rowsEl = rootEl.querySelector("#seat-rows");
  function renderRows() {
    rowsEl.innerHTML = m2().seats.map((s, i) => {
      const ph = placeholderFor(s, i, llmSeenAt(m2(), i));
      const locked = s.lockedHuman;
      return `<div class="seat-row" data-i="${i}">
        <span class="seat-num">${i + 1}</span>
        <input type="text" class="name-in" data-name="${i}" placeholder="${esc(ph)}" maxlength="32"
          value="${esc(s.name)}" ${locked ? 'aria-label="Seat 1 name — this seat must stay human"' : ''}>
        <span class="seg" role="group" aria-label="Seat ${i + 1} type">
          <button type="button" data-type-human="${i}" class="${s.type === "human" ? "on" : ""}" ${locked ? "disabled title='the first seat must be human'" : ""}>Human</button>
          <button type="button" data-type-llm="${i}" class="${s.type === "llm" ? "on" : ""}" ${locked ? "disabled title='the first seat must be human'" : ""}>AI</button>
        </span>
        <button type="button" class="row-x" data-remove="${i}" aria-label="Remove seat ${i + 1}" ${m2().seats.length <= 2 ? "disabled" : ""}>×</button>
      </div>`;
    }).join("") + `<button type="button" class="add-seat" id="add-seat" ${m2().seats.length >= 6 ? "disabled" : ""}>+ Add a seat</button>`;

    rowsEl.querySelectorAll("[data-name]").forEach((inp) => {
      inp.addEventListener("input", () => { m2().seats[parseInt(inp.dataset.name, 10)].name = inp.value; syncCreate(); });
    });
    const setType = (i, t) => {
      const seat = m2().seats[i];
      if (seat.lockedHuman && t === "llm") return;
      // §6.4: toggling the LAST human to AI disables Create with a why-title.
      seat.type = t;
      renderRows(); syncCreate();
    };
    rowsEl.querySelectorAll("[data-type-human]").forEach((b) => b.addEventListener("click", () => setType(parseInt(b.dataset.typeHuman, 10), "human")));
    rowsEl.querySelectorAll("[data-type-llm]").forEach((b) => b.addEventListener("click", () => setType(parseInt(b.dataset.typeLlm, 10), "llm")));
    rowsEl.querySelectorAll("[data-remove]").forEach((b) => b.addEventListener("click", () => {
      const i = parseInt(b.dataset.remove, 10);
      if (m2().seats.length <= 2) return;
      m2().seats.splice(i, 1);
      renderRows(); syncCreate();
    }));
    rootEl.querySelector("#add-seat").addEventListener("click", () => {
      if (m2().seats.length >= 6) return;
      m2().seats.push({ name: "", type: "llm" });
      renderRows(); syncCreate();
    });
  }

  // --- window settings ---------------------------------------------------------
  const reqAllEl = rootEl.querySelector("#ws-reqall");
  const timeoutEl = rootEl.querySelector("#ws-timeout");
  reqAllEl.checked = model.requireAll;
  timeoutEl.value = model.timeout;
  reqAllEl.addEventListener("change", () => { model.requireAll = reqAllEl.checked; });
  timeoutEl.addEventListener("input", () => { model.timeout = timeoutEl.value; syncCreate(); });

  // --- create ------------------------------------------------------------------
  const createBtn = rootEl.querySelector("#create-btn");
  function syncCreate() {
    const err = validate(model);
    createBtn.disabled = !!err || model.submitting;
    createBtn.title = err ? err : ""; // §6.4: the why lives in the button title
    rootEl.querySelector("#ws-hint").textContent = TIMEOUT_RE.test((model.timeout || "").trim()) && model.timeout !== ""
      ? "Windows auto-pass silent seats after this duration."
      : "Default is open-ended — a stalled window can be resolved by a forfeit vote.";
  }

  createBtn.addEventListener("click", async () => {
    const err = validate(model);
    if (err) { rejectToast("invalid_payload", err); return; } // mirrors the server's table voice
    model.submitting = true;
    createBtn.disabled = true;
    createBtn.textContent = "Seating…";

    const players = model.seats.map((s) => ({ name: s.name.trim(), type: s.type }));
    const winObj = {}; // omitted ⇒ A8 server defaults (§6.1)
    let windowKey = null;
    if (model.requireAll) { winObj.require_all_responses = true; windowKey = "require"; }
    const t = model.timeout.trim();
    if (t) {
      winObj.response_timeout = /^\d+$/.test(t) ? t + "s" : t; // bare number ⇒ seconds (§6.1 free text)
      windowKey = "timeout";
    }
    const body = { players };
    if (windowKey) body.window = winObj;

    try {
      const res = await apiPost({ token: null }, "/games", body); // unauthenticated (no session yet)
      // 201 — the ONLY place tokens appear (A2). Viewer = seat 1 (the creator;
      // row 1 is locked human, so it always carries a token).
      const mySeat = res.seats.find((x) => x.token) || res.seats[0];
      onCreated(res.game_id, { gameId: res.game_id, seatId: mySeat.id, token: mySeat.token });
    } catch (e) {
      if (e instanceof ApiError) rejectToast(e.code, e.message); // verbatim — incl. the LLM fast-fail (§13)
    } finally {
      model.submitting = false;
      createBtn.disabled = !!validate(model);
      createBtn.textContent = "Create game";
    }
  });

  // --- join ----------------------------------------------------------------------
  const idEl = rootEl.querySelector("#join-id");
  const tokenEl = rootEl.querySelector("#join-token");
  const errEl = rootEl.querySelector("#join-err");
  if (joinPrefill) idEl.value = joinPrefill; // bounced from a failed S3 bootstrap / rejoin
  const doJoin = () => {
    const gid = idEl.value.trim();
    const tok = tokenEl.value.trim();
    errEl.textContent = "";
    if (!gid || !tok) { errEl.textContent = "You need both a game id and your token."; return; }
    onJoin(gid, tok);
  };
  rootEl.querySelector("#join-form").addEventListener("submit", (e) => { e.preventDefault(); doJoin(); });

  renderRows();
  syncCreate();
}

function m2() { return model; }

// Wired by main.js (composition boundary — keeps lobby.js free of session/routing).
let onCreated = null, onJoin = null;
export function setLobbyHooks(h) { if (h.onCreated) onCreated = h.onCreated; if (h.onJoin) onJoin = h.onJoin; }

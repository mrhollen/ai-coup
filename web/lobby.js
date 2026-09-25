// ===========================================================================
// lobby.js — S1/S2 (ui-spec §6): new-game card (2–6 seats, per-seat name +
// human/llm picker, window settings) and the join card. POST /games is the
// only unauthenticated call; tokens appear exactly once in the 201 body (A2).
// ===========================================================================

import { esc } from "./state.js";
import { apiPost, ApiError } from "./poll.js";
import { rejectToast, infoToast } from "./decide.js";
import { toggleRulesModal } from "./rules_modal.js";
import { toggleSound, isSoundEnabled, initAudio, playClick } from "./sound.js";
import { getPace, cyclePace, PACE_CONFIG } from "./pace.js";

export const LLM_FAST_FAIL_HINT = // verbatim server message (this build) — shown as a toast (§13).
  "llm seats cannot join in this build: the LLM driver is not available yet — create a human-only game";

const DEFAULT_LLM_NAMES = ["Vito", "Sonny", "Carmine", "Rocco", "Salvatore", "Bruno"]; // A14 placeholders
const TIMEOUT_RE = /^\d+(ms|s|m|h)?$/; // §6.4 state table

export const PRESETS = [
  {
    id: "solo",
    title: "Solo",
    desc: "You + 1 AI",
    buildSeats: (hostName) => [
      { name: hostName, type: "human", lockedHuman: true },
      { name: "", type: "llm" },
    ],
    matches: (seats) =>
      seats.length === 2 && seats[0]?.type === "human" && seats[1]?.type === "llm",
  },
  {
    id: "standard",
    title: "Standard",
    desc: "You + 3 AI",
    buildSeats: (hostName) => [
      { name: hostName, type: "human", lockedHuman: true },
      { name: "", type: "llm" },
      { name: "", type: "llm" },
      { name: "", type: "llm" },
    ],
    matches: (seats) =>
      seats.length === 4 &&
      seats[0]?.type === "human" &&
      seats.slice(1).every((s) => s.type === "llm"),
  },
  {
    id: "full",
    title: "Full Table",
    desc: "You + 5 AI",
    buildSeats: (hostName) => [
      { name: hostName, type: "human", lockedHuman: true },
      { name: "", type: "llm" },
      { name: "", type: "llm" },
      { name: "", type: "llm" },
      { name: "", type: "llm" },
      { name: "", type: "llm" },
    ],
    matches: (seats) =>
      seats.length === 6 &&
      seats[0]?.type === "human" &&
      seats.slice(1).every((s) => s.type === "llm"),
  },
  {
    id: "passplay",
    title: "Pass & Play",
    desc: "3 Humans",
    buildSeats: (hostName) => [
      { name: hostName, type: "human", lockedHuman: true },
      { name: "", type: "human" },
      { name: "", type: "human" },
    ],
    matches: (seats) =>
      seats.length === 3 && seats.every((s) => s.type === "human"),
  },
];

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
    requireAll: true,                                  // Recommended default for AI games so windows resolve cleanly
    timeout: "",
    submitting: false,
  };
}

function ensureSeat0Locked(m) {
  if (m && m.seats && m.seats.length > 0) {
    m.seats[0].lockedHuman = true;
    m.seats[0].type = "human";
  }
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
  ensureSeat0Locked(model);

  rootEl.innerHTML = `
    <div class="lobby-brand">
      Coup<small>the table is set for two to six — claims are cheap, challenges cost.</small>
      <div class="lobby-toolbar">
        <button type="button" class="topbar-btn" id="lobby-rules-btn" title="View Coup rules &amp; roles" aria-label="View Coup rules &amp; roles (Press ? or H)">
          <span class="topbar-icon" aria-hidden="true">📜</span> Rules &amp; Roles <span class="kbd-hint" aria-hidden="true">?</span>
        </button>
        <button type="button" class="topbar-btn ${!isSoundEnabled() ? "muted" : ""}" id="lobby-sound-btn" title="Toggle sound" aria-label="Toggle sound (Press M)" aria-pressed="${isSoundEnabled() ? "true" : "false"}">
          <span class="topbar-icon" id="lobby-sound-icon" aria-hidden="true">${isSoundEnabled() ? "🔊" : "🔇"}</span>
          <span id="lobby-sound-text">${isSoundEnabled() ? "Sound" : "Muted"}</span>
          <span class="kbd-hint" aria-hidden="true">M</span>
        </button>
        <button type="button" class="topbar-btn" id="lobby-pace-btn" title="AI Turn Pace: ${(PACE_CONFIG[getPace()] || PACE_CONFIG.normal).label.replace("Pace: ", "")} (${(PACE_CONFIG[getPace()] || PACE_CONFIG.normal).desc} · Press P to cycle)" aria-label="AI Turn Pace: ${(PACE_CONFIG[getPace()] || PACE_CONFIG.normal).label}">
          <span class="topbar-icon" id="lobby-pace-icon" aria-hidden="true">${(PACE_CONFIG[getPace()] || PACE_CONFIG.normal).icon}</span>
          <span id="lobby-pace-text">${(PACE_CONFIG[getPace()] || PACE_CONFIG.normal).label}</span>
          <span class="kbd-hint" aria-hidden="true">P</span>
        </button>
      </div>
    </div>

    <section class="lobby-card" aria-labelledby="newgame-h">
      <span class="kicker">SEAT THE TABLE</span>
      <h1 id="newgame-h">New game</h1>

      <div class="lobby-presets-wrap">
        <div class="preset-header">
          <span class="preset-label">Quick Presets</span>
        </div>
        <div class="preset-buttons" role="group" aria-label="Game setup presets">
          ${PRESETS.map((p) => `
            <button type="button" class="preset-btn" data-preset="${p.id}" title="${p.title} (${p.desc})">
              <span class="p-title">${p.title}</span>
              <span class="p-desc">${p.desc}</span>
            </button>
          `).join("")}
        </div>
      </div>

      <div class="seat-rows" id="seat-rows"></div>

      <details class="win-settings" open>
        <summary>
          <span>Window settings</span>
          <span class="ws-summary-badge" id="ws-summary-badge"></span>
        </summary>
        <div class="ws-body">
          <div class="ws-card">
            <label class="ws-toggle-label" for="ws-reqall">
              <input type="checkbox" id="ws-reqall">
              <div class="ws-toggle-text">
                <div class="ws-toggle-head">Wait for every eligible seat to answer</div>
                <div class="ws-toggle-sub">
                  When enabled, windows wait for all players to respond before resolving.
                  When disabled, the first real answer ends the window.
                </div>
              </div>
            </label>
          </div>
          <div class="ws-card">
            <div class="ws-timeout-head">
              <label for="ws-timeout">Auto-pass unresponded seats after:</label>
              <span class="ws-subtext">(blank = never)</span>
            </div>
            <div class="ws-duration-group">
              <div class="duration-pills" role="group" aria-label="Timeout duration options">
                <button type="button" class="duration-pill" data-dur="30s">30s</button>
                <button type="button" class="duration-pill" data-dur="45s">45s</button>
                <button type="button" class="duration-pill" data-dur="1m">1m</button>
                <button type="button" class="duration-pill" data-dur="">Never</button>
              </div>
              <div class="timeout-input-wrap">
                <input type="text" class="timeout-in" id="ws-timeout" placeholder="e.g. 45s, 2m" autocomplete="off" spellcheck="false">
              </div>
            </div>
            <div class="ws-hint" id="ws-hint">Default is open-ended — a stalled window can be resolved by a forfeit vote.</div>
          </div>
        </div>
      </details>
      <button type="button" class="btn btn-primary" id="create-btn" style="width:100%">Create game</button>
    </section>

    <form class="lobby-card" aria-labelledby="join-h" id="join-form" novalidate>
      <span class="kicker">ALREADY SEATED?</span>
      <h1 id="join-h">Join a game</h1>
      <div class="join-field-group">
        <label for="join-id" class="join-label">Game ID</label>
        <input type="text" class="field-in" id="join-id" placeholder="e.g. g_0a1b2c3d4e5f" autocomplete="off" spellcheck="false">
        <div class="field-help">Found in game URL or invitation</div>
      </div>
      <div class="join-field-group">
        <label for="join-token" class="join-label">Seat Token</label>
        <input type="password" class="field-in" id="join-token" placeholder="Your secret seat token" autocomplete="off" spellcheck="false">
        <div class="field-help">Assigned secret token for your seat</div>
      </div>
      <button type="submit" class="btn btn-outline" id="join-btn" style="width:100%">Enter the table</button>
      <div class="join-inline-err" id="join-err" role="alert" aria-live="polite"></div>
    </form>`;

  const lRulesBtn = rootEl.querySelector("#lobby-rules-btn");
  if (lRulesBtn) {
    lRulesBtn.addEventListener("click", () => {
      initAudio();
      playClick();
      toggleRulesModal();
    });
  }

  const lSoundBtn = rootEl.querySelector("#lobby-sound-btn");
  if (lSoundBtn) {
    lSoundBtn.addEventListener("click", () => {
      toggleSound();
    });
  }

  const lPaceBtn = rootEl.querySelector("#lobby-pace-btn");
  if (lPaceBtn) {
    lPaceBtn.addEventListener("click", () => {
      initAudio();
      playClick();
      cyclePace();
    });
  }

  // --- presets sync ----------------------------------------------------------
  function syncPresetButtons() {
    const activePreset = PRESETS.find((p) => p.matches(m2().seats));
    rootEl.querySelectorAll(".preset-btn").forEach((btn) => {
      btn.classList.toggle("active", activePreset ? btn.dataset.preset === activePreset.id : false);
    });
  }

  rootEl.querySelectorAll(".preset-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      const presetId = btn.dataset.preset;
      const preset = PRESETS.find((p) => p.id === presetId);
      if (!preset) return;
      const hostName = m2().seats[0]?.name || "";
      m2().seats = preset.buildSeats(hostName);
      ensureSeat0Locked(m2());
      renderRows();
      syncCreate();
    });
  });

  // --- seat rows -------------------------------------------------------------
  const rowsEl = rootEl.querySelector("#seat-rows");
  function renderRows() {
    ensureSeat0Locked(m2());
    const seats = m2().seats;
    const count = seats.length;

    rowsEl.innerHTML = seats.map((s, i) => {
      const ph = placeholderFor(s, i, llmSeenAt(m2(), i));
      const locked = (i === 0);
      const isHuman = s.type === "human";
      const isLlm = s.type === "llm";

      return `<div class="seat-row" data-i="${i}">
        <span class="seat-num ${locked ? "seat-num-host" : ""}" title="${locked ? "Seat 1 (Host — locked human)" : `Seat ${i + 1}`}">${i + 1}</span>
        <input type="text" class="name-in" data-name="${i}" placeholder="${esc(ph)}" maxlength="32"
          value="${esc(s.name)}" ${locked ? 'aria-label="Seat 1 name — host seat"' : `aria-label="Seat ${i + 1} name"`}>
        <span class="seg" role="group" aria-label="Seat ${i + 1} type">
          <button type="button" data-type-human="${i}" class="${isHuman ? "on" : ""}"
            ${locked ? "disabled title='Seat 1 is the game host and must be human'" : `title="Seat ${i + 1} Human"`}>Human</button>
          <button type="button" data-type-llm="${i}" class="${isLlm ? "on" : ""}"
            ${locked ? "disabled title='Seat 1 is the game host and must be human'" : `title="Seat ${i + 1} AI"`}>AI</button>
        </span>
        ${locked
          ? `<button type="button" class="row-x" disabled style="visibility:hidden;pointer-events:none;" aria-hidden="true" tabindex="-1">×</button>`
          : `<button type="button" class="row-x" data-remove="${i}" aria-label="Remove seat ${i + 1}"
              ${count <= 2 ? 'disabled title="A game requires at least 2 seats"' : `title="Remove seat ${i + 1}"`}>×</button>`
        }
      </div>`;
    }).join("") +
    `<button type="button" class="add-seat" id="add-seat" ${count >= 6 ? 'disabled title="Maximum 6 seats reached"' : 'title="Add another seat"'}>
      + Add a seat <span class="seat-count">(${count}/6)</span>
    </button>`;

    // Wire input changes
    rowsEl.querySelectorAll("[data-name]").forEach((inp) => {
      inp.addEventListener("input", () => {
        const idx = parseInt(inp.dataset.name, 10);
        if (m2().seats[idx]) {
          m2().seats[idx].name = inp.value;
          syncCreate();
        }
      });
    });

    // Wire type toggle
    const setType = (i, t) => {
      if (i === 0) return; // Row 0 is locked human
      const seat = m2().seats[i];
      if (!seat || seat.lockedHuman) return;
      seat.type = t;
      renderRows();
      syncCreate();
    };

    rowsEl.querySelectorAll("[data-type-human]").forEach((b) => {
      b.addEventListener("click", () => setType(parseInt(b.dataset.typeHuman, 10), "human"));
    });
    rowsEl.querySelectorAll("[data-type-llm]").forEach((b) => {
      b.addEventListener("click", () => setType(parseInt(b.dataset.typeLlm, 10), "llm"));
    });

    // Wire remove seat
    rowsEl.querySelectorAll("[data-remove]").forEach((b) => {
      b.addEventListener("click", () => {
        const i = parseInt(b.dataset.remove, 10);
        if (i === 0 || m2().seats.length <= 2) return;
        m2().seats.splice(i, 1);
        ensureSeat0Locked(m2());
        renderRows();
        syncCreate();
      });
    });

    // Wire add seat
    rootEl.querySelector("#add-seat").addEventListener("click", () => {
      if (m2().seats.length >= 6) return;
      m2().seats.push({ name: "", type: "llm" });
      ensureSeat0Locked(m2());
      renderRows();
      syncCreate();
    });

    syncPresetButtons();
  }

  // --- window settings ---------------------------------------------------------
  const reqAllEl = rootEl.querySelector("#ws-reqall");
  const timeoutEl = rootEl.querySelector("#ws-timeout");
  const summaryBadgeEl = rootEl.querySelector("#ws-summary-badge");
  const hintEl = rootEl.querySelector("#ws-hint");

  reqAllEl.checked = model.requireAll;
  timeoutEl.value = model.timeout;

  function syncSummaryBadge() {
    if (!summaryBadgeEl) return;
    const modeText = model.requireAll ? "Wait for all" : "First answer";
    const cur = (model.timeout || "").trim();
    const timeText = cur ? cur : "Open-ended";
    summaryBadgeEl.textContent = `${modeText} · ${timeText}`;
  }

  function syncDurationPills() {
    const cur = (model.timeout || "").trim();
    rootEl.querySelectorAll(".duration-pill").forEach((btn) => {
      const dur = btn.dataset.dur;
      const isActive = (dur === "" && cur === "") || (dur !== "" && cur === dur);
      btn.classList.toggle("active", isActive);
    });
  }

  function syncWsHint() {
    if (!hintEl) return;
    const t = (model.timeout || "").trim();
    if (t && TIMEOUT_RE.test(t)) {
      hintEl.textContent = `Silent seats auto-pass after ${t}.`;
    } else if (t) {
      hintEl.textContent = "Duration must be like 30s or 2m (or blank).";
    } else {
      hintEl.textContent = "Default is open-ended — a stalled window can be resolved by a forfeit vote.";
    }
  }

  reqAllEl.addEventListener("change", () => {
    model.requireAll = reqAllEl.checked;
    syncSummaryBadge();
  });

  timeoutEl.addEventListener("input", () => {
    model.timeout = timeoutEl.value;
    syncDurationPills();
    syncSummaryBadge();
    syncWsHint();
    syncCreate();
  });

  rootEl.querySelectorAll(".duration-pill").forEach((btn) => {
    btn.addEventListener("click", () => {
      const dur = btn.dataset.dur;
      model.timeout = dur;
      timeoutEl.value = dur;
      syncDurationPills();
      syncSummaryBadge();
      syncWsHint();
      syncCreate();
    });
  });

  // --- create ------------------------------------------------------------------
  const createBtn = rootEl.querySelector("#create-btn");
  function syncCreate() {
    const err = validate(model);
    createBtn.disabled = !!err || model.submitting;
    createBtn.title = err ? err : ""; // §6.4: the why lives in the button title
    syncWsHint();
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
      if (onCreated) {
        onCreated(res.game_id, { gameId: res.game_id, seatId: mySeat.id, token: mySeat.token });
      }
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

  function cleanGameId(val) {
    let v = (val || "").trim();
    if (v.includes("#/g/")) {
      const parts = v.split("#/g/");
      if (parts[1]) v = parts[1].split(/[?&/#\s]/)[0].trim();
    }
    return v.replace(/\s+/g, "");
  }

  function cleanToken(val) {
    return (val || "").trim().replace(/\s+/g, "");
  }

  if (joinPrefill) {
    idEl.value = cleanGameId(joinPrefill);
    setTimeout(() => {
      tokenEl.focus();
    }, 60);
  } else if (location.hash === "#/?join") {
    setTimeout(() => {
      idEl.focus();
    }, 60);
  }

  idEl.addEventListener("input", () => {
    errEl.textContent = "";
    idEl.classList.remove("input-invalid");
  });
  idEl.addEventListener("blur", () => {
    idEl.value = cleanGameId(idEl.value);
  });
  idEl.addEventListener("paste", () => {
    setTimeout(() => {
      idEl.value = cleanGameId(idEl.value);
    }, 0);
  });

  tokenEl.addEventListener("input", () => {
    errEl.textContent = "";
    tokenEl.classList.remove("input-invalid");
  });
  tokenEl.addEventListener("blur", () => {
    tokenEl.value = cleanToken(tokenEl.value);
  });
  tokenEl.addEventListener("paste", () => {
    setTimeout(() => {
      tokenEl.value = cleanToken(tokenEl.value);
    }, 0);
  });

  const doJoin = () => {
    const gid = cleanGameId(idEl.value);
    const tok = cleanToken(tokenEl.value);
    idEl.value = gid;
    tokenEl.value = tok;
    errEl.textContent = "";
    idEl.classList.remove("input-invalid");
    tokenEl.classList.remove("input-invalid");

    if (!gid && !tok) {
      errEl.textContent = "Please enter both a game ID and your seat token.";
      idEl.classList.add("input-invalid");
      tokenEl.classList.add("input-invalid");
      idEl.focus();
      return;
    }
    if (!gid) {
      errEl.textContent = "Please enter the game ID.";
      idEl.classList.add("input-invalid");
      idEl.focus();
      return;
    }
    if (!tok) {
      errEl.textContent = "Please enter your seat token.";
      tokenEl.classList.add("input-invalid");
      tokenEl.focus();
      return;
    }
    if (onJoin) {
      onJoin(gid, tok);
    }
  };

  rootEl.querySelector("#join-form").addEventListener("submit", (e) => {
    e.preventDefault();
    doJoin();
  });

  renderRows();
  syncDurationPills();
  syncSummaryBadge();
  syncCreate();
}

function m2() { return model; }

// Wired by main.js (composition boundary — keeps lobby.js free of session/routing).
let onCreated = null, onJoin = null;
export function setLobbyHooks(h) { if (h.onCreated) onCreated = h.onCreated; if (h.onJoin) onJoin = h.onJoin; }

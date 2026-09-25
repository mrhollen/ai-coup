// ===========================================================================
// decide.js — every decision surface of ui-spec §8–§10 plus §6.3/§13 glue:
//   window response modal (data-driven from your_allowed_responses), flip
//   choice, exchange finish, forfeit initiation + ballot, game-over overlay,
//   auth-error / game-gone overlays, the one-time token notice, toasts.
// Owns all POST traffic (§7.4) and the stale-response UX (§7.5).
// ===========================================================================

import { cap, esc, seatById } from "./state.js";
import { apiGet, apiPost, ApiError, kickerForCode } from "./poll.js";
import { chipHtml, renderName, charColor, charMonogram, consoleActBody, consoleUi } from "./render.js";

let refresh = () => {};        // set by main.js — full table re-render
let resnapshotFn = null;       // set by main.js — recovery resnapshot (§7.4)
export function setDecideHooks(h) { Object.assign({}, h); if (h.refresh) refresh = h.refresh; if (h.resnapshot) resnapshotFn = h.resnapshot; }

const $modalRoot = () => document.getElementById("modal-root");
const $overlayRoot = () => document.getElementById("overlay-root");
const $toasts = () => document.getElementById("toast-root");

// ---------------------------------------------------------------------------
// §4.7 toasts — verbatim table-voice, no paraphrase (§13).
// ---------------------------------------------------------------------------

export function toast(kind, kickerText, message, ttlMs) {
  const root = $toasts();
  while (root.children.length >= 3) root.firstElementChild.remove(); // max 3, discard oldest
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.setAttribute("role", kind === "error" ? "alert" : "status");
  el.innerHTML = `<span class="t-kicker">${esc(kickerText)}</span>
    <div class="t-msg"></div>
    <button type="button" class="t-x" aria-label="Dismiss">×</button>`;
  el.querySelector(".t-msg").textContent = message; // verbatim — textContent, not innerHTML
  root.appendChild(el);
  const kill = () => el.remove();
  el.querySelector(".t-x").addEventListener("click", kill);
  setTimeout(kill, ttlMs || (kind === "error" ? 8000 : 5000)); // §4.7 auto-dismiss
}

/** Server rejection → error toast with the §13 kicker + verbatim message. */
export function rejectToast(code, message) {
  toast("error", kickerForCode(code), message || "the table has something to say");
}
export function infoToast(message) { toast("info", "TABLE", message); }

// ---------------------------------------------------------------------------
// §4.8 modal shell — one backdrop + panel per root; focus trap (§15).
// Returns a handle: .panel, .bodyEl, .footEl, .close(fn), .setFoot(html) …
// ---------------------------------------------------------------------------

function openShell({ wide = false, closable = false, labelledBy = null, onEsc = null, initialFocusSel = null } = {}) {
  window.__openRoots = window.__openRoots || [];
  closeAllModals();
  const root = $modalRoot();
  const wrap = document.createElement("div");
  wrap.className = "modal-root";
  wrap.innerHTML = `<div class="backdrop"></div>`;
  const panel = document.createElement("section");
  panel.className = "modal-panel" + (wide ? " wide" : "");
  panel.setAttribute("role", "dialog");
  panel.setAttribute("aria-modal", "true");
  if (labelledBy) panel.setAttribute("aria-labelledby", labelledBy);
  wrap.appendChild(panel);
  root.appendChild(wrap);

  let lastFocus = document.activeElement;
  const focusables = () => [...panel.querySelectorAll('button:not([disabled]), [href], input, [tabindex]:not([tabindex="-1"])')]
    .filter((x) => x.offsetParent !== null || x === document.activeElement);
  panel.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && onEsc) { e.preventDefault(); onEsc(); return; }
    if (e.key !== "Tab") return;
    const f = focusables();
    if (!f.length) return;
    const first = f[0], last = f[f.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });

  const handle = {
    panel,
    wrap,
    get open() { return root.contains(wrap); },
    focusInitial(sel) {
      const el = sel ? panel.querySelector(sel) : null;
      (el || focusables()[0] || panel).focus();
    },
    close() {
      if (!handle.open) return;
      wrap.remove();
      if (lastFocus && document.contains(lastFocus)) lastFocus.focus(); // §15 focus return
    },
  };
  return handle;
}

export function closeAllModals() {
  $modalRoot().innerHTML = "";
}
function closeOverlays() { $overlayRoot().innerHTML = ""; }

// ---------------------------------------------------------------------------
// Internal surface bookkeeping (module-local, not part of `state`).
// ---------------------------------------------------------------------------
const ui = {
  win: null,        // {h, windowId, mode:'deciding'|'locked'|'closed'|'stale', respondedAt, timer}
  dec: null,        // flip/exchange modal handle keyed by decision id
  ballot: null,     // {h, voteId, voted, closedByEsc, lastCastSeen}
  tokenShown: false,
  tokenModal: null, // open shell handle of the one-time token notice (null when closed)
  gameOverShown: false,
  heldWindowId: null,   // response modal held (✕/Esc) for this window (§8.1)
  reopenPill: null,     // docked pill element
};

// ===========================================================================
// §8 WINDOW RESPONSE modal
// ===========================================================================

const RESP_BUTTONS = {
  pass:                    { label: "Pass",                   cls: "btn-resp-pass",       icon: "✕", post: () => ({ response: { type: "pass" } }) },
  challenge:               { label: "Challenge",              cls: "btn-resp-challenge",  icon: "⚔️", post: () => ({ response: { type: "challenge" } }) },
  counterclaim_contessa:   { label: "Block as Contessa",      cls: "btn-resp-contessa",   icon: "🛡️", post: () => ({ response: { type: "counterclaim", character: "contessa" } }) },
  counterclaim_captain:    { label: "Block as Captain",       cls: "btn-resp-captain",    icon: "🛡️", post: () => ({ response: { type: "counterclaim", character: "captain" } }) },
  counterclaim_ambassador: { label: "Block as Ambassador",    cls: "btn-resp-ambassador", icon: "🛡️", post: () => ({ response: { type: "counterclaim", character: "ambassador" } }) },
  counterclaim_duke:       { label: "Block as Duke",          cls: "btn-resp-duke",       icon: "🛡️", post: () => ({ response: { type: "counterclaim", character: "duke" } }) },
};

// §8.1 button order left→right: blocks (listed order), Challenge, Pass.
const RESP_ORDER = ["counterclaim_contessa", "counterclaim_captain", "counterclaim_ambassador", "counterclaim_duke", "challenge", "pass"];

function openWindowModal(state) {
  const w = state.window;
  if (!w || !w.eligible.includes(state.mySeatId)) return;
  // An actor can NEVER challenge or vote on their own declared turn action
  if (w.kind === "challenge" && w.actorId === state.mySeatId) return;
  // A blocker can NEVER challenge their own counterclaim block
  if (w.kind === "counterclaim_challenge" && w.blockerId === state.mySeatId) return;
  if (ui.win && ui.win.windowId === w.id && ui.win.open) { updateWindowModal(state); return; }

  const isBlock = w.kind === "counterclaim_challenge";
  const isTarget = !isBlock && !!w.targetSeatId && w.targetSeatId === state.mySeatId;
  const isActorOfBlockedAction = isBlock && w.actorId === state.mySeatId;

  let kicker = "THE WINDOW";
  let titleText = "";
  let subText = "";

  if (isBlock) {
    const blocker = seatById(state, w.blockerId);
    const bName = blocker ? blocker.name : "An opponent";
    const charName = w.blockChar ? cap(w.blockChar) : "a character";
    kicker = isActorOfBlockedAction ? "🛡️ YOUR ACTION IS BEING BLOCKED" : "🛡️ OPPONENT BLOCK CLAIM";
    titleText = isActorOfBlockedAction
      ? `${bName} claims ${charName} to block your action!`
      : `${bName} claims ${charName} to block the action`;
    subText = isActorOfBlockedAction
      ? `If you believe ${bName} is bluffing, you can CHALLENGE their claim. If you accept the block (Pass), your action fizzles.`
      : `Any active player may challenge ${bName}'s block claim, or allow it to stand.`;
  } else if (isTarget) {
    const attacker = seatById(state, w.actorId);
    const aName = attacker ? attacker.name : "An opponent";
    const act = w.triggerActionSeq ? state.actionsBySeq[w.triggerActionSeq] : null;
    kicker = "🚨 YOU ARE BEING TARGETED!";
    titleText = `${aName} is targeting YOU with ${cap(act?.type || "an attack")}!`;
    subText = "Choose your response: defend with an allowed character, challenge their claim if you suspect a bluff, or pass.";
  } else {
    const actor = seatById(state, w.actorId);
    const aName = actor ? actor.name : "An opponent";
    kicker = "⚡ OPPONENT CLAIM";
    titleText = w.triggerText || windowTitleFallback(state, w);
    subText = `Do you want to challenge ${aName}'s claim or allow the action to proceed?`;
  }

  const h = openShell({
    wide: true,
    closable: true,
    labelledBy: "win-title",
    onEsc: () => holdWindowModal(state), // Esc = "hold" — close without responding (§8.1)
    initialFocusSel: "[data-resp='pass']", // the safe default gets first focus (§15)
  });
  h.panel.innerHTML = `
    <div class="m-head">
      <span class="modal-kicker">${kicker}</span>
      <button type="button" class="m-close" aria-label="Hold — close without responding">×</button>
      <h2 class="modal-title" id="win-title"></h2>
      <p class="modal-sub" id="win-desc" style="margin:6px 0 0;font-size:13px;color:var(--ink-mid);line-height:1.45"></p>
      ${isTarget ? `
        <div class="target-warning-banner" role="alert">
          <span class="target-warning-icon">⚠️</span>
          <div class="target-warning-body">
            <div class="target-warning-title">YOU ARE THE TARGET!</div>
            <div class="target-warning-desc">This action directly attacks your influence. Choose your response carefully: pass, challenge, or block with an allowed character!</div>
          </div>
        </div>` : ""}
    </div>
    <div class="m-body"><div class="chips" id="win-chips"></div></div>
    <div class="m-set" id="win-set"></div>
    <div class="m-foot" id="win-foot"></div>`;
  const titleEl = h.panel.querySelector("#win-title");
  if (titleEl) titleEl.textContent = titleText;
  const descEl = h.panel.querySelector("#win-desc");
  if (descEl) descEl.textContent = subText;
  h.panel.querySelector(".m-close").addEventListener("click", () => holdWindowModal(state));

  ui.win = { h, windowId: w.id, mode: w.iResponded ? "locked" : "deciding", respondedAt: Date.now(), timer: null,
    get open() { return this.h.open; } }; // .open lives on the shell handle
  updateWindowModal(state);
  h.focusInitial("[data-resp='pass']");
}

function windowTitleFallback(state, w) {
  const a = w.triggerActionSeq ? state.actionsBySeq[w.triggerActionSeq] : null;
  if (a && a.actorId) return `${seatById(state, a.actorId)?.name || "Opponent"} claims ${cap(a.character || a.type || "an action")}`;
  return "An action is pending review";
}

function holdWindowModal(state) {
  if (!ui.win || !ui.win.open) return;
  ui.heldWindowId = state.window ? state.window.id : null; // reopen pill tracks this (§8.1)
  ui.win.timer && clearTimeout(ui.win.timer);
  ui.win.h.close();
  ui.win = null;
  refresh(); // surfaces the docked "Respond — a window is open" pill
}

function updateWindowModal(state) {
  const w = state.window;
  if (!ui.win || !ui.win.open || !w || w.id !== ui.win.windowId) return;
  const h = ui.win.h;

  // Response chips (§8.1): one per eligible seat, seat order, live state words.
  const chipsEl = h.panel.querySelector("#win-chips");
  if (chipsEl) {
    chipsEl.innerHTML = w.eligible.map((id) => {
      const seat = seatById(state, id);
      if (!seat) return "";
      const mine = id === state.mySeatId;
      const val = w.responses[id];
      let cls = "", word = `<span class="pending-dot">…</span>`, aria = "has not responded yet";
      let icon = "⏳";
      if (mine && w.iResponded) {
        word = `<span>${esc(w.myResponseLabel || "answered")}</span>`;
        cls = "mine-answered";
        icon = "✓";
      } else if (val === "pass") {
        cls = "passed";
        word = "passed";
        icon = "—";
      } else if (val === "challenge") {
        cls = "challenged";
        word = "challenged!";
        icon = "⚔️";
      } else if (val && val.startsWith("counterclaim:")) {
        const ch = val.slice(13);
        cls = `blocked blocked-${ch}`;
        word = `blocks as ${cap(ch)}`;
        icon = "🛡️";
      }
      return `<span class="pchip ${cls}${mine ? " mine" : ""}"><span class="pchip-icon">${icon}</span>${renderName(seat)}<span class="pstate" aria-label="${aria}">${word}</span></span>`;
    }).join("");
  }

  // Settings line (§8.1): countdown (timeout) / require-all progress / first-answer note.
  const setEl = h.panel.querySelector("#win-set");
  if (setEl) {
    let left = "", right = "";
    if (w.requireAll) {
      const k = Object.keys(w.responses).length;
      left = `waits for every seat to answer · ${k} of ${w.eligible.length} answered`;
    } else {
      left = "first real answer ends it";
    }
    if (w.deadlineAt != null) {
      const rem = Math.max(0, w.deadlineAt - Date.now());
      const s = Math.ceil(rem / 1000);
      right = `<span class="cd${s <= 10 ? " hot" : ""}">closes in ${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}</span>`;
    }
    setEl.innerHTML = `<span>${esc(left)}</span>${right}`;
    // §15: static accessible alternative refreshed per minute, not per second.
    if (w.deadlineAt != null) {
      const remS = Math.max(0, Math.round((w.deadlineAt - Date.now()) / 1000));
      setEl.setAttribute("aria-label", `Window closes in about ${Math.ceil(remS / 60)} minutes. ${left}`);
    }
  }

  // Footer: buttons (deciding) or status line (locked/closed/stale).
  const foot = h.panel.querySelector("#win-foot");
  if (!foot) return;
  if (ui.win.mode === "deciding" && !w.iResponded) {
    foot.innerHTML = "";
    const isBlock = w.kind === "counterclaim_challenge";
    const isTarget = !isBlock && !!w.targetSeatId && w.targetSeatId === state.mySeatId;
    const order = RESP_ORDER.filter((k) => w.myAllowed.includes(k));
    // Unknown future grammar values render raw and fail closed to a toast (§8.1).
    for (const k of w.myAllowed) if (!order.includes(k)) order.push(k);
    for (const k of order) {
      const def = RESP_BUTTONS[k];
      const btn = document.createElement("button");
      btn.type = "button";
      btn.dataset.resp = k;
      btn.className = "btn " + (def ? def.cls : "btn-outline"); // unknown → plain outline

      let label = def ? def.label : esc(k);
      let icon = def ? def.icon : "";
      if (k === "pass") {
        if (isBlock) label = "Accept Block (Pass)";
        else if (isTarget) label = "Yield / Allow (Pass)";
        else label = "Allow (Pass)";
      } else if (k === "challenge") {
        if (isBlock) label = "Challenge Block";
        else label = "Challenge Claim";
      }

      if (icon) {
        btn.innerHTML = `<span class="btn-ic">${icon}</span> ${esc(label)}`;
      } else {
        btn.textContent = label;
      }
      btn.addEventListener("click", () => submitResponse(state, k));
      foot.appendChild(btn);
    }
  } else if (ui.win.mode === "locked") {
    foot.innerHTML = `<div class="m-status" style="padding:16px 0 4px">You answered: ${esc(w.myResponseLabel || "?")} — the table is deciding…</div>`;
  }
}

async function submitResponse(state, grammarValue) {
  const body = RESP_BUTTONS[grammarValue] ? RESP_BUTTONS[grammarValue].post() : { response: { type: grammarValue } };
  setButtonsDisabled(state, true);
  try {
    await apiPost(state, `/games/${state.gameId}/decisions`, { kind: "respond", ...body });
    // 202 = queued (§7.4): enter S2 locked-awaiting; outcome arrives as events.
    if (ui.win) ui.win.mode = "locked";
    state.consoleLocked = true; // harmless if the console is hidden
    refresh();
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    handleDecisionRejection(state, e, {
      staleNotice: (deciderName) => {
        if (ui.win && ui.win.open) {
          ui.win.mode = "stale";
          const foot = ui.win.h.panel.querySelector("#win-foot");
          foot.innerHTML = `<div class="m-status stale">The moment has passed — ${esc(deciderName)} answered first.</div>`;
          ui.win.timer && clearTimeout(ui.win.timer);
          ui.win.timer = setTimeout(() => { closeWindowModal(); }, 3500); // U10
        }
      },
    });
  }
}

function setButtonsDisabled(state, disabled) {
  const foot = ui.win && ui.win.open ? ui.win.h.panel.querySelector("#win-foot") : null;
  if (!foot) return;
  foot.querySelectorAll("button").forEach((b) => { b.disabled = disabled; });
}

function closeWindowModal() {
  if (ui.win) { ui.win.timer && clearTimeout(ui.win.timer); ui.win.h.close(); }
  ui.win = null;
  ui.heldWindowId = null;
  removeReopenPill();
  refresh();
}

// §7.5 layer 3 — window_closed landed while the modal is open (no POST race):
// neutral closed-state, auto-close 3.5s (S3). For S2 (I answered) a neutral
// "the table decided" close once the cascade leaves the window phase.
export function transitionWindowModalOnClose(state, justClosedId) {
  if (!ui.win || !ui.win.open || ui.win.windowId !== justClosedId) return;
  const w = state.lastWindowClose;
  const decider = w && w.deciderSeatId ? seatById(state, w.deciderSeatId)?.name : null;
  let line;
  if (ui.win.mode === "deciding") {
    // I had not acted (§8.1 S3): neutral per-reason copy.
    switch (w && w.reason) {
      case "first_response": line = `Window closed — ${decider || "?"} moved first.`; break;
      case "all_responded": line = "Window closed — every seat has answered."; break;
      case "timeout": line = "Window closed — the window timed out; silent seats pass."; break;
      case "auto_no_responders": line = "Window closed — no one was left to answer."; break;
      default: line = "The window closed.";
    }
  } else {
    line = "The table decided."; // S2 aftermath (unspecified gap in §8.1 — resolved here)
  }
  ui.win.mode = "closed";
  const foot = ui.win.h.panel.querySelector("#win-foot");
  if (foot) foot.innerHTML = `<div class="m-status" style="padding:16px 0 4px">${esc(line)}</div>`;
  ui.win.timer && clearTimeout(ui.win.timer);
  ui.win.timer = setTimeout(() => closeWindowModal(), 3500); // U10
}

// The docked "Respond — a window is open" pill (§8.1 close affordance).
function syncReopenPill(state) {
  const felt = document.getElementById("felt");
  if (!felt) return;
  const w = state.window;
  const canRespond = !!(w && Array.isArray(w.eligible) && w.eligible.includes(state.mySeatId)
    && (w.kind !== "challenge" || w.actorId !== state.mySeatId)
    && (w.kind !== "counterclaim_challenge" || w.blockerId !== state.mySeatId));
  const should = !!(canRespond && !w.iResponded && (!ui.win || !ui.win.open) && state.status === "in_progress");
  if (should && !ui.reopenPill) {
    const el = document.createElement("div");
    el.className = "reopen-pill";
    el.innerHTML = `<button type="button" class="btn btn-outline">Respond — a window is open</button>`;
    el.querySelector("button").addEventListener("click", () => { ui.heldWindowId = null; removeReopenPill(); refresh(); }); // explicit reopen lifts the hold
    felt.appendChild(el);
    ui.reopenPill = el;
  } else if (!should && ui.reopenPill) {
    removeReopenPill();
  }
}
function removeReopenPill() { if (ui.reopenPill) { ui.reopenPill.remove(); ui.reopenPill = null; } }

// ===========================================================================
// §9 SINGLE-SEAT modals — flip choice & exchange finish. NOT dismissable:
// no ✕, no Esc (U14); the hint line explains why.
// ===========================================================================

function openDecisionModal(state) {
  const d = state.decision;
  if (!d || d.seatId !== state.mySeatId) return;
  // Already open for this exact decision: leave it be — the 250ms ticker keeps
  // its countdown line fresh (ui-spec §9), and re-opening would reset focus.
  if (ui.dec && ui.dec.id === d.id && ui.dec.open) return;
  if (d.kind === "flip_choice") openFlipModal(state, d);
  else openExchangeModal(state, d);
}

function baseDecisionShell(state, title, kickerText) {
  const h = openShell({ labelledBy: "dec-title" }); // no onEsc → Esc does nothing (U14)
  h.panel.innerHTML = `
    <div class="m-head">
      <span class="modal-kicker">${esc(kickerText)}</span>
      <h2 class="modal-title" id="dec-title">${esc(title)}</h2>
    </div>
    <div class="m-body" id="dec-body"></div>
    <div class="m-set" id="dec-set" hidden></div>
    <div class="m-foot" id="dec-foot"></div>`;
  const hint = `<p class="modal-sub" style="margin:8px 0 0">Stuck? The other humans can call a forfeit vote.</p>`;
  h.panel.querySelector(".m-body").insertAdjacentHTML("beforeend", hint);
  return h;
}

function decisionCountdown(state, d) {
  const setEl = document.getElementById("dec-set");
  if (!setEl) return;
  if (d.deadlineAt == null) { setEl.hidden = true; return; }
  setEl.hidden = false;
  const rem = Math.max(0, d.deadlineAt - Date.now());
  const s = Math.ceil(rem / 1000);
  setEl.innerHTML = `<span>the table waits for your choice</span>
    <span class="cd${s <= 10 ? " hot" : ""}">closes in ${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}</span>`;
}

function setDecisionStatus(h, html, stale = false) {
  const foot = h.panel.querySelector("#dec-foot");
  if (foot) foot.innerHTML = `<div class="m-status${stale ? " stale" : ""}" style="padding:12px 0 4px">${html}</div>`;
}

// ---- flip choice (§9.2) ----------------------------------------------------
function openFlipModal(state, d) {
  const h = baseDecisionShell(state, "Which card falls?", "INFLUENCE LOST");
  const cards = state.myHand.slice(0, 2); // option index i ↔ your_face_down_cards[i] (§5.4/U5)
  const bodyEl = h.panel.querySelector("#dec-body");

  bodyEl.innerHTML = `
    <div class="flip-intro">Select an influence card to reveal face-up. This influence is permanently lost.</div>
    <div class="card-grid" id="flip-cards">
      ${cards.map((c, i) => `
        <div class="card-choice flip-card" id="flip-card-${i}" data-idx="${i}" role="button" tabindex="0"
             aria-label="Flip Card ${i + 1}: ${cap(c)}. Press key ${i + 1} or Enter to flip.">
          <span class="stripe" style="background:${charColor(c)}"></span>
          <span class="card-key-badge">Key [ ${i + 1} ]</span>
          <div class="card-mono" style="color:${charColor(c)}">${charMonogram(c)}</div>
          <div class="card-name">${cap(c)}</div>
          <div class="card-tag">Flip Card ${i + 1}</div>
        </div>
      `).join("")}
    </div>
    <p class="modal-sub" style="margin:10px 0 0;text-align:center">Click a card or press <strong>1</strong> ${cards.length > 1 ? "or <strong>2</strong>" : ""} on your keyboard.</p>`;

  const foot = h.panel.querySelector("#dec-foot");
  cards.forEach((c, i) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn btn-danger";
    btn.innerHTML = `<span>Flip Card ${i + 1} (${cap(c)})</span> <kbd class="kbd-badge">[${i + 1}]</kbd>`;
    btn.addEventListener("click", () => submitFlipChoice(state, d, `face_down_${i}`, i));
    foot.appendChild(btn);
  });

  // Card click & keyboard handlers
  cards.forEach((_, i) => {
    const el = bodyEl.querySelector(`#flip-card-${i}`);
    if (!el) return;
    const trigger = () => submitFlipChoice(state, d, `face_down_${i}`, i);
    el.addEventListener("click", trigger);
    el.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        trigger();
      }
    });
  });

  // Global key listener for keys 1 and 2
  const onKey = (e) => {
    if (ui.dec && ui.dec.id === d.id && ui.dec.mode === "deciding") {
      if (e.key === "1" || e.code === "Digit1" || e.code === "Numpad1") {
        e.preventDefault();
        submitFlipChoice(state, d, "face_down_0", 0);
      } else if ((e.key === "2" || e.code === "Digit2" || e.code === "Numpad2") && cards.length >= 2) {
        e.preventDefault();
        submitFlipChoice(state, d, "face_down_1", 1);
      }
    }
  };
  window.addEventListener("keydown", onKey);

  ui.dec = { h, id: d.id, kind: "flip_choice", mode: "deciding", timer: null,
    cleanup() { window.removeEventListener("keydown", onKey); },
    get open() { return this.h.open; } }; // .open lives on the shell handle
  decisionCountdown(state, d);
  h.focusInitial("#flip-card-0");
}

async function submitFlipChoice(state, d, option, index) {
  if (ui.dec && ui.dec.mode !== "deciding") return;
  if (ui.dec && ui.dec.cleanup) ui.dec.cleanup();
  const foot = ui.dec.h.panel.querySelector("#dec-foot");
  if (foot) foot.querySelectorAll("button").forEach((b) => (b.disabled = true));
  const cards = ui.dec.h.panel.querySelectorAll(".card-choice");
  cards.forEach((c) => { c.classList.add("disabled"); c.setAttribute("aria-disabled", "true"); });
  try {
    await apiPost(state, `/games/${state.gameId}/decisions`, { kind: "choose", decision_id: d.id, choice: { flip_choice: option } });
    d.submitted = true; // settles S3 vs S4 copy when the card_flipped event lands (§9.2)
    d.chosenIndex = index; // state.js#applyMyFlip uses this to remove from myHand
    if (ui.dec) { ui.dec.mode = "locked"; setDecisionStatus(ui.dec.h, "Chosen — waiting for the flip."); }
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    rejectToast(e.code, e.message); // incl. 409 "that decision was already made" (§9.2: no state change)
    if (foot) foot.querySelectorAll("button").forEach((b) => (b.disabled = false));
    cards.forEach((c) => { c.classList.remove("disabled"); c.removeAttribute("aria-disabled"); });
  }
}

// ---- exchange finish (§9.3) --------------------------------------------------
function openExchangeModal(state, d) {
  const h = baseDecisionShell(state, "Exchange Influence", "COURT EXCHANGE");
  const drawn = d.drawn || [];
  const hand = state.myHand || [];
  if (drawn.length === 0) {
    // Bootstrap without a replayed reveal — the private event is coming; wait.
    setDecisionStatus(h, "The draw is on its way…");
    ui.dec = { h, id: d.id, kind: "exchange_finish", mode: "waiting", timer: null,
      get open() { return this.h.open; } }; // .open lives on the shell handle
    return;
  }

  const bodyEl = h.panel.querySelector("#dec-body");
  bodyEl.innerHTML = `
    <div class="ex-no-swap-banner">
      <button type="button" class="btn btn-outline" id="btn-no-swap">
        <span class="tally-ic">✓</span> Keep Both Hand Cards (No Swap)
      </button>
    </div>

    <div class="ex-box">
      <div class="ex-box-head">
        <span class="ex-box-title">Court Deck Draw — Visible only to you</span>
        <span class="ex-box-badge">🔒 Private</span>
      </div>
      <div class="card-grid" id="ex-drawn-cards">
        ${drawn.map((c, i) => `
          <div class="card-choice ex-card" id="ex-drawn-${i}" data-idx="${i}" role="button" tabindex="0"
               aria-label="Drawn Card ${i + 1}: ${cap(c)}">
            <span class="stripe" style="background:${charColor(c)}"></span>
            <span class="card-key-badge">${i === 0 ? "Left" : "Right"}</span>
            <div class="card-mono" style="color:${charColor(c)}">${charMonogram(c)}</div>
            <div class="card-name">${cap(c)}</div>
            <div class="card-tag" id="ex-drawn-tag-${i}">Keep Card</div>
          </div>
        `).join("")}
      </div>
    </div>

    <div class="ex-box">
      <div class="ex-box-head">
        <span class="ex-box-title">Your Current Hand</span>
        <span class="ex-box-badge">${hand.length} Card${hand.length === 1 ? "" : "s"}</span>
      </div>
      <div class="card-grid" id="ex-hand-cards">
        ${hand.map((c, i) => `
          <div class="card-choice ex-card" id="ex-hand-${i}" data-idx="${i}" role="button" tabindex="0"
               aria-label="Hand Card ${i + 1}: ${cap(c)}">
            <span class="stripe" style="background:${charColor(c)}"></span>
            <span class="card-key-badge">Card ${i + 1}</span>
            <div class="card-mono" style="color:${charColor(c)}">${charMonogram(c)}</div>
            <div class="card-name">${cap(c)}</div>
            <div class="card-tag" id="ex-hand-tag-${i}">Keep in Hand</div>
          </div>
        `).join("")}
      </div>
    </div>

    <div class="ex-summary-bar" id="ex-summary">
      <span class="ex-summary-icon" id="ex-summary-icon">ℹ️</span>
      <span class="ex-summary-text" id="ex-summary-text"></span>
    </div>`;

  const choice = { keep: null, handIdx: null }; // keep: null|0|1 (drawn_index), handIdx: returned card index (0 or 1)
  const foot = h.panel.querySelector("#dec-foot");

  function updateExchangeView() {
    const noSwapBtn = bodyEl.querySelector("#btn-no-swap");
    const summaryIcon = bodyEl.querySelector("#ex-summary-icon");
    const summaryText = bodyEl.querySelector("#ex-summary-text");

    if (choice.keep === null) {
      if (noSwapBtn) noSwapBtn.classList.add("active");
      // Drawn cards: both return to deck
      drawn.forEach((_, i) => {
        const el = bodyEl.querySelector(`#ex-drawn-${i}`);
        const tag = bodyEl.querySelector(`#ex-drawn-tag-${i}`);
        if (el) {
          el.classList.remove("kept");
          el.classList.add("dimmed");
        }
        if (tag) tag.textContent = "↩ Returns to Deck";
      });
      // Hand cards: both kept in hand
      hand.forEach((_, i) => {
        const el = bodyEl.querySelector(`#ex-hand-${i}`);
        const tag = bodyEl.querySelector(`#ex-hand-tag-${i}`);
        if (el) {
          el.classList.add("kept");
          el.classList.remove("returned", "dimmed");
        }
        if (tag) tag.textContent = "✓ Keep in Hand";
      });
      if (summaryIcon) summaryIcon.textContent = "✓";
      if (summaryText) summaryText.textContent = "Keeping both hand cards. Both drawn cards will return to the Court Deck.";
    } else {
      if (noSwapBtn) noSwapBtn.classList.remove("active");
      // Drawn cards: choice.keep is kept, other is returned
      drawn.forEach((c, i) => {
        const el = bodyEl.querySelector(`#ex-drawn-${i}`);
        const tag = bodyEl.querySelector(`#ex-drawn-tag-${i}`);
        if (i === choice.keep) {
          if (el) { el.classList.add("kept"); el.classList.remove("dimmed"); }
          if (tag) tag.textContent = "✓ KEEP THIS CARD";
        } else {
          if (el) { el.classList.remove("kept"); el.classList.add("dimmed"); }
          if (tag) tag.textContent = "↩ Returns to Deck";
        }
      });
      // Hand cards: choice.handIdx is replaced & returned, other kept
      hand.forEach((c, i) => {
        const el = bodyEl.querySelector(`#ex-hand-${i}`);
        const tag = bodyEl.querySelector(`#ex-hand-tag-${i}`);
        if (i === choice.handIdx) {
          if (el) { el.classList.add("returned"); el.classList.remove("kept", "dimmed"); }
          if (tag) tag.textContent = "↩ REPLACING (TO DECK)";
        } else {
          if (el) { el.classList.add("kept"); el.classList.remove("returned", "dimmed"); }
          if (tag) tag.textContent = "✓ Keep in Hand";
        }
      });
      if (summaryIcon) summaryIcon.textContent = "🔄";
      if (summaryText) {
        if (choice.handIdx != null && hand[choice.handIdx]) {
          summaryText.textContent = `Swap: Keeping ${cap(drawn[choice.keep])} and returning ${cap(hand[choice.handIdx])} to the Court Deck.`;
        } else {
          summaryText.textContent = `Keeping ${cap(drawn[choice.keep])}. Click a hand card above to choose which card to replace.`;
        }
      }
    }
    renderFooter();
  }

  function renderFooter() {
    foot.innerHTML = "";
    const noSwapAction = document.createElement("button");
    noSwapAction.type = "button";
    noSwapAction.className = "btn btn-outline";
    noSwapAction.textContent = "Keep Current Hand (No Swap)";
    noSwapAction.addEventListener("click", () => {
      choice.keep = null;
      choice.handIdx = null;
      updateExchangeView();
    });

    const confirm = document.createElement("button");
    confirm.type = "button";
    confirm.className = "btn btn-primary";
    const ready = choice.keep === null || (choice.keep !== null && choice.handIdx != null);
    confirm.disabled = !ready;
    confirm.textContent = choice.keep === null ? "Seal Exchange (No Swap)" : "Seal Exchange (Confirm Swap)";
    confirm.addEventListener("click", () => submitExchange(state, d, choice));

    foot.appendChild(noSwapAction);
    foot.appendChild(confirm);
  }

  // Wire up no-swap button
  const noSwapBtn = bodyEl.querySelector("#btn-no-swap");
  if (noSwapBtn) {
    noSwapBtn.addEventListener("click", () => {
      choice.keep = null;
      choice.handIdx = null;
      updateExchangeView();
    });
  }

  // Drawn card clicks & keyboard
  drawn.forEach((_, i) => {
    const el = bodyEl.querySelector(`#ex-drawn-${i}`);
    if (!el) return;
    const select = () => {
      choice.keep = i;
      if (hand.length === 1) {
        choice.handIdx = 0;
      } else if (choice.handIdx == null) {
        choice.handIdx = 0; // sensible default so it's ready, but user can click either hand card
      }
      updateExchangeView();
    };
    el.addEventListener("click", select);
    el.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(); }
    });
  });

  // Hand card clicks & keyboard
  hand.forEach((_, i) => {
    const el = bodyEl.querySelector(`#ex-hand-${i}`);
    if (!el) return;
    const select = () => {
      if (choice.keep === null) {
        choice.keep = 0; // if no drawn card was selected yet, default to first drawn card
      }
      choice.handIdx = i;
      updateExchangeView();
    };
    el.addEventListener("click", select);
    el.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(); }
    });
  });

  updateExchangeView();

  ui.dec = { h, id: d.id, kind: "exchange_finish", mode: "deciding", timer: null,
    get open() { return this.h.open; } }; // .open lives on the shell handle
  decisionCountdown(state, d);
  h.focusInitial("#btn-no-swap");
}

async function submitExchange(state, d, choice) {
  if (ui.dec && ui.dec.mode !== "deciding") return;
  let bodyChoice;
  if (choice.keep === null) {
    bodyChoice = { exchange_finish: { no_swap: true } }; // U4 — wire shape confirmed against tech spec §11.2(a)
  } else {
    const handIdx = choice.handIdx != null ? choice.handIdx : 0;
    bodyChoice = { exchange_finish: { drawn_index: choice.keep, hand_option: `face_down_${handIdx}` } };
  }
  const foot = ui.dec.h.panel.querySelector("#dec-foot");
  if (foot) foot.querySelectorAll("button").forEach((b) => (b.disabled = true));
  const cards = ui.dec.h.panel.querySelectorAll(".card-choice");
  cards.forEach((c) => { c.classList.add("disabled"); c.setAttribute("aria-disabled", "true"); });
  try {
    await apiPost(state, `/games/${state.gameId}/decisions`, { kind: "choose", decision_id: d.id, choice: bodyChoice });
    // §9.3/§7.3: apply MY OWN hand change locally now — I know both sides of the
    // swap; the public exchange_completed must not double-apply it.
    if (choice.keep !== null) {
      // FIX CRITICAL HAND CORRUPTION BUG:
      // In Go backend (domain/resolve.go line 301): actor.FaceDown[handIdx] = kept.
      // The card at handIdx is replaced by the chosen drawn card.
      const handIdx = choice.handIdx != null ? choice.handIdx : 0;
      state.myHand[handIdx] = d.drawn[choice.keep];
    } // keep-neither: hand unchanged
    d.submitted = true;
    if (ui.dec) { ui.dec.mode = "locked"; setDecisionStatus(ui.dec.h, "Sealed — the deck took two back."); }
    refresh();
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    rejectToast(e.code, e.message);
    if (foot) foot.querySelectorAll("button").forEach((b) => (b.disabled = false));
    cards.forEach((c) => { c.classList.remove("disabled"); c.removeAttribute("aria-disabled"); });
  }
}

// Settlement for both single-seat modals (driven from ensureSurfaces and from
// main.js#applyEvents when a same-batch turn_start clears state.decision first).
export function settleDecisionModal(state, d) {
  const m = ui.dec;
  if (!m || m.id !== d.id || !m.open) return;
  if (m.cleanup) m.cleanup();
  let line, stale;
  if (d.settled === "done") line = "Done.";
  else if (d.settled === "expired") { line = "Time passed — the table chose for you."; stale = true; }
  else if (d.settled) line = `${cap(d.settled)} is face-up now.`; // card name (flip S3)
  else return;
  m.mode = "settled";
  setDecisionStatus(m.h, line, stale);
  m.timer && clearTimeout(m.timer);
  m.timer = setTimeout(() => {
    if (ui.dec === m) {
      if (ui.dec.cleanup) ui.dec.cleanup();
      ui.dec = null;
      if (state.decision === d) state.decision = null;
      m.h.close();
      refresh();
    }
  }, 3500); // U10
}

// ===========================================================================
// §10 Forfeit voting — initiation modal + ballot modal + passive banner
// ===========================================================================

export function openForfeitInitiation(state) {
  const h = openShell({ labelledBy: "feat-title", onEsc: () => h.close() });
  h.panel.innerHTML = `
    <div class="m-head">
      <span class="modal-kicker">FORFEIT VOTE</span>
      <h2 class="modal-title" id="feat-title">Call a forfeit vote</h2>
      <p class="modal-sub">Use it when a seat — human or AI — is stuck or disconnected and the table cannot proceed.
      The other human seats vote; a majority is required to forfeit the player.</p>
    </div>
    <div class="m-body"><div id="feat-list"></div></div>
    <div class="m-foot"><button type="button" class="btn btn-outline" id="feat-close-btn">Cancel</button></div>`;
  h.panel.querySelector("#feat-close-btn").addEventListener("click", () => h.close());
  const listEl = h.panel.querySelector("#feat-list");

  function renderTargets() {
    const targets = state.seats.filter((s) => s.active && s.id !== state.mySeatId); // U18: never self
    if (targets.length === 0) {
      listEl.innerHTML = `<p class="modal-sub" style="text-align:center;padding:16px 0">No other active players to forfeit.</p>`;
      return;
    }
    listEl.innerHTML = `<div class="feat-list">` + targets.map((t) =>
      `<button type="button" class="feat-row" data-tid="${t.id}" aria-label="Call forfeit vote against ${esc(t.name)}">
        <div style="display:flex;align-items:center;gap:8px">
          <span class="seat-num">${t.seatNum != null ? t.seatNum : "?"}</span>
          <span>${renderName(t)}</span>
        </div>
        <div style="display:flex;align-items:center;gap:12px">
          <span class="fcards" title="Influences remaining">🂠 ${t.faceDownCount != null ? t.faceDownCount : "?"} cards</span>
          <span class="fcoins">🪙 ${t.coins}</span>
          <span class="btn btn-outline btn-ghost" style="font-size:12px;padding:2px 8px;border:1px solid var(--line-strong)">Vote</span>
        </div>
      </button>`).join("") + `</div>`;
    listEl.querySelectorAll(".feat-row").forEach((b) => b.addEventListener("click", () => renderConfirm(b.dataset.tid)));
  }

  function renderConfirm(tid) {
    const t = seatById(state, tid);
    if (!t) return;
    listEl.innerHTML = `
      <div style="background:rgba(179,64,47,0.14);border:1px solid var(--red-bright);border-radius:8px;padding:14px;margin-bottom:14px">
        <div style="font-weight:700;color:var(--red-bright);margin-bottom:4px;font-size:14px">⚠️ Call forfeit vote against ${renderName(t)}?</div>
        <div style="font-size:12.5px;color:var(--ink);line-height:1.45">All other human seats will vote YES or NO. If a majority agrees, ${renderName(t)} forfeits immediately and their cards are returned unrevealed to the deck.</div>
      </div>
      <div style="display:flex;gap:10px;justify-content:flex-end">
        <button type="button" class="btn btn-outline" id="feat-cancel">Cancel</button>
        <button type="button" class="btn btn-danger" id="feat-confirm">Initiate Vote</button>
      </div>`;
    listEl.querySelector("#feat-cancel").addEventListener("click", renderTargets);
    listEl.querySelector("#feat-confirm").addEventListener("click", () => {
      listEl.querySelector("#feat-confirm").disabled = true;
      apiPost(state, `/games/${state.gameId}/forfeit-votes`, { target_seat_id: tid })
        .then(() => h.close()) // 201 → the opened event opens the ballot modal a moment later
        .catch((e) => {
          if (e instanceof ApiError) rejectToast(e.code, e.message); // verbatim; modal stays open (§10.5)
          const c = listEl.querySelector("#feat-confirm");
          if (c) c.disabled = false;
        });
    });
  }

  renderTargets();
  h.focusInitial(".feat-row");
}

export function isBallotModalOpen() { return !!(ui.ballot && ui.ballot.open); }

function openBallot(state, isNewCast) {
  const v = state.vote;
  if (!v || v.result) return;
  if (!v.eligibleVoters.includes(state.mySeatId)) return; // only voters get the modal (§10.5)
  if (ui.ballot && ui.ballot.open && ui.ballot.voteId === v.id) { updateBallot(state); return; }

  const target = seatById(state, v.targetSeatId);
  const init = seatById(state, v.initiatorSeatId);
  const h = openShell({ labelledBy: "vote-title", onEsc: () => closeBallot(state) }); // §15: Esc closes without voting
  h.panel.innerHTML = `
    <div class="m-head">
      <span class="modal-kicker">THE TABLE ASKS</span>
      <h2 class="modal-title" id="vote-title">Forfeit vote — ${renderName(target)}</h2>
      <p class="modal-sub">Called by ${renderName(init)}. The other human seats decide; a majority ends it early.</p>
    </div>
    <div class="m-body" id="vote-body"></div>
    <div class="m-foot" id="vote-foot"></div>`;

  ui.ballot = { h, voteId: v.id, voted: !!v.myVote, closedByEsc: false, lastCastSeen: castCount(v),
    get open() { return this.h.open; } }; // .open lives on the shell handle
  updateBallot(state);
  if (isNewCast && !v.myVote) h.focusInitial("#vote-yes"); // a new cast pulled me back in (§10.1)
}

function castCount(v) { return Object.keys(v.votes || {}).length; }

function closeBallot(state) {
  const m = ui.ballot;
  if (!m || !m.open) return;
  m.closedByEsc = true; // the passive banner takes over (§10.1)
  m.lastCastSeen = castCount(state.vote); // keep the record: Esc must not auto-reopen
  // (no new casts since) — a genuinely NEW cast still reopens it (§10.1).
  m.h.close();
  refresh();
}

function updateBallot(state) {
  const m = ui.ballot;
  if (!m || !m.open) return;
  const v = state.vote;
  if (!v || v.id !== m.voteId) return;

  const y = Object.values(v.votes).filter((x) => x === "yes").length;
  const n = Object.values(v.votes).filter((x) => x === "no").length;
  const total = Math.max(1, y + n);
  const totalEligible = Math.max(1, v.eligibleVoters.length);
  const pctY = (y / total) * 100;
  const needFrac = v.majorityNeeded / totalEligible;

  m.h.panel.querySelector("#vote-body").innerHTML = `
    <div class="tally-line">
      <span class="tally-counts"><span class="y"><span class="tally-ic">✓</span>YES ${y}</span> &nbsp;&nbsp; <span class="n"><span class="tally-ic">✕</span>NO ${n}</span></span>
      <span class="tally-need">Majority required: <strong>${v.majorityNeeded} of ${v.eligibleVoters.length}</strong></span>
    </div>
    <div class="tally-bar" aria-hidden="true" title="${y} Yes, ${n} No">
      <span class="tally-fill-y" style="width:${pctY}%"></span>
      <span class="tally-fill-n" style="width:${100 - pctY}%"></span>
      <span class="tally-tick" style="left:${(needFrac * 100).toFixed(1)}%" title="Threshold (${v.majorityNeeded})"></span>
    </div>
    <div style="font-size:11px;font-weight:700;letter-spacing:.1em;color:var(--ink-dim);text-transform:uppercase;margin:12px 0 6px">Voter Status</div>
    <div class="voters">` + v.eligibleVoters.map((id) => {
      const seat = seatById(state, id);
      const vote = v.votes[id];
      const chip = vote === "yes" ? `<span class="vchip yes">✓ VOTED YES</span>`
        : vote === "no" ? `<span class="vchip no">✕ VOTED NO</span>`
        : `<span class="vchip pending">⏳ Waiting…</span>`;
      return `<div class="voter-row">${renderName(seat)}${chip}</div>`;
    }).join("") + `</div>`;

  const foot = m.h.panel.querySelector("#vote-foot");
  if (v.result) {
    const target = seatById(state, v.targetSeatId);
    const line = v.result === "forfeited"
      ? `🏛️ ${renderName(target)} is forfeited. Their cards are never shown.`
      : `The vote fails ${y}–${n}. ${renderName(target)} remains at the table.`; // U12: not_forfeited/voided render the fail line
    foot.innerHTML = `<div class="m-status" style="padding:12px 0 4px">${line}</div>`;
    m.timer && clearTimeout(m.timer);
    m.timer = setTimeout(() => { if (ui.ballot === m) closeBallotResolved(); }, 4000); // U10
  } else if (v.myVote) {
    foot.innerHTML = `<div class="m-status" style="padding:12px 0 4px">You voted <strong>${v.myVote.toUpperCase()}</strong>. Waiting for other ballots…</div>`;
  } else {
    foot.innerHTML = "";
    const yes = document.createElement("button");
    yes.type = "button"; yes.id = "vote-yes"; yes.className = "btn btn-yes"; yes.style.flex = "1";
    yes.innerHTML = `<span>✓ Vote YES (Forfeit)</span>`;
    yes.addEventListener("click", () => castBallot(state, true));

    const no = document.createElement("button");
    no.type = "button"; no.id = "vote-no"; no.className = "btn btn-danger"; no.style.flex = "1";
    no.innerHTML = `<span>✕ Vote NO (Keep)</span>`;
    no.addEventListener("click", () => castBallot(state, false));

    foot.appendChild(yes); foot.appendChild(no);
  }
}

function closeBallotResolved() {
  if (ui.ballot) ui.ballot.h.close();
  ui.ballot = null;
  refresh();
}

async function castBallot(state, yes) {
  const v = state.vote;
  if (!v || !v.eligibleVoters.includes(state.mySeatId)) return;
  try {
    await apiPost(state, `/games/${state.gameId}/forfeit-votes/${encodeURIComponent(v.id)}/ballots`, { vote: yes ? "yes" : "no" });
    if (ui.ballot) ui.ballot.voted = true; // the cast event arrives on the stream; latch for immediate UI
    state.vote.myVote = yes ? "yes" : "no";
    updateBallot(state);
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    rejectToast(e.code, e.message); // vote_closed / not_a_voter → verbatim + resolved state arrives via events
    resnapshotNow(state); // §7.4: close the ballot from matching events; resync
  }
}

// ===========================================================================
// Overlays — game over (§10.6), auth error (S4), game gone (U17)
// ===========================================================================

export function showGameOver(state) {
  if (ui.gameOverShown || state.status !== "finished") return;
  ui.gameOverShown = true;
  closeAllModals();
  removeReopenPill();

  const winner = seatById(state, state.winnerId);
  const iWin = !!winner && winner.id === state.mySeatId;
  // Standings: winner first, then active, then eliminated (§10.6).
  const ordered = [
    ...(winner ? [winner] : []),
    ...state.seats.filter((s) => s.id !== state.winnerId && s.active),
    ...state.seats.filter((s) => !s.active),
  ];

  const root = $overlayRoot();
  root.innerHTML = `<div class="overlay-root">
    <div class="overlay-backdrop"></div>
    <div class="go-panel" role="dialog" aria-modal="true" aria-labelledby="go-title">
      <div class="go-trophy">${iWin ? "🏆" : "👑"}</div>
      <span class="modal-kicker">THE TABLE CLOSES</span>
      <h2 class="go-title" id="go-title">${iWin ? "VICTORY!" : "GAME OVER"}</h2>
      <div class="${iWin ? "go-victory-badge" : "go-defeat-badge"}">
        ${iWin ? "YOU WON THE TABLE" : `${winner ? esc(winner.name) : "?"} WINS`}
      </div>
      <div class="go-sub">
        ${iWin ? "Congratulations! You eliminated all rivals and claimed the table." : `${winner ? renderName(winner) : "?"} is the sole survivor.`}
      </div>
      <table class="standings-table">
        <thead>
          <tr>
            <th>Rank</th>
            <th>Player</th>
            <th>Status</th>
            <th>Influence</th>
            <th>Coins</th>
          </tr>
        </thead>
        <tbody>` + ordered.map((s, idx) => {
          const isW = s.id === state.winnerId;
          const rankText = isW ? "🏆 1st" : `${idx + 1}th`;
          const statusText = isW ? "Winner" : (s.active ? "Surviving" : "Eliminated");
          let cardsDisplay = "";
          if (isW) {
            if (s.id === state.mySeatId && state.myHand && state.myHand.length) {
              cardsDisplay = state.myHand.map((c) => chipHtml(c, s.name)).join(" ");
            } else {
              cardsDisplay = `${s.faceDownCount || 1} hidden card${(s.faceDownCount || 1) === 1 ? "" : "s"}`;
            }
          } else {
            cardsDisplay = (s.faceUp || []).map((c) => chipHtml(c, s.name)).join(" ") || "—";
          }
          return `
            <tr class="${isW ? "winner-row" : "eliminated-row"}">
              <td class="rank-badge">${rankText}</td>
              <td>${renderName(s)}</td>
              <td>${statusText}</td>
              <td>${cardsDisplay}</td>
              <td style="font-family:var(--font-mono);color:var(--gold-bright)">🪙 ${s.coins}</td>
            </tr>`;
        }).join("") + `</tbody>
      </table>
      <button type="button" class="btn btn-primary" id="go-lobby" style="width:100%;height:44px;font-size:16px">
        🏠 Return to Lobby
      </button>
    </div></div>`;
  root.querySelector("#go-lobby").addEventListener("click", () => {
    closeOverlays();
    ui.gameOverShown = false;
    const done = document.getElementById("go-lobby-done"); void done;
    window.dispatchEvent(new CustomEvent("coup:back-to-lobby")); // main.js clears the session + routes
  });
  root.querySelector("#go-lobby").focus();
}

export function showAuthError(state, err) {
  closeAllModals();
  const msg = (err && err.envelope && err.envelope.message) ? err.envelope.message : "Check your token.";
  const root = $overlayRoot();
  root.innerHTML = `<div class="overlay-root">
    <div class="overlay-backdrop"></div>
    <div class="go-panel" role="dialog" aria-modal="true" aria-labelledby="auth-title">
      <span class="modal-kicker">ACCESS DENIED</span>
      <h2 class="go-title" id="auth-title" style="font-size:1.6rem">Your key no longer opens this table</h2>
      <div class="go-sub">${esc(msg)}</div>
      <button type="button" class="btn btn-primary" id="auth-rejoin" style="width:100%">Rejoin</button>
    </div></div>`;
  root.querySelector("#auth-rejoin").addEventListener("click", () => {
    closeOverlays();
    window.dispatchEvent(new CustomEvent("coup:rejoin", { detail: { gameId: state.gameId } }));
  });
}

export function showGameGone() {
  closeAllModals();
  const root = $overlayRoot();
  root.innerHTML = `<div class="overlay-root">
    <div class="overlay-backdrop"></div>
    <div class="go-panel" role="dialog" aria-modal="true" aria-labelledby="gone-title">
      <span class="modal-kicker">NOT FOUND</span>
      <h2 class="go-title" id="gone-title" style="font-size:1.6rem">That game is gone.</h2>
      <div class="go-sub">The server keeps games in memory — a restart drops them.</div>
      <button type="button" class="btn btn-primary" id="gone-lobby" style="width:100%">Back to the lobby</button>
    </div></div>`;
  root.querySelector("#gone-lobby").addEventListener("click", () => {
    closeOverlays();
    window.dispatchEvent(new CustomEvent("coup:back-to-lobby"));
  });
}

// §6.3 one-time token notice — over S3's first paint after creation.
export function showTokenNotice(state) {
  if (ui.tokenShown) return;
  ui.tokenShown = true;
  const me = state.seats.find((s) => s.id === state.mySeatId);
  const h = openShell({ wide: false, labelledBy: "tok-title" }); // Esc does nothing (§6.3)
  h.panel.innerHTML = `
    <div class="m-head">
      <span class="modal-kicker">YOUR KEYS</span>
      <h2 class="modal-title" id="tok-title">Keep these to yourself.</h2>
    </div>
    <div class="m-body">
      <div class="warn-once">shown once — there is no second time</div>
      <div class="token-block"><code id="tok-text"></code>
        <button type="button" class="btn btn-outline" id="tok-copy">Copy</button></div>
      <p class="modal-sub">Closing this tab loses nothing — rejoin any time with your game id and token.</p>
    </div>
    <div class="m-foot"><button type="button" class="btn btn-primary" id="tok-deal" style="flex:1">Deal me in</button></div>`;
  ui.tokenModal = h; // decision surfaces yield to the notice until it is closed (§6.3)
  h.panel.querySelector("#tok-text").textContent = state.token || "";
  h.panel.querySelector("#tok-copy").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(state.token || ""); infoToast("token copied"); }
    catch { infoToast("copy failed — select the text manually"); }
  });
  h.panel.querySelector("#tok-deal").addEventListener("click", () => {
    ui.tokenModal = null;
    h.close();
    if (refresh) refresh(); // re-run surface detection now that the notice is gone
  });
  h.focusInitial("#tok-deal"); // §15 initial focus
}

// ===========================================================================
// §7.4 per-code recovery + resnapshot
// ===========================================================================

async function resnapshotNow(state) {
  if (resnapshotFn) { try { await resnapshotFn(); } catch { /* next poll recovers */ } }
}

/**
 * Shared rejection handler for every POST surface (§7.4): verbatim error
 * toast + per-code recovery. `staleNotice(deciderName)` is the surface's own
 * moment-has-passed hook (§7.5 layer 2).
 */
export async function handleDecisionRejection(state, e, { staleNotice = null } = {}) {
  rejectToast(e.code, e.message); // verbatim — always
  const code = e.code;
  switch (code) {
    case "already_resolved": {
      const decider = state.lastWindowClose && state.lastWindowClose.deciderSeatId
        ? seatById(state, state.lastWindowClose.deciderSeatId)?.name : null;
      if (staleNotice) staleNotice(decider || "?…already been resolved");
      await resnapshotNow(state); // a window may have closed and another opened in the gap
      break;
    }
    case "not_your_turn":
    case "game_over":
    case "not_eligible":
      await resnapshotNow(state);
      break;
    case "vote_closed":
    case "not_a_voter":
      await resnapshotNow(state); // the _resolved event is already on / arriving via the stream
      break;
    default:
      break; // other codes: toast only (invalid_payload, target_not_active, self_target…)
  }
  refresh();
}

// ===========================================================================
// Surface manager — called after every render tick and batch.
// ===========================================================================

export function ensureSurfaces(state) {
  if (state.status !== "in_progress") { showGameOverIfFinished(state); return; }

  // §6.3: the one-time token notice outranks every decision surface — it is the
  // ONLY chance to keep the seat's token, and openShell() would otherwise close
  // it when a window modal lands on the same tick. Surfaces re-run when the
  // notice's "Deal me in" closes it (refresh).
  if (ui.tokenModal && ui.tokenModal.open) return;

  // §8 window response modal (+ reopen pill). A held window (✕/Esc) does NOT
  // auto-reopen — the docked pill is the explicit affordance (§8.1).
  const w = state.window;
  const canRespond = !!(w && Array.isArray(w.eligible) && w.eligible.includes(state.mySeatId)
    && (w.kind !== "challenge" || w.actorId !== state.mySeatId)
    && (w.kind !== "counterclaim_challenge" || w.blockerId !== state.mySeatId));

  if (canRespond) {
    if ((!ui.win || !ui.win.open) && ui.heldWindowId !== w.id) openWindowModal(state);
    else if (ui.win && ui.win.open) updateWindowModal(state);
  } else if (ui.win && ui.win.open) {
    // Window gone or viewer is actor/ineligible: close response modal immediately so viewer never votes on own action!
    closeWindowModal();
  }
  syncReopenPill(state);

  // §9 single-seat modals (flip choice / exchange finish).
  const d = state.decision;
  if (d && d.seatId === state.mySeatId && !d.settled) openDecisionModal(state);
  if (d && d.seatId === state.mySeatId && d.settled) settleDecisionModal(state, d);

  // §10 ballot modal. First sight opens it; after an Esc-close the record's
  // lastCastSeen gates reopens to genuinely NEW casts (§10.1).
  const v = state.vote;
  if (v && !v.result && v.eligibleVoters.includes(state.mySeatId)) {
    const known = !!(ui.ballot && ui.ballot.voteId === v.id);
    const newCast = castCount(v) > (known ? ui.ballot.lastCastSeen : -1);
    if (!known || newCast) openBallot(state, newCast);
    else if (isBallotModalOpen()) updateBallot(state);
  } else if (v && v.result && isBallotModalOpen()) {
    updateBallot(state); // arms the 4s auto-close in its resolved state
  }
}

function showGameOverIfFinished(state) { if (state.status !== "in_progress") showGameOver(state); }

// The turn console's POST path (§5.4 → §7.4). Called from render.js wiring.
export async function submitConsoleAction(state, body) {
  try {
    await apiPost(state, `/games/${state.gameId}/decisions`, body);
    // 202 = queued: lock the console; the outcome is entirely event-driven (§7.4).
    state.consoleLocked = true;
    consoleUi.pick = null;
    refresh();
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    await handleDecisionRejection(state, e); // console stays interactive unless the code says otherwise
  }
}

/** Countdown ticker driver — updates visible deadline lines without re-render. */
export function startTickers(stateRef, onTick) {
  const t = setInterval(() => {
    const s = stateRef();
    if (!s || document.hidden) return;
    try {
      if (ui.win && ui.win.open) updateWindowModal(s);
      if (ui.dec && ui.dec.open) decisionCountdown(s, s.decision || { deadlineAt: null });
      if (onTick) onTick();
    } catch (e) {
      // A tick must never kill the interval — log and keep counting down.
      console.error("coup: countdown ticker tick failed", e);
    }
  }, 250);
  return () => clearInterval(t);
}

// ===========================================================================
// decide.js — every decision surface of ui-spec §8–§10 plus §6.3/§13 glue:
//   window response modal (data-driven from your_allowed_responses), flip
//   choice, exchange finish, forfeit initiation + ballot, game-over overlay,
//   auth-error / game-gone overlays, the one-time token notice, toasts.
// Owns all POST traffic (§7.4) and the stale-response UX (§7.5).
// ===========================================================================

import { cap, esc, seatById } from "./state.js";
import { apiGet, apiPost, ApiError, kickerForCode } from "./poll.js";
import { chipHtml, renderName, charColor, consoleActBody, consoleUi } from "./render.js";

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
  pass:                    { label: "Pass",                 cls: "btn-outline", post: () => ({ response: { type: "pass" } }) },
  challenge:               { label: "Challenge",            cls: "btn-danger",  post: () => ({ response: { type: "challenge" } }) },
  counterclaim_contessa:   { label: "Block — I'm Contessa", cls: "btn-primary", post: () => ({ response: { type: "counterclaim", character: "contessa" } }) },
  counterclaim_captain:    { label: "Block — I'm Captain",  cls: "btn-primary", post: () => ({ response: { type: "counterclaim", character: "captain" } }) },
  counterclaim_ambassador: { label: "Block — I'm Ambassador", cls: "btn-primary", post: () => ({ response: { type: "counterclaim", character: "ambassador" } }) },
  counterclaim_duke:       { label: "Block — I'm Duke",     cls: "btn-primary", post: () => ({ response: { type: "counterclaim", character: "duke" } }) },
};

// §8.1 button order left→right: blocks (listed order), Challenge, Pass.
const RESP_ORDER = ["counterclaim_contessa", "counterclaim_captain", "counterclaim_ambassador", "counterclaim_duke", "challenge", "pass"];

function openWindowModal(state) {
  const w = state.window;
  if (!w || !w.eligible.includes(state.mySeatId)) return;
  if (ui.win && ui.win.windowId === w.id && ui.win.open) { updateWindowModal(state); return; }

  const isTarget = !!w.targetSeatId && w.targetSeatId === state.mySeatId;
  const titleText = w.triggerText || windowTitleFallback(state, w);
  const h = openShell({
    wide: true,
    closable: true,
    labelledBy: "win-title",
    onEsc: () => holdWindowModal(state), // Esc = "hold" — close without responding (§8.1)
    initialFocusSel: "[data-resp='pass']", // the safe default gets first focus (§15)
  });
  h.panel.innerHTML = `
    <div class="m-head">
      <span class="modal-kicker">THE WINDOW</span>
      <button type="button" class="m-close" aria-label="Hold — close without responding">×</button>
      <h2 class="modal-title" id="win-title"></h2>
      ${isTarget ? `<p class="modal-sub">You are the target.</p>` : ""}
    </div>
    <div class="m-body"><div class="chips" id="win-chips"></div></div>
    <div class="m-set" id="win-set"></div>
    <div class="m-foot" id="win-foot"></div>`;
  const titleEl = h.panel.querySelector("#win-title");
  if (titleEl) titleEl.textContent = titleText; // verbatim (U9) — textContent: the copy is the server's, no markup to decode
  else console.error("coup: window modal lost its title node", { titleText });
  h.panel.querySelector(".m-close").addEventListener("click", () => holdWindowModal(state));

  ui.win = { h, windowId: w.id, mode: w.iResponded ? "locked" : "deciding", respondedAt: Date.now(), timer: null,
    get open() { return this.h.open; } }; // .open lives on the shell handle
  updateWindowModal(state);
  h.focusInitial("[data-resp='pass']");
}

function windowTitleFallback(state, w) {
  const a = w.triggerActionSeq ? state.actionsBySeq[w.triggerActionSeq] : null;
  if (a && a.actorId) return `${seatById(state, a.actorId)?.name || "?"} declares ${cap(a.type || "an action")}`;
  return "A window is open";
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
      if (mine && w.iResponded) { word = `<span>${esc(w.myResponseLabel || "answered")}</span>`; }
      else if (val === "pass") { cls = "passed"; word = "passed"; }
      else if (val === "challenge") { cls = "challenged"; word = "challenged!"; }
      else if (val && val.startsWith("counterclaim:")) { cls = "blocked"; word = `blocks as ${cap(val.slice(13))}`; }
      return `<span class="pchip ${cls}${mine ? " mine" : ""}">${renderName(seat)}<span class="pstate" aria-label="${aria}">${word}</span></span>`;
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
    const order = RESP_ORDER.filter((k) => w.myAllowed.includes(k));
    // Unknown future grammar values render raw and fail closed to a toast (§8.1).
    for (const k of w.myAllowed) if (!order.includes(k)) order.push(k);
    for (const k of order) {
      const def = RESP_BUTTONS[k];
      const btn = document.createElement("button");
      btn.type = "button";
      btn.dataset.resp = k;
      btn.className = "btn " + (def ? def.cls : "btn-outline"); // unknown → plain outline
      btn.textContent = def ? def.label : esc(k);
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
  const should = !!(state.window && state.window.eligible.includes(state.mySeatId)
    && !state.window.iResponded && (!ui.win || !ui.win.open) && state.status === "in_progress");
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
  const h = baseDecisionShell(state, "Which card falls?", "SINGLE CHOICE");
  const cards = state.myHand.slice(0, 2); // option index i ↔ your_face_down_cards[i] (§5.4/U5)
  h.panel.querySelector("#dec-body").innerHTML = `
    <div class="dc-cards">
      ${cards.map((c, i) => `<div class="dcard"><span class="stripe" style="background:${charColor(c)}"></span>
        <span class="cname">${cap(c)}</span><span class="pos">${i + 1}</span></div>`).join("")}
    </div>
    <div class="dc-labels">
      ${cards.map((_, i) => `<span>Card ${i + 1} — face_down_${i}</span>`).join("")}
    </div>`;

  const foot = h.panel.querySelector("#dec-foot");
  cards.forEach((_, i) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn btn-outline";
    btn.textContent = `Flip card ${i + 1}`;
    btn.addEventListener("click", () => submitFlipChoice(state, d, `face_down_${i}`, i));
    foot.appendChild(btn);
  });

  ui.dec = { h, id: d.id, kind: "flip_choice", mode: "deciding", timer: null,
    get open() { return this.h.open; } }; // .open lives on the shell handle
  decisionCountdown(state, d);
  h.focusInitial(".m-foot button");
}

async function submitFlipChoice(state, d, option, index) {
  if (ui.dec && ui.dec.mode !== "deciding") return;
  const foot = ui.dec.h.panel.querySelector("#dec-foot");
  if (foot) foot.querySelectorAll("button").forEach((b) => (b.disabled = true));
  try {
    await apiPost(state, `/games/${state.gameId}/decisions`, { kind: "choose", decision_id: d.id, choice: { flip_choice: option } });
    d.submitted = true; // settles S3 vs S4 copy when the card_flipped event lands (§9.2)
    d.chosenIndex = index; // state.js#applyMyFlip uses this to remove from myHand
    if (ui.dec) { ui.dec.mode = "locked"; setDecisionStatus(ui.dec.h, "Chosen — waiting for the flip."); }
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    rejectToast(e.code, e.message); // incl. 409 "that decision was already made" (§9.2: no state change)
    if (foot) foot.querySelectorAll("button").forEach((b) => (b.disabled = false));
  }
}

// ---- exchange finish (§9.3) --------------------------------------------------
function openExchangeModal(state, d) {
  const h = baseDecisionShell(state, "Your Exchange held — choose what to keep.", "SINGLE CHOICE");
  const drawn = d.drawn || [];
  const hand = state.myHand;
  if (drawn.length === 0) {
    // Bootstrap without a replayed reveal — the private event is coming; wait.
    setDecisionStatus(h, "The draw is on its way…");
    ui.dec = { h, id: d.id, kind: "exchange_finish", mode: "waiting", timer: null,
    get open() { return this.h.open; } }; // .open lives on the shell handle
    return;
  }

  const bodyEl = h.panel.querySelector("#dec-body");
  const orig = bodyEl.innerHTML; // drop the hint into foot area? keep as-is; add rows
  bodyEl.insertAdjacentHTML("afterbegin", `
    <div class="dc-rowcap">YOU DREW (only you can see these)</div>
    <div class="dc-cards">
      ${drawn.map((c, i) => `<div class="dcard" id="dc-drawn-${i}"><span class="stripe" style="background:${charColor(c)}"></span>
        <span class="cname">${cap(c)}</span><span class="pos">${i === 0 ? "L" : "R"}</span></div>`).join("")}
    </div>
    <div class="dc-rowcap" style="margin-top:14px">YOUR HAND</div>
    <div class="dc-cards">
      ${hand.map((c, i) => `<div class="dcard"><span class="stripe" style="background:${charColor(c)}"></span>
        <span class="cname">${cap(c)}</span><span class="pos">${i + 1}</span></div>`).join("")}
    </div>
    <div class="dc-rowcap" style="margin-top:14px">KEEP WHAT YOU WANT</div>
    <div id="ex-choice"></div>
    <div id="ex-return" hidden></div>`);
  void orig;

  const choice = { keep: null, handIdx: null }; // keep: null|0|1 (drawn_index), handIdx: returned card index
  const choiceEl = bodyEl.querySelector("#ex-choice");
  const returnEl = bodyEl.querySelector("#ex-return");
  const foot = h.panel.querySelector("#dec-foot");

  function renderChoice() {
    choiceEl.innerHTML = ["Keep neither", "Keep left", "Keep right"].map((label) => {
      const idx = label === "Keep neither" ? null : (label === "Keep left" ? 0 : 1);
      const sel = choice.keep === idx && (idx === null || choice.handIdx != null);
      return `<button type="button" class="btn btn-outline${sel ? "" : ""}" data-keep="${idx === null ? -1 : idx}"
        style="${choice.keep === idx ? "border-color:var(--gold-bright);color:var(--gold-bright)" : ""}">${label}</button>`;
    }).join(" ");
    choiceEl.querySelectorAll("[data-keep]").forEach((b) => {
      b.addEventListener("click", () => {
        const k = parseInt(b.dataset.keep, 10);
        choice.keep = k === -1 ? null : k;
        choice.handIdx = null;
        [document.getElementById("dc-drawn-0"), document.getElementById("dc-drawn-1")].forEach((el) => el && el.classList.remove("hl"));
        if (choice.keep !== null) {
          const cardEl = document.getElementById(`dc-drawn-${choice.keep}`);
          if (cardEl) cardEl.classList.add("hl"); // gold ring on the kept draw (§9.3)
          returnEl.hidden = false;
          returnEl.innerHTML = `<div class="dc-rowcap">Return which hand card?</div><div id="ex-return-btns"></div>`;
          const rb = returnEl.querySelector("#ex-return-btns");
          rb.innerHTML = hand.map((_, i) => `<button type="button" class="btn btn-outline" data-hi="${i}" style="margin-right:8px">Card ${i + 1}</button>`).join("");
          rb.querySelectorAll("[data-hi]").forEach((bb) => bb.addEventListener("click", () => {
            choice.handIdx = parseInt(bb.dataset.hi, 10);
            renderChoice();
          }));
        } else {
          returnEl.hidden = true; // keep-neither skips straight to confirm (§9.3)
        }
        renderChoiceButtons();
      });
    });
  }
  function renderChoiceButtons() {
    foot.innerHTML = "";
    const confirm = document.createElement("button");
    confirm.type = "button";
    confirm.className = "btn btn-primary";
    confirm.textContent = "Seal it";
    const ready = choice.keep === null || (choice.keep !== null && choice.handIdx != null);
    confirm.disabled = !ready;
    confirm.addEventListener("click", () => submitExchange(state, d, choice));
    foot.appendChild(confirm);
  }
  renderChoice();
  renderChoiceButtons();

  ui.dec = { h, id: d.id, kind: "exchange_finish", mode: "deciding", timer: null,
    get open() { return this.h.open; } }; // .open lives on the shell handle
  decisionCountdown(state, d);
  h.focusInitial("#ex-choice button");
}

async function submitExchange(state, d, choice) {
  if (ui.dec && ui.dec.mode !== "deciding") return;
  let bodyChoice;
  if (choice.keep === null) {
    bodyChoice = { exchange_finish: { no_swap: true } }; // U4 — wire shape confirmed against tech spec §11.2(a)
  } else {
    bodyChoice = { exchange_finish: { drawn_index: choice.keep, hand_option: `face_down_${choice.handIdx}` } };
  }
  const foot = ui.dec.h.panel.querySelector("#dec-foot");
  if (foot) foot.querySelectorAll("button").forEach((b) => (b.disabled = true));
  try {
    await apiPost(state, `/games/${state.gameId}/decisions`, { kind: "choose", decision_id: d.id, choice: bodyChoice });
    // §9.3/§7.3: apply MY OWN hand change locally now — I know both sides of the
    // swap; the public exchange_completed must not double-apply it.
    if (choice.keep !== null) {
      const returned = choice.handIdx;
      const other = returned === 0 ? 1 : 0;
      state.myHand = [d.drawn[choice.keep], state.myHand[other]];
    } // keep-neither: hand unchanged
    d.submitted = true;
    if (ui.dec) { ui.dec.mode = "locked"; setDecisionStatus(ui.dec.h, "Sealed — the deck took two back."); }
    refresh();
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    rejectToast(e.code, e.message);
    if (foot) foot.querySelectorAll("button").forEach((b) => (b.disabled = false));
  }
}

// Settlement for both single-seat modals (driven from ensureSurfaces and from
// main.js#applyEvents when a same-batch turn_start clears state.decision first).
export function settleDecisionModal(state, d) {
  const m = ui.dec;
  if (!m || m.id !== d.id || !m.open) return;
  let line, stale;
  if (d.settled === "done") line = "Done.";
  else if (d.settled === "expired") { line = "Time passed — the table chose for you."; stale = true; }
  else if (d.settled) line = `${cap(d.settled)} is face-up now.`; // card name (flip S3)
  else return;
  m.mode = "settled";
  setDecisionStatus(m.h, line, stale);
  m.timer && clearTimeout(m.timer);
  m.timer = setTimeout(() => {
    if (ui.dec === m) { ui.dec = null; if (state.decision === d) state.decision = null; m.h.close(); refresh(); }
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
      <p class="modal-sub">Use it when a seat — human or AI — is stuck and the table can't move on.
      The other human seats vote; nothing happens unless a majority agrees.</p>
    </div>
    <div class="m-body"><div id="feat-list"></div></div>`;
  const listEl = h.panel.querySelector("#feat-list");

  function renderTargets() {
    const targets = state.seats.filter((s) => s.active && s.id !== state.mySeatId); // U18: never self
    listEl.innerHTML = `<div class="feat-list">` + targets.map((t) =>
      `<button type="button" class="feat-row" data-tid="${t.id}">${renderName(t)} <span class="fcoins">${t.coins}</span></button>`).join("") + `</div>`;
    listEl.querySelectorAll(".feat-row").forEach((b) => b.addEventListener("click", () => renderConfirm(b.dataset.tid)));
  }

  function renderConfirm(tid) {
    const t = seatById(state, tid);
    if (!t) return;
    listEl.innerHTML = `
      <div class="confirm-line">Really call ${renderName(t)}? The other humans get asked.</div>
      <div style="display:flex;gap:8px;justify-content:flex-end">
        <button type="button" class="btn btn-outline" id="feat-cancel">Cancel</button>
        <button type="button" class="btn btn-danger" id="feat-confirm">Confirm</button>
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
  const pctY = (y / total) * 100;
  const needFrac = v.majorityNeeded / Math.max(1, v.eligibleVoters.length);

  m.h.panel.querySelector("#vote-body").innerHTML = `
    <div class="tally-line">
      <span class="tally-counts"><span class="y">YES ${y}</span> &nbsp;&nbsp; <span class="n">NO ${n}</span></span>
      <span class="tally-need">majority: ${v.majorityNeeded} of ${v.eligibleVoters.length}</span>
    </div>
    <div class="tally-bar" aria-hidden="true">
      <span class="tally-fill-y" style="width:${pctY}%"></span>
      <span class="tally-fill-n" style="width:${100 - pctY}%"></span>
      <span class="tally-tick" style="left:${(needFrac * 100).toFixed(1)}%"></span>
    </div>
    <div class="voters">` + v.eligibleVoters.map((id) => {
      const seat = seatById(state, id);
      const vote = v.votes[id];
      const chip = vote === "yes" ? `<span class="vchip yes">VOTED YES</span>`
        : vote === "no" ? `<span class="vchip no">VOTED NO</span>`
        : `<span class="vchip pending">…</span>`;
      return `<div class="voter-row">${renderName(seat)}${chip}</div>`;
    }).join("") + `</div>`;

  const foot = m.h.panel.querySelector("#vote-foot");
  if (v.result) {
    const target = seatById(state, v.targetSeatId);
    const line = v.result === "forfeited"
      ? `${renderName(target)} is forfeited. Their cards are never shown.`
      : `The vote fails ${y}–${n}. ${renderName(target)} stays.`; // U12: not_forfeited/voided render the fail line
    foot.innerHTML = `<div class="m-status" style="padding:8px 0 4px">${line}</div>`;
    m.timer && clearTimeout(m.timer);
    m.timer = setTimeout(() => { if (ui.ballot === m) closeBallotResolved(); }, 4000); // U10
  } else if (v.myVote) {
    foot.innerHTML = `<div class="m-status" style="padding:8px 0 4px">You voted ${v.myVote.toUpperCase()}.</div>`;
  } else {
    foot.innerHTML = "";
    const yes = document.createElement("button");
    yes.type = "button"; yes.id = "vote-yes"; yes.className = "btn btn-yes"; yes.textContent = "Vote YES";
    yes.addEventListener("click", () => castBallot(state, true));
    const no = document.createElement("button");
    no.type = "button"; no.className = "btn btn-danger"; no.textContent = "Vote NO";
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
  // Standings: active-first then eliminated (§10.6).
  const ordered = [
    ...state.seats.filter((s) => s.active),
    ...state.seats.filter((s) => !s.active),
  ];

  const root = $overlayRoot();
  root.innerHTML = `<div class="overlay-root">
    <div class="overlay-backdrop"></div>
    <div class="go-panel" role="dialog" aria-modal="true" aria-labelledby="go-title">
      <span class="modal-kicker">THE TABLE CLOSES</span>
      <h2 class="go-title" id="go-title">${iWin ? "GAME OVER — YOU WIN THE TABLE." : "GAME OVER"}</h2>
      <div class="go-winner">${winner ? renderName(winner) : "?"}</div>
      <div class="go-sub">wins the table.</div>
      <div class="standings">` + ordered.map((s) => {
        const isW = s.id === state.winnerId;
        return `<div class="standing-row ${isW ? "win" : ""} ${s.active ? "" : "out"}">
          <span>${renderName(s)}</span><span class="s-coins-in">${s.coins}</span></div>`;
      }).join("") + `</div>
      <button type="button" class="btn btn-primary" id="go-lobby" style="width:100%">Back to lobby</button>
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
  if (w && w.eligible.includes(state.mySeatId)) {
    if ((!ui.win || !ui.win.open) && ui.heldWindowId !== w.id) openWindowModal(state);
    else if (ui.win && ui.win.open) updateWindowModal(state);
  } else if (ui.win && ui.win.open) {
    // Window gone or I'm not eligible: close it out per the §7.5/§8.1 states.
    transitionWindowModalOnClose(state, w ? null : ui.win.windowId);
    if (!state.window) { /* closed-state auto-close is arming */ }
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

// ===========================================================================
// render.js — pure-over-state DOM rendering (ui-spec §4, §5, §12).
//   renderTable / renderTopbar / renderBanners  — table chrome
//   rowForEvent / appendLog                      — the §12 log contract
// No POST traffic here: decision callbacks are passed in by decide.js.
// ===========================================================================

import { cap, esc, clockTime, seatById } from "./state.js";

// --- Character display vocabulary (§3.2 stripes, §4.3 monograms) -----------
const CHAR_COLORS = {
  duke: "var(--char-duke)",
  assassin: "var(--char-assassin)",
  contessa: "var(--char-contessa)",
  captain: "var(--char-captain)",
  ambassador: "var(--char-ambassador)",
};
const CHAR_MONO = { duke: "D", assassin: "A", contessa: "C", captain: "C", ambassador: "A" };

export function charColor(c) { return CHAR_COLORS[c] || "var(--line-strong)"; }
export function charMonogram(c) { return CHAR_MONO[c] || "?"; }

/** §4.2 — every rendered seat name goes through this: name + LLM badge. */
export function renderName(seat, extraClass = "") {
  if (!seat) return esc("?");
  const badge = seat.type === "llm" ? `<span class="llm-badge">AI</span>` : "";
  return `<span class="${extraClass}">${esc(seat.name)}${badge}</span>`;
}

/** A mini face-up card chip (icon column, eliminated rows) per §4.3/§4.6. */
export function chipHtml(card, ownerName = "") {
  if (card === "forfeited") {
    return `<span class="chip unknown" title="${esc(ownerName)}'s forfeited card — never revealed" aria-label="forfeited card">?</span>`;
  }
  const label = `${ownerName ? ownerName + "'s face-up card: " : ""}${cap(card)}`;
  return `<span class="chip" style="--stripe:${charColor(card)}" title="${esc(label)}" aria-label="${esc(label)}">${charMonogram(card)}</span>`;
}

// ---------------------------------------------------------------------------
// §5.3 ring geometry — seats on a regular N-gon, first seat at 12 o'clock.
// ---------------------------------------------------------------------------
export function slotAnchors(n) {
  const out = [];
  if (n <= 1) return [{ x: 50, y: 50 }];
  if (n === 2) return [{ x: 18, y: 50 }, { x: 82, y: 50 }];
  const R = n >= 6 ? 40 : 41; // % of the felt box
  for (let i = 0; i < n; i++) {
    const theta = (i * 360) / n;
    const rad = (theta * Math.PI) / 180;
    out.push({ x: 50 + R * Math.sin(rad), y: 50 - R * Math.cos(rad) });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Table: seats ring, center board, turn pointer, decision pulse (§4.3/§4.5)
// ---------------------------------------------------------------------------

export function renderTable(state, feltEl, seatsEl, centerEl) {
  const n = state.seats.length;
  const anchors = slotAnchors(n);

  seatsEl.innerHTML = state.seats.map((st, i) => seatTileHtml(state, st, anchors[i] || { x: 50, y: 50 })).join("");

  // Center board (§4.5): phase-driven, driven by the same state as the pointer.
  const deckLine = `court deck · ${state.deckSize != null ? state.deckSize : 0} cards`;
  let main = "", sub = "", gold = false;
  switch (state.phase) {
    case "turn_action": {
      const t = seatById(state, state.turnSeatId);
      if (t && t.id === state.mySeatId) {
        main = "YOUR TURN";
        gold = true;
        sub = "choose an action below";
      } else {
        const isAI = t && t.type === "llm";
        main = t ? `${esc(t.name.toUpperCase())}'S TURN` : "PLAYER'S TURN";
        gold = false;
        sub = isAI ? "AI is deliberating..." : "awaiting turn declaration";
      }
      break;
    }
    case "window_open": {
      gold = true;
      if (state.window && state.window.actorId === state.mySeatId) {
        const act = state.window.triggerActionSeq ? state.actionsBySeq[state.window.triggerActionSeq] : null;
        main = `YOUR ${act ? cap(act.type).toUpperCase() : "ACTION"} IN PROGRESS`;
        sub = "Waiting for other players to respond...";
      } else if (state.window && state.window.kind === "counterclaim_challenge") {
        const bSeat = seatById(state, state.window.blockerId);
        const bName = bSeat ? bSeat.name.toUpperCase() : "OPPONENT";
        const charName = state.window.blockChar ? cap(state.window.blockChar).toUpperCase() : "CHARACTER";
        main = `${bName} BLOCKS AS ${charName}`;
        sub = "A challenge window is open on this block";
      } else {
        const aSeat = state.window ? seatById(state, state.window.actorId) : null;
        const aName = aSeat ? aSeat.name.toUpperCase() : "OPPONENT";
        const act = state.window && state.window.triggerActionSeq ? state.actionsBySeq[state.window.triggerActionSeq] : null;
        main = `${aName}'S CLAIM`;
        sub = state.window && state.window.triggerText ? state.window.triggerText : (act ? `${aName} declares ${cap(act.type)}` : "A claim hangs in the air");
      }
      break;
    }
    case "single_seat_decision": {
      const d = state.decision;
      if (d && d.seatId === state.mySeatId) {
        main = d.kind === "exchange_finish" ? "EXCHANGE CARDS" : "CHOOSE A CARD";
        gold = true;
        sub = "your decision is required";
      } else {
        const st = seatById(state, d ? d.seatId : null);
        main = `WAITING ON ${esc(st ? st.name.toUpperCase() : "?")}`;
        sub = d && d.kind === "exchange_finish" ? "exchanging cards" : "choosing a card to reveal";
      }
      break;
    }
    default: // finished
      {
        const w = seatById(state, state.winnerId);
        main = `${w ? esc(w.name.toUpperCase()) : "?"} WINS`;
        gold = true;
        sub = "game over";
      }
  }

  const mainEl = centerEl.querySelector(".c-main");
  if (mainEl) {
    mainEl.textContent = main;
    mainEl.classList.toggle("gold", gold);
  }
  const subEl = centerEl.querySelector(".c-sub");
  if (subEl) {
    subEl.textContent = sub;
  }
  const deckEl = centerEl.querySelector(".c-deck");
  if (deckEl) {
    deckEl.textContent = deckLine;
  }
}

function seatTileHtml(state, st, anchor) {
  const isMe = st.id === state.mySeatId;
  const isAI = st.type === "llm";
  const acting = state.phase === "turn_action" && state.turnSeatId === st.id && st.active;
  // Decision required: eligible in open window without responded, or single-seat decision addressed to you
  const deciding = st.active && isMe && (
    (state.window && state.window.eligible && state.window.eligible.includes(st.id) && !state.window.iResponded) ||
    (state.phase === "single_seat_decision" && state.decision && state.decision.seatId === st.id)
  );

  // Check if this seat is currently targeting the human player
  const windowTargetingMe = !!(state.window && state.window.targetSeatId === state.mySeatId && state.window.actorId === st.id);
  const recentActionTargetingMe = !!(state.lastAction && state.lastAction.actorId === st.id && state.lastAction.targetId === state.mySeatId && (Date.now() - state.lastAction.ts < 5500));
  const isTargetingMe = !isMe && (windowTargetingMe || recentActionTargetingMe);

  const cls = ["seat"];
  if (acting) {
    cls.push("acting");
    if (isAI) cls.push("acting-ai");
  }
  if (deciding) cls.push("decision");
  if (!st.active) cls.push("eliminated");
  if (isTargetingMe) cls.push("targeting-me-border");

  // Name row: name + badge (§4.2), right slot = YOU tag / OUT tag (§4.3).
  let right = "";
  if (!st.active) right = `<span class="out-tag">OUT</span>`;
  else if (isMe) right = `<span class="you-tag">YOU</span>`;
  else if (isTargetingMe) right = `<span class="target-badge-pill">TARGETING YOU</span>`;

  // Coins row — dimmed + inert title on eliminated seats (§4.3).
  const coinsTitle = !st.active ? ` title="coins are inert — this seat is out"` : ` title="${st.coins} coins"`;
  const coinsRow = `<div class="s-coins"${coinsTitle}><span class="coin"></span><b>${st.coins}</b></div>`;

  // Card row: face-down backs (2 while active) + face-up chips (§4.3).
  let cards = "";
  if (!st.active) {
    for (const c of st.faceUp) cards += chipHtml(c, st.name);
    for (let i = 0; i < (st.forfeitedCount || 0); i++) cards += chipHtml("forfeited", st.name);
    if (st.faceUp.length === 0 && (!st.forfeitedCount || st.forfeitedCount === 0)) {
      cards += chipHtml("forfeited", st.name);
      cards += chipHtml("forfeited", st.name);
    }
  } else {
    const faceDown = Math.max(0, 2 - st.faceUp.length);
    for (let i = 0; i < faceDown; i++) cards += `<span class="back" title="face-down card"></span>`;
    for (const c of st.faceUp) cards += chipHtml(c, st.name);
  }

  // Floating Action Speech Bubble for AI/opponent feedback
  let speechBubble = "";
  const recentAct = state.lastActionBySeat && state.lastActionBySeat[st.id];
  const isWindowActor = state.window && (state.window.actorId === st.id || state.window.blockerId === st.id);
  const isRecent = recentAct && (Date.now() - recentAct.ts < 5500 || (state.phase === "window_open" && isWindowActor));

  if (recentAct && isRecent && st.active) {
    const tSeat = recentAct.targetId ? seatById(state, recentAct.targetId) : null;
    const targetName = tSeat ? (tSeat.id === state.mySeatId ? "YOU" : tSeat.name) : "";
    let icon = "💬";
    let bText = "";
    let charType = recentAct.character || recentAct.type;

    switch (recentAct.type) {
      case "income":
        icon = "🪙";
        bText = "Taking Income (+1)";
        break;
      case "foreign_aid":
        icon = "💰";
        bText = "Foreign Aid (+2)";
        break;
      case "tax":
        icon = "👑";
        bText = "Tax (+3) · Claim Duke";
        break;
      case "steal":
        icon = "🗡️";
        bText = targetName ? `Stealing 2 from ${targetName}` : "Stealing 2 coins";
        break;
      case "assassinate":
        icon = "☠️";
        bText = targetName ? `Assassinating ${targetName}!` : "Assassinating!";
        break;
      case "exchange":
        icon = "🔄";
        bText = "Exchange · Claim Ambassador";
        break;
      case "coup":
        icon = "💥";
        bText = targetName ? `Coup on ${targetName}!` : "Coup!";
        break;
      case "counterclaim":
        icon = "🛡️";
        bText = `Blocks as ${cap(recentAct.character || "character")}`;
        break;
      case "challenge":
        icon = "⚡";
        bText = recentAct.text || "Challenges claim!";
        break;
      default:
        bText = recentAct.text || "Taking action";
    }

    if (recentAct.fizzled) {
      icon = "✕";
      bText = recentAct.fizzled === "counterclaim_success" ? "Blocked!" : "Action fizzled";
    }

    const bubblePos = anchor.y < 28 ? "bubble-down" : "bubble-up";
    const targetingMeBubble = (recentAct.targetId === state.mySeatId && !isMe) ? " bubble-warn" : "";
    speechBubble = `<div class="seat-speech-bubble ${bubblePos}${targetingMeBubble}" data-char="${esc(charType || "")}" role="status">
      <span class="bubble-icon">${icon}</span>
      <span class="bubble-msg">${esc(bText)}</span>
    </div>`;
  }

  // AI Thinking indicator if currently acting
  let aiIndicator = "";
  if (st.active && isAI && acting) {
    aiIndicator = `<div class="ai-turn-pulse"><span class="ai-pulse-dot"></span> Deliberating…</div>`;
  }

  const sx = `${anchor.x.toFixed(2)}%`;
  const sy = `${anchor.y.toFixed(2)}%`;
  return `<div class="${cls.join(" ")}" data-slot="${st.id}" style="--sx:${sx};--sy:${sy}">
    ${speechBubble}
    <div class="s-row"><span class="s-name">${renderName(st)}</span>${right}</div>
    ${aiIndicator}
    ${coinsRow}
    <div class="s-cards">${cards}</div>
  </div>`;
}

// ---------------------------------------------------------------------------
// Hand strip (§5.2/§4.4) — viewer-only, two face-down cards labeled 1/2.
// The numerals are the index into your_face_down_cards (flip-choice §9.2).
// ---------------------------------------------------------------------------
export function renderHand(state, handRowEl, hcardsEl) {
  if (!state.myHand || state.myHand.length === 0 || state.eliminatedMe) {
    handRowEl.hidden = true; // spectator mode unmounts the strip (§11)
    return;
  }
  handRowEl.hidden = false;
  const single = state.myHand.length === 1;
  hcardsEl.innerHTML = state.myHand.map((c, i) => {
    const pos = single ? "" : `<span class="pos">${i + 1}</span>`; // suppressed with one card left (§5.2)
    return `<div class="hcard" title="Your card ${i + 1} (face_down_${i})">
      <span class="stripe" style="background:${charColor(c)}"></span>
      <span class="cname">${cap(c)}</span>${pos}</div>`;
  }).join("");
}

// ---------------------------------------------------------------------------
// §5.4 turn-action console (docked strip). Rendered from state; the target
// picker is a transient module-local selection handed to decide.js on click.
// ---------------------------------------------------------------------------

export const CONSOLE_ACTIONS = [
  { type: "income",        shortcut: 1, label: "Income",       gain: "+1", cost: 0, cap: "+1 coin",                        target: false },
  { type: "foreign_aid",   shortcut: 2, label: "Foreign Aid",  gain: "+2", cost: 0, cap: "+2 coins",                       target: false },
  { type: "coup",          shortcut: 3, label: "Coup",         gain: null, cost: 7, cap: "−7 now · target loses a card",   target: true },
  { type: "tax",           shortcut: 4, label: "Tax",          gain: "+3", cost: 0, cap: "claim Duke · +3 if it holds",    target: false, claim: "duke" },
  { type: "steal",         shortcut: 5, label: "Steal",        gain: "+2", cost: 0, cap: "claim Captain or Ambassador",    target: true,  claimPicker: true },
  { type: "assassinate",   shortcut: 6, label: "Assassinate",  gain: null, cost: 3, cap: "−3 now, sunk · claim Assassin",  target: true,  claim: "assassin" },
  { type: "exchange",      shortcut: 7, label: "Exchange",     gain: null, cost: 0, cap: "claim Ambassador",               target: false, claim: "ambassador" },
];

// Character attached to a claim-and-act submission (§5.4).
const ACTION_CLAIMS = { tax: "duke", steal: null, assassinate: "assassin", exchange: "ambassador" };

export const consoleUi = { pick: null }; // transient target-picker selection (module-local)

let consoleKeyHandler = null;

function detachConsoleKeys() {
  if (consoleKeyHandler) {
    window.removeEventListener("keydown", consoleKeyHandler);
    consoleKeyHandler = null;
  }
}

export function renderConsole(state, el, onAction) {
  const isActorWaiting = state.phase === "window_open" && state.window && state.window.actorId === state.mySeatId
    && !state.eliminatedMe && state.status === "in_progress";
  const show = (state.phase === "turn_action" && state.turnSeatId === state.mySeatId && !state.eliminatedMe
    && state.status === "in_progress") || isActorWaiting;

  if (!show) {
    el.hidden = true;
    consoleUi.pick = null;
    detachConsoleKeys();
    return;
  }
  el.hidden = false;

  if (isActorWaiting) {
    detachConsoleKeys();
    const w = state.window;
    const act = w.triggerActionSeq ? state.actionsBySeq[w.triggerActionSeq] : null;
    const actName = act ? cap(act.type) : "Action";
    const respChips = (w.eligible || []).map((id) => {
      const s = seatById(state, id);
      if (!s) return "";
      const val = w.responses[id];
      let icon = "⏳", text = "Deliberating...", cls = "pending";
      if (val === "pass") { icon = "✓"; text = "Passed"; cls = "passed"; }
      else if (val === "challenge") { icon = "⚔️"; text = "Challenged!"; cls = "challenged"; }
      else if (val && val.startsWith("counterclaim:")) { icon = "🛡️"; text = `Blocked as ${cap(val.slice(13))}`; cls = "blocked"; }
      return `<span class="pchip ${cls}"><span class="pchip-icon">${icon}</span> ${esc(s.name)}: <strong>${text}</strong></span>`;
    }).join("");

    el.innerHTML = `<div class="console-kicker" aria-live="polite">YOUR ACTION: ${actName.toUpperCase()} — AWAITING OPPONENTS</div>
      <div class="console-actor-waiting">
        <div class="waiting-header">
          <div class="waiting-spinner"></div>
          <span class="waiting-label">Waiting for opponents to accept or challenge your <strong>${actName}</strong>…</span>
        </div>
        <div class="chips" style="justify-content:center;margin-top:10px">${respChips}</div>
      </div>`;
    return;
  }

  const me = seatById(state, state.mySeatId);
  const myCoins = me ? me.coins : 0;
  const mandatory = !!me && myCoins >= 10; // "≥10" mandatory Coup
  const mandTitle = "You must Coup when you have 10 or more coins";

  const kicker = `<div class="console-kicker" aria-live="polite">YOUR MOVE — TURN ${state.turnId || "?"}` +
    (mandatory ? ` <span class="mandatory">· COUP IS MANDATORY</span>` : "") + `</div>`;

  if (state.consoleLocked) {
    // locked-awaiting (§5.4 state table): the POST got a 202, outcome is on its way.
    el.innerHTML = kicker + `<div class="console-status">Waiting on the table…</div>`;
    detachConsoleKeys();
    return;
  }

  // Clear pick if chosen action is no longer valid
  if (consoleUi.pick) {
    if (mandatory && consoleUi.pick !== "coup") {
      consoleUi.pick = null;
    } else if (consoleUi.pick === "coup" && myCoins < 7) {
      consoleUi.pick = null;
    } else if (consoleUi.pick === "assassinate" && myCoins < 3) {
      consoleUi.pick = null;
    }
  }

  const targets = state.seats.filter((s) => s.active && s.id !== state.mySeatId);

  let btns = "";
  for (const a of CONSOLE_ACTIONS) {
    let disabled = false;
    let title = "";

    if (mandatory) {
      if (a.type !== "coup") {
        disabled = true;
        title = mandTitle;
      }
    } else {
      if (a.type === "coup" && myCoins < 7) {
        disabled = true;
        title = `Coup requires 7 coins (you have ${myCoins})`;
      } else if (a.type === "assassinate" && myCoins < 3) {
        disabled = true;
        title = `Assassinate costs 3 coins (you have ${myCoins})`;
      }
    }

    const titleAttr = title ? ` title="${esc(title)}"` : "";
    const sel = consoleUi.pick === a.type ? " selected" : "";

    let badges = "";
    if (a.cost > 0) {
      badges += `<span class="cbtn-badge cost">${a.cost} coins</span>`;
    }
    if (a.gain) {
      badges += `<span class="cbtn-badge gain">${a.gain}</span>`;
    }
    if (a.claim) {
      badges += `<span class="cbtn-badge claim" style="--char-color:${charColor(a.claim)}">${cap(a.claim)}</span>`;
    } else if (a.claimPicker) {
      badges += `<span class="cbtn-badge claim">Capt / Amb</span>`;
    }

    btns += `<button type="button" class="cbtn${sel}" data-act="${a.type}"${titleAttr}${disabled ? " disabled" : ""}>
      <div class="cbtn-top">
        <span class="cbtn-key">[${a.shortcut}]</span>
        <span class="lab">${a.label}</span>
        <span class="cbtn-badges">${badges}</span>
      </div>
      <span class="cap">${a.cap}</span>
    </button>`;
  }

  let picker = "";
  if (consoleUi.pick && CONSOLE_ACTIONS.find((x) => x.type === consoleUi.pick && x.target)) {
    const kind = consoleUi.pick;
    const actionDef = CONSOLE_ACTIONS.find((x) => x.type === kind);
    const actionLabel = actionDef ? actionDef.label : kind;

    const targetsHtml = targets.length === 0
      ? `<div class="tpick-empty">No eligible targets remaining.</div>`
      : targets.map((t) => {
          const noCoins = t.coins < 1;
          const disabledTarget = kind === "steal" && noCoins;
          const stealDisabledTitle = "Target has no coins to steal";

          if (kind === "steal") {
            return `<div class="tchip steal-chip${disabledTarget ? " disabled" : ""}"${disabledTarget ? ` title="${stealDisabledTitle}"` : ""}>
              <div class="tchip-target">
                <span class="tchip-name">${renderName(t)}</span>
                <span class="tcoins" title="${t.coins} coins"><span class="coin"></span><b>${t.coins}</b></span>
              </div>
              <div class="tclaim" role="group" aria-label="Claim character to steal">
                <button type="button" class="tclaim-btn tclaim-captain" data-act="steal" data-target="${t.id}" data-char="captain"${disabledTarget ? ` disabled title="${stealDisabledTitle}"` : ' title="Claim Captain to steal 2 coins"'}>
                  <span class="chip-mini" style="--stripe:var(--char-captain)">C</span>
                  <span>as Captain</span>
                </button>
                <button type="button" class="tclaim-btn tclaim-ambassador" data-act="steal" data-target="${t.id}" data-char="ambassador"${disabledTarget ? ` disabled title="${stealDisabledTitle}"` : ' title="Claim Ambassador to steal 2 coins"'}>
                  <span class="chip-mini" style="--stripe:var(--char-ambassador)">A</span>
                  <span>as Ambassador</span>
                </button>
              </div>
            </div>`;
          }

          // coup / assassinate: one chip per target, click submits
          return `<button type="button" class="tchip action-chip" data-act="${kind}" data-target="${t.id}" title="Target ${esc(t.name)}">
            <span class="tchip-name">${renderName(t)}</span>
            <span class="tcoins" title="${t.coins} coins"><span class="coin"></span><b>${t.coins}</b></span>
          </button>`;
        }).join("");

    picker = `<div class="tpick-wrap">
      <div class="tpick-kicker">CHOOSE TARGET FOR ${actionLabel.toUpperCase()}:</div>
      <div class="tpick" role="group" aria-label="Choose a target">${targetsHtml}</div>
    </div>`;
  }

  el.innerHTML = kicker + `<div class="console-btns">${btns}</div>${picker}`;

  // Keyboard shortcut listener (keys 1-7)
  detachConsoleKeys();
  consoleKeyHandler = (e) => {
    const t = e.target;
    if (t) {
      const tag = t.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || t.isContentEditable) {
        return;
      }
    }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    if (document.querySelector(".modal-panel, .go-panel")) return;

    if (e.key >= "1" && e.key <= "7") {
      const num = parseInt(e.key, 10);
      const actionDef = CONSOLE_ACTIONS[num - 1];
      if (actionDef) {
        const btn = el.querySelector(`.cbtn[data-act="${actionDef.type}"]`);
        if (btn && !btn.disabled) {
          e.preventDefault();
          btn.click();
        }
      }
    } else if (e.key === "Escape" && consoleUi.pick) {
      e.preventDefault();
      consoleUi.pick = null;
      renderConsole(state, el, onAction);
    }
  };
  window.addEventListener("keydown", consoleKeyHandler);

  // Wire clicks (delegated per render — the strip is small and rebuilt often).
  el.querySelectorAll("[data-act]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const act = btn.dataset.act;
      if (!CONSOLE_ACTIONS.find((x) => x.type === act)) return;
      if (btn.dataset.target) {
        // A concrete target (and claim, for steal) was chosen — submit now (§5.4).
        onAction(consoleActBody(act, btn.dataset.target, btn.dataset.char || undefined));
        return;
      }
      const a = CONSOLE_ACTIONS.find((x) => x.type === act);
      if (a.target) {
        consoleUi.pick = consoleUi.pick === act ? null : act; // toggle the inline picker
        renderConsole(state, el, onAction);
      } else {
        onAction(consoleActBody(act));
      }
    });
  });
}

/** Build the POST /decisions body for a console selection (§5.4). */
export function consoleActBody(type, targetId, character) {
  const action = { type };
  if (character) action.character = character;
  else if (ACTION_CLAIMS[type]) action.character = ACTION_CLAIMS[type];
  if (targetId) action.target_id = targetId;
  return { kind: "act", action };
}

// ---------------------------------------------------------------------------
// Topbar (§4.1) + passive banners (§7.3/§10.1)
// ---------------------------------------------------------------------------

export function renderTopbar(state, { gameChipEl, viewerEl, forfeitBtnEl }) {
  const me = seatById(state, state.mySeatId);
  gameChipEl.textContent = state.gameId.slice(0, 8); // "g_0a1b2c" (§4.1)
  gameChipEl.title = "Copy the full game id: " + state.gameId;

  viewerEl.innerHTML = renderName(me, "viewer-inner") + (state.eliminatedMe ? ` <span class="out-tag">OUT</span>` : "");

  const over = state.status !== "in_progress";
  forfeitBtnEl.hidden = over || state.eliminatedMe; // §4.1 item 5
  forfeitBtnEl.disabled = !!(state.vote && !state.vote.result);
  forfeitBtnEl.title = state.vote && !state.vote.result
    ? "a forfeit vote is already in progress"
    : "The other human seats will be asked to vote";
}

export function updateConn(state, { connDot, connLabel, connText }) {
  const live = state.conn === "live";
  connDot.classList.toggle("reconnecting", !live);
  connLabel.hidden = live;
  connText.textContent = live ? "live" : "reconnecting";
}

/** Passive banners: an unanswerable open window, or a ballot you're not in. */
export function renderBanners(state, stripEl, isBallotOpen) {
  const pills = [];
  if (state.window && !state.window.eligible.includes(state.mySeatId)) {
    pills.push({ id: "w:" + state.window.id, html: `A window is open — ${state.window.eligible.length} seats may respond.` });
  }
  if (state.vote && !state.vote.result && !(isBallotOpen && state.vote.eligibleVoters.includes(state.mySeatId))) {
    const target = seatById(state, state.vote.targetSeatId);
    const yes = Object.values(state.vote.votes).filter((v) => v === "yes").length;
    const no = Object.values(state.vote.votes).filter((v) => v === "no").length;
    pills.push({
      id: "v:" + state.vote.id,
      html: `Forfeit vote against ${target ? renderName(target) : "?"} — <b>${yes}–${no}</b>, needs ${state.vote.majorityNeeded}.`,
    });
  }
  stripEl.innerHTML = pills.map((p) => {
    if (dismissedBanners.has(p.id)) return "";
    return `<div class="banner-pill"><span>${p.html}</span>
      <button type="button" class="b-x" data-bid="${esc(p.id)}" aria-label="Dismiss">×</button></div>`;
  }).join("");
  stripEl.querySelectorAll(".b-x").forEach((b) => {
    b.addEventListener("click", () => { dismissedBanners.add(b.dataset.bid); renderBanners(state, stripEl, isBallotOpen); });
  });
}

const dismissedBanners = new Set();
export function resetBannerDismissals() { dismissedBanners.clear(); }

// ---------------------------------------------------------------------------
// §12 event log rendering — one row per event, in seq order. Names via
// renderName; action_declared.text and rejection.message verbatim (U9/§13).
// ---------------------------------------------------------------------------

const nameIn = (s, id) => renderName(seatById(s, id)); // badge-carrying inline name

function coinDeltaBadge(delta) {
  const num = Math.abs(delta);
  if (delta > 0) return `<span class="coin-badge plus">+${num}</span>`;
  if (delta < 0) return `<span class="coin-badge minus">−${num}</span>`;
  return `<span class="coin-badge">0</span>`;
}

export function rowForEvent(ev, s) {
  const p = ev;
  switch (p.type) {
    case "game_created": {
      const names = (p.seats || []).map((x) => renderName({ name: x.name, type: x.type }));
      const first = seatById(s, p.first_turn_seat_id);
      return { kind: "divider", text: `THE TABLE SEATS ${names.join(" · ")} — ${first ? esc(first.name.toUpperCase()) : "?"} OPENS` };
    }

    case "turn_start": {
      const st = seatById(s, p.seat_id);
      const isMe = st && st.id === s.mySeatId;
      return { kind: "divider", text: `TURN ${p.turn_id} — <span class="${isMe ? "you" : ""}">${st ? renderName(st) : "?"}</span>` };
    }

    case "action_declared":
      // The server supplies this string; it is the copy (§12, U9).
      return { icon: "◆", iconCls: "glyph-action", html: `<b>${esc(p.text || "")}</b>` };

    case "window_opened": {
      const names = (p.eligible || []).map((id) => nameIn(s, id)).join(", ");
      let text = `Responses open — ${names} may act.`;
      if (p.kind === "counterclaim_challenge") {
        const blocker = s.window && s.window.blockerId ? nameIn(s, s.window.blockerId) : "?";
        text += ` — ${blocker}'s block is itself challenged.`;
      }
      return { cls: "dim", icon: "⏳", html: text };
    }

    case "response_received": {
      const who = nameIn(s, p.seat_id);
      const r = p.response || {};
      if (r.type === "challenge") {
        return { cls: "red bold", icon: "✕", iconCls: "glyph-challenge", html: `${who} <b class="word-challenge">challenges!</b>` };
      }
      if (r.type === "counterclaim") {
        const charName = cap(r.character || "?");
        const chip = r.character ? chipHtml(r.character) : "";
        return { cls: "green bold", icon: "🛡", iconCls: "glyph-block", html: `${who} <b class="word-block">blocks</b> as <b>${charName}</b> ${chip}` };
      }
      return { cls: "dim", icon: "—", html: `${who} passed` };
    }

    case "window_closed": {
      const who = p.decider_seat_id ? nameIn(s, p.decider_seat_id) : null;
      let text = "";
      switch (p.reason) {
        case "first_response": text = `The window closes — ${who || "?"} moved first.`; break;
        case "all_responded": text = "The window closes — every seat has answered."; break;
        case "timeout": text = "The window closes — the window timed out; silent seats pass."; break;
        case "auto_no_responders": text = "The window closes — no one is left to answer."; break;
        default: text = `The window closes (${p.reason}).`;
      }
      return { cls: "dim bold", icon: "✓", html: text };
    }

    case "claim_succeeded": {
      const who = nameIn(s, p.seat_id);
      const ch = nameIn(s, p.challenger_seat_id);
      const chip = p.character ? chipHtml(p.character) : "";
      return { cls: "gold bold", icon: "✓", iconChip: p.character, html: `<b>${who}</b> was holding the <b>${cap(p.character)}</b> ${chip}! ${ch}&rsquo;s challenge fails.` };
    }

    case "claim_failed": {
      const who = nameIn(s, p.seat_id);
      return { cls: "red bold", icon: "✕", iconCls: "glyph-danger", html: `✕ <b>${who}</b> did not hold the <b>${cap(p.claimed_character)}</b>. The play is <span class="void-word">void</span>; ${who} loses a card.` };
    }

    case "card_flipped": {
      const who = nameIn(s, p.seat_id);
      const card = cap(p.card);
      let html;
      switch (p.reason) {
        case "penalty_failed_claim": html = `${who}&rsquo;s <b>${card}</b> flips face-up — busted.`; break;
        case "penalty_failed_challenge": html = `${who}&rsquo;s <b>${card}</b> flips face-up — the challenge failed.`; break;
        case "coup": html = `A coup lands on ${who} — <b>${card}</b> flips face-up.`; break;
        case "assassinate": html = `The assassin&rsquo;s strike lands — ${who}&rsquo;s <b>${card}</b> flips face-up.`; break;
        case "successful_reveal": {
          const seat = seatById(s, p.seat_id);
          html = `${who} was called out — <b>${card}</b> is revealed in public, then straight back to the Court Deck (${seat ? esc(seat.name) : "?"} draws a replacement; this card never joins their face-up list).`;
          break;
        }
        default: html = `${who}&rsquo;s <b>${card}</b> flips face-up.`;
      }
      return { cls: p.reason === "successful_reveal" ? "gold" : "red", iconChip: p.card, html };
    }

    case "coins_changed": {
      const who = nameIn(s, p.seat_id);
      const t = p.total;
      const d = Math.abs(p.delta || 0);
      switch (p.reason) {
        case "income":
          return { cls: "gold", icon: "🪙", iconCls: "coins", html: `${who} takes Income ${coinDeltaBadge(1)} — now <b>${t}</b>` };
        case "foreign_aid":
          return { cls: "gold", icon: "🪙", iconCls: "coins", html: `${who} takes Foreign Aid ${coinDeltaBadge(2)} — now <b>${t}</b>` };
        case "tax":
          return { cls: "gold", icon: "🪙", iconCls: "coins", html: `${who} taxes the treasury ${coinDeltaBadge(3)} — now <b>${t}</b>` };
        case "coup_paid":
          return { cls: "red-dim", icon: "🪙", iconCls: "coins", html: `${who} pays ${coinDeltaBadge(-7)} for a Coup — now <b>${t}</b>` };
        case "assassination_paid":
          return { cls: "red-dim", icon: "🪙", iconCls: "coins", html: `${who} pays ${coinDeltaBadge(-3)} — sunk — for an assassination, now <b>${t}</b>` };
        case "steal_given":
          return { cls: "red-dim", icon: "🪙", iconCls: "coins", html: `${who} is robbed ${coinDeltaBadge(-d)} — now <b>${t}</b>` };
        case "steal_took":
          return { cls: "gold", icon: "🪙", iconCls: "coins", html: `${who} steals ${coinDeltaBadge(d)} — now <b>${t}</b>` };
        default: {
          const delta = p.delta || 0;
          return { cls: delta >= 0 ? "gold" : "red-dim", icon: "🪙", iconCls: "coins", html: `${who}&rsquo;s coins change ${coinDeltaBadge(delta)} — now <b>${t}</b>` };
        }
      }
    }

    case "action_fizzled": {
      const rec = s.actionsBySeq[p.action_seq];
      const word = cap(rec && rec.type ? rec.type : "play");
      switch (p.reason) {
        case "counterclaim_success": return { cls: "red-dim", icon: "✕", html: `✕ The <b>${word}</b> fizzles — blocked. (Sunk costs stay sunk.)` };
        case "target_eliminated": return { cls: "red-dim", icon: "✕", html: `✕ The <b>${word}</b> fizzles — its target is already gone.` };
        case "target_forfeited": return { cls: "red-dim", icon: "✕", html: `✕ The <b>${word}</b> fizzles — its target has been forfeited.` };
        default: return { cls: "red-dim", icon: "✕", html: `✕ The <b>${word}</b> fizzles (${p.reason}).` };
      }
    }

    case "eliminated": {
      const st = seatById(s, p.seat_id);
      let chips = "";
      for (const c of p.cards || []) {
        if (c.card === "forfeited") chips += chipHtml("forfeited", st ? st.name : "");
        else chips += chipHtml(c.card, st ? st.name : "");
      }
      return { cls: "red bold", icon: "✕", iconChip: (p.cards && p.cards.length) ? firstRealCard(p.cards) : null, html: `<b>${nameIn(s, p.seat_id)} is eliminated.</b> ${chips}` };
    }

    case "exchange_revealed": { // private — only ever on the viewer's stream
      return { cls: "row-private", icon: "👁", html: `You drew ${(p.drawn || []).map(cap).join(" and ")}. Only you see this.` };
    }

    case "card_flip_choice_requested":
      if (p.seat_id === s.mySeatId) return { cls: "row-private", icon: "!", html: "You must choose which card falls." };
      return null; // private to another seat — never in this viewer's stream, guard anyway

    case "exchange_completed":
      return { cls: "dim", icon: "↺", html: `${nameIn(s, p.seat_id)} completes an exchange. (Two cards return to the deck, order unknown.)` };

    case "forfeit_vote_opened": {
      const target = nameIn(s, p.target_seat_id);
      const init = nameIn(s, p.initiator_seat_id);
      return { icon: "⚖", cls: "bold", html: `<b>FORFEIT VOTE</b> opened against ${target} by ${init} — majority ${p.majority_needed} of ${(p.eligible_voters || []).length}.` };
    }

    case "forfeit_vote_cast": {
      const word = p.vote === "yes" ? `<span class="word-yes">YES</span>` : `<span class="word-no">NO</span>`;
      return { icon: "⚖", html: `${nameIn(s, p.voter_seat_id)} votes ${word} on the forfeit vote.` };
    }

    case "forfeit_vote_resolved": {
      const y = (p.tally || {}).yes || 0;
      const n = (p.tally || {}).no || 0;
      const target = nameIn(s, s.vote && s.vote.targetSeatId ? s.vote.targetSeatId : null);
      if (p.result === "forfeited") {
        return { icon: "⚖", cls: "bold", html: `Vote passes (${y}–${n}). ${target} is eliminated — their cards are forfeited, never revealed.` };
      }
      return { icon: "⚖", html: `The vote fails (${y}–${n}). ${target} stays at the table.` };
    }

    case "game_over": {
      const w = seatById(s, p.winner_seat_id);
      return { kind: "banquet", text: `GAME OVER — ${w ? renderName(w) : "?"} WINS THE TABLE` };
    }

    case "rejection": // private; the HTTP 4xx already toasted (§7.3). Row = history.
      return { cls: "row-warning", icon: "⚠", html: `The table rejects yours: &ldquo;${esc(p.message || "")}&rdquo;` };

    default:
      return { cls: "dim", icon: "·", html: esc(p.type || "unknown event") };
  }
}

function firstRealCard(cards) {
  for (const c of cards || []) if (c.card !== "forfeited") return c.card;
  return null;
}

// ---------------------------------------------------------------------------
// Log DOM management (§4.6): append, follow/auto-scroll, 400-row trim cap (U13).
// ---------------------------------------------------------------------------

export const logUi = { follow: true };

const MAX_ROWS = 400; // U13

/**
 * Wire the log panel and return append(ev, row): build the element from the
 * §12 row descriptor (with the event's ts for the time slot), append it under
 * the §4.6 auto-scroll rules (follow ON + within 40px of bottom ⇒ stick;
 * otherwise surface the "new entries" jump pill), and enforce the 400-row cap.
 */
export function setupLog(bodyEl, newEntriesEl, followBtnEl) {
  const nearBottom = () => bodyEl.scrollTop + bodyEl.clientHeight >= bodyEl.scrollHeight - 40;
  followBtnEl.addEventListener("click", () => {
    logUi.follow = !logUi.follow;
    followBtnEl.textContent = logUi.follow ? "following" : "paused";
    followBtnEl.setAttribute("aria-pressed", String(logUi.follow));
    pendingNew = 0;
    newEntriesEl.hidden = true;
    if (logUi.follow) bodyEl.scrollTop = bodyEl.scrollHeight;
  });
  let pendingNew = 0;

  newEntriesEl.addEventListener("click", () => {
    pendingNew = 0;
    newEntriesEl.hidden = true;
    bodyEl.scrollTop = bodyEl.scrollHeight;
  });

  return function append(ev, row) {
    const el = makeRowEl(row, ev ? ev.ts : null);
    if (!el) return;
    bodyEl.appendChild(el);
    trimLog(bodyEl);
    const stick = logUi.follow && nearBottom();
    if (stick) {
      bodyEl.scrollTop = bodyEl.scrollHeight;
    } else if (logUi.follow) {
      pendingNew += 1;
      newEntriesEl.textContent = `New entries (${pendingNew}) ↓`;
      newEntriesEl.hidden = false;
    }
  };
}

function makeRowEl(row, ts) {
  if (!row) return null;
  const div = document.createElement("div");
  if (row.kind === "divider") {
    div.className = "row-divider";
    div.innerHTML = `<span>${row.text}</span>`;
    return div;
  }
  if (row.kind === "banquet") {
    div.className = "row-banquet";
    div.innerHTML = `<span>${row.text}</span>`;
    return div;
  }
  div.className = "row" + (row.cls ? " " + row.cls : "");
  const icon = row.iconChip != null ? chipHtml(row.iconChip) : (row.icon || "");
  div.innerHTML =
    `<span class="t">${clockTime(ts)}</span>` +
    `<span class="ic ${row.iconCls || ""}">${icon}</span>` +
    `<span class="tx">${row.html}</span>`;
  return div;
}

/** §4.6 retention: cap rendered rows at 400, oldest dropped behind a marker (U13). */
function trimLog(bodyEl) {
  const rows = bodyEl.querySelectorAll(".row, .row-divider, .row-banquet");
  if (rows.length <= MAX_ROWS) return;
  let excess = rows.length - MAX_ROWS;
  // Keep the trim marker (if present) — drop from the front.
  for (const el of [...rows]) {
    if (excess <= 0) break;
    if (el.classList.contains("trim-marker")) continue;
    el.remove();
    excess -= 1;
  }
  if (!bodyEl.querySelector(".trim-marker")) {
    const m = document.createElement("div");
    m.className = "trim trim-marker";
    m.textContent = "… earlier entries trimmed …";
    bodyEl.prepend(m);
  }
}

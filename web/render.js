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
  if (n === 2) return [{ x: 18, y: 50 }, { x: 82, y: 50 }];
  const R = 41; // % of the felt box
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
  const deckLine = `court deck · ${state.deckSize} cards`;
  let main = "", sub = "", gold = false;
  switch (state.phase) {
    case "turn_action": {
      const t = seatById(state, state.turnSeatId);
      main = `${esc(t ? t.name : "?")}'S TURN`;
      if (t && t.id === state.mySeatId) gold = true;
      sub = deckLine;
      break;
    }
    case "window_open": {
      main = "WINDOW OPEN";
      gold = true;
      sub = state.window && state.window.kind === "counterclaim_challenge"
        ? "the block itself is challenged"
        : "a claim hangs in the air";
      break;
    }
    case "single_seat_decision": {
      const d = state.decision;
      if (d && d.seatId === state.mySeatId) main = "CHOOSE A CARD";
      else {
        const st = seatById(state, d ? d.seatId : null);
        main = `WAITING ON ${esc(st ? st.name : "?")}`;
      }
      sub = deckLine;
      break;
    }
    default: // finished
      {
        const w = seatById(state, state.winnerId);
        main = `${w ? esc(w.name) : "?"} WINS`;
        gold = true;
        sub = deckLine;
      }
  }
  centerEl.querySelector(".c-main").textContent = main;
  centerEl.querySelector(".c-main").classList.toggle("gold", gold);
  centerEl.querySelector(".c-sub").textContent = sub;
  centerEl.querySelector(".c-deck").textContent = ""; // deck count lives in the sub line (§4.5)
}

function seatTileHtml(state, st, anchor) {
  const isMe = st.id === state.mySeatId;
  const acting = state.phase === "turn_action" && state.turnSeatId === st.id && st.active;
  const deciding = !!(state.window && st.active && state.window.eligible.includes(st.id)
    && !state.window.iResponded && isMe);
  const cls = ["seat"];
  if (acting) cls.push("acting");
  if (deciding) cls.push("decision");
  if (!st.active) cls.push("eliminated");

  // Name row: name + badge (§4.2), right slot = YOU tag / OUT tag (§4.3).
  let right = "";
  if (!st.active) right = `<span class="out-tag">OUT</span>`;
  else if (isMe) right = `<span class="you-tag">YOU</span>`;

  // Coins row — dimmed + inert title on eliminated seats (§4.3).
  const coinsTitle = !st.active ? ` title="coins are inert — this seat is out"` : "";
  const coinsRow = `<div class="s-coins"${coinsTitle}><span class="coin"></span><b>${st.coins}</b></div>`;

  // Card row: face-down backs (2 while active) + face-up chips (§4.3).
  let cards = "";
  if (!st.active) {
    for (const c of st.faceUp) cards += chipHtml(c, st.name);
    for (let i = 0; i < st.forfeitedCount; i++) cards += chipHtml("forfeited", st.name);
  } else {
    const faceDown = Math.max(0, 2 - st.faceUp.length);
    for (let i = 0; i < faceDown; i++) cards += `<span class="back" title="face-down"></span>`;
    for (const c of st.faceUp) cards += chipHtml(c, st.name);
  }

  const sx = `${anchor.x.toFixed(2)}%`;
  const sy = `${anchor.y.toFixed(2)}%`;
  return `<div class="${cls.join(" ")}" data-slot="${st.id}" style="--sx:${sx};--sy:${sy}">
    <div class="s-row"><span class="s-name">${renderName(st)}</span>${right}</div>
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

const CONSOLE_ACTIONS = [
  { type: "income",        label: "Income",       cap: "+1 coin",                        target: false },
  { type: "foreign_aid",   label: "Foreign Aid",  cap: "+2 coins",                       target: false },
  { type: "coup",          label: "Coup",         cap: "−7 now · target loses a card",   target: true },
  { type: "tax",           label: "Tax",          cap: "claim Duke · +3 if it holds",    target: false },
  { type: "steal",         label: "Steal",        cap: "claim Captain or Ambassador",    target: true, claimPicker: true },
  { type: "assassinate",   label: "Assassinate",  cap: "−3 now, sunk · claim Assassin",  target: true },
  { type: "exchange",      label: "Exchange",     cap: "claim Ambassador",               target: false },
];

// Character attached to a claim-and-act submission (§5.4).
const ACTION_CLAIMS = { tax: "duke", steal: null, assassinate: "assassin", exchange: "ambassador" };

export const consoleUi = { pick: null }; // transient target-picker selection (module-local)

export function renderConsole(state, el, onAction) {
  const show = state.phase === "turn_action" && state.turnSeatId === state.mySeatId && !state.eliminatedMe
    && state.status === "in_progress";
  if (!show) {
    el.hidden = true;
    consoleUi.pick = null;
    return;
  }
  el.hidden = false;

  const me = seatById(state, state.mySeatId);
  const mandatory = !!me && me.coins >= 10; // "≥10" superset reading (rules.md Ruling 1)
  const mandTitle = "you must take a coup while you have ten or more coins";

  const kicker = `<div class="console-kicker" aria-live="polite">YOUR MOVE — TURN ${state.turnId || "?"}` +
    (mandatory ? ` <span class="mandatory">· COUP IS MANDATORY</span>` : "") + `</div>`;

  if (state.consoleLocked) {
    // locked-awaiting (§5.4 state table): the POST got a 202, outcome is on its way.
    el.innerHTML = kicker + `<div class="console-status">Waiting on the table…</div>`;
    return;
  }

  const targets = state.seats.filter((s) => s.active && s.id !== state.mySeatId);

  let btns = "";
  for (const a of CONSOLE_ACTIONS) {
    const disabled = mandatory && a.type !== "coup";
    const title = disabled ? ` title="${mandTitle}"` : "";
    const sel = consoleUi.pick === a.type ? " selected" : "";
    btns += `<button type="button" class="cbtn${sel}" data-act="${a.type}"${title}${disabled ? " disabled" : ""}>
      <span class="lab">${a.label}</span><span class="cap">${a.cap}</span></button>`;
  }

  let picker = "";
  if (consoleUi.pick && CONSOLE_ACTIONS.find((x) => x.type === consoleUi.pick && x.target)) {
    const kind = consoleUi.pick;
    picker = `<div class="tpick" role="group" aria-label="Choose a target">` + targets.map((t) => {
      const noCoins = t.coins < 1;
      const disabledTarget = kind === "steal" && noCoins;
      const title = disabledTarget ? ` title="that player has no coins to steal from"` : "";
      if (kind === "steal") {
        // claim picker: two mini-buttons per candidate target (§5.4).
        return `<span class="tchip"${disabledTarget ? "" : ""}>${renderName(t)} <span class="tcoins">${t.coins}</span>
          <span class="tclaim">
            <button type="button" data-act="steal" data-target="${t.id}" data-char="captain"${disabledTarget ? " disabled" : title}>as Captain</button>
            <button type="button" data-act="steal" data-target="${t.id}" data-char="ambassador"${disabledTarget ? " disabled" : title}>as Ambassador</button>
          </span></span>`;
      }
      // coup / assassinate: one chip per target, click submits.
      return `<button type="button" class="tchip" data-act="${kind}" data-target="${t.id}"${title}>
        ${renderName(t)} <span class="tcoins">${t.coins}</span></button>`;
    }).join("") + `</div>`;
  }

  el.innerHTML = kicker + `<div class="console-btns">${btns}</div>${picker}`;

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
      return { icon: "◆", html: esc(p.text || "") };

    case "window_opened": {
      const names = (p.eligible || []).map((id) => nameIn(s, id)).join(", ");
      let text = `Responses open — ${names} may act.`;
      if (p.kind === "counterclaim_challenge") {
        const blocker = s.window && s.window.blockerId ? nameIn(s, s.window.blockerId) : "?";
        text += ` — ${blocker}'s block is itself challenged.`;
      }
      return { cls: "dim", html: text };
    }

    case "response_received": {
      const who = nameIn(s, p.seat_id);
      const r = p.response || {};
      if (r.type === "challenge") return { cls: "red", icon: "✕", html: `${who} <b>challenges!</b>` };
      if (r.type === "counterclaim") return { cls: "green", icon: "⬛", html: `${who} blocks as ${cap(r.character || "?")}` };
      return { cls: "dim", html: `${who} passed` };
    }

    case "window_closed": {
      const who = p.decider_seat_id ? nameIn(s, p.decider_seat_id) : null;
      switch (p.reason) {
        case "first_response": return { cls: "dim bold", html: `The window closes — ${who || "?"} moved first.` };
        case "all_responded": return { cls: "dim bold", html: "The window closes — every seat has answered." };
        case "timeout": return { cls: "dim bold", html: "The window closes — the window timed out; silent seats pass." };
        case "auto_no_responders": return { cls: "dim bold", html: "The window closes — no one is left to answer." };
        default: return { cls: "dim", html: `The window closes (${p.reason}).` };
      }
    }

    case "claim_succeeded": {
      const who = nameIn(s, p.seat_id);
      const ch = nameIn(s, p.challenger_seat_id);
      return { cls: "gold-dim", icon: "⬛", html: `${who} was holding the ${cap(p.character)}. ${ch}&rsquo;s challenge fails.` };
    }

    case "claim_failed": {
      const who = nameIn(s, p.seat_id);
      return { cls: "red", icon: "✕", html: `✕ ${who} did not hold the ${cap(p.claimed_character)}. The play is <span class="void-word">void</span>; ${who} loses a card.` };
    }

    case "card_flipped": {
      const who = nameIn(s, p.seat_id);
      const card = cap(p.card);
      let html;
      switch (p.reason) {
        case "penalty_failed_claim": html = `${who}&rsquo;s ${card} flips face-up — busted.`; break;
        case "penalty_failed_challenge": html = `${who}&rsquo;s ${card} flips face-up — the challenge failed.`; break;
        case "coup": html = `A coup lands on ${who} — ${card} flips face-up.`; break;
        case "assassinate": html = `The assassin&rsquo;s strike lands — ${who}&rsquo;s ${card} flips face-up.`; break;
        case "successful_reveal": {
          const seat = seatById(s, p.seat_id);
          html = `${who} was called out — ${card} is revealed in public, then straight back to the Court Deck (${seat ? esc(seat.name) : "?"} draws a replacement; this card never joins their face-up list).`;
          break;
        }
        default: html = `${who}&rsquo;s ${card} flips face-up.`;
      }
      return { iconChip: p.card, html };
    }

    case "coins_changed": {
      const who = nameIn(s, p.seat_id);
      const t = p.total;
      const d = Math.abs(p.delta || 0);
      switch (p.reason) {
        case "income": return { icon: "→", iconCls: "coins", html: `${who} takes Income (+1) — now ${t}` };
        case "foreign_aid": return { icon: "→", iconCls: "coins", html: `${who} takes Foreign Aid (+2) — now ${t}` };
        case "tax": return { icon: "→", iconCls: "coins", html: `${who} taxes the treasury (+3) — now ${t}` };
        case "coup_paid": return { icon: "→", iconCls: "coins", html: `${who} pays 7 for a Coup — now ${t}` };
        case "assassination_paid": return { icon: "→", iconCls: "coins", html: `${who} pays 3 — sunk — for an assassination, now ${t}` };
        case "steal_given": return { icon: "→", iconCls: "coins", html: `${who} is robbed (−${d}) — now ${t}` };
        case "steal_took": return { icon: "→", iconCls: "coins", html: `${who} steals +${d} — now ${t}` };
        default: return { icon: "→", iconCls: "coins", html: `${who}&rsquo;s coins change (${p.delta >= 0 ? "+" : ""}${p.delta}) — now ${t}` };
      }
    }

    case "action_fizzled": {
      const rec = s.actionsBySeq[p.action_seq];
      const word = cap(rec && rec.type ? rec.type : "play");
      switch (p.reason) {
        case "counterclaim_success": return { icon: "✕", html: `✕ The ${word} fizzles — blocked. (Sunk costs stay sunk.)` };
        case "target_eliminated": return { icon: "✕", html: `✕ The ${word} fizzles — its target is already gone.` };
        case "target_forfeited": return { icon: "✕", html: `✕ The ${word} fizzles — its target has been forfeited.` };
        default: return { icon: "✕", html: `✕ The ${word} fizzles (${p.reason}).` };
      }
    }

    case "eliminated": {
      const st = seatById(s, p.seat_id);
      let chips = "";
      for (const c of p.cards || []) {
        if (c.card === "forfeited") chips += chipHtml("forfeited", st ? st.name : "");
        else chips += chipHtml(c.card, st ? st.name : "");
      }
      return { cls: "bold", iconChip: (p.cards && p.cards.length) ? firstRealCard(p.cards) : null, html: `<b>${nameIn(s, p.seat_id)} is eliminated.</b> ${chips}` };
    }

    case "exchange_revealed": { // private — only ever on the viewer's stream
      return { cls: "row-private", html: `You drew ${(p.drawn || []).map(cap).join(" and ")}. Only you see this.` };
    }

    case "card_flip_choice_requested":
      if (p.seat_id === s.mySeatId) return { cls: "row-private", html: "You must choose which card falls." };
      return null; // private to another seat — never in this viewer's stream, guard anyway

    case "exchange_completed":
      return { cls: "dim", html: `${nameIn(s, p.seat_id)} completes an exchange. (Two cards return to the deck, order unknown.)` };

    case "forfeit_vote_opened": {
      const target = nameIn(s, p.target_seat_id);
      const init = nameIn(s, p.initiator_seat_id);
      return { icon: "⚖", cls: "bold", html: `FORFEIT VOTE opened against ${target} by ${init} — majority ${p.majority_needed} of ${(p.eligible_voters || []).length}.` };
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
        return { icon: "⚖", html: `Vote passes (${y}–${n}). ${target} is eliminated — their cards are forfeited, never revealed.` };
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
      return { cls: "dim", html: esc(p.type || "unknown event") };
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

// ===========================================================================
// state.js — local state object, snapshot seeding, event-batch reducer
// (ui-spec §7). Single source of truth; render functions are pure over it.
// Wire contract: docs/technical-spec.md §10/§11 (executable in the httptest
// suites under infrastructure/http/).
// ===========================================================================

export function cap(s) {
  return s ? String(s).charAt(0).toUpperCase() + String(s).slice(1) : "";
}

export function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

// HH:MM from an RFC3339 ts (browser local time).
export function clockTime(ts) {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "--:--";
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}`;
}

// RFC3339 → ms timestamp (or null when unparseable/absent).
export function tsMs(ts) {
  if (!ts) return null;
  const t = Date.parse(ts);
  return Number.isNaN(t) ? null : t;
}

/**
 * The single local state object of ui-spec §7.1. `logRows` holds the rendered
 * log as row descriptors (appended by the batch applier in main.js); `lastSeq`
 * is the poll cursor. Structural fields are mutated ONLY by reduce() and the
 * snapshot seeder — never by render code.
 */
export function createState(gameId, session) {
  return {
    gameId,
    mySeatId: session ? session.seatId : null,
    token: session ? session.token : null,

    seats: [],          // [{id,name,type,coins,faceUp[],forfeitedCount,active}] (snapshot §11.2 shape)
    myHand: [],         // your_face_down_cards
    deckSize: 0,
    status: "in_progress",
    winnerId: null,

    windowSettings: { requireAllResponses: false, responseTimeoutMs: 0 },
    phase: "turn_action",   // turn_action | window_open | single_seat_decision | finished
    turnSeatId: null,
    turnId: 0,

    // Populated when phase === window_open (ui-spec §7.1).
    window: null,

    // Populated for a pending single-seat decision addressed to the viewer
    // (flip_choice | exchange_finish). `drawn` only known via exchange_revealed.
    decision: null,

    // Open forfeit ballot (public; ui-spec §7/§10).
    vote: null,

    // action_declared events keyed by their own seq — fizzle wording, window
    // modal titles, "action word" lookups (ui-spec §7.3, §12).
    actionsBySeq: {},
    // response_received events keyed by seq — drives nested block window titles
    responsesBySeq: {},

    // Most recent action declared across table and per seat — drives live feedback bubbles & banners
    lastAction: null,          // { actorId, type, character, targetId, text, seq, ts }
    lastActionBySeat: {},      // [seatId] -> { type, character, targetId, text, ts }
    lastAlert: null,           // { kind: 'targeted'|'ai_turn', actorId, targetId, text, ts }

    logRows: [],
    lastSeq: 0,

    conn: "live",           // live | reconnecting
    consoleLocked: false,   // turn console awaiting its own accepted POST (§5.4)
    eliminatedMe: false,    // spectator mode (§11)
    outToastShown: false,

    // The most recent window_closed (drives stale/closed modal notices §7.5).
    lastWindowClose: null,   // {windowId, reason, deciderSeatId, seq}

    // Last still-open window from the replay pass (merged after bootstrap;
    // see reduce "window_opened"). Null when no open window is pending replay.
    replayWindow: null,

    justCreated: false,      // show the one-time token notice over first paint
  };
}

/**
 * After the bootstrap replay pass (ui-spec §7.1 step 4→5): fold the replayed
 * open-window shape into the snapshot-seeded state.window — the snapshot is
 * seed truth for seats/coins/hand, but its window object carries no eligible
 * list and no trigger text; the replay supplies both plus the authoritative
 * deadline. No-op unless both agree on the window id.
 */
export function mergeReplayedWindow(s) {
  const w = s.window;
  const r = s.replayWindow;
  if (!w || !r || r.windowId !== w.id) return;
  w.eligible = r.eligible;
  w.kind = r.kind;
  w.targetSeatId = r.targetSeatId;
  w.actorId = r.actorId;
  w.triggerActionSeq = r.triggerActionSeq;
  w.blockerId = r.blockerId;
  if (r.blockChar) w.blockChar = r.blockChar;
  if (r.deadlineAt != null) w.deadlineAt = r.deadlineAt; // event ts is authoritative
  const trig = r.triggerActionSeq ? s.actionsBySeq[r.triggerActionSeq] : null;
  if (trig && trig.text) {
    w.triggerText = trig.text;
  } else if (r.triggerActionSeq && s.responsesBySeq[r.triggerActionSeq]) {
    const respItem = s.responsesBySeq[r.triggerActionSeq];
    const bSeat = seatById(s, respItem.seatId);
    const charName = respItem.response && respItem.response.character;
    w.triggerText = `${bSeat ? bSeat.name : "Opponent"} claims ${cap(charName || "a character")} to block`;
  }
  // Only derive response grammar if the viewer is eligible and NOT the actor/blocker
  const isEligible = s.mySeatId && r.eligible.includes(s.mySeatId) && r.actorId !== s.mySeatId
    && (!r.blockerId || r.blockerId !== s.mySeatId);
  if (isEligible && !w.iResponded && w.myAllowed.length === 0) {
    w.myAllowed = deriveResponseGrammar(s);
  } else if (!isEligible) {
    w.myAllowed = [];
  }
}

/**
 * Seed state from GET /games/{id} (ui-spec §7.1 step 2). The snapshot is the
 * seed truth for seat state — event deltas are applied only after this.
 */
export function seedFromSnapshot(snap) {
  const s = _current;
  s.seats = snap.seats.map((st) => ({
    id: st.id,
    name: st.name,
    type: st.type,          // "human" | "llm"
    coins: st.coins,
    faceUp: Array.isArray(st.face_up_cards) ? [...st.face_up_cards] : [],
    forfeitedCount: 0,
    active: !!st.active,
  }));
  s.myHand = Array.isArray(snap.your_face_down_cards) ? [...snap.your_face_down_cards] : [];
  s.deckSize = snap.court_deck_size;
  s.status = snap.status;
  s.winnerId = snap.winner_id || null;
  s.windowSettings = {
    requireAllResponses: !!snap.window_settings.require_all_responses,
    responseTimeoutMs: snap.window_settings.response_timeout_ms || 0,
  };
  s.mySeatId = snap.your_seat_id; // the snapshot is authoritative for the viewer's seat
  s.eliminatedMe = false;
  s.seats.forEach((st) => {
    if (st.id === s.mySeatId && !st.active) s.eliminatedMe = true;
  });

  const cur = snap.current || { phase: "finished" };
  s.phase = cur.phase || "finished";
  s.turnId = cur.turn_id || 0;
  s.turnSeatId = cur.turn_seat_id || null;
  s.window = null;
  s.decision = null;
  s.vote = null;            // re-established by the replay pass if one is live
  s.replayWindow = null;

  if (s.phase === "window_open" && cur.window) {
    const w = cur.window;
    s.window = {
      id: w.window_id,
      kind: w.kind,                              // challenge | counterclaim_challenge
      targetSeatId: w.target_seat_id || null,
      eligible: [],                               // snapshot carries none — mergeReplayedWindow fills it
      actorId: null,
      triggerActionSeq: null,
      deadlineAt: tsMs(w.deadline_at),            // server-computed at read time; replay refines it
      requireAll: !!w.require_all_responses,
      timeoutMs: w.timeout_ms || 0,
      myAllowed: Array.isArray(w.your_allowed_responses) ? [...w.your_allowed_responses] : [],
      responses: Object.assign({}, w.responses_so_far || {}),
      iResponded: !!(w.responses_so_far && w.responses_so_far[s.mySeatId]),
      myResponseLabel: windowResponsesLabel(w.responses_so_far && w.responses_so_far[s.mySeatId]),
      triggerText: "",                            // filled by the replay pass (action_declared.text)
      blockerId: null,
    };
  }

  if (s.phase === "single_seat_decision" && cur.decision) {
    const d = cur.decision;
    s.decision = {
      id: d.id,
      kind: d.kind,                              // flip_choice | exchange_finish
      seatId: d.seat_id,
      options: d.options || null,                // present only for the addressed viewer
      drawn: null,                               // filled by replayed exchange_revealed (private)
      deadlineAt: tsMs(d.deadline_at),
      timeoutMs: d.timeout_ms || 0,
      submitted: false,
      settled: null,                             // "done" | card name | expired copy key
    };
  }

  if (s.status !== "in_progress" || s.phase === "finished") {
    s.phase = "finished";
  }
  return s;
}

// Map a canonical responses_so_far value to the modal's "you answered" label.
function windowResponsesLabel(v) {
  if (!v) return null;
  if (v === "pass") return "PASS";
  if (v === "challenge") return "CHALLENGE";
  if (v.startsWith("counterclaim:")) return "BLOCK — " + cap(v.slice(13));
  return v.toUpperCase();
}

/** The module's live state handle (main.js assigns it once per game view). */
export let _current = null;
export function setCurrentState(s) { _current = s; return s; }
export function getCurrentState() { return _current; }

/** Seat lookup helper (undefined when absent — call sites render defensively). */
export function seatById(s, id) {
  if (!id) return undefined;
  return s.seats.find((x) => x.id === id);
}
export function nameOf(s, id) {
  const st = seatById(s, id);
  return st ? st.name : "?";
}

/**
 * Canonical response value from a response_received event payload.
 * "pass" | "challenge" | "counterclaim:<character>" (matches responses_so_far).
 */
export function canonicalResponse(resp) {
  if (!resp) return null;
  if (resp.type === "counterclaim") return "counterclaim:" + (resp.character || "?");
  return resp.type || null;
}

// ---------------------------------------------------------------------------
// Event reducer — per-type STATE effects (ui-spec §7.3). Log rendering is a
// separate concern (render.js#rowForEvent); this mutates only structural state.
// `forLog` = bootstrap replay pass: log/memory only, no seat-state deltas
// (the snapshot is the seed truth; the replay exists for the log and for
// reconstructing private knowledge such as exchange draws — ui-spec §7.1.4).
// ---------------------------------------------------------------------------

export function reduce(s, ev, opts = {}) {
  const p = ev || {};
  switch (p.type) {
    case "game_created":
      // Seats arrive from the snapshot; nothing structural to apply here.
      break;

    case "turn_start":
      if (!opts.forLog) {
        s.phase = "turn_action";
        s.turnSeatId = p.seat_id;
        s.turnId = p.turn_id || s.turnId;
        s.window = null;
        s.decision = null;
        if (p.seat_id === s.mySeatId) s.consoleLocked = false; // a fresh turn re-opens the console
      }
      break;

    case "action_declared": {
      const actObj = {
        type: p.action && p.action.type,
        character: p.action && p.action.character,
        actorId: p.actor_id,
        targetId: p.action && p.action.target_id,
        text: p.text || "",
        fizzled: null,
        seq: p.seq,
        ts: Date.now(),
      };
      s.actionsBySeq[p.seq] = actObj;
      s.lastAction = actObj;
      s.lastActionBySeat[p.actor_id] = actObj;
      if (p.action && p.action.target_id === s.mySeatId && p.actor_id !== s.mySeatId) {
        s.lastAlert = { kind: "targeted", actorId: p.actor_id, actionType: p.action.type, text: p.text, ts: Date.now() };
      }
      break;
    }

    case "coins_changed":
      if (!opts.forLog) {
        const st = seatById(s, p.seat_id);
        if (st) st.coins = p.total; // trust the absolute total (§7.3)
      }
      break;

    case "window_opened": {
      const prevDecider = s.lastWindowClose ? s.lastWindowClose.deciderSeatId : null;
      const trigSeq = p.trigger && p.trigger.action_seq;
      let trigText = "";
      let blockChar = null;
      let blockerId = null;

      if (p.kind === "counterclaim_challenge") {
        blockerId = (p.trigger && p.trigger.actor_id) || prevDecider;
        if (trigSeq && s.responsesBySeq[trigSeq]) {
          const respItem = s.responsesBySeq[trigSeq];
          if (respItem.response && respItem.response.character) {
            blockChar = respItem.response.character;
          }
          if (respItem.seatId) blockerId = respItem.seatId;
        }
        const blockerSeat = seatById(s, blockerId);
        const charName = blockChar ? cap(blockChar) : "their character";
        trigText = `${blockerSeat ? blockerSeat.name : "Opponent"} claims ${charName} to block`;
      } else if (trigSeq && s.actionsBySeq[trigSeq]) {
        trigText = s.actionsBySeq[trigSeq].text || "";
      }

      // The snapshot (seed truth) carries no eligible list — the live event is
      // the only source. In the forLog replay pass we keep a record of the last
      // still-open window so main.js can merge it into the seeded state.window
      // (ui-spec §7.1 step 4: replay reconstructs the current window's shape).
      s.replayWindow = {
        windowId: p.window_id,
        kind: p.kind,
        targetSeatId: p.target_seat_id || null,
        eligible: [...(p.eligible || [])],
        actorId: p.trigger && p.trigger.actor_id || null,
        triggerActionSeq: trigSeq || null,
        deadlineAt: (p.timeout_ms ? Date.parse(ev.ts) + p.timeout_ms : null),
        requireAll: !!p.require_all_responses,
        timeoutMs: p.timeout_ms || 0,
        blockerId,
        blockChar,
      };
      if (!opts.forLog) {
        s.phase = "window_open";
        s.window = {
          ...s.replayWindow,
          id: p.window_id,
          myAllowed: [],                          // snapshot-only field; re-derived per §5.1 below.
          responses: {},
          iResponded: false,
          myResponseLabel: null,
          triggerText: trigText || (trigSeq && s.actionsBySeq[trigSeq] && s.actionsBySeq[trigSeq].text) || "",
          blockerId,
          blockChar,
        };
        // Only derive response grammar if viewer is eligible and NOT the actor/blocker
        const isEligible = s.mySeatId && s.window.eligible.includes(s.mySeatId)
          && (s.window.kind !== "challenge" || s.window.actorId !== s.mySeatId)
          && (s.window.kind !== "counterclaim_challenge" || blockerId !== s.mySeatId);
        if (isEligible) {
          s.window.myAllowed = deriveResponseGrammar(s);
        } else {
          s.window.myAllowed = [];
        }
      }
      break;
    }

    case "response_received":
      if (p.seq) {
        s.responsesBySeq[p.seq] = { seq: p.seq, seatId: p.seat_id, response: p.response, windowId: p.window_id, ts: Date.now() };
      }
      if (p.response && p.response.type === "counterclaim") {
        s.lastActionBySeat[p.seat_id] = {
          type: "counterclaim",
          character: p.response.character,
          targetId: null,
          text: `${seatById(s, p.seat_id)?.name || "Opponent"} blocks as ${cap(p.response.character || "character")}`,
          ts: Date.now(),
        };
      } else if (p.response && (p.response.type === "challenge" || p.response === "challenge")) {
        const actorName = s.window && s.window.actorId ? (seatById(s, s.window.actorId)?.name || "Opponent") : "claim";
        s.lastActionBySeat[p.seat_id] = {
          type: "challenge",
          character: null,
          targetId: s.window ? s.window.actorId : null,
          text: `Challenges ${actorName}!`,
          ts: Date.now(),
        };
      }
      if (!opts.forLog && s.window && p.window_id === s.window.id) {
        const v = canonicalResponse(p.response);
        if (v) {
          s.window.responses[p.seat_id] = v;
          if (p.seat_id === s.mySeatId && !s.window.iResponded) {
            s.window.iResponded = true;
            s.window.myResponseLabel = windowResponsesLabel(v);
          }
        }
      }
      break;

    case "window_closed":
      // Tracked even on the forLog pass: it drives nested-block attribution
      // (blockerId) and is part of replayed window shape reconstruction.
      s.lastWindowClose = {
        windowId: p.window_id,
        reason: p.reason,
        deciderSeatId: p.decider_seat_id || null,
        seq: p.seq,
      };
      s.replayWindow = null;
      if (!opts.forLog) {
        // Tentative phase per §7.3 — the rest of the batch settles it (a nested
        // window_opened in the same batch re-opens state.window).
        s.phase = "turn_action";
        s.window = null;
      }
      break;

    case "claim_succeeded":
    case "claim_failed":
      // No seat-state change beyond what accompanying events carry (§7.3).
      break;

    case "card_flipped":
      if (!opts.forLog) {
        // A flip settles a pending flip-choice decision for the flipped seat —
        // mark it FIRST (the modal's S3/S4 copy needs this), then adjust myHand.
        if (s.decision && s.decision.kind === "flip_choice" && p.seat_id === s.decision.seatId) {
          s.decision.settled = s.decision.submitted ? p.card : "expired";
        }
        const st = seatById(s, p.seat_id);
        if (st) {
          // successful_reveal returns to the deck — it does NOT join face-up (§10.1).
          if (p.reason !== "successful_reveal") st.faceUp.push(p.card);
          if (p.seat_id === s.mySeatId) applyMyFlip(s, p.card);
        }
      }
      break;

    case "action_fizzled": {
      const rec = s.actionsBySeq[p.action_seq];
      if (rec) rec.fizzled = p.reason;
      if (s.lastAction && s.lastAction.seq === p.action_seq) {
        s.lastAction.fizzled = p.reason;
      }
      if (rec && rec.actorId && s.lastActionBySeat[rec.actorId]) {
        s.lastActionBySeat[rec.actorId].fizzled = p.reason;
      }
      break;
    }

    case "eliminated":
      if (!opts.forLog) {
        const st = seatById(s, p.seat_id);
        if (st) {
          st.active = false;
          let revealed = 0;
          for (const c of p.cards || []) {
            if (c.card === "forfeited") st.forfeitedCount++; // never shown as a real card (§7)
            else { st.faceUp.push(c.card); revealed++; }
          }
          if (p.seat_id === s.mySeatId) s.eliminatedMe = true;
        }
      }
      break;

    case "exchange_revealed":
      // Private to the exchanging seat. The drawn cards are the ONLY place they
      // ever appear — the snapshot deliberately omits them, so:
      //   live pass: create the pending decision (the modal opens from it);
      //   forLog pass (bootstrap/recovery replay): do NOT re-create stale
      //   decisions from history — only attach the draw to the snapshot-seeded
      //   pending decision when the ids match (seed truth, §7.1).
      if (p.seat_id !== s.mySeatId) break;
      if (opts.forLog) {
        if (s.decision && s.decision.kind === "exchange_finish" && p.decision && s.decision.id === p.decision.id) {
          s.decision.drawn = [...(p.drawn || [])];
          if (s.decision.deadlineAt == null && p.decision.timeout_ms) {
            s.decision.deadlineAt = Date.parse(ev.ts) + p.decision.timeout_ms;
          }
        }
      } else {
        s.decision = {
          id: p.decision && p.decision.id,
          kind: "exchange_finish",
          seatId: p.seat_id,
          options: p.decision ? p.decision.options : null,
          drawn: [...(p.drawn || [])],
          deadlineAt: (p.decision && p.decision.timeout_ms) ? Date.parse(ev.ts) + p.decision.timeout_ms : null,
          timeoutMs: (p.decision && p.decision.timeout_ms) || 0,
          submitted: false,
          settled: null,
        };
      }
      break;

    case "card_flip_choice_requested":
      // Private. History-only on the forLog pass — a snapshot-seeded pending
      // decision (or absence of one) is seed truth; re-creating from an old
      // event would open a modal for a decision already made.
      if (!opts.forLog && p.seat_id === s.mySeatId) {
        // Options are positional (§5.4): face_down_i ↔ myHand[i].
        s.decision = {
          id: p.decision && p.decision.id,
          kind: "flip_choice",
          seatId: p.seat_id,
          options: [...(p.decision && p.decision.options || [])],
          drawn: null,
          deadlineAt: (p.decision && p.decision.timeout_ms) ? Date.parse(ev.ts) + p.decision.timeout_ms : null,
          timeoutMs: (p.decision && p.decision.timeout_ms) || 0,
          submitted: false,
          settled: null,
        };
      }
      break;

    case "exchange_completed":
      // No card identities are public here — myHand was already adjusted
      // locally at submit time (§7.3: do NOT double-apply). Marks the pending
      // decision settled on both passes (the forLog case guards a replayed draw
      // against reviving an already-finished exchange).
      if (s.decision && s.decision.kind === "exchange_finish" && p.seat_id === s.mySeatId) {
        s.decision.settled = "done";
      }
      break;

    case "forfeit_vote_opened":
      if (!opts.forLog) {
        s.vote = {
          id: p.vote_id,
          targetSeatId: p.target_seat_id,
          initiatorSeatId: p.initiator_seat_id,
          eligibleVoters: [...(p.eligible_voters || [])],
          majorityNeeded: p.majority_needed || 0,
          votes: {},
          result: null,
          tally: null,
          myVote: null,
        };
      }
      break;

    case "forfeit_vote_cast":
      if (!opts.forLog && s.vote && p.vote_id === s.vote.id) {
        s.vote.votes[p.voter_seat_id] = p.vote; // "yes" | "no"
        if (p.voter_seat_id === s.mySeatId) s.vote.myVote = p.vote;
      }
      break;

    case "forfeit_vote_resolved":
      if (!opts.forLog && s.vote && p.vote_id === s.vote.id) {
        s.vote.result = p.result;
        s.vote.tally = p.tally || { yes: 0, no: 0, eligible: s.vote.eligibleVoters.length };
      }
      break;

    case "game_over":
      if (!opts.forLog) {
        s.status = "finished";
        s.winnerId = p.winner_seat_id || null;
        s.phase = "finished";
        s.window = null;
        s.decision = null;
      }
      break;

    case "rejection":
      // Log row only — the HTTP 4xx that caused it already toasted (§7.3).
      break;

    default:
      // Unknown future event types are ignored state-wise (rows still render
      // a generic line so nothing disappears silently).
      break;
  }
}

/** Remove the flipped card from myHand at the right position index (§7.3). */
function applyMyFlip(s, card) {
  if (s.myHand.length === 0) return;
  let idx = null;
  if (s.decision && s.decision.kind === "flip_choice" && s.decision.submitted && s.decision.chosenIndex != null) {
    idx = s.decision.chosenIndex;
  } else if (s.myHand.length === 1) {
    idx = 0; // auto-flip: only one card remains
  } else {
    idx = s.myHand.findIndex((c) => c === card); // best effort
  }
  if (idx == null || !s.myHand[idx]) return;
  s.myHand.splice(idx, 1);
  // The decision object itself is kept until its modal closes (the settled flag
  // drives the S3/S4 copy); decide.js drops state.decision on close.
}

/**
 * Re-derive the viewer's response grammar for a LIVE window (ui-spec §8 / tech
 * spec §5.1) — the window_opened event carries no per-seat list, which is
 * snapshot-only. Mirrors domain.WindowView.responseTokensForSeat exactly:
 *   nested counterclaim window        → pass | challenge
 *   foreign_aid block window          → pass | counterclaim_duke (no challenge exists)
 *   tax / exchange                    → pass | challenge
 *   attack window, viewer is target   → pass | blocks for the attack | challenge
 *   attack window, viewer a bystander → pass | challenge
 */
export function deriveResponseGrammar(s) {
  const w = s.window;
  if (!w || !s.mySeatId) return [];

  // An actor can NEVER challenge or block their own declared turn action
  if (w.kind === "challenge" && w.actorId === s.mySeatId) return [];
  // A blocker can NEVER challenge their own counterclaim block
  if (w.kind === "counterclaim_challenge" && w.blockerId === s.mySeatId) return [];
  // If the eligible responder list is populated and the viewer is not in it, return []
  if (Array.isArray(w.eligible) && w.eligible.length > 0 && !w.eligible.includes(s.mySeatId)) return [];

  const act = w.triggerActionSeq ? s.actionsBySeq[w.triggerActionSeq] : null;
  switch (w.kind) {
    case "counterclaim_challenge":
      return ["pass", "challenge"];
    case "challenge":
      if (act && act.type === "foreign_aid") return ["pass", "counterclaim_duke"];
      if (act && (act.type === "tax" || act.type === "exchange")) return ["pass", "challenge"];
      if (w.targetSeatId && w.targetSeatId === s.mySeatId) {
        // Attack window — block options follow the attack's character (§5.1).
        if (act && act.type === "steal") return ["pass", "counterclaim_captain", "counterclaim_ambassador", "challenge"];
        if (act && act.type === "assassinate") return ["pass", "counterclaim_contessa", "challenge"];
      }
      return ["pass", "challenge"];
    default:
      return []; // unknown window kind — fail closed to no buttons (toast on submit)
  }
}

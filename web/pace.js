// ===========================================================================
// pace.js — AI turn cadence & animation pacing controller (ui-spec §7).
// Provides natural, human-friendly delays (~600–800ms) so players can follow
// AI turns, speech bubbles, and table resolutions, with speed controls.
// ===========================================================================

const PACE_STORAGE_KEY = "coup.pace";
export const PACES = ["normal", "fast", "instant"];

export const PACE_CONFIG = {
  normal: {
    id: "normal",
    label: "Pace: Normal",
    icon: "⏱️",
    desc: "Natural human delay · AI moves are clearly readable",
    multiplier: 1.0,
  },
  fast: {
    id: "fast",
    label: "Pace: Fast",
    icon: "⚡",
    desc: "Snappy cadence · Quick game pace",
    multiplier: 0.35,
  },
  instant: {
    id: "instant",
    label: "Pace: Instant",
    icon: "🚀",
    desc: "No delays · Turns resolve immediately",
    multiplier: 0,
  },
};

let currentPace = (function () {
  try {
    if (typeof localStorage !== "undefined") {
      const saved = localStorage.getItem(PACE_STORAGE_KEY);
      if (saved && PACE_CONFIG[saved]) return saved;
    }
  } catch {}
  return "normal";
})();

export function getPace() {
  return currentPace;
}

export function setPace(pace) {
  if (!PACE_CONFIG[pace]) pace = "normal";
  currentPace = pace;
  try {
    if (typeof localStorage !== "undefined") {
      localStorage.setItem(PACE_STORAGE_KEY, pace);
    }
  } catch {}
  if (typeof window !== "undefined") {
    window.dispatchEvent(new CustomEvent("coup:pace-changed", { detail: { pace: currentPace } }));
  }
}

export function cyclePace() {
  const idx = PACES.indexOf(currentPace);
  const next = PACES[(idx + 1) % PACES.length];
  setPace(next);
  return next;
}

/**
 * Computes the recommended display duration (in ms) for an event, based on
 * the current playback pace, the type of event, and whether the viewer is involved.
 */
export function delayForEvent(ev, queueLen = 0, state = null) {
  if (currentPace === "instant") return 0;

  const cfg = PACE_CONFIG[currentPace] || PACE_CONFIG.normal;
  const baseMultiplier = cfg.multiplier;

  // Scale down gracefully if queue is backing up
  let catchupFactor = 1.0;
  if (queueLen > 10) catchupFactor = 0.35;
  else if (queueLen > 5) catchupFactor = 0.6;
  else if (queueLen > 2) catchupFactor = 0.85;

  const factor = baseMultiplier * catchupFactor;

  const p = ev.payload || ev;
  const type = p.type || ev.type;

  switch (type) {
    case "turn_start": {
      // If it's the viewer's turn, immediate console readiness
      if (state && p.seat_id === state.mySeatId) return 0;
      // AI turn: brief pause to show the AI deliberating
      return Math.round(550 * factor);
    }

    case "action_declared": {
      const isTargetingMe = state && p.action && p.action.target_id === state.mySeatId && p.actor_id !== state.mySeatId;
      if (isTargetingMe) {
        return Math.round(900 * factor); // Extra time to digest being targeted
      }
      return Math.round(800 * factor); // Time to read speech bubble & hear audio cue
    }

    case "window_opened": {
      // If viewer must respond, fast transition into the response modal
      const eligible = p.eligible || [];
      if (state && state.mySeatId && eligible.includes(state.mySeatId)) {
        return Math.round(250 * factor);
      }
      return Math.round(350 * factor);
    }

    case "response_received": {
      const resp = p.response;
      const respType = String(typeof resp === "string" ? resp : ((resp && resp.type) || "")).toLowerCase();
      if (respType.includes("challenge") || respType.includes("counterclaim") || respType.includes("block")) {
        return Math.round(750 * factor); // Dramatic block or challenge
      }
      return Math.round(400 * factor); // Standard pass
    }

    case "window_closed":
      return Math.round(350 * factor);

    case "coins_changed":
      return Math.round(450 * factor);

    case "card_flipped":
      return Math.round(750 * factor); // Drama of losing or revealing card

    case "card_flip_choice_requested":
      return Math.round(250 * factor);

    case "exchange_revealed":
    case "exchange_completed":
      return Math.round(500 * factor);

    case "eliminated":
      return Math.round(700 * factor);

    case "game_over":
      return Math.round(600 * factor);

    case "action_fizzled":
      return Math.round(400 * factor);

    default:
      return Math.round(250 * factor);
  }
}

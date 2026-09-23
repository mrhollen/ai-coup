// ===========================================================================
// poll.js — single-flight long-poll loop (ui-spec §7.2), 70s client watchdog
// (U6), backoff reconnect (§7.2 errors), re-snapshot policy (U7), and the
// shared API helpers for all POST traffic (§7.4). No UI here: hooks only.
// ===========================================================================

export const WATCHDOG_MS = 70_000;     // U6: server hold is ≤30s, so only genuine hangs fire this
const BACKOFF_STEPS_MS = [1000, 2000, 4000, 8000]; // §7.2: 1s, 2s, 4s, 8s (capped)

/** Parsed API failure with the §11.1 envelope body ({code, message, request_id}). */
export class ApiError extends Error {
  constructor(status, envelope, network = false) {
    super(envelope && envelope.message ? envelope.message : `HTTP ${status}`);
    this.status = status;
    this.envelope = envelope || null; // {code, message, request_id} | null
    this.network = network;           // true for transport failures / watchdog aborts
  }
  get code() { return this.envelope ? this.envelope.code : null; }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * GET a JSON API path with the session's bearer token. Throws ApiError on any
 * non-2xx (with the parsed error envelope) or transport failure.
 */
export async function apiGet(state, path) {
  const res = await doFetch(state, path, { method: "GET" });
  if (!res.ok) throw await toApiError(res);
  return res.json();
}

/**
 * POST a JSON decision/vote body (ui-spec §7.4). On non-2xx the envelope is
 * parsed and thrown as ApiError — callers toast verbatim + recover per code.
 * A 202 means *queued*, not *resolved*: never treat the body as an outcome.
 */
export async function apiPost(state, path, body) {
  const res = await doFetch(state, path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw await toApiError(res);
  return res.json();
}

async function toApiError(res) {
  let envelope = null;
  try {
    const j = await res.json();
    if (j && j.error) envelope = j.error;
  } catch { /* non-JSON error body — keep status only */ }
  return new ApiError(res.status, envelope);
}

async function doFetch(state, path, init) {
  const headers = Object.assign({}, init.headers || {});
  if (state.token) headers["Authorization"] = "Bearer " + state.token;
  try {
    return await fetch(path, Object.assign({}, init, { headers }));
  } catch (e) {
    throw new ApiError(0, null, true); // transport failure (network down / watchdog abort surfaced)
  }
}

// ---------------------------------------------------------------------------
// The long-poll loop (§7.2). Exactly one in-flight GET …/events?after={seq};
// the next poll is issued only after the previous settles. Empty arrays are a
// normal hold-expiry return — just re-poll (A1), no UI change, no log entry.
// ---------------------------------------------------------------------------

/**
 * Start polling for `state`. Hooks:
 *   onBatch(events)        — apply an accepted batch (ascending seq)
 *   onConn(conn)           — "live" | "reconnecting" (topbar dot, §4.1)
 *   resnapshot()           — async; recovery resnapshot after failures (§7.2/U7).
 *                            Must re-seed state from the fresh snapshot and set
 *                            state.lastSeq to its last_event_seq; returns true on success.
 *   onFailToast()          — >3 consecutive failures: info toast "Connection lost — retrying…"
 *   onUnauthorized(err)    — 401 anywhere: stop everything, S4 overlay (§7.2)
 *   onGameGone(err)        — 404 anywhere: one-shot "game gone" modal (U17)
 * Returns a handle with .stop(). Polling continues harmlessly after game_over
 * (streams stay empty — owner ruling for this build).
 */
export function startPolling(state, hooks) {
  let stopped = false;
  let inFlight = false;
  let failures = 0; // consecutive transport/5xx failures (resets on any 200)
  let toastShownForEpisode = false;

  const loop = async () => {
    while (!stopped) {
      if (inFlight) await sleep(150); // another loop iteration owns the flight
      inFlight = true;
      const after = state.lastSeq;
      try {
        const res = await pollOnce(state, after);
        inFlight = false;
        failures = 0;
        toastShownForEpisode = false;
        if (stopped) return;
        if (hooks.onConn && state.conn !== "live") {
          state.conn = "live";
          hooks.onConn("live");
        }
        const evts = res.events || [];
        if (evts.length > 0) {
          hooks.onBatch(evts); // apply-then-repoll (§7.2)
        }
        continue; // immediately issue the next poll
      } catch (err) {
        inFlight = false;
        if (stopped) return;

        if (err instanceof ApiError && err.status === 401) { hooks.onUnauthorized(err); return; }
        if (err instanceof ApiError && err.status === 404)  { hooks.onGameGone(err); return; }

        // Transport failure, watchdog abort, or 5xx/other → reconnecting (§7.2).
        failures += 1;
        state.conn = "reconnecting";
        if (hooks.onConn) hooks.onConn("reconnecting");
        if (failures === 4 && !toastShownForEpisode) { // >3 consecutive failures (§7.2)
          toastShownForEpisode = true;
          if (hooks.onFailToast) hooks.onFailToast();
        }
        const delay = BACKOFF_STEPS_MS[Math.min(failures - 1, BACKOFF_STEPS_MS.length - 1)];
        await sleep(delay);
        if (stopped) return;
        // Re-snapshot once on recovery (§7.2/U7): the fresh snapshot re-seeds
        // state (and its last_event_seq advances the poll cursor). The log is
        // never cleared — it keeps its own rows either way.
        if (hooks.resnapshot) {
          try { await hooks.resnapshot(); } catch { /* keep polling from the same after */ }
        }
      }
    }
  };

  let flightController = null;
  function pollOnce(stateRef, after) {
    flightController && flightController.abort();
    const ctl = new AbortController();
    flightController = ctl;
    const t = setTimeout(() => ctl.abort(), WATCHDOG_MS); // U6: abort + re-issue with the SAME after
    return (async () => {
      try {
        const res = await doFetchRaw(stateRef, `/games/${stateRef.gameId}/events?after=${encodeURIComponent(after)}`, ctl.signal);
        if (!res.ok) throw await toApiError(res);
        return await res.json();
      } finally {
        clearTimeout(t);
      }
    })();
  }

  // Raw fetch for the poll: an AbortError must surface as a network ApiError.
  async function doFetchRaw(stateRef, path, signal) {
    const headers = {};
    if (stateRef.token) headers["Authorization"] = "Bearer " + stateRef.token;
    try {
      return await fetch(path, { method: "GET", headers, signal });
    } catch (e) {
      throw new ApiError(0, null, true);
    }
  }

  loop();
  return {
    stop() { stopped = true; if (flightController) flightController.abort(); },
  };
}

// ---------------------------------------------------------------------------
// Decision submission plumbing (§7.4) — shared by every surface in decide.js.
// Tosts the verbatim table-voice message (the copy is the server's, ui-spec
// §13) and returns the error so callers can run per-code recovery.
// ---------------------------------------------------------------------------

export function kickerForCode(code) {
  switch (code) { // §13 mapping — unknown codes render upper-cased code
    case "not_your_turn": return "NOT YOUR TURN";
    case "already_resolved": return "TOO LATE";
    case "not_eligible": return "NO SAY SO";
    case "invalid_payload": return "ILLEGAL MOVE";
    case "game_over": return "GAME OVER";
    case "unauthorized": return "ACCESS DENIED";
    case "not_found": return "NOT FOUND";
    case "vote_not_found": return "NO SUCH VOTE";
    case "vote_closed": return "VOTE CLOSED";
    case "not_a_voter": return "NOT A VOTER";
    case "self_target": return "CAN'T TARGET YOURSELF";
    case "target_not_active": return "ALREADY ELIMINATED";
    default: return code ? String(code).toUpperCase() : "THE TABLE HICCUPS";
  }
}

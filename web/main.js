// ===========================================================================
// main.js — composition root of the Coup web client (ui-spec §2/§7).
//   hash router (#/ , #/?join , #/g/{id}) · sessionStorage sessions (U2)
//   S3 bootstrap sequence (§7.1) · event-batch application (§7.2/§7.3)
//   wires state.js / poll.js / render.js / decide.js / lobby.js together.
// ===========================================================================

import { createState, setCurrentState, seedFromSnapshot, reduce, mergeReplayedWindow } from "./state.js";
import { startPolling, apiGet, ApiError } from "./poll.js";
import {
  renderTable, renderHand, renderConsole, renderTopbar, updateConn, renderBanners,
  setupLog, rowForEvent, resetBannerDismissals, consoleActBody, consoleUi,
} from "./render.js";
import {
  ensureSurfaces, submitConsoleAction, openForfeitInitiation, isBallotModalOpen,
  showTokenNotice, showGameOver, showAuthError, showGameGone, closeAllModals,
  setDecideHooks, rejectToast, infoToast, startTickers, transitionWindowModalOnClose,
  settleDecisionModal,
} from "./decide.js";
import { renderLobby, setLobbyHooks } from "./lobby.js";

// ---------------------------------------------------------------------------
// §2 sessions — (gameId, seatId, token) in sessionStorage, per tab (U2). A
// new tab starts with no session; two tabs can play two seats of one game.
// ---------------------------------------------------------------------------
const SESSION_PREFIX = "coup.session.";
const sessKey = (gid) => SESSION_PREFIX + gid;

function loadSession(gid) {
  try {
    const raw = sessionStorage.getItem(sessKey(gid));
    return raw ? JSON.parse(raw) : null;
  } catch { return null; }
}
function saveSession(gid, sess) {
  try { sessionStorage.setItem(sessKey(gid), JSON.stringify(sess)); } catch { /* private mode — degrade to in-memory */ }
}
function clearSession(gid) {
  try { sessionStorage.removeItem(sessKey(gid)); } catch { /* ignore */ }
}

// ---------------------------------------------------------------------------
// Module state for the view layer.
// ---------------------------------------------------------------------------
let state = null;             // current game state (null in the lobby)
let pollHandle = null;        // polling handle or null
let appendLog = null;         // log appender from setupLog()
let tickerStop = null;
let pendingJustCreated = null; // gameId whose creator just got 201 (token notice)
let prefillGameId = null;     // bounced-to-lobby prefill (§7.1 step 1 / S4)

const el = {
  topbar: document.getElementById("topbar"),
  gameView: document.getElementById("game-view"),
  felt: document.getElementById("felt"),
  seats: document.getElementById("seats"),
  center: document.getElementById("center"),
  console: document.getElementById("console"),
  handRow: document.getElementById("hand-row"),
  hcards: document.getElementById("hcards"),
  logPanel: document.getElementById("log-panel"),
  logBody: document.getElementById("log-body"),
  newEntries: document.getElementById("new-entries"),
  followBtn: document.getElementById("log-follow"),
  lobbyRoot: document.getElementById("lobby-root"),
  gameChip: document.getElementById("game-chip"),
  forfeitBtn: document.getElementById("forfeit-btn"),
  viewerName: document.getElementById("viewer-name"),
  connDot: document.getElementById("conn-dot"),
  connLabel: document.getElementById("conn-label"),
  connText: document.getElementById("conn-text"),
  bannerStrip: document.getElementById("banner-strip"),
};

// ---------------------------------------------------------------------------
// Routing (ui-spec §2): #/ , #/?join , #/g/{gameId}. The hash is a label only.
// ---------------------------------------------------------------------------

function route() {
  const h = location.hash || "#/";
  if (h.startsWith("#/g/")) enterGame(h.slice(4));
  else showLobby(h === "#/?join");
}

function leaveCurrentView() {
  if (pollHandle) { pollHandle.stop(); pollHandle = null; }
  closeAllModals();
  document.getElementById("overlay-root").innerHTML = "";
  if (tickerStop) { tickerStop(); tickerStop = null; }
  el.topbar.hidden = true;
  el.gameView.hidden = true;
  el.lobbyRoot.style.display = "";
  state = null;
  setCurrentState(null);
}

function showLobby(joinMode) {
  leaveCurrentView();
  const prefill = joinMode ? prefillGameId : null;
  prefillGameId = null;
  el.lobbyRoot.innerHTML = "";
  renderLobby(el.lobbyRoot, { joinPrefill: prefill });
}

// ---------------------------------------------------------------------------
// S3 bootstrap (ui-spec §7.1) — the normative entry sequence.
// ---------------------------------------------------------------------------

async function enterGame(gameId) {
  leaveCurrentView();
  const sess = loadSession(gameId);
  if (!sess || !sess.token) {
    prefillGameId = gameId;      // no key in this tab → join screen with the id pre-filled
    location.hash = "#/?join";
    return;
  }

  state = createState(gameId, sess);
  setCurrentState(state);
  if (pendingJustCreated === gameId) { state.justCreated = true; pendingJustCreated = null; }

  appendLog = setupLog(el.logBody, el.newEntries, el.followBtn);

  // Step 2 — snapshot: the seed truth for seat state.
  let snap;
  try {
    snap = await apiGet(state, `/games/${gameId}`);
  } catch (e) {
    if (e instanceof ApiError) {
      if (e.status === 401) { showAuthError(state, e); return; }        // S4 overlay
      if (e.status === 404) { showGameGone(); return; }                 // U17 one-shot modal
      rejectToast(e.code, e.message);
    } else {
      infoToast("Could not reach the table — check your connection.");
    }
    prefillGameId = gameId;
    location.hash = "#/?join";
    return;
  }
  seedFromSnapshot(snap) // state comes from _current;
  saveSession(gameId, { gameId, seatId: snap.your_seat_id, token: state.token }); // resolved seat id

  el.topbar.hidden = false;
  el.gameView.hidden = false;
  el.lobbyRoot.style.display = "none";
  resetBannerDismissals();
  wireTopbar();

  // Step 3 — a finished game renders final + overlay and never polls (§7.1).
  if (state.status !== "in_progress") {
    fullRender();
    guarded("game-over overlay", () => showGameOver(state));
    return;
  }

  // Step 4 — replay the FULL stream: the log is built here, and private events
  // of this seat (exchange reveals, flip requests) are recovered for display.
  // Seat state is NOT re-applied from events — the snapshot is seed truth (§7.1).
  try {
    const res = await apiGet(state, `/games/${gameId}/events?after=0`);
    applyEvents(res.events || [], { forLog: true, appendRows: true });
  } catch (e) {
    if (!(e instanceof ApiError)) throw e;
    // Replay is best-effort on bootstrap: the poll loop below starts from the
    // snapshot's cursor and the log simply starts where it could (§7.1 U7).
  }
  mergeReplayedWindow(state);

  fullRender();

  // Step 5 — start the long-poll loop (single-flight, §7.2).
  pollHandle = startPolling(state, {
    // Live batches append their rows to the log (the log is built by the
    // bootstrap replay and grows with every accepted batch — §4.6/§7.2).
    onBatch: (evts) => { applyEvents(evts, { appendRows: true }); },
    onConn: () => updateConn(state, el),
    resnapshot: async () => recoveryResnapshot(),
    onFailToast: () => infoToast("Connection lost — retrying…"), // >3 consecutive failures (§7.2)
    onUnauthorized: (e) => { if (pollHandle) pollHandle.stop(); showAuthError(state, e); },
    onGameGone: () => { if (pollHandle) pollHandle.stop(); showGameGone(); },
  });

  tickerStop = startTickers(() => state, () => {});

  // Step 6 — render once; open any decision surface implied by current.phase.
  fullRender();
  guarded("token notice", () => { if (state.justCreated) showTokenNotice(state); }); // §6.3 over the first paint
  guarded("decision surfaces", () => ensureSurfaces(state));
}

/**
 * §7.4/U7 recovery resnapshot: re-seed from a fresh snapshot + one quiet
 * replay (restores open-window shape, vote state, and private decision draws).
 * The log is NEVER cleared — it keeps its own rows; the poll cursor advances
 * to the snapshot's last_event_seq so nothing is double-applied.
 */
async function recoveryResnapshot() {
  const snap = await apiGet(state, `/games/${state.gameId}`);
  seedFromSnapshot(snap) // state comes from _current;
  try {
    const res = await apiGet(state, `/games/${state.gameId}/events?after=0`);
    applyEvents(res.events || [], { forLog: true, appendRows: false }); // never duplicate the log (U7)
  } catch { /* snapshot alone is enough truth */ }
  mergeReplayedWindow(state);
  state.lastSeq = Math.max(state.lastSeq, snap.last_event_seq || 0);
  return true;
}

// ---------------------------------------------------------------------------
// Event batches (§7.2 apply-then-repoll / §7.3 reducer / §12 rows).
//   forLog=true    — structural deltas skipped (snapshot is seed truth); used
//                     by bootstrap replay and recovery resnapshots.
//   appendRows     — bootstrap replay BUILDS the log; recovery resnapshots must
//                     NOT duplicate visible rows (the log is append-only truth, U7).
// ---------------------------------------------------------------------------

function applyEvents(events, opts) {
  const sorted = [...events].sort((a, b) => a.seq - b.seq); // defensive: seq ascending always
  let closedInBatch = null;
  // A pending decision object survives the batch even when a later turn_start in
  // the SAME batch clears state.decision (the reduce mutates it in place and sets
  // .settled) — settleDecisionModal needs that reference afterwards (§9.2).
  const decBefore = state.decision;
  for (const ev of sorted) {
    reduce(state, ev, opts);
    if (opts.appendRows) appendLog(ev, rowForEvent(ev, state));
    if (ev.seq > state.lastSeq) state.lastSeq = ev.seq;
    if (ev.type === "window_closed") closedInBatch = ev;
  }
  if (opts.forLog) return; // log/memory pass only — no transitions, no re-render

  // A window_closed in this batch transitions an open response modal to its
  // closed/stale-adjacent state (§7.5 layer 3, §8.1 S3).
  if (closedInBatch) {
    guarded("window modal close transition", () => transitionWindowModalOnClose(state, closedInBatch.window_id));
  }

  fullRender();
  guarded("decision surfaces", () => ensureSurfaces(state));

  // A submitted decision that was settled by this batch but whose state slot was
  // already cleared by a same-batch turn_start (e.g. coup → flip → next turn in
  // one burst): settle its modal here or it would sit locked forever (§9.2).
  // NOTE: no `submitted` gate — the choice may have been accepted from another
  // client of the same seat (or the batch raced our own POST in flight), and a
  // settled decision object still has to close its modal (§9.2). The copy picks
  // between "card is face-up" and "the table chose for you" via d.settled.
  if (decBefore && decBefore.settled && state.decision !== decBefore) {
    guarded("decision modal settlement", () => settleDecisionModal(state, decBefore));
  }

  // Spectator toast — once, when the viewer's own seat is eliminated (§11).
  if (state.eliminatedMe && !state.outToastShown) {
    state.outToastShown = true;
    infoToast("You are out of the game — spectating.");
  }

  // Terminal: the overlay must appear even though polling continues harmlessly.
  if (state.status !== "in_progress") guarded("game-over overlay", () => showGameOver(state));
}

// ---------------------------------------------------------------------------
// Full re-render of every state-driven region (renderers are pure over state).
// ---------------------------------------------------------------------------

/**
 * Render-layer guard: a throwing renderer or surface handler must log to the
 * console and leave the rest of the table usable — never silently abort the
 * bootstrap, the batch applier, or the poll loop behind it. State is untouched;
 * the next tick (poll batch / 250ms ticker) retries the same work.
 */
function guarded(label, fn) {
  try { fn(); } catch (e) { console.error(`coup: ${label} failed — state kept, will retry`, e); }
}

function fullRender() {
  if (!state) return;
  guarded("full render pass", () => {
    renderTable(state, el.felt, el.seats, el.center);
    renderHand(state, el.handRow, el.hcards);
    renderConsole(state, el.console, (body) => submitConsoleAction(state, body));
    renderTopbar(state, { gameChipEl: el.gameChip, viewerEl: el.viewerName, forfeitBtnEl: el.forfeitBtn });
    updateConn(state, el);
    renderBanners(state, el.bannerStrip, isBallotModalOpen());
  });
}

let topbarWired = false;
function wireTopbar() {
  if (topbarWired) return;
  topbarWired = true;
  el.gameChip.addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(state ? state.gameId : ""); infoToast("id copied"); }
    catch { infoToast("copy failed — the id is in the address bar"); }
  });
  el.forfeitBtn.addEventListener("click", () => { if (state) openForfeitInitiation(state); });
}

// Cross-module events (decide.js overlays ask to go home).
window.addEventListener("coup:back-to-lobby", () => {
  if (state) clearSession(state.gameId); // §10.6: that is the only action on game over
  location.hash = "#/";
});
window.addEventListener("coup:rejoin", (e) => {
  prefillGameId = e.detail && e.detail.gameId;
  location.hash = "#/?join";
});

// ---------------------------------------------------------------------------
// Composition wiring + boot.
// ---------------------------------------------------------------------------

setLobbyHooks({
  onCreated(gameId, sess) {
    saveSession(gameId, sess);
    pendingJustCreated = gameId; // §6.3: token notice over S3's first paint
    location.hash = "#/g/" + gameId;
  },
  onJoin(gid, tok) {
    saveSession(gid, { gameId: gid, seatId: null, token: tok }); // seat id resolved at bootstrap
    location.hash = "#/g/" + gid;
  },
});

setDecideHooks({
  // Surface-aware refresh: repaint the table AND re-run surface detection.
  // The token notice's close button relies on this to surface a window/decision
  // modal that was waiting behind it (§6.3).
  refresh: () => { if (state) { fullRender(); guarded("decision surfaces", () => ensureSurfaces(state)); } },
  resnapshot: recoveryResnapshot,
});

window.addEventListener("hashchange", route);
route();

// ===========================================================================
// rules_modal.js — Interactive Coup Rules & Roles Cheatsheet Modal
// Provides an accessible dialog with complete character reference,
// action costs, block relationships, bluffing & challenge rules.
// Keyboard accessible (Esc to close, focus trapped and restored).
// ===========================================================================

import { playClick } from "./sound.js";

let activeModalEl = null;
let lastActiveElement = null;

/**
 * Checks if the rules modal is currently open.
 */
export function isRulesModalOpen() {
  return activeModalEl !== null && document.contains(activeModalEl);
}

/**
 * Closes the rules modal if open and restores focus.
 */
export function closeRulesModal() {
  if (!activeModalEl) return;
  const el = activeModalEl;
  activeModalEl = null;
  el.remove();
  document.removeEventListener("keydown", handleKeydown, true);

  // Return focus to the trigger element
  if (lastActiveElement && typeof lastActiveElement.focus === "function") {
    try {
      lastActiveElement.focus();
    } catch {}
  }
  lastActiveElement = null;
}

/**
 * Toggles the rules modal.
 */
export function toggleRulesModal() {
  if (isRulesModalOpen()) {
    closeRulesModal();
  } else {
    openRulesModal();
  }
}

/**
 * Handles keyboard events inside and outside the modal (Esc to close, Tab trap).
 */
function handleKeydown(e) {
  if (!activeModalEl) return;

  if (e.key === "Escape") {
    e.preventDefault();
    e.stopPropagation();
    playClick();
    closeRulesModal();
    return;
  }

  if (e.key === "Tab") {
    const focusables = [...activeModalEl.querySelectorAll('button:not([disabled]), [href], input, [tabindex]:not([tabindex="-1"])')]
      .filter((el) => el.offsetParent !== null || el === document.activeElement);

    if (!focusables.length) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];

    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }
}

/**
 * Opens the interactive Rules & Roles cheatsheet modal.
 */
export function openRulesModal() {
  if (isRulesModalOpen()) return;

  lastActiveElement = document.activeElement;

  const root = document.getElementById("rules-root") || document.body;

  const wrap = document.createElement("div");
  wrap.className = "modal-root rules-modal-wrap";
  wrap.setAttribute("role", "dialog");
  wrap.setAttribute("aria-modal", "true");
  wrap.setAttribute("aria-labelledby", "rules-dialog-title");
  wrap.setAttribute("aria-describedby", "rules-dialog-desc");

  wrap.innerHTML = `
    <div class="backdrop" id="rules-backdrop" aria-hidden="true"></div>
    <section class="modal-panel rules-modal">
      <header class="m-head">
        <button type="button" class="m-close" id="rules-close-x" aria-label="Close rules cheatsheet (Escape)">✕</button>
        <span class="modal-kicker">OFFICIAL REFERENCE &amp; CHEATSHEET</span>
        <h2 class="modal-title" id="rules-dialog-title">Coup: Rules of Engagement</h2>
        <div class="modal-sub" id="rules-dialog-desc">Claims are cheap, challenges cost. In Coup, you can claim any character — unless challenged.</div>
      </header>

      <div class="rules-body">
        <!-- Character Roles Table -->
        <section aria-labelledby="roles-heading">
          <h3 class="rules-section-title" id="roles-heading">
            <span>🎭</span> Character Roles &amp; Actions
          </h3>
          <div class="rules-table-wrap">
            <table class="rules-table">
              <thead>
                <tr>
                  <th scope="col">Character</th>
                  <th scope="col">Action</th>
                  <th scope="col">Cost / Gain</th>
                  <th scope="col">Blocks / Counteractions</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td><span class="role-badge duke">Duke</span></td>
                  <td><strong>Tax</strong>: Collect from Treasury. Not blockable.</td>
                  <td><strong style="color:var(--gold-bright)">+3 Coins</strong></td>
                  <td><strong>Blocks Foreign Aid</strong></td>
                </tr>
                <tr>
                  <td><span class="role-badge assassin">Assassin</span></td>
                  <td><strong>Assassinate</strong>: Choose target to lose 1 influence card.</td>
                  <td><strong style="color:var(--red-bright)">-3 Coins</strong></td>
                  <td><em style="color:var(--ink-faint)">None</em></td>
                </tr>
                <tr>
                  <td><span class="role-badge captain">Captain</span></td>
                  <td><strong>Steal</strong>: Take 2 coins from another player.</td>
                  <td><strong style="color:var(--gold-bright)">+2 Coins</strong></td>
                  <td><strong>Blocks Steal</strong></td>
                </tr>
                <tr>
                  <td><span class="role-badge ambassador">Ambassador</span></td>
                  <td><strong>Exchange</strong>: Draw 2 cards from Court Deck, pick which to keep, return 2.</td>
                  <td><em style="color:var(--ink-faint)">Free</em></td>
                  <td><strong>Blocks Steal</strong></td>
                </tr>
                <tr>
                  <td><span class="role-badge contessa">Contessa</span></td>
                  <td><em style="color:var(--ink-faint)">No action</em></td>
                  <td><em style="color:var(--ink-faint)">—</em></td>
                  <td><strong>Blocks Assassination</strong></td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>

        <!-- General Turn Actions -->
        <section aria-labelledby="general-heading">
          <h3 class="rules-section-title" id="general-heading">
            <span>⚔️</span> General Turn Actions
          </h3>
          <div class="rules-table-wrap">
            <table class="rules-table">
              <thead>
                <tr>
                  <th scope="col">Action</th>
                  <th scope="col">Effect</th>
                  <th scope="col">Cost / Gain</th>
                  <th scope="col">Blockable / Challengable</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td><strong>Income</strong></td>
                  <td>Take 1 coin from the Treasury.</td>
                  <td><strong style="color:var(--gold-bright)">+1 Coin</strong></td>
                  <td>Cannot be blocked. Cannot be challenged.</td>
                </tr>
                <tr>
                  <td><strong>Foreign Aid</strong></td>
                  <td>Take 2 coins from the Treasury.</td>
                  <td><strong style="color:var(--gold-bright)">+2 Coins</strong></td>
                  <td><strong>Blockable by Duke</strong>. Cannot be challenged.</td>
                </tr>
                <tr>
                  <td><strong>Coup</strong></td>
                  <td>Target player must immediately flip 1 influence card face-up.</td>
                  <td><strong style="color:var(--red-bright)">-7 Coins</strong></td>
                  <td><strong>Cannot be blocked</strong>. Cannot be challenged.</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div class="rules-callout" style="margin-top: 10px;">
            <strong>Mandatory Coup Rule:</strong> If you start your turn with <strong>10 or more coins</strong>, you have no choice—you MUST declare a Coup against an opponent.
          </div>
        </section>

        <!-- Bluffing & Challenges Grid -->
        <section aria-labelledby="challenges-heading">
          <h3 class="rules-section-title" id="challenges-heading">
            <span>⚖️</span> Bluffing &amp; Challenges
          </h3>
          <div class="rules-grid">
            <div class="rules-card">
              <h4>When Challenged</h4>
              <ul>
                <li><strong>If you were Telling the Truth:</strong> Reveal the claimed card to prove it. The challenger <strong>loses 1 card</strong> (flips it face-up). You shuffle your card back into the Court Deck and draw a fresh replacement. Your action/block succeeds.</li>
                <li><strong>If you were Bluffing:</strong> You fail to prove the claim and <strong>lose 1 card</strong> (flip it face-up). Your action or block fails.</li>
              </ul>
            </div>
            <div class="rules-card">
              <h4>Counteractions &amp; Double-Loss</h4>
              <ul>
                <li><strong>Counteractions (Blocks):</strong> When another player declares an action, an eligible character may declare a block. Blocks can be challenged too!</li>
                <li><strong>The Assassin Trap:</strong> If you challenge an Assassin claim and lose the challenge, you lose 1 card from the failed challenge AND 1 card from the assassination itself!</li>
                <li><strong>Winning:</strong> The last player with unrevealed influence wins.</li>
              </ul>
            </div>
          </div>
        </section>
      </div>

      <footer class="m-foot">
        <span style="font-size: 11.5px; color: var(--ink-faint); margin-right: auto; align-self: center;">
          Keyboard: Press <kbd style="font-family:var(--font-mono);background:rgba(255,255,255,0.06);padding:2px 5px;border-radius:3px">?</kbd> or <kbd style="font-family:var(--font-mono);background:rgba(255,255,255,0.06);padding:2px 5px;border-radius:3px">H</kbd> to toggle, <kbd style="font-family:var(--font-mono);background:rgba(255,255,255,0.06);padding:2px 5px;border-radius:3px">Esc</kbd> to close
        </span>
        <button type="button" class="btn btn-primary" id="rules-done-btn">Back to Table</button>
      </footer>
    </section>
  `;

  root.appendChild(wrap);
  activeModalEl = wrap;

  // Add listeners
  document.addEventListener("keydown", handleKeydown, true);

  wrap.querySelector("#rules-backdrop").addEventListener("click", () => {
    playClick();
    closeRulesModal();
  });
  wrap.querySelector("#rules-close-x").addEventListener("click", () => {
    playClick();
    closeRulesModal();
  });
  wrap.querySelector("#rules-done-btn").addEventListener("click", () => {
    playClick();
    closeRulesModal();
  });

  // Focus the "Back to Table" button for immediate keyboard operation
  setTimeout(() => {
    const btn = wrap.querySelector("#rules-done-btn");
    if (btn) btn.focus();
  }, 10);
}

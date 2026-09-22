# Coup — Rules Specification

This document specifies **Coup (2012)** by Rikki Tahta (Indie Boards & Cards / La Mame Games),
the social-deduction card game this project recreates. It is NOT the unrelated 2018 We The
People board game also titled *Coup* — do not confuse them.

Sources: BGG wiki description for boardgame/131357 (based on the printed manual, incl.
designer comments) and the Wikipedia article "Coup (card game)". Where sources leave gaps,
they are listed in [Open questions](#open-questions) with a suggested default.

## Objective & Win Condition

You are head of a family in an Italian city-state; influence is held as face-down character
cards. Be the last player with influence: you win when every other player has both of their
character cards turned face-up (eliminated).

## Components

- 15 character cards — **three copies** of each of five characters: Duke, Assassin, Contessa, Captain, Ambassador
- Coins (a common treasury pool; 50 in the physical game)
- Players: 2–6 (best with 5), ~15 min

## Setup

1. Shuffle the 15-card deck and deal **two face-down cards to each player**. Each player sees only their own cards.
2. The remaining cards form the **Court Deck** in the centre (face down).
3. Each player starts with **2 coins**.

## State model

- `Player`: coins, list of face-down cards (secret), list of face-up cards (lost, unusable)
- `CourtDeck`: face-down pool; only its top is ever drawn, and revealed cards are shuffled back in
- A player with two face-up cards is eliminated. Eliminated players keep their coins but take no turns.

## Turn structure

Play proceeds in fixed seat order (skipping eliminated players). On your turn you perform
**exactly one** action from the list below, then the turn passes to the next player.

### Actions anyone may take (no character required)

| Action | Effect |
|---|---|
| Income | Take 1 coin from the treasury |
| Foreign Aid | Take 2 coins from the treasury. May be blocked by a counterclaim of **Duke** (see below) |
| Coup | Pay 7 coins; an opponent is forced to lose one influence (one of their face-down cards, their choice, turned face-up). **If you have 10 or more coins, you must take this action** |

### Character actions (claimed, whether or not you actually hold the character)

You may claim any character for its action regardless of what is in your hand. Claims are
challengeable — see [Claims & challenges](#claims--challenges).

| Character | Action | Effect | Can be used defensively to block |
|---|---|---|---|
| Duke | Tax | Take 3 coins from the treasury | Foreign Aid |
| Assassin | Assassinate | Pay 3 coins; an opponent is forced to lose one influence (one face-down card, their choice, turned face-up) | — |
| Contessa | — (no offensive action) | — | Assassinate |
| Captain | Steal | Take 2 coins from another player | Steal (target's coins) |
| Ambassador | Exchange | Draw the top two cards of the Court Deck; you may swap one drawn card with one of your face-down cards, then return two cards to the Court Deck (order not visible to others) | Steal (target's coins) |

### Defensive counteractions

A defensive use (blocking an attack on you / a general action) is itself a character claim and
resolves under the same challenge rules. Only one block is needed: if it succeeds, the attacking
action fails with no effect.

## Claims & challenges

This is the core bluff mechanic:

1. A player performs or defends with a claimed character action. It **succeeds immediately** unless challenged.
2. Any other player may challenge the claim (whether or not they are the target).
3. The claimant must **reveal that exact character** from their face-down cards.
   - **Cannot / chooses not to reveal** → the claimant loses one influence: one of their remaining
     face-down cards, their choice, is turned face-up in front of them (publicly known).
   - **Reveals it** → the challenger loses one influence the same way. A player may challenge only once per action; if multiple players challenge, resolve first-come-first-served (see open questions).
4. On a successful reveal, the revealed card is **shuffled into the Court Deck and its owner draws
   one replacement** face-down card (so they may or may not regain the same character).

Notes:

- Face-up cards are public knowledge to all players, cannot be used for any action or counterclaim, and do not leave the game.
- Revealing a character on a successful challenge is the *only* way anyone learns another player's true cards. AI agents should treat revealed/face-up cards as common knowledge and face-down cards as private information with beliefs to track.

## Elimination & end of turn effects

- Whenever a player loses influence (failed claim, failed challenge, Coup, or Assassinate),
  they choose which of their remaining face-down cards goes face-up. (Interpretation — see open questions.)
- When both cards are face-up the player is eliminated; turns skip them.
- Game ends immediately when only one player has any influence left.

## Suggested action resolution flow (for the engine)

```
turn(player)
  → choose action
      ├─ Income / Tax: gain coins            [Tax requires successful-or-unchallenged Duke claim]
      ├─ Foreign Aid: +2, unless blocked by unchallenged-successful Duke counterclaim
      ├─ Coup: pay 7; target discards a card  (mandatory at ≥10 coins)
      ├─ Steal / Assassinate: claimed action; target may counterclaim (Steal→Captain|Ambassador,
      │                        Assassinate→Contessa); then challenge window
      └─ Exchange: draw 2 from Court Deck, optional swap, return 2
  → challenge window for the claimed portion (if any)
      → resolve reveal-or-penalty; on success shuffle revealed card back + redraw
```

## Open questions

Not fully pinned down by the sources above; decide and record the ruling here before implementing:

1. **Who may counterclaim?** "Block someone from stealing *your* coins" implies only the target
   may block Steal/Assassinate; Duke-vs-Foreign-Aid has no obvious target (blocker is a bystander?).
   Suggested default: Steal/Assassinate — target only; Foreign Aid — any player.
2. **Coup at exactly 10 coins** is mandatory ("if you have ten coins, you must take this action");
   the "≥10" reading used above is a superset. Pick one and encode it.
3. **Card lost to Coup/Assassinate:** whose choice — the victim's or the attacker's?
   Suggested default: victim chooses (mirrors the challenge penalty).
4. **Court Deck exhaustion:** if Exchange draws the last cards, presumably Exchange is simply
   unavailable; game continues to elimination. Confirm.
5. **Multiple challengers / timing of counterclaims in a digital room** — define the request/response
   ordering for AI agents (suggested: target gets first response, then any other player may challenge).
6. **Starting turn order** — pick randomly or fixed seat order; not specified by the sources.

## Out of scope (expansions exist but are not part of this spec)

- *Coup: Reformation* (2014): factions, Inquisitor role, 5 cards per role, up to 10 players
- *Coup: Rebellion Guatemala 1954*: variable deck of 25 characters
- Promo cards: Speculator, Bureaucrat, Jester, Socialist

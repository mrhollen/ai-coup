package domain

import "math/rand"

// CourtDeck is the face-down pool of cards in the middle of the table.
//
// Size invariant (spec §3.2.1): for a game with n seats, |deck| = 15 − 2n at
// every moment — Exchange draws two and returns two (net zero) and a
// successful challenge resolution shuffles one card back in while its owner
// draws one replacement (net zero). With six players the deck never drops
// below three, so exhaustion is impossible by construction; no code path may
// check for it.
type CourtDeck struct {
	order []Card // index 0 is the top of the deck
}

// NewCourtDeck builds a shuffled Court Deck from the given cards. The full
// shuffle also serves as the dealing shuffle in game setup (rules.md Setup).
func NewCourtDeck(cards []Card, rng *rand.Rand) *CourtDeck {
	order := make([]Card, len(cards))
	copy(order, cards)
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	return &CourtDeck{order: order}
}

// Size returns the number of cards in the Court Deck.
func (d *CourtDeck) Size() int { return len(d.order) }

// Cards returns the current deck composition, top first (index 0 is the top).
// This is read-only access for the debug surface only (§11 GET /debug: "actual
// deck composition … for debugging"); the engine itself uses DrawTop and
// ReturnShuffled exclusively. The copy means the caller can never mutate the
// deck through it, so no invariant or lock discipline changes.
func (d *CourtDeck) Cards() []Card {
	cards := make([]Card, len(d.order))
	copy(cards, d.order)
	return cards
}

// DrawTop removes and returns the top n cards, in draw order. Callers rely on
// the size invariant; drawing more than is present is a caller bug.
func (d *CourtDeck) DrawTop(n int) []Card {
	cards := make([]Card, n)
	copy(cards, d.order[:n])
	d.order = d.order[n:]
	return cards
}

// ReturnShuffled returns the given set of cards to the Court Deck with hidden
// order: each card is inserted at an independent random position (spec §3.1 —
// revealed cards are shuffled back in; returned exchange cards come back as a
// set whose order no one can see). The net size change is +len(cards).
func (d *CourtDeck) ReturnShuffled(cards []Card, rng *rand.Rand) {
	for _, c := range cards {
		pos := rng.Intn(len(d.order) + 1)
		d.order = append(d.order, Card{})
		copy(d.order[pos+1:], d.order[pos:])
		d.order[pos] = c
	}
}

// Package domain is the pure Coup (2012) game engine.
//
// It contains no I/O, no wall-clock time, no network access and no use of the
// sync package: all state mutation happens synchronously inside one call, and
// concurrency is enforced by the caller holding one mutex per game (spec §9).
// Durations are carried as plain integer values; deadlines are never computed
// here (the application layer owns the Clock port).
package domain

// Character is one of the five characters in Coup (2012) (spec §3.1).
type Character int

// The five characters, each with exactly three copies in the 15-card deck.
// The zero value is deliberately not a character so that an unset claim is
// distinguishable from Duke.
const (
	CharacterNone Character = iota // zero value: no character
	Duke
	Assassin
	Contessa
	Captain
	Ambassador
)

// allCharacters is the complete set of characters.
func allCharacters() []Character {
	return []Character{Duke, Assassin, Contessa, Captain, Ambassador}
}

// String returns the canonical lowercase name used in events and on the wire
// (spec §10).
func (c Character) String() string {
	switch c {
	case Duke:
		return "duke"
	case Assassin:
		return "assassin"
	case Contessa:
		return "contessa"
	case Captain:
		return "captain"
	case Ambassador:
		return "ambassador"
	default:
		return "invalid_character"
	}
}

// CharacterFromName parses a canonical character name (as used on the wire).
func CharacterFromName(name string) (Character, bool) {
	switch name {
	case "duke":
		return Duke, true
	case "assassin":
		return Assassin, true
	case "contessa":
		return Contessa, true
	case "captain":
		return Captain, true
	case "ambassador":
		return Ambassador, true
	default:
		return 0, false
	}
}

// Card identifies a physical card by character and copy index (0..2). The
// copy index never leaves the server and is irrelevant to the rules: cards of
// one character are interchangeable (spec §3.1).
type Card struct {
	Character Character
	Copy      int
}

// FullDeck returns the complete 15-card deck of the base game: three copies of
// each of the five characters.
func FullDeck() []Card {
	cards := make([]Card, 0, 15)
	for _, c := range allCharacters() {
		for copyIdx := 0; copyIdx < 3; copyIdx++ {
			cards = append(cards, Card{Character: c, Copy: copyIdx})
		}
	}
	return cards
}

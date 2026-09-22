package domain

// SeatType distinguishes human players from server-managed LLM seats.
type SeatType int

const (
	SeatHuman SeatType = iota
	SeatLLM
)

// String returns the wire value ("human" | "llm").
func (t SeatType) String() string {
	if t == SeatHuman {
		return "human"
	}
	return "llm"
}

// Seat is one player at the table (spec §3.1). FaceDown holds exactly two
// cards while the seat is active by normal play and shrinks only via flips;
// face-up cards never leave the game and are public knowledge (§3.2.4).
type Seat struct {
	ID       string
	Name     string
	Type     SeatType
	Coins    int // ≥ 0, unbounded on top (no treasury cap, §3.2.5)
	FaceDown []Card
	FaceUp   []Card

	// Forfeited marks elimination by forfeit vote (§7): the seat's remaining
	// face-down cards are never revealed — they stay in FaceDown as unknowns
	// for belief tracking and keep counting toward invariant §3.2.2, but the
	// seat has no influence left and is excluded from everything else.
	Forfeited bool
}

// Active reports whether the seat may act: it holds influence and was not
// forfeited. An eliminated seat (both cards face-up) has empty FaceDown and is
// inactive (§3.2.3).
func (s *Seat) Active() bool { return !s.Forfeited && len(s.FaceDown) > 0 }

// Eliminated reports whether the seat has no influence left: both cards are
// face-up, or the seat was eliminated by forfeit vote.
func (s *Seat) Eliminated() bool { return s.Forfeited || len(s.FaceDown) == 0 }

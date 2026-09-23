package clock

import (
	"crypto/rand"
	"encoding/hex"

	"forgejo.lan/mrhollen/ai-coup/domain"
)

// RealIDGen is the production IDGen of spec §13: kind prefix + 12 hex chars of
// crypto-randomness. Kinds used by the system: "g" game, "s" seat, "w" window,
// "d" decision, "v" vote (domain mints the latter three internally).
type RealIDGen struct{}

var _ domain.IDGen = (*RealIDGen)(nil)

// NewRealIDGen builds a RealIDGen.
func NewRealIDGen() *RealIDGen { return &RealIDGen{} }

// New implements the IDGen port. An unknown kind still gets the stable
// "<kind>_" + 12-hex shape so IDs stay opaque and format-uniform.
func (g *RealIDGen) New(kind string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is a process-level fault; fall back to an
		// empty suffix rather than panic in the middle of a request.
		return kind + "_000000000000"
	}
	return kind + "_" + hex.EncodeToString(b[:])
}

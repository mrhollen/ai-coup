package llm

import (
	"context"
	"fmt"

	"forgejo.lan/mrhollen/ai-coup/application"
)

// UnavailableDriver is a build-level placeholder for LLM seats that reports
// CanDriveLLM() == false (the real heuristic/openai drivers both claim the
// capability; this stub remains for test compositions and builds that must
// reject LLM seats fast). CreateGame then rejects any request containing LLM
// seats at creation time with a clear message, while human-only games run
// end-to-end unaffected (§6.2). If Decide is ever reached anyway — a wiring
// bug — it fails loudly instead of silently degrading to fallbacks.
type UnavailableDriver struct {
	mode string // the COUP_DRIVER_MODE value ("heuristic" | "openai")
}

// NewUnavailableDriver builds a stub reporting the configured driver mode so
// debug output and log lines name what was asked for.
func NewUnavailableDriver(mode string) *UnavailableDriver { return &UnavailableDriver{mode: mode} }

// Name implements application.MoveDriver. It reports the configured mode —
// the driver that mode would have bound — not "unavailable", so operators see
// which knob to expect real behaviour from later.
func (u *UnavailableDriver) Name() string { return u.mode }

// CanDriveLLM implements application.LLMCapable: this build cannot drive LLM
// seats (§6.2).
func (u *UnavailableDriver) CanDriveLLM() bool { return false }

// Decide implements application.MoveDriver and always fails: a game created in
// this build can only contain human seats, so any call here is a wiring error.
func (u *UnavailableDriver) Decide(ctx context.Context, in application.DecisionInput) (application.Candidate, error) {
	return application.Candidate{}, fmt.Errorf(
		"llm driver %q is not available in this build: LLM seats cannot be driven (a human-only game was expected)", u.mode)
}

var _ application.MoveDriver = (*UnavailableDriver)(nil)
var _ application.LLMCapable = (*UnavailableDriver)(nil)

// Package llm hosts the MoveDriver adapters (spec §2.1/§6.2). This slice ships
// the scripted fake driver (tests only) and the unavailable stub that stands
// in for the heuristic/openai drivers, which arrive in later slices.
package llm

import (
	"context"
	"sync"

	"forgejo.lan/mrhollen/ai-coup/application"
)

// Step is one scripted answer the fake driver pops for a seat. Exactly one of
// Candidate / Err / BlockUntilCancel should be set:
//   - Candidate: return it (nil with everything else nil = terminal queue error)
//   - Err: return it immediately
//   - BlockUntilCancel: block until the decision context is cancelled, then
//     return the ctx error — used to assert cancellation properties (§5.2.6)
//
// Gate (optional test barrier): when non-nil, Decide waits for it to close
// BEFORE acting on the step; a concurrent ctx cancellation still wins.
type Step struct {
	Candidate        *application.Candidate
	Err              error
	BlockUntilCancel bool
	Gate             chan struct{}
}

// CandidateStep is sugar for a plain candidate step.
func CandidateStep(c application.Candidate) Step { return Step{Candidate: &c} }

// GatedStep holds the given candidate until the gate closes (test barrier).
func GatedStep(gate chan struct{}, c application.Candidate) Step {
	return Step{Candidate: &c, Gate: gate}
}

// BlockStep is sugar for a cancel-blocking step.
func BlockStep() Step { return Step{BlockUntilCancel: true} }

// ErrStep is sugar for an immediate-error step.
func ErrStep(err error) Step { return Step{Err: err} }

// FakeDriver is the scripted-queue MoveDriver of spec §6.2 (tests only). Each
// seat has its own FIFO of steps so concurrent window fan-out stays
// deterministic per seat; when a seat's queue is empty the driver either
// returns a terminal error (default) or blocks on ctx (EmptyBlock), letting
// tests exercise both "driver gives up" and "driver wedged" liveness paths.
type FakeDriver struct {
	mu    sync.Mutex
	steps map[string][]Step
	// EmptyBlock makes an exhausted queue block on the decision context
	// instead of returning a terminal error (simulates a wedged model).
	EmptyBlock bool

	calls    []application.DecisionInput
	canceled []string // seat IDs whose blocked Decide exited via ctx cancellation
}

var _ application.MoveDriver = (*FakeDriver)(nil)

// NewFakeDriver builds an empty fake driver (terminal-error on empty queues).
func NewFakeDriver() *FakeDriver {
	return &FakeDriver{steps: map[string][]Step{}}
}

// Name implements MoveDriver.
func (f *FakeDriver) Name() string { return "fake" }

// CanDriveLLM implements application.LLMCapable: the fake can drive LLM seats
// with scripted steps, so test compositions may create games containing them.
func (f *FakeDriver) CanDriveLLM() bool { return true }

// Queue appends scripted steps for one seat (FIFO per seat).
func (f *FakeDriver) Queue(seatID string, steps ...Step) {
	f.mu.Lock()
	f.steps[seatID] = append(f.steps[seatID], steps...)
	f.mu.Unlock()
}

// Decide pops the next scripted step for the seat. Blocked steps release only
// on context cancellation, mirroring how a wedged LLM HTTP call behaves under
// §6.4.7. Every input is recorded (Calls) so tests can assert exactly what the
// driver was shown — world view, request shape, and retry feedback.
func (f *FakeDriver) Decide(ctx context.Context, in application.DecisionInput) (application.Candidate, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in)
	var step Step
	if len(f.steps[in.SeatID]) > 0 {
		step = f.steps[in.SeatID][0]
		f.steps[in.SeatID] = f.steps[in.SeatID][1:]
	} else if f.EmptyBlock {
		step = BlockStep()
	}
	f.mu.Unlock()

	if step.Gate != nil {
		select {
		case <-step.Gate:
		case <-ctx.Done():
			f.mu.Lock()
			f.canceled = append(f.canceled, in.SeatID)
			f.mu.Unlock()
			return application.Candidate{}, ctx.Err()
		}
	}
	switch {
	case step.BlockUntilCancel:
		<-ctx.Done()
		f.mu.Lock()
		f.canceled = append(f.canceled, in.SeatID)
		f.mu.Unlock()
		return application.Candidate{}, ctx.Err()
	case step.Err != nil:
		return application.Candidate{}, step.Err
	case step.Candidate != nil:
		return *step.Candidate, nil
	default:
		return application.Candidate{}, errEmptyQueue
	}
}

// Calls returns the recorded Decide inputs in order (assertion surface for
// §6.3/§6.4.4: world view contents and Feedback on retries).
func (f *FakeDriver) Calls() []application.DecisionInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]application.DecisionInput, len(f.calls))
	copy(out, f.calls)
	return out
}

// Canceled returns the seat IDs of blocked Decides that exited via context
// cancellation (assertion surface for §5.2.6).
func (f *FakeDriver) Canceled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.canceled))
	copy(out, f.canceled)
	return out
}

// errEmptyQueue is the terminal error for an exhausted script (§6.2: "returns
// a terminal error if the queue is empty").
type fakeQueueError struct{}

func (fakeQueueError) Error() string { return "fake driver: scripted queue is empty" }

var errEmptyQueue = fakeQueueError{}

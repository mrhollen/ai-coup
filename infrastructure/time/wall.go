// Package clock provides the Clock adapters at spec path infrastructure/time:
// WallClock for production, ManualClock for tests (driven explicitly via
// Advance), plus the real crypto-random IDGen (§13).
package clock

import "time"

// WallClock is the production Clock: wall time and time.After.
type WallClock struct{}

// NewWallClock builds a WallClock.
func NewWallClock() *WallClock { return &WallClock{} }

// Now implements application.Clock.
func (w *WallClock) Now() time.Time { return time.Now() }

// After implements application.Clock.
func (w *WallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

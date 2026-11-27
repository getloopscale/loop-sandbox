// Package clock is a small determinism seam for time. It lives outside the
// runtime/Driver port because telling time is a cross-cutting application concern,
// not part of the sandbox runtime boundary (rules/architecture.md §3).
package clock

import "time"

// Clock abstracts the current time so tests are deterministic (rules/testing.md §6).
type Clock interface {
	Now() time.Time
}

// System is the production clock (UTC).
type System struct{}

// Now returns the current UTC time.
func (System) Now() time.Time { return time.Now().UTC() }

// Fixed is a deterministic clock for tests.
type Fixed struct{ T time.Time }

// Now returns the fixed instant.
func (f Fixed) Now() time.Time { return f.T }

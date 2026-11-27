// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 The LoopScale Authors

package serviceauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrTimestampOutsideWindow is returned when the asserted request
// timestamp is outside the replay-protection tolerance configured in
// the ReplayWindow. RFC default: ±60 seconds.
var ErrTimestampOutsideWindow = errors.New("service-auth: timestamp outside replay window")

// ErrNonceReused is returned by NonceStore.Claim when a previously-seen
// (user_id, nonce) tuple is presented within the TTL. Indicates either
// (a) a client-side bug (nonce generator collision — extraordinarily
// unlikely with 16 random bytes), or (b) a replay attack.
var ErrNonceReused = errors.New("service-auth: nonce reused")

// ReplayWindow validates that an asserted timestamp is within the
// configured tolerance of "now". Used by the API middleware before it
// even attempts signature verification — stale timestamps are cheaper
// to reject than to verify.
//
// Tolerance is symmetric: the boundary check is `|ts - now| <= tolerance`.
// Future timestamps are rejected too because (a) they indicate clock
// skew the operator should know about and (b) without the upper bound
// an attacker who captures a signature could wait for the timestamp to
// "become current" again (clock drift in long-running boxes).
type ReplayWindow struct {
	tolerance time.Duration
}

// NewReplayWindow constructs a window validator with the given
// symmetric tolerance. RFC recommends 60 seconds — wider raises replay
// risk; narrower raises clock-skew failure rate.
func NewReplayWindow(tolerance time.Duration) *ReplayWindow {
	return &ReplayWindow{tolerance: tolerance}
}

// Validate returns nil iff |asserted - now| <= tolerance.
// Wraps ErrTimestampOutsideWindow on failure so callers can audit.
func (r *ReplayWindow) Validate(asserted, now time.Time) error {
	delta := asserted.Sub(now)
	if delta < 0 {
		delta = -delta
	}
	if delta > r.tolerance {
		return fmt.Errorf("%w: delta=%s tolerance=%s", ErrTimestampOutsideWindow, delta, r.tolerance)
	}
	return nil
}

// NonceStore enforces single-use semantics on the (user_id, nonce)
// tuple within a TTL window. The TTL must exceed the ReplayWindow
// tolerance (otherwise a replay arriving within the timestamp window
// but after nonce expiry would slip through).
//
// Two implementations ship:
//   - InMemoryNonceStore: process-local, suitable for single-replica
//     dev/test only. Multi-replica deployments need RedisNonceStore.
//   - RedisNonceStore: shared across replicas via Redis. RFC Q2 decision.
//
// Callers MUST call Claim BEFORE accepting the request. Returning nil
// transfers ownership of (user_id, nonce) to this request — repeating
// the Claim within the TTL returns ErrNonceReused.
type NonceStore interface {
	Claim(ctx context.Context, userID, nonce string) error
}

// InMemoryNonceStore is a process-local NonceStore backed by a
// sync.Map with manual TTL eviction. Suitable for unit tests + single-
// replica dev environments. Production with multiple API replicas MUST
// use a shared store (RedisNonceStore) — otherwise a replay against a
// different replica slips past the local-only cache.
type InMemoryNonceStore struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]time.Time // key = userID + "|" + nonce → expiresAt
	now     func() time.Time     // injectable for tests; defaults to time.Now
}

// NewInMemoryNonceStore constructs a process-local store with the
// given TTL. RFC default: 5 minutes.
func NewInMemoryNonceStore(ttl time.Duration) *InMemoryNonceStore {
	return &InMemoryNonceStore{
		ttl:     ttl,
		entries: make(map[string]time.Time),
		now:     time.Now,
	}
}

// Claim attempts to register (userID, nonce). Returns nil on first use,
// ErrNonceReused if the tuple is currently live in the cache. Expired
// entries are evicted opportunistically.
//
// Concurrency: a sync.Mutex serializes Claim calls. The hot-path is
// O(1) for the happy case; eviction is amortized across Claims that
// observe expired entries. Multi-replica deployments must use a Redis-
// backed store — process-local mutex does not extend across replicas.
func (s *InMemoryNonceStore) Claim(_ context.Context, userID, nonce string) error {
	if userID == "" || nonce == "" {
		return fmt.Errorf("service-auth: claim requires non-empty userID and nonce")
	}
	key := userID + "|" + nonce

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if expiresAt, exists := s.entries[key]; exists {
		if now.Before(expiresAt) {
			return ErrNonceReused
		}
		// Expired — eviction path; fall through to register fresh.
		delete(s.entries, key)
	}

	// Opportunistic GC: every claim sweeps a bounded number of expired
	// entries. Bounded so a flood of unique nonces doesn't block on
	// O(N) eviction.
	const maxSweep = 16
	swept := 0
	for k, exp := range s.entries {
		if swept >= maxSweep {
			break
		}
		if now.After(exp) {
			delete(s.entries, k)
		}
		swept++
	}

	s.entries[key] = now.Add(s.ttl)
	return nil
}

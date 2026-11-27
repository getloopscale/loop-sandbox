// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 The LoopScale Authors

package serviceauth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisNonceStore is the production NonceStore implementation, backed
// by a Redis instance shared across all API replicas.
//
// Per ADR `decisions/2026-05-13-service-auth-ed25519-adoption.md` Q2:
// multi-replica API needs a SHARED nonce store. The InMemoryNonceStore
// works only when one process serves all traffic — in a multi-replica
// deployment, a replay against a different replica would slip past a
// process-local cache. RedisNonceStore uses `SET ... NX EX` which is
// atomic at the Redis server level and survives the cross-replica race.
//
// Key format (wire contract — change forces a flag-day rotation):
//
//	loopscale:service-auth:nonce:<userID>:<nonce>
//
// Value: a sentinel "1" — the store only cares about presence, not
// content. Redis TTL handles eviction without a sweep job.
type RedisNonceStore struct {
	client *redis.Client
	ttl    time.Duration
}

// keyPrefix is the canonical Redis namespace for nonce dedupe.
// Documented in the ADR. Co-locating it here so the test
// `TestRedisNonceStore_KeyFormatMatchesADR` validates the contract
// against this constant rather than a magic string elsewhere.
const keyPrefix = "loopscale:service-auth:nonce:"

// NewRedisNonceStore constructs the production store. The caller is
// responsible for configuring the *redis.Client (TLS, auth, address
// from LOOPSCALE_SERVICE_AUTH_NONCE_REDIS_ADDR or equivalent env). The
// store does not own the client lifecycle — caller closes it on
// shutdown.
//
// TTL must exceed the ReplayWindow tolerance (default 60s); otherwise
// a replay arriving within the timestamp window but after nonce
// expiry would slip through. The ADR default is 5 minutes.
func NewRedisNonceStore(client *redis.Client, ttl time.Duration) *RedisNonceStore {
	return &RedisNonceStore{client: client, ttl: ttl}
}

// Claim attempts to register (userID, nonce) as single-use within
// the TTL window. Returns:
//
//   - nil on first successful claim.
//   - ErrNonceReused if the (userID, nonce) is already live in Redis.
//   - A wrapped error if Redis itself is unreachable (DO NOT treat as
//     accept — caller's fail-closed semantics depend on errors
//     surfacing rather than silently succeeding).
//
// The atomicity guarantee comes from Redis's `SET key value NX EX`:
// either the SET succeeds (key did not exist; we now own it for
// the TTL) OR the SET returns nil (key exists; another claimer won
// the race).
func (s *RedisNonceStore) Claim(ctx context.Context, userID, nonce string) error {
	if userID == "" || nonce == "" {
		return fmt.Errorf("service-auth: redis claim requires non-empty userID and nonce")
	}
	key := keyPrefix + userID + ":" + nonce

	ok, err := s.client.SetNX(ctx, key, "1", s.ttl).Result()
	if err != nil {
		// Network / Redis unavailability. Surface as wrapped error so
		// audit log can grep on 'service-auth' and the caller can
		// fail the request closed.
		return fmt.Errorf("service-auth: redis nonce store unavailable: %w", err)
	}
	if !ok {
		// Key already existed → another claimer (or replay attacker)
		// got there first within the TTL window.
		return ErrNonceReused
	}
	return nil
}

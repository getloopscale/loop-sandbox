package domain

import (
	"context"
	"time"
)

// TenantStore persists tenants.
type TenantStore interface {
	SaveTenant(ctx context.Context, t *Tenant) error
	GetTenant(ctx context.Context, id string) (*Tenant, error)
}

// APIKeyStore persists API keys and resolves a raw key to its record. Resolution
// is by hash (never plaintext) and returns ErrNotFound for unknown keys.
type APIKeyStore interface {
	SaveAPIKey(ctx context.Context, k *APIKey) error
	ResolveByRawKey(ctx context.Context, raw string) (*APIKey, error)
	// Revoke marks the tenant's key as revoked at the given time. Revoking a key
	// that is already revoked keeps its first revocation time and is not an error;
	// a key the tenant does not own is ErrNotFound.
	Revoke(ctx context.Context, tenantID, id string, at time.Time) error
	// ListAPIKeys returns the tenant's keys, revoked ones included, oldest first.
	ListAPIKeys(ctx context.Context, tenantID string) ([]*APIKey, error)
}

// AuditStore appends audit events. Append MUST NOT block the caller's hot path —
// callers use the async sink (internal/audit) which batches into this store.
type AuditStore interface {
	Append(ctx context.Context, e *AuditEvent) error
}

// IdempotencyRecord is a completed idempotent response (TRD §6.3).
type IdempotencyRecord struct {
	Status      int
	Body        []byte
	RequestHash string
}

// ReserveOutcome is the result of claiming an idempotency key.
type ReserveOutcome int

const (
	// Claimed — the caller won the reservation and MUST create the resource then
	// Complete (or Release on failure).
	Claimed ReserveOutcome = iota
	// Replay — a completed record with the same request hash exists; return it.
	Replay
	// Conflict — a record with a DIFFERENT request hash exists; return 409.
	Conflict
	// InProgress — a concurrent request with the same hash holds the reservation
	// but has not completed yet; the caller returns 409 (request in flight).
	InProgress
)

// ReserveResult carries the outcome and, for Replay, the stored record.
type ReserveResult struct {
	Outcome ReserveOutcome
	Record  *IdempotencyRecord
}

// IdempotencyStore serializes idempotent creates via a reserve-before-act pattern
// so concurrent same-key requests cannot double-create (TRD §6.3). The unique
// (tenant,key) constraint is the serialization point.
type IdempotencyStore interface {
	// Reserve atomically claims (tenant,key) for requestHash, or reports what
	// already holds it (Replay / Conflict / InProgress).
	Reserve(ctx context.Context, tenantID, key, requestHash string, nowUnix int64) (ReserveResult, error)
	// Complete finalizes a Claimed reservation with the response to replay later.
	Complete(ctx context.Context, tenantID, key string, status int, body []byte) error
	// Release removes a Claimed-but-unfinished reservation (e.g. the create failed)
	// so a later retry can proceed.
	Release(ctx context.Context, tenantID, key string) error
}

// CostStore accumulates per-tenant usage. M1 only counts; M7 swaps it for
// LoopScale enforcement.CapChecker (blocking) behind the same call site.
type CostStore interface {
	Increment(ctx context.Context, tenantID string, metric CostMetric, n, periodStart, periodEnd int64) error
	Value(ctx context.Context, tenantID string, metric CostMetric, periodStart int64) (int64, error)
}

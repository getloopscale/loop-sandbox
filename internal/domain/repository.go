package domain

import (
	"context"
	"time"
)

// Repository is the persistence port the domain requires. Infrastructure
// (PostgreSQL, in-memory) implements it; the domain never imports a DB driver.
//
// Every method is tenant-scoped: reads/deletes take a tenantID so the
// multi-tenant-always-filtered invariant is structural, not incidental.
type Repository interface {
	// Save inserts or updates a sandbox.
	Save(ctx context.Context, s *Sandbox) error
	// Get returns the sandbox by id for the given tenant, or ErrNotFound.
	// A sandbox that belongs to another tenant MUST be indistinguishable from
	// a non-existent one (US-05).
	Get(ctx context.Context, tenantID, id string) (*Sandbox, error)
	// Delete removes the sandbox row for the given tenant. Deleting a missing
	// row is not an error (idempotent).
	Delete(ctx context.Context, tenantID, id string) error

	// ListExpired returns up to limit non-destroyed sandboxes whose ExpiresAt is
	// before now, across all tenants — the reconciler's work list (M4). It is
	// deliberately tenant-agnostic (the reconciler is a system actor) but ordered
	// so work is bounded; the query is indexed by (state, expires_at).
	ListExpired(ctx context.Context, now time.Time, limit int) ([]*Sandbox, error)

	// List returns one page of the tenant's sandboxes that match q, newest first
	// (creation time descending, then id descending), at most q.Limit of them. A
	// sandbox of another tenant is never returned.
	List(ctx context.Context, tenantID string, q SandboxQuery) ([]*Sandbox, error)
}

// ActivityStore is an optional capability of a Repository: it records when a
// sandbox was last used and finds the ones that went idle.
type ActivityStore interface {
	// TouchSandbox sets the last activity of the sandbox to at, unless a later
	// one is recorded. It changes nothing else of the record.
	TouchSandbox(ctx context.Context, tenantID, id string, at time.Time) error
	// ListIdle returns up to limit sandboxes, of any tenant, for which Idle(now)
	// holds, least recently used first.
	ListIdle(ctx context.Context, now time.Time, limit int) ([]*Sandbox, error)
}

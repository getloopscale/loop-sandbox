// Package memory is an in-memory Repository for unit/E2E tests and local dev.
// The PostgreSQL implementation (persistence/postgres) is the production path;
// both satisfy domain.Repository so the composition root can swap them.
package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// Store is a concurrency-safe in-memory repository keyed by (tenant, id).
type Store struct {
	mu        sync.RWMutex
	data      map[string]domain.Sandbox  // key = tenantID + "\x00" + id
	volumes   map[string]domain.Volume   // key = tenantID + "\x00" + id (M16)
	templates map[string]domain.Template // key = tenantID + "\x00" + id (M17)
	snapshots map[string]domain.Snapshot // key = tenantID + "\x00" + id
	secrets   map[string]domain.Secret   // key = tenantID + "\x00" + name
}

// New returns an empty in-memory store.
func New() *Store {
	return &Store{
		data:      make(map[string]domain.Sandbox),
		volumes:   make(map[string]domain.Volume),
		templates: make(map[string]domain.Template),
	}
}

func key(tenantID, id string) string { return tenantID + "\x00" + id }

// Save implements domain.Repository.
func (s *Store) Save(_ context.Context, sb *domain.Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sb
	// The last activity only moves forward, as in the PostgreSQL store.
	if prev, ok := s.data[key(sb.TenantID, sb.ID)]; ok && prev.LastActiveAt.After(cp.LastActiveAt) {
		cp.LastActiveAt = prev.LastActiveAt
	}
	s.data[key(sb.TenantID, sb.ID)] = cp
	return nil
}

// SetSandboxNetwork implements domain.NetworkStore.
func (s *Store) SetSandboxNetwork(_ context.Context, tenantID, id string, allowOut, denyOut []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.data[key(tenantID, id)]
	if !ok {
		return domain.ErrNotFound
	}
	sb.NetworkAllowOut = append([]string(nil), allowOut...)
	sb.NetworkDenyOut = append([]string(nil), denyOut...)
	s.data[key(tenantID, id)] = sb
	return nil
}

// TouchSandbox implements domain.ActivityStore.
func (s *Store) TouchSandbox(_ context.Context, tenantID, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb, ok := s.data[key(tenantID, id)]; ok && at.After(sb.LastActiveAt) {
		sb.LastActiveAt = at
		s.data[key(tenantID, id)] = sb
	}
	return nil
}

// ListIdle implements domain.ActivityStore.
func (s *Store) ListIdle(_ context.Context, now time.Time, limit int) ([]*domain.Sandbox, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Sandbox
	for _, sb := range s.data {
		if sb.Idle(now) {
			cp := sb
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActiveAt.Equal(out[j].LastActiveAt) {
			return out[i].LastActiveAt.Before(out[j].LastActiveAt)
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var _ domain.ActivityStore = (*Store)(nil)

// Get implements domain.Repository. Cross-tenant lookups are indistinguishable
// from not-found (US-05) because the key includes tenantID.
func (s *Store) Get(_ context.Context, tenantID, id string) (*domain.Sandbox, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sb, ok := s.data[key(tenantID, id)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := sb
	return &cp, nil
}

// Delete implements domain.Repository (idempotent).
func (s *Store) Delete(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key(tenantID, id))
	return nil
}

// ListExpired returns non-destroyed sandboxes past their TTL (reconciler work list).
func (s *Store) ListExpired(_ context.Context, now time.Time, limit int) ([]*domain.Sandbox, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Sandbox
	for _, sb := range s.data {
		if sb.State != domain.StateDestroyed && !sb.ExpiresAt.IsZero() && now.After(sb.ExpiresAt) {
			cp := sb
			out = append(out, &cp)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// List implements domain.Repository: one page of the tenant's sandboxes, newest first.
func (s *Store) List(_ context.Context, tenantID string, q domain.SandboxQuery) ([]*domain.Sandbox, error) {
	if q.Limit <= 0 {
		return nil, fmt.Errorf("list sandboxes: limit must be positive, got %d", q.Limit)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Sandbox
	for _, sb := range s.data {
		if sb.TenantID != tenantID || !sb.MatchesMetadata(q.Metadata) {
			continue
		}
		if len(q.States) > 0 && !slices.Contains(q.States, sb.State) {
			continue
		}
		if q.After != nil && !sb.ComesAfter(*q.After) {
			continue
		}
		cp := sb
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[j].ComesAfter(domain.SandboxCursor{CreatedAt: out[i].CreatedAt, ID: out[i].ID})
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

var _ domain.Repository = (*Store)(nil)

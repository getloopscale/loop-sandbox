package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// TenantStore is an in-memory domain.TenantStore.
type TenantStore struct {
	mu   sync.RWMutex
	data map[string]domain.Tenant
}

// NewTenants returns an empty in-memory tenant store.
func NewTenants() *TenantStore { return &TenantStore{data: make(map[string]domain.Tenant)} }

func (s *TenantStore) SaveTenant(_ context.Context, t *domain.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[t.ID] = *t
	return nil
}

func (s *TenantStore) GetTenant(_ context.Context, id string) (*domain.Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.data[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := t
	return &cp, nil
}

// APIKeyStore is an in-memory domain.APIKeyStore keyed by hash.
type APIKeyStore struct {
	mu     sync.RWMutex
	byHash map[string]domain.APIKey
}

// NewAPIKeys returns an empty in-memory API key store.
func NewAPIKeys() *APIKeyStore { return &APIKeyStore{byHash: make(map[string]domain.APIKey)} }

func (s *APIKeyStore) SaveAPIKey(_ context.Context, k *domain.APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHash[k.KeyHash] = *k
	return nil
}

func (s *APIKeyStore) ResolveByRawKey(_ context.Context, raw string) (*domain.APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.byHash[domain.HashAPIKey(raw)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := k
	return &cp, nil
}

// Revoke implements domain.APIKeyStore.
func (s *APIKeyStore) Revoke(_ context.Context, tenantID, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, k := range s.byHash {
		if k.TenantID != tenantID || k.ID != id {
			continue
		}
		if k.RevokedAt == nil {
			k.RevokedAt = &at
			s.byHash[h] = k
		}
		return nil
	}
	return domain.ErrNotFound
}

// ListAPIKeys implements domain.APIKeyStore.
func (s *APIKeyStore) ListAPIKeys(_ context.Context, tenantID string) ([]*domain.APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.APIKey
	for _, k := range s.byHash {
		if k.TenantID == tenantID {
			cp := k
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

var (
	_ domain.TenantStore = (*TenantStore)(nil)
	_ domain.APIKeyStore = (*APIKeyStore)(nil)
)

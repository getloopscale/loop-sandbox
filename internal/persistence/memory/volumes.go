package memory

import (
	"context"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// SaveVolume implements domain.VolumeStore (upsert).
func (s *Store) SaveVolume(_ context.Context, v *domain.Volume) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumes[key(v.TenantID, v.ID)] = *v
	return nil
}

// GetVolume implements domain.VolumeStore. Cross-tenant lookups are indistinguishable
// from not-found (US-05) because the key includes tenantID.
func (s *Store) GetVolume(_ context.Context, tenantID, id string) (*domain.Volume, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.volumes[key(tenantID, id)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := v
	return &cp, nil
}

// GetVolumeByName resolves a tenant's volume by its unique name (named-volume model).
func (s *Store) GetVolumeByName(_ context.Context, tenantID, name string) (*domain.Volume, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.volumes {
		if v.TenantID == tenantID && v.Name == name {
			cp := v
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

// ListVolumes returns the tenant's volumes attached to sandboxID.
func (s *Store) ListVolumes(_ context.Context, tenantID, sandboxID string) ([]*domain.Volume, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Volume
	for _, v := range s.volumes {
		if v.TenantID == tenantID && v.SandboxID == sandboxID {
			cp := v
			out = append(out, &cp)
		}
	}
	return out, nil
}

// TotalSizeGB sums the tenant's provisioned volume storage (live gauge source of truth).
func (s *Store) TotalSizeGB(_ context.Context, tenantID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, v := range s.volumes {
		if v.TenantID == tenantID {
			total += v.SizeGB
		}
	}
	return total, nil
}

// DeleteVolume implements domain.VolumeStore (idempotent).
func (s *Store) DeleteVolume(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.volumes, key(tenantID, id))
	return nil
}

var _ domain.VolumeStore = (*Store)(nil)

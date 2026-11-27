package memory

import (
	"context"
	"sort"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

var _ domain.SnapshotStore = (*Store)(nil)

func cloneSnapshot(s domain.Snapshot) *domain.Snapshot {
	cp := s
	cp.Metadata, cp.Env = cloneMap(s.Metadata), cloneMap(s.Env)
	return &cp
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SaveSnapshot implements domain.SnapshotStore.
func (s *Store) SaveSnapshot(_ context.Context, snap *domain.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range s.snapshots {
		if other.TenantID == snap.TenantID && other.Name == snap.Name {
			return domain.ErrAlreadyExists
		}
	}
	if s.snapshots == nil {
		s.snapshots = map[string]domain.Snapshot{}
	}
	s.snapshots[key(snap.TenantID, snap.ID)] = *cloneSnapshot(*snap)
	return nil
}

// GetSnapshot implements domain.SnapshotStore.
func (s *Store) GetSnapshot(_ context.Context, tenantID, id string) (*domain.Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.snapshots[key(tenantID, id)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cloneSnapshot(snap), nil
}

// ListSnapshots implements domain.SnapshotStore.
func (s *Store) ListSnapshots(_ context.Context, tenantID string) ([]*domain.Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Snapshot
	for _, snap := range s.snapshots {
		if snap.TenantID == tenantID {
			out = append(out, cloneSnapshot(snap))
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

// DeleteSnapshot implements domain.SnapshotStore.
func (s *Store) DeleteSnapshot(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.snapshots, key(tenantID, id))
	return nil
}

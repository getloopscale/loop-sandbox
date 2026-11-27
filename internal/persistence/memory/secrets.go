package memory

import (
	"context"
	"sort"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

var _ domain.SecretStore = (*Store)(nil)

func cloneSecret(s domain.Secret) *domain.Secret {
	cp := s
	cp.Ciphertext = append([]byte(nil), s.Ciphertext...)
	return &cp
}

// PutSecret implements domain.SecretStore.
func (s *Store) PutSecret(_ context.Context, secret *domain.Secret) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secrets == nil {
		s.secrets = map[string]domain.Secret{}
	}
	k := key(secret.TenantID, secret.Name)
	next := *cloneSecret(*secret)
	prev, existed := s.secrets[k]
	if existed {
		next.CreatedAt = prev.CreatedAt
	}
	s.secrets[k] = next
	return !existed, nil
}

// GetSecret implements domain.SecretStore.
func (s *Store) GetSecret(_ context.Context, tenantID, name string) (*domain.Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	secret, ok := s.secrets[key(tenantID, name)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return cloneSecret(secret), nil
}

// ListSecrets implements domain.SecretStore.
func (s *Store) ListSecrets(_ context.Context, tenantID string) ([]*domain.Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Secret
	for _, secret := range s.secrets {
		if secret.TenantID == tenantID {
			out = append(out, cloneSecret(secret))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteSecret implements domain.SecretStore.
func (s *Store) DeleteSecret(_ context.Context, tenantID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[key(tenantID, name)]; !ok {
		return domain.ErrNotFound
	}
	delete(s.secrets, key(tenantID, name))
	return nil
}

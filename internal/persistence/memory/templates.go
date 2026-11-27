package memory

import (
	"context"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// SaveTemplate implements domain.TemplateStore (upsert).
func (s *Store) SaveTemplate(_ context.Context, t *domain.Template) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.templates == nil {
		s.templates = make(map[string]domain.Template)
	}
	s.templates[key(t.TenantID, t.ID)] = *t
	return nil
}

// GetTemplate returns a template visible to the tenant: one it owns OR a public one.
func (s *Store) GetTemplate(_ context.Context, tenantID, id string) (*domain.Template, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.templates[key(tenantID, id)]; ok {
		cp := t
		return &cp, nil
	}
	// Fall back to a public template with this id owned by any tenant.
	for _, t := range s.templates {
		if t.ID == id && t.Public {
			cp := t
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

// ListTemplates returns the tenant's own templates plus all public ones.
func (s *Store) ListTemplates(_ context.Context, tenantID string) ([]*domain.Template, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.Template
	for _, t := range s.templates {
		if t.TenantID == tenantID || t.Public {
			cp := t
			out = append(out, &cp)
		}
	}
	return out, nil
}

var _ domain.TemplateStore = (*Store)(nil)

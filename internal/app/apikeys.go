package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// ErrAPIKeysUnsupported is returned when the deployment has no API key store, as
// when another system issues the credentials.
var ErrAPIKeysUnsupported = errors.New("api keys are not managed by this deployment")

// SetAPIKeyStore wires the store behind the API key operations.
func (s *Service) SetAPIKeyStore(keys domain.APIKeyStore) { s.keys = keys }

// CreateAPIKey issues a key for the actor's tenant and returns its plaintext,
// which exists nowhere else: only the hash is stored. The new key can carry only
// scopes the actor's own credential holds.
func (s *Service) CreateAPIKey(ctx context.Context, actor Actor, name string, scopes []string) (string, *domain.APIKey, error) {
	if s.keys == nil {
		return "", nil, ErrAPIKeysUnsupported
	}
	if len(scopes) == 0 {
		scopes = []string{domain.ScopeSandboxExecute}
	}
	if err := domain.ValidateAPIKeyRequest(name, scopes, actor.Scopes); err != nil {
		return "", nil, err
	}
	// Serialized per tenant so two concurrent creates cannot both pass the limit.
	unlock := s.locks.lock("keys:" + actor.TenantID)
	defer unlock()
	existing, err := s.keys.ListAPIKeys(ctx, actor.TenantID)
	if err != nil {
		return "", nil, err
	}
	active := 0
	for _, k := range existing {
		if !k.Revoked() {
			active++
		}
	}
	if active >= domain.MaxActiveAPIKeys {
		return "", nil, &domain.ValidationError{Field: "api_keys",
			Reason: fmt.Sprintf("the tenant already has %d active keys, the limit; revoke one first", active)}
	}
	now := s.clock.Now()
	raw, key, err := domain.GenerateAPIKey("key_"+uuid.NewString(), actor.TenantID, name, scopes, now)
	if err != nil {
		return "", nil, err
	}
	if err := s.keys.SaveAPIKey(ctx, key); err != nil {
		return "", nil, err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "api_key.create", "api_key", key.ID, actor.RequestID, now)
	return raw, key, nil
}

// ListAPIKeys returns the keys of the actor's tenant, revoked ones included.
func (s *Service) ListAPIKeys(ctx context.Context, actor Actor) ([]*domain.APIKey, error) {
	if s.keys == nil {
		return nil, ErrAPIKeysUnsupported
	}
	return s.keys.ListAPIKeys(ctx, actor.TenantID)
}

// RevokeAPIKey revokes a key of the actor's tenant. The key stops authenticating
// from the next request on. Revoking the key that made the call is allowed.
func (s *Service) RevokeAPIKey(ctx context.Context, actor Actor, id string) error {
	if s.keys == nil {
		return ErrAPIKeysUnsupported
	}
	now := s.clock.Now()
	if err := s.keys.Revoke(ctx, actor.TenantID, id, now); err != nil {
		return err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "api_key.revoke", "api_key", id, actor.RequestID, now)
	return nil
}

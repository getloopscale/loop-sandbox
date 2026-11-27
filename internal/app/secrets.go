package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/secretbox"
)

// Secret errors.
var (
	// ErrSecretsUnsupported is returned when the deployment has no key to
	// encrypt secrets with.
	ErrSecretsUnsupported = errors.New("secrets not supported by this deployment")
	// ErrTooManySecrets is returned when the tenant already keeps
	// domain.MaxSecretsPerTenant secrets.
	ErrTooManySecrets = errors.New("too many secrets")
)

// SecretUnavailableError is returned when a command cannot run because a secret
// its sandbox names no longer exists.
type SecretUnavailableError struct{ Name string }

func (e *SecretUnavailableError) Error() string {
	return fmt.Sprintf("secret %q, which the sandbox receives, no longer exists", e.Name)
}

// SetSecrets enables secrets (composition root only): store keeps them and box
// encrypts them.
func (s *Service) SetSecrets(store domain.SecretStore, box *secretbox.Box) {
	s.secrets, s.secretBox = store, box
}

func (s *Service) secretsEnabled() bool { return s.secrets != nil && s.secretBox != nil }

// secretBinding is what a ciphertext is bound to: a row moved to another tenant
// or name does not open.
func secretBinding(tenantID, name string) []byte {
	return []byte("loopsandbox secret\x00" + tenantID + "\x00" + name)
}

// PutSecret creates the secret, or replaces its value. It reports whether it
// created it. The value is never returned by any method of the service.
func (s *Service) PutSecret(ctx context.Context, actor Actor, name, value string) (*domain.Secret, bool, error) {
	if !s.secretsEnabled() {
		return nil, false, ErrSecretsUnsupported
	}
	if err := domain.ValidateSecretName(name); err != nil {
		return nil, false, err
	}
	if err := domain.ValidateSecretValue(value); err != nil {
		return nil, false, err
	}
	// The count is checked and then written, so one writer per tenant at a time.
	unlock := s.locks.lock("secrets:" + actor.TenantID)
	defer unlock()
	existing, err := s.secrets.ListSecrets(ctx, actor.TenantID)
	if err != nil {
		return nil, false, fmt.Errorf("list secrets: %w", err)
	}
	known := false
	for _, other := range existing {
		known = known || other.Name == name
	}
	if !known && len(existing) >= domain.MaxSecretsPerTenant {
		return nil, false, ErrTooManySecrets
	}
	sealed, err := s.secretBox.Seal([]byte(value), secretBinding(actor.TenantID, name))
	if err != nil {
		return nil, false, fmt.Errorf("encrypt secret %s: %w", name, err)
	}
	now := s.clock.Now()
	secret := &domain.Secret{
		TenantID: actor.TenantID, Name: name, Ciphertext: sealed, KeyID: s.secretBox.KeyID(),
		CreatedAt: now, UpdatedAt: now,
	}
	created, err := s.secrets.PutSecret(ctx, secret)
	if err != nil {
		return nil, false, err
	}
	action := "secret.update"
	if created {
		action = "secret.create"
	}
	s.audit.Record(actor.TenantID, actor.KeyID, action, "secret", name, actor.RequestID, now)
	stored, err := s.secrets.GetSecret(ctx, actor.TenantID, name)
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

// ListSecrets returns the tenant's secrets ordered by name. The records carry
// the ciphertext, which the API layer does not expose.
func (s *Service) ListSecrets(ctx context.Context, actor Actor) ([]*domain.Secret, error) {
	if !s.secretsEnabled() {
		return nil, ErrSecretsUnsupported
	}
	return s.secrets.ListSecrets(ctx, actor.TenantID)
}

// DeleteSecret removes a secret. A sandbox that names it can no longer run
// commands, which fail with SecretUnavailableError.
func (s *Service) DeleteSecret(ctx context.Context, actor Actor, name string) error {
	if !s.secretsEnabled() {
		return ErrSecretsUnsupported
	}
	if err := s.secrets.DeleteSecret(ctx, actor.TenantID, name); err != nil {
		return err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "secret.delete", "secret", name, actor.RequestID, s.clock.Now())
	return nil
}

// validateSandboxSecrets checks the secrets a create request names: valid
// names, of secrets the tenant has.
func (s *Service) validateSandboxSecrets(ctx context.Context, actor Actor, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if !s.secretsEnabled() {
		return ErrSecretsUnsupported
	}
	if err := domain.ValidateSecretNames(names); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := s.secrets.GetSecret(ctx, actor.TenantID, name); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return &domain.ValidationError{Field: "secrets", Reason: fmt.Sprintf("no secret named %q", name)}
			}
			return fmt.Errorf("read secret %s: %w", name, err)
		}
	}
	return nil
}

// commandEnv is the environment of a command run in sb: the default
// environment of the sandbox, then its secrets, then what the request gives.
// A later source replaces a variable of an earlier one.
func (s *Service) commandEnv(ctx context.Context, sb *domain.Sandbox, requested map[string]string) (map[string]string, error) {
	if len(sb.Env) == 0 && len(sb.Secrets) == 0 {
		return requested, nil
	}
	merged := make(map[string]string, len(sb.Env)+len(sb.Secrets)+len(requested))
	for k, v := range sb.Env {
		merged[k] = v
	}
	if len(sb.Secrets) > 0 && !s.secretsEnabled() {
		return nil, ErrSecretsUnsupported
	}
	for _, name := range sb.Secrets {
		secret, err := s.secrets.GetSecret(ctx, sb.TenantID, name)
		if errors.Is(err, domain.ErrNotFound) {
			return nil, &SecretUnavailableError{Name: name}
		}
		if err != nil {
			return nil, fmt.Errorf("read secret %s: %w", name, err)
		}
		value, err := s.secretBox.Open(secret.Ciphertext, secretBinding(sb.TenantID, name))
		if err != nil {
			// The message names the keys, never the value.
			return nil, fmt.Errorf("decrypt secret %s, sealed by key %s, with key %s: %w", name, secret.KeyID, s.secretBox.KeyID(), err)
		}
		merged[name] = string(value)
	}
	for k, v := range requested {
		merged[k] = v
	}
	return merged, nil
}

// withCommandEnv returns req with the environment commandEnv gives it.
func (s *Service) withCommandEnv(ctx context.Context, sb *domain.Sandbox, req runtime.ExecRequest) (runtime.ExecRequest, error) {
	env, err := s.commandEnv(ctx, sb, req.Env)
	if err != nil {
		return req, err
	}
	req.Env = env
	return req, nil
}

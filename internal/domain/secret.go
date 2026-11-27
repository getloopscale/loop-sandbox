package domain

import (
	"context"
	"time"
)

// Limits on secrets.
const (
	// MaxSecretsPerTenant bounds the secrets one tenant keeps.
	MaxSecretsPerTenant = 100
	// MaxSecretsPerSandbox bounds the secrets one sandbox receives.
	MaxSecretsPerSandbox = 32
)

// ScopeSecretsManage allows listing, writing and deleting the tenant's secrets.
// Using a secret in a sandbox needs only ScopeSandboxExecute.
const ScopeSecretsManage = "secrets:manage"

// Secret is a named value of a tenant, stored encrypted. A sandbox that names
// it receives the value as an environment variable of the same name.
type Secret struct {
	TenantID string
	// Name is unique within the tenant and is the name of the environment variable.
	Name string
	// Ciphertext is the value sealed by the key identified by KeyID.
	Ciphertext []byte
	KeyID      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ValidateSecretName checks that name can be an environment variable.
func ValidateSecretName(name string) error {
	if name == "" || len(name) > MaxEnvNameLen || !envNameRE.MatchString(name) {
		return invalid("name", "must be 1 to %d characters of letters, digits or '_', not starting with a digit", MaxEnvNameLen)
	}
	return nil
}

// ValidateSecretValue checks the value of a secret: what an environment
// variable can hold.
func ValidateSecretValue(value string) error {
	if value == "" {
		return invalid("value", "must not be empty")
	}
	if len(value) > MaxEnvValueLen {
		return invalid("value", "is %d bytes, the limit is %d", len(value), MaxEnvValueLen)
	}
	for i := 0; i < len(value); i++ {
		if value[i] == 0 {
			return invalid("value", "contains a NUL byte")
		}
	}
	return nil
}

// ValidateSecretNames checks the secrets a sandbox asks for.
func ValidateSecretNames(names []string) error {
	if len(names) > MaxSecretsPerSandbox {
		return invalid("secrets", "names %d secrets, the limit is %d", len(names), MaxSecretsPerSandbox)
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || len(name) > MaxEnvNameLen || !envNameRE.MatchString(name) {
			return invalid("secrets", "%q is not a secret name", name)
		}
		if seen[name] {
			return invalid("secrets", "%q is repeated", name)
		}
		seen[name] = true
	}
	return nil
}

// SecretStore persists secrets. Every method is scoped to a tenant: another
// tenant's secret is not found.
type SecretStore interface {
	// PutSecret inserts the secret, or replaces the ciphertext, key id and
	// UpdatedAt of the one with the same name. It reports whether it inserted.
	PutSecret(ctx context.Context, s *Secret) (created bool, err error)
	GetSecret(ctx context.Context, tenantID, name string) (*Secret, error)
	// ListSecrets returns the tenant's secrets ordered by name.
	ListSecrets(ctx context.Context, tenantID string) ([]*Secret, error)
	// DeleteSecret removes the secret, and returns ErrNotFound when there is none.
	DeleteSecret(ctx context.Context, tenantID, name string) error
}

package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// keyPrefix namespaces LoopSandbox API keys (M1 standalone). M7 aligns the
// SaaS surface with LoopScale's `loopscale_live_*` consumer keys via the edge, not here.
const keyPrefix = "lsb_"

// APIKey is a tenant credential. Only the SHA-256 hash of the full key is stored
// (TRD §5.2) — the plaintext is shown once at creation and never persisted.
//
// SHA-256 (not bcrypt) is used deliberately: the key is high-entropy (256 bits
// from crypto/rand), so a fast hash is safe and enables O(1) lookup-by-hash.
// bcrypt is for low-entropy human passwords; using it here would prevent the
// indexed hash lookup with no security gain (see ADR in the plan).
type APIKey struct {
	ID        string
	TenantID  string
	Name      string
	Prefix    string // first chars of the key, for display/audit (never the secret)
	KeyHash   string // hex(sha256(rawKey))
	Scopes    []string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// The scopes a key can carry. A route names the scope it needs, and a key without
// it is refused with ErrForbidden.
const (
	// ScopeSandboxExecute allows every sandbox, volume and template operation.
	ScopeSandboxExecute = "sandbox:execute"
	// ScopeAPIKeysManage allows listing, creating and revoking the tenant's keys.
	ScopeAPIKeysManage = "api_keys:manage"
)

// Limits on API keys.
const (
	MaxAPIKeyNameLen = 64
	// MaxActiveAPIKeys bounds the keys of one tenant that are not revoked.
	MaxActiveAPIKeys = 100
)

// AllScopes returns every scope a key can carry. An operator, who is not bound
// by the scopes of a calling key, can grant all of them.
func AllScopes() []string {
	return []string{ScopeSandboxExecute, ScopeAPIKeysManage, ScopeSecretsManage}
}

// ValidScope reports whether scope is one a key can carry.
func ValidScope(scope string) bool {
	return slices.Contains(AllScopes(), scope)
}

// ValidateAPIKeyRequest checks the name and scopes asked for a new key. granted
// are the scopes of whoever is creating it: a key cannot hand out a scope it does
// not hold itself.
func ValidateAPIKeyRequest(name string, scopes, granted []string) error {
	if name == "" || len(name) > MaxAPIKeyNameLen || !utf8.ValidString(name) {
		return invalid("name", "must be 1 to %d bytes of valid UTF-8", MaxAPIKeyNameLen)
	}
	if len(scopes) == 0 {
		return invalid("scopes", "must name at least one scope")
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if !ValidScope(scope) {
			return invalid("scopes", "unknown scope %q: use %s, %s or %s", scope, ScopeSandboxExecute, ScopeAPIKeysManage, ScopeSecretsManage)
		}
		if seen[scope] {
			return invalid("scopes", "scope %q is repeated", scope)
		}
		seen[scope] = true
		if !slices.Contains(granted, scope) {
			return fmt.Errorf("%w: cannot grant scope %q, which the calling key does not hold", ErrForbidden, scope)
		}
	}
	return nil
}

// GenerateAPIKey creates a new key: returns the one-time plaintext AND the APIKey
// record (with only the hash). The caller shows the plaintext to the user once.
func GenerateAPIKey(id, tenantID, name string, scopes []string, now time.Time) (raw string, key *APIKey, err error) {
	if id == "" || tenantID == "" {
		return "", nil, ErrInvalidArgument
	}
	buf := make([]byte, 32) // 256 bits of entropy
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	raw = keyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	hash := HashAPIKey(raw)
	key = &APIKey{
		ID:       id,
		TenantID: tenantID,
		Name:     name,
		// Display prefix derived from the NON-secret hash, never from the raw key
		// bytes — a DB dump must not leak any part of the secret.
		Prefix:    keyPrefix + hash[:8],
		KeyHash:   hash,
		Scopes:    scopes,
		CreatedAt: now,
	}
	return raw, key, nil
}

// HashAPIKey returns the hex SHA-256 of a raw key — the value stored and queried.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Revoked reports whether the key has been revoked.
func (k *APIKey) Revoked() bool { return k.RevokedAt != nil }

// HasScope reports whether the key carries the given scope.
func (k *APIKey) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// LooksLikeAPIKey is a cheap boundary check before hashing/lookup.
func LooksLikeAPIKey(raw string) bool {
	return strings.HasPrefix(raw, keyPrefix) && len(raw) > len(keyPrefix)+6
}

// Package auth resolves an incoming credential to a tenant Principal. It is the
// single point of tenant resolution (rules/architecture.md) so that M7 can add a
// LoopScale adapter (consumer key loopscale_live_* + JWT) behind the same seam without
// touching handlers or the domain.
package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// ErrUnauthorized is returned for any authentication failure. It is intentionally
// coarse: callers map it to 401 without revealing whether the key was unknown,
// revoked, or the tenant suspended (no oracle for attackers).
var ErrUnauthorized = errors.New("unauthorized")

// Principal is the authenticated identity attached to a request context.
type Principal struct {
	TenantID string
	KeyID    string
	Scopes   []string
}

// HasScope reports whether the principal carries a scope.
func (p *Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// TenantAuthenticator resolves a raw credential to a Principal.
//
// Constant-time note for future adapters (M7 LoopScale): the M1 API-key path compares
// a SHA-256 hash of a 256-bit random key via an indexed DB lookup, where a timing
// side channel is not exploitable. An adapter that compares low-entropy or
// signed material (JWT signatures, HMACs, consumer-key secrets) MUST use
// crypto/subtle.ConstantTimeCompare — do not reintroduce a variable-time compare.
type TenantAuthenticator interface {
	Authenticate(ctx context.Context, rawCredential string) (*Principal, error)
}

// APIKeyAuthenticator is the M1 standalone adapter: a raw API key → tenant.
type APIKeyAuthenticator struct {
	keys    domain.APIKeyStore
	tenants domain.TenantStore
}

// NewAPIKeyAuthenticator wires the adapter.
func NewAPIKeyAuthenticator(keys domain.APIKeyStore, tenants domain.TenantStore) *APIKeyAuthenticator {
	return &APIKeyAuthenticator{keys: keys, tenants: tenants}
}

// Authenticate resolves a raw API key to a Principal, enforcing key validity and
// tenant status. Every failure returns ErrUnauthorized (no distinguishing detail).
func (a *APIKeyAuthenticator) Authenticate(ctx context.Context, raw string) (*Principal, error) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if !domain.LooksLikeAPIKey(raw) {
		return nil, ErrUnauthorized
	}
	key, err := a.keys.ResolveByRawKey(ctx, raw)
	if err != nil {
		return nil, ErrUnauthorized
	}
	if key.Revoked() {
		return nil, ErrUnauthorized
	}
	tenant, err := a.tenants.GetTenant(ctx, key.TenantID)
	if err != nil || !tenant.Active() {
		return nil, ErrUnauthorized
	}
	return &Principal{TenantID: key.TenantID, KeyID: key.ID, Scopes: key.Scopes}, nil
}

var _ TenantAuthenticator = (*APIKeyAuthenticator)(nil)

// --- context propagation ---

type ctxKey struct{}

// WithPrincipal returns a context carrying the principal.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom extracts the principal, or nil if absent.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

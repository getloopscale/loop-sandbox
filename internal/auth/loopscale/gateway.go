package loopscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// GatewayAuthenticator trusts a gateway that authenticated the caller itself. The
// gateway signs every request it forwards with Ed25519, over the tenant, the
// user, the method, the path, a timestamp and a nonce, and the sandbox accepts
// the tenant and the user of a valid signature. There is no consumer token: the
// caller's credential, an API key or a session, stays at the gateway.
//
// Whoever holds the gateway's signing key acts as any tenant, so the API must be
// reachable only from the gateway.
type GatewayAuthenticator struct {
	verifier *ForwardAuthVerifier
	tenants  domain.TenantStore
	plan     domain.Plan
	scopes   []string
	now      func() time.Time
}

// GatewayScopes are what a caller the gateway vouches for may do. The gateway
// decides which of its callers reach which path: a request it signed for the
// secrets routes comes from a caller it allowed to manage secrets.
func GatewayScopes() []string {
	return []string{domain.ScopeSandboxExecute, domain.ScopeSecretsManage}
}

// NewGatewayAuthenticator wires the gate. A tenant the sandbox has not seen is
// created on plan; every principal holds scopes.
func NewGatewayAuthenticator(verifier *ForwardAuthVerifier, tenants domain.TenantStore, plan domain.Plan, scopes []string) *GatewayAuthenticator {
	return &GatewayAuthenticator{verifier: verifier, tenants: tenants, plan: plan, scopes: scopes, now: time.Now}
}

// SetClock overrides the time source (tests). nil restores time.Now.
func (g *GatewayAuthenticator) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	g.now = now
}

// Middleware verifies the forward and injects the Principal. It answers 401 for a
// request the gateway did not sign or a suspended tenant, and 503 when the
// tenant cannot be read or created.
func (g *GatewayAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := g.authenticate(r)
		switch {
		case errors.Is(err, auth.ErrUnauthorized):
			writeUnauthorized(w)
		case err != nil:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "unavailable", "message": "the tenant could not be resolved; try again"},
			})
		default:
			next.ServeHTTP(w, auth.WithPrincipalRequest(r, p))
		}
	})
}

func (g *GatewayAuthenticator) authenticate(r *http.Request) (*auth.Principal, error) {
	h := ForwardHeaders{
		UserID:    r.Header.Get("X-Loop-Runtime-User-ID"),
		TenantID:  r.Header.Get("X-Loop-Runtime-Tenant"),
		Timestamp: r.Header.Get("X-Loop-Runtime-Timestamp"),
		Nonce:     r.Header.Get("X-Loop-Runtime-Nonce"),
		Signature: r.Header.Get("X-Loop-Runtime-Signature"),
	}
	assertion, err := g.verifier.Verify(r.Context(), r.Method, r.URL.Path, h, g.now())
	if err != nil {
		return nil, auth.ErrUnauthorized
	}
	tenant, err := g.tenant(r.Context(), assertion.TenantID)
	if err != nil {
		return nil, err
	}
	if !tenant.Active() {
		return nil, auth.ErrUnauthorized
	}
	return &auth.Principal{TenantID: tenant.ID, KeyID: assertion.UserID, Scopes: append([]string{}, g.scopes...)}, nil
}

// tenant returns the tenant of a verified forward, creating it on first sight.
// An existing tenant is never rewritten, so a plan or a suspension set by the
// operator outlives the requests of that tenant.
func (g *GatewayAuthenticator) tenant(ctx context.Context, id string) (*domain.Tenant, error) {
	tenant, err := g.tenants.GetTenant(ctx, id)
	if err == nil {
		return tenant, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("read tenant %s: %w", id, err)
	}
	tenant, err = domain.NewTenant(id, id, g.plan, g.now().UTC())
	if err != nil {
		return nil, auth.ErrUnauthorized
	}
	if err := g.tenants.SaveTenant(ctx, tenant); err != nil {
		return nil, fmt.Errorf("create tenant %s: %w", id, err)
	}
	return tenant, nil
}

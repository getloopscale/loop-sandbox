package loopscale

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	tjwt "github.com/getloopscale/loop-sandbox/internal/auth/loopscale/jwt"

	"github.com/getloopscale/loop-sandbox/internal/auth"
)

// TokenValidator abstracts LoopScale contracts/jwt.Validator so the middleware can be
// unit-tested with a real RS256/JWKS validator or a stub. *tjwt.Validator satisfies it.
type TokenValidator interface {
	Validate(token string) (*tjwt.Claims, error)
}

// ForwardAuthenticator is the LoopScale edge forward-auth gate. It combines:
//   - serviceauth Ed25519 verification (the request came from the trusted edge,
//     binding tenant/user/method/path + replay/nonce), and
//   - LoopScale JWT validation (the end consumer's identity: tenant_id + scope),
//
// then cross-checks the JWT tenant against the signed tenant (the single
// tenant-identity resolution point) and enforces the required scope. It produces a
// Principal for the request context. EVERY failure is 401 fail-closed.
type ForwardAuthenticator struct {
	verifier      *ForwardAuthVerifier
	jwt           TokenValidator
	requiredScope string
	now           func() time.Time
}

// NewForwardAuthenticator wires the gate. requiredScope (e.g. "sandbox:execute") is
// enforced against the JWT scope claim.
func NewForwardAuthenticator(verifier *ForwardAuthVerifier, jwtValidator TokenValidator, requiredScope string) *ForwardAuthenticator {
	return &ForwardAuthenticator{
		verifier:      verifier,
		jwt:           jwtValidator,
		requiredScope: requiredScope,
		now:           time.Now,
	}
}

// SetClock overrides the time source (tests). nil restores time.Now.
func (f *ForwardAuthenticator) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	f.now = now
}

// Middleware verifies the forward and injects the Principal, or writes 401 and stops.
func (f *ForwardAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := f.authenticate(r)
		if err != nil {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, auth.WithPrincipalRequest(r, p))
	})
}

// authenticate performs the full verification chain. Returns ErrUnauthorized on any
// failure (fail-closed) — never a nil error with a nil principal.
func (f *ForwardAuthenticator) authenticate(r *http.Request) (*auth.Principal, error) {
	h := ForwardHeaders{
		UserID:    r.Header.Get("X-Loop-Runtime-User-ID"),
		TenantID:  r.Header.Get("X-Loop-Runtime-Tenant"),
		Timestamp: r.Header.Get("X-Loop-Runtime-Timestamp"),
		Nonce:     r.Header.Get("X-Loop-Runtime-Nonce"),
		Signature: r.Header.Get("X-Loop-Runtime-Signature"),
	}
	// 1. Edge trust + tenant binding + replay/nonce. The signature is over the
	//    byte-exact r.URL.Path (no query string). Contract: the LoopScale edge MUST sign
	//    the same normalized path the sandbox receives.
	assertion, err := f.verifier.Verify(r.Context(), r.Method, r.URL.Path, h, f.now())
	if err != nil {
		return nil, auth.ErrUnauthorized
	}
	// 2. End-consumer JWT: tenant_id + scope (cryptographically signed by LoopScale OIDC).
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if raw == "" {
		return nil, auth.ErrUnauthorized
	}
	claims, err := f.jwt.Validate(raw)
	if err != nil {
		return nil, auth.ErrUnauthorized
	}
	tenant := jwtTenant(claims)
	if tenant == "" {
		return nil, auth.ErrUnauthorized
	}
	// 3. Single tenant-identity resolution point: the JWT tenant MUST equal the
	//    signed X-Loop-Runtime-Tenant, or the edge/token disagree → reject (top risk #1).
	if tenant != assertion.TenantID {
		return nil, auth.ErrUnauthorized
	}
	// 3b. Bind the USER identity too: the JWT subject MUST equal the signed
	//     X-Loop-Runtime-User-ID, so a valid same-tenant token for a different user cannot
	//     be pasted onto another user's signed forward (audit-attribution + scope
	//     escalation defense). The Principal's KeyID is this agreed identity.
	if claims.Subject == "" || claims.Subject != assertion.UserID {
		return nil, auth.ErrUnauthorized
	}
	// 4. Scope: the JWT must carry the required scope.
	scopes := strings.Fields(claims.Scope)
	if !hasScope(scopes, f.requiredScope) {
		return nil, auth.ErrUnauthorized
	}
	return &auth.Principal{TenantID: tenant, KeyID: assertion.UserID, Scopes: scopes}, nil
}

// jwtTenant reads the tenant from the claim, tolerating the Hydra "ext" placement.
func jwtTenant(c *tjwt.Claims) string {
	if c.TenantID != "" {
		return c.TenantID
	}
	return c.Ext.TenantID
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": "unauthorized", "message": "invalid or missing LoopScale credential"},
	})
}

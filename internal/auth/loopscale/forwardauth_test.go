package loopscale_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	tjwt "github.com/getloopscale/loop-sandbox/internal/auth/loopscale/jwt"
	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale/serviceauth"
	goJWT "github.com/golang-jwt/jwt/v5"

	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale"
)

const (
	issuer = "https://loopscale.test"
	aud    = "loopscale-sandbox"
	kid    = "kid-1"
)

// jwks mirrors LoopScale contracts' own test helper: serve the RSA public key so the
// validator can verify RS256 tokens we mint.
func serveJWKS(t *testing.T, pub *rsa.PublicKey) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": kid, "kty": "RSA", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func mintToken(t *testing.T, key *rsa.PrivateKey, tenantID, scope string, exp time.Time) string {
	t.Helper()
	claims := tjwt.Claims{
		RegisteredClaims: goJWT.RegisteredClaims{
			Issuer: issuer, Subject: "usr-1", Audience: goJWT.ClaimStrings{aud},
			ExpiresAt: goJWT.NewNumericDate(exp),
		},
		TenantID: tenantID, Scope: scope,
	}
	tok := goJWT.NewWithClaims(goJWT.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// harness builds a fully-configured ForwardAuthenticator + a signed request factory.
type harness struct {
	fa       *loopscale.ForwardAuthenticator
	edgePriv ed25519.PrivateKey
	rsaKey   *rsa.PrivateKey
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	edgePub, edgePriv, _ := ed25519.GenerateKey(nil)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwksURL := serveJWKS(t, &rsaKey.PublicKey)
	validator := tjwt.NewValidator(jwksURL, issuer, aud)
	verifier := loopscale.NewForwardAuthVerifier([]ed25519.PublicKey{edgePub}, 30*time.Second, serviceauth.NewInMemoryNonceStore(time.Minute))
	fa := loopscale.NewForwardAuthenticator(verifier, validator, "sandbox:execute")
	now := time.Now()
	fa.SetClock(func() time.Time { return now })
	return &harness{fa: fa, edgePriv: edgePriv, rsaKey: rsaKey, now: now}
}

// request builds a signed forwarded request for (method,path) with a JWT carrying
// jwtTenant/scope, signed-header tenant sigTenant, and nonce.
func (h *harness) request(t *testing.T, method, path, sigTenant, jwtTenant, scope, nonce string, exp time.Time) *http.Request {
	t.Helper()
	ts := h.now.Unix()
	in := serviceauth.PayloadInput{UserID: "usr-1", TenantID: sigTenant, Timestamp: ts, Nonce: nonce, Method: method, Path: path}
	sig, err := serviceauth.Sign(h.edgePriv, in)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Loop-Runtime-User-ID", "usr-1")
	req.Header.Set("X-Loop-Runtime-Tenant", sigTenant)
	req.Header.Set("X-Loop-Runtime-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Loop-Runtime-Nonce", nonce)
	req.Header.Set("X-Loop-Runtime-Signature", sig)
	req.Header.Set("Authorization", "Bearer "+mintToken(t, h.rsaKey, jwtTenant, scope, exp))
	return req
}

// run drives the middleware and returns (status, principal-or-nil).
func (h *harness) run(req *http.Request) (int, *auth.Principal) {
	var captured *auth.Principal
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = auth.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	h.fa.Middleware(terminal).ServeHTTP(rec, req)
	return rec.Code, captured
}

func TestForward_valid_loopscale_request_yields_principal(t *testing.T) {
	h := newHarness(t)
	req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "openid sandbox:execute", "n1", h.now.Add(time.Hour))
	code, p := h.run(req)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if p == nil || p.TenantID != "tenant-a" || p.KeyID != "usr-1" || !p.HasScope("sandbox:execute") {
		t.Errorf("principal = %+v, want tenant-a/usr-1/sandbox:execute", p)
	}
}

func TestForward_jwt_tenant_mismatch_is_401(t *testing.T) {
	h := newHarness(t)
	// signed tenant-a, but the JWT claims tenant-b → cross-check fails (top risk #1).
	req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-b", "sandbox:execute", "n1", h.now.Add(time.Hour))
	if code, p := h.run(req); code != http.StatusUnauthorized || p != nil {
		t.Errorf("tenant mismatch → status %d principal %+v, want 401/nil", code, p)
	}
}

func TestForward_missing_scope_is_401(t *testing.T) {
	h := newHarness(t)
	req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "openid profile", "n1", h.now.Add(time.Hour))
	if code, _ := h.run(req); code != http.StatusUnauthorized {
		t.Errorf("missing scope → status %d, want 401", code)
	}
}

func TestForward_expired_jwt_is_401(t *testing.T) {
	h := newHarness(t)
	req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "sandbox:execute", "n1", h.now.Add(-time.Hour))
	if code, _ := h.run(req); code != http.StatusUnauthorized {
		t.Errorf("expired jwt → status %d, want 401", code)
	}
}

func TestForward_missing_signature_is_401(t *testing.T) {
	h := newHarness(t)
	req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "sandbox:execute", "n1", h.now.Add(time.Hour))
	req.Header.Del("X-Loop-Runtime-Signature") // no edge signature → fail-closed
	if code, _ := h.run(req); code != http.StatusUnauthorized {
		t.Errorf("missing signature → status %d, want 401", code)
	}
}

func TestForward_missing_jwt_is_401(t *testing.T) {
	h := newHarness(t)
	req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "sandbox:execute", "n1", h.now.Add(time.Hour))
	req.Header.Del("Authorization")
	if code, _ := h.run(req); code != http.StatusUnauthorized {
		t.Errorf("missing jwt → status %d, want 401", code)
	}
}

// mintCustom mints an RS256 token with explicit subject, top-level tenant, ext
// tenant, and scope — for the sub/tenant/scope edge cases.
func (h *harness) mintCustom(t *testing.T, sub, tenant, extTenant, scope string, exp time.Time) string {
	t.Helper()
	claims := tjwt.Claims{
		RegisteredClaims: goJWT.RegisteredClaims{
			Issuer: issuer, Subject: sub, Audience: goJWT.ClaimStrings{aud},
			ExpiresAt: goJWT.NewNumericDate(exp),
		},
		TenantID: tenant, Scope: scope,
	}
	claims.Ext.TenantID = extTenant
	tok := goJWT.NewWithClaims(goJWT.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(h.rsaKey)
	if err != nil {
		t.Fatalf("mintCustom: %v", err)
	}
	return s
}

func TestForward_jwt_subject_mismatch_is_401(t *testing.T) {
	h := newHarness(t)
	// signed X-Loop-Runtime-User-ID is usr-1 (from request); JWT subject is usr-2, same
	// tenant + scope → user-identity cross-check must reject (H1: no impersonation).
	req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "sandbox:execute", "n1", h.now.Add(time.Hour))
	req.Header.Set("Authorization", "Bearer "+h.mintCustom(t, "usr-2", "tenant-a", "", "sandbox:execute", h.now.Add(time.Hour)))
	if code, p := h.run(req); code != http.StatusUnauthorized || p != nil {
		t.Errorf("subject mismatch → status %d principal %+v, want 401/nil", code, p)
	}
}

func TestForward_scope_substring_does_not_match(t *testing.T) {
	h := newHarness(t)
	for _, scope := range []string{"sandbox:executex", "sandbox:exec", "sandbox:execute:readonly"} {
		req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", scope, "n-"+scope, h.now.Add(time.Hour))
		if code, _ := h.run(req); code != http.StatusUnauthorized {
			t.Errorf("scope %q → status %d, want 401 (exact match only)", scope, code)
		}
	}
}

func TestForward_empty_signed_tenant_is_401(t *testing.T) {
	h := newHarness(t)
	// An empty X-Loop-Runtime-Tenant can never carry a valid signature (serviceauth rejects
	// empty fields at sign AND verify), so the gate must reject it. Build the request
	// manually with an empty tenant + a placeholder signature.
	req := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	req.Header.Set("X-Loop-Runtime-User-ID", "usr-1")
	req.Header.Set("X-Loop-Runtime-Tenant", "") // empty
	req.Header.Set("X-Loop-Runtime-Timestamp", strconv.FormatInt(h.now.Unix(), 10))
	req.Header.Set("X-Loop-Runtime-Nonce", "n1")
	req.Header.Set("X-Loop-Runtime-Signature", base64.StdEncoding.EncodeToString(make([]byte, 64)))
	req.Header.Set("Authorization", "Bearer "+h.mintCustom(t, "usr-1", "", "", "sandbox:execute", h.now.Add(time.Hour)))
	if code, _ := h.run(req); code != http.StatusUnauthorized {
		t.Errorf("empty tenant → status %d, want 401", code)
	}
}

func TestForward_tenant_from_ext_claim(t *testing.T) {
	h := newHarness(t)
	// top-level tenant_id empty, tenant carried in the Hydra ext claim → resolves.
	req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "sandbox:execute", "n1", h.now.Add(time.Hour))
	req.Header.Set("Authorization", "Bearer "+h.mintCustom(t, "usr-1", "", "tenant-a", "sandbox:execute", h.now.Add(time.Hour)))
	if code, p := h.run(req); code != http.StatusOK || p == nil || p.TenantID != "tenant-a" {
		t.Errorf("ext-claim tenant → status %d principal %+v, want 200/tenant-a", code, p)
	}
	// negative twin: ext tenant-b vs signed tenant-a → 401.
	req2 := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "sandbox:execute", "n2", h.now.Add(time.Hour))
	req2.Header.Set("Authorization", "Bearer "+h.mintCustom(t, "usr-1", "", "tenant-b", "sandbox:execute", h.now.Add(time.Hour)))
	if code, _ := h.run(req2); code != http.StatusUnauthorized {
		t.Errorf("ext tenant mismatch → status %d, want 401", code)
	}
}

func TestForward_bearer_without_token_is_401(t *testing.T) {
	h := newHarness(t)
	for _, av := range []string{"Bearer ", "Bearer", "key-a"} {
		req := h.request(t, "POST", "/v1/sandboxes", "tenant-a", "tenant-a", "sandbox:execute", "n-"+av, h.now.Add(time.Hour))
		req.Header.Set("Authorization", av)
		if code, _ := h.run(req); code != http.StatusUnauthorized {
			t.Errorf("Authorization %q → status %d, want 401", av, code)
		}
	}
}

package api_test

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
	"strings"
	"testing"
	"time"

	tjwt "github.com/getloopscale/loop-sandbox/internal/auth/loopscale/jwt"
	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale/serviceauth"
	goJWT "github.com/golang-jwt/jwt/v5"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	lsauth "github.com/getloopscale/loop-sandbox/internal/auth/loopscale"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

const (
	loopScaleIss = "https://loopscale.test"
	loopScaleAud = "loopscale-sandbox"
	loopScaleKID = "kid-e2e"
)

// loopScaleEnv is a full loopscale-mode server wired through the REAL router + service, with
// a signing edge (Ed25519) and a JWKS-backed JWT validator.
type loopScaleEnv struct {
	srv      http.Handler
	edgePriv ed25519.PrivateKey
	rsaKey   *rsa.PrivateKey
	now      time.Time
}

func newLoopScaleEnv(t *testing.T) *loopScaleEnv {
	t.Helper()
	edgePub, edgePriv, _ := ed25519.GenerateKey(nil)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": loopScaleKID, "kty": "RSA", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()),
	}}})
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(jwks) }))
	t.Cleanup(jwksSrv.Close)

	validator := tjwt.NewValidator(jwksSrv.URL, loopScaleIss, loopScaleAud)
	verifier := lsauth.NewForwardAuthVerifier([]ed25519.PublicKey{edgePub}, 30*time.Second, serviceauth.NewInMemoryNonceStore(time.Minute))
	fa := lsauth.NewForwardAuthenticator(verifier, validator, "sandbox:execute")
	now := time.Now()
	fa.SetClock(func() time.Time { return now })

	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	h := api.NewHandler(svc, memory.NewIdempotency())
	router := api.NewRouter(h, fa.Middleware, nil)
	return &loopScaleEnv{srv: router, edgePriv: edgePriv, rsaKey: rsaKey, now: now}
}

func (e *loopScaleEnv) mint(t *testing.T, tenantID, scope string) string {
	t.Helper()
	claims := tjwt.Claims{
		RegisteredClaims: goJWT.RegisteredClaims{
			Issuer: loopScaleIss, Subject: "usr-1", Audience: goJWT.ClaimStrings{loopScaleAud},
			ExpiresAt: goJWT.NewNumericDate(e.now.Add(time.Hour)),
		},
		TenantID: tenantID, Scope: scope,
	}
	tok := goJWT.NewWithClaims(goJWT.SigningMethodRS256, claims)
	tok.Header["kid"] = loopScaleKID
	s, err := tok.SignedString(e.rsaKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// do issues a signed LoopScale forward request for (method, path) as tenant, returning
// the recorder. nonce must be unique per call.
func (e *loopScaleEnv) do(t *testing.T, method, path, tenant, nonce, body string) *httptest.ResponseRecorder {
	t.Helper()
	ts := e.now.Unix()
	sig, err := serviceauth.Sign(e.edgePriv, serviceauth.PayloadInput{
		UserID: "usr-1", TenantID: tenant, Timestamp: ts, Nonce: nonce, Method: method, Path: path,
	})
	if err != nil {
		t.Fatalf("sign forward: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Loop-Runtime-User-ID", "usr-1")
	req.Header.Set("X-Loop-Runtime-Tenant", tenant)
	req.Header.Set("X-Loop-Runtime-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Loop-Runtime-Nonce", nonce)
	req.Header.Set("X-Loop-Runtime-Signature", sig)
	req.Header.Set("Authorization", "Bearer "+e.mint(t, tenant, "openid sandbox:execute"))
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

// TestLoopScaleE2E_create_via_edge_and_cross_tenant_isolation proves the DoD: a valid
// LoopScale forward creates a sandbox; another tenant's forward cannot see it (US-05).
func TestLoopScaleE2E_create_via_edge_and_cross_tenant_isolation(t *testing.T) {
	e := newLoopScaleEnv(t)

	rec := e.do(t, http.MethodPost, "/v1/sandboxes", "tenant-a", "n1", `{"image":"busybox:latest"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create via LoopScale edge = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID       string `json:"id"`
		TenantID string `json:"tenant_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.TenantID != "tenant-a" {
		t.Errorf("created tenant = %q, want tenant-a", created.TenantID)
	}

	// tenant-a can GET its own sandbox.
	if g := e.do(t, http.MethodGet, "/v1/sandboxes/"+created.ID, "tenant-a", "n2", ""); g.Code != http.StatusOK {
		t.Errorf("owner GET = %d, want 200", g.Code)
	}
	// tenant-b (valid LoopScale forward) must NOT see tenant-a's sandbox → 404 (US-05).
	if g := e.do(t, http.MethodGet, "/v1/sandboxes/"+created.ID, "tenant-b", "n3", ""); g.Code != http.StatusNotFound {
		t.Errorf("cross-tenant GET = %d, want 404 (isolation)", g.Code)
	}
}

func TestLoopScaleE2E_invalid_signature_is_401_fail_closed(t *testing.T) {
	e := newLoopScaleEnv(t)
	rec := e.do(t, http.MethodPost, "/v1/sandboxes", "tenant-a", "n1", `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("baseline create = %d", rec.Code)
	}
	// Tamper: reuse everything but corrupt the signature header via a second request.
	ts := e.now.Unix()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{}`))
	req.Header.Set("X-Loop-Runtime-User-ID", "usr-1")
	req.Header.Set("X-Loop-Runtime-Tenant", "tenant-a")
	req.Header.Set("X-Loop-Runtime-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Loop-Runtime-Nonce", "n-bad")
	req.Header.Set("X-Loop-Runtime-Signature", "AAAAinvalidsignatureAAAA")
	req.Header.Set("Authorization", "Bearer "+e.mint(t, "tenant-a", "sandbox:execute"))
	rec2 := httptest.NewRecorder()
	e.srv.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("invalid signature = %d, want 401 (fail-closed)", rec2.Code)
	}
}

// TestLoopScaleE2E_standalone_key_rejected_in_loopscale_mode proves the mode boundary: a bare
// standalone-style API key (no X-Loop-Runtime-* signed headers) hitting a loopscale-mode router
// is 401, never silently accepted.
func TestLoopScaleE2E_standalone_key_rejected_in_loopscale_mode(t *testing.T) {
	e := newLoopScaleEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer lsb_some_standalone_key") // no LoopScale headers
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("standalone key in LoopScale mode = %d, want 401", rec.Code)
	}
}

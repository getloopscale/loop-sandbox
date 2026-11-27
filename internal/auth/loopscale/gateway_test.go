package loopscale_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale"
	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale/serviceauth"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
)

type gatewayRig struct {
	gw      *loopscale.GatewayAuthenticator
	priv    ed25519.PrivateKey
	tenants domain.TenantStore
	now     time.Time
}

func newGatewayRig(t *testing.T, tenants domain.TenantStore) *gatewayRig {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	verifier := loopscale.NewForwardAuthVerifier([]ed25519.PublicKey{pub}, 30*time.Second, serviceauth.NewInMemoryNonceStore(time.Minute))
	gw := loopscale.NewGatewayAuthenticator(verifier, tenants, domain.PlanPro, []string{domain.ScopeSandboxExecute})
	now := time.Now()
	gw.SetClock(func() time.Time { return now })
	return &gatewayRig{gw: gw, priv: priv, tenants: tenants, now: now}
}

// signed builds a request the gateway signed for signedPath and sends to path.
func (g *gatewayRig) signed(t *testing.T, method, signedPath, path, tenant, nonce string) *http.Request {
	t.Helper()
	ts := g.now.Unix()
	sig, err := serviceauth.Sign(g.priv, serviceauth.PayloadInput{UserID: "usr-1", TenantID: tenant, Timestamp: ts, Nonce: nonce, Method: method, Path: signedPath})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Loop-Runtime-User-ID", "usr-1")
	req.Header.Set("X-Loop-Runtime-Tenant", tenant)
	req.Header.Set("X-Loop-Runtime-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Loop-Runtime-Nonce", nonce)
	req.Header.Set("X-Loop-Runtime-Signature", sig)
	return req
}

func (g *gatewayRig) run(req *http.Request) (int, *auth.Principal) {
	var got *auth.Principal
	rec := httptest.NewRecorder()
	g.gw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = auth.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	return rec.Code, got
}

func TestGatewayAcceptsASignedForwardWithoutAConsumerToken(t *testing.T) {
	g := newGatewayRig(t, memory.NewTenants())

	code, p := g.run(g.signed(t, "POST", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1"))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if p == nil || p.TenantID != "tenant-a" || p.KeyID != "usr-1" || !p.HasScope(domain.ScopeSandboxExecute) {
		t.Errorf("principal = %+v, want tenant-a, usr-1 and the scope sandbox:execute", p)
	}
	if p != nil && p.HasScope(domain.ScopeAPIKeysManage) {
		t.Error("the principal holds a scope the gateway was not configured to grant")
	}
}

func TestGatewayCreatesTheTenantOnItsFirstRequest(t *testing.T) {
	g := newGatewayRig(t, memory.NewTenants())

	g.run(g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-new", "n1"))

	tn, err := g.tenants.GetTenant(context.Background(), "tenant-new")
	if err != nil {
		t.Fatalf("the tenant was not created: %v", err)
	}
	if tn.Plan != domain.PlanPro || !tn.Active() {
		t.Errorf("tenant = %+v, want an active tenant on the configured plan", tn)
	}
}

func TestGatewayLeavesAnExistingTenantAsItIs(t *testing.T) {
	tenants := memory.NewTenants()
	existing, _ := domain.NewTenant("tenant-a", "Acme", domain.PlanTeam, time.Now())
	_ = tenants.SaveTenant(context.Background(), existing)
	g := newGatewayRig(t, tenants)

	g.run(g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1"))

	tn, _ := tenants.GetTenant(context.Background(), "tenant-a")
	if tn.Plan != domain.PlanTeam || tn.Name != "Acme" {
		t.Errorf("tenant = %+v, want the plan and name it already had", tn)
	}
}

func TestGatewayRefusesASuspendedTenant(t *testing.T) {
	tenants := memory.NewTenants()
	suspended, _ := domain.NewTenant("tenant-a", "Acme", domain.PlanPro, time.Now())
	suspended.Status = domain.TenantSuspended
	_ = tenants.SaveTenant(context.Background(), suspended)
	g := newGatewayRig(t, tenants)

	if code, _ := g.run(g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1")); code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

func TestGatewayRefusesWhatTheGatewayDidNotSign(t *testing.T) {
	cases := map[string]func(g *gatewayRig) *http.Request{
		"no signature": func(g *gatewayRig) *http.Request {
			r := g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1")
			r.Header.Del("X-Loop-Runtime-Signature")
			return r
		},
		"another path": func(g *gatewayRig) *http.Request {
			return g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes/sbx_1", "tenant-a", "n1")
		},
		"another tenant": func(g *gatewayRig) *http.Request {
			r := g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1")
			r.Header.Set("X-Loop-Runtime-Tenant", "tenant-b")
			return r
		},
		"empty tenant": func(g *gatewayRig) *http.Request {
			r := g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1")
			r.Header.Set("X-Loop-Runtime-Tenant", "")
			return r
		},
		"a key the sandbox does not trust": func(g *gatewayRig) *http.Request {
			_, other, _ := ed25519.GenerateKey(nil)
			forged := *g
			forged.priv = other
			return forged.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1")
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGatewayRig(t, memory.NewTenants())

			code, p := g.run(build(g))

			if code != http.StatusUnauthorized || p != nil {
				t.Errorf("status = %d, principal = %+v; want 401 and no principal", code, p)
			}
			if _, err := g.tenants.GetTenant(context.Background(), "tenant-a"); err == nil {
				t.Error("a refused request created a tenant")
			}
		})
	}
}

func TestGatewayRefusesAReplayedForward(t *testing.T) {
	g := newGatewayRig(t, memory.NewTenants())
	first := g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1")
	replay := first.Clone(context.Background())

	if code, _ := g.run(first); code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", code)
	}
	if code, _ := g.run(replay); code != http.StatusUnauthorized {
		t.Errorf("replayed request: status = %d, want 401", code)
	}
}

type failingTenants struct{ domain.TenantStore }

func (failingTenants) GetTenant(context.Context, string) (*domain.Tenant, error) {
	return nil, errors.New("connection refused")
}

func TestGatewayAnswers503WhenTheTenantStoreFails(t *testing.T) {
	// A valid caller must not be told its credential is wrong during an outage.
	g := newGatewayRig(t, failingTenants{memory.NewTenants()})

	if code, _ := g.run(g.signed(t, "GET", "/v1/sandboxes", "/v1/sandboxes", "tenant-a", "n1")); code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
}

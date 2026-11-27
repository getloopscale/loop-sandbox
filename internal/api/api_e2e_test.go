package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tcplan "github.com/getloopscale/loop-sandbox/internal/plan"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/metrics"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
	"github.com/getloopscale/loop-sandbox/internal/secretbox"
)

// stubAuth is a test double for the TenantAuthenticator port: "key-a" → tenant-a,
// "key-b" → tenant-b, anything else → unauthorized.
type stubAuth struct{}

func (stubAuth) Authenticate(_ context.Context, raw string) (*auth.Principal, error) {
	switch strings.TrimPrefix(strings.TrimSpace(raw), "Bearer ") {
	case "key-a":
		return &auth.Principal{TenantID: "tenant-a", KeyID: "ka", Scopes: []string{"sandbox:execute"}}, nil
	case "key-b":
		return &auth.Principal{TenantID: "tenant-b", KeyID: "kb", Scopes: []string{"sandbox:execute"}}, nil
	case "key-admin": // tenant-a, may also manage API keys
		return &auth.Principal{TenantID: "tenant-a", KeyID: "kadmin", Scopes: []string{"sandbox:execute", "api_keys:manage"}}, nil
	case "key-admin-b":
		return &auth.Principal{TenantID: "tenant-b", KeyID: "kadminb", Scopes: []string{"sandbox:execute", "api_keys:manage"}}, nil
	case "key-secrets": // tenant-a, may also manage secrets
		return &auth.Principal{TenantID: "tenant-a", KeyID: "ksec", Scopes: []string{"sandbox:execute", "secrets:manage"}}, nil
	case "key-secrets-b":
		return &auth.Principal{TenantID: "tenant-b", KeyID: "ksecb", Scopes: []string{"sandbox:execute", "secrets:manage"}}, nil
	case "key-keys-only": // tenant-a, manages keys and nothing else
		return &auth.Principal{TenantID: "tenant-a", KeyID: "kkeys", Scopes: []string{"api_keys:manage"}}, nil
	case "key-noscope":
		return &auth.Principal{TenantID: "tenant-a", KeyID: "knone"}, nil
	default:
		return nil, auth.ErrUnauthorized
	}
}

type noAudit struct{}

func (noAudit) Record(_, _, _, _, _, _ string, _ time.Time) {}

func newServer() http.Handler {
	srv, _ := newServerWithMetrics()
	return srv
}

// newServerScanner builds a server whose template registry uses the given scanner (M17).
func newServerScanner(scanner app.TemplateScanner) http.Handler {
	store := memory.New()
	svc := app.NewService(store, fake.NewWithSessions(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	svc.SetVolumeStore(store)
	svc.SetTemplateRegistry(store, scanner)
	h := api.NewHandler(svc, memory.NewIdempotency())
	return api.NewRouter(h, auth.Middleware(stubAuth{}), nil)
}

// newServerWithMetrics also wires a Prometheus registry + observer so tests can
// scrape /metrics and assert the counters moved.
func newServerWithMetrics() (http.Handler, *prometheus.Registry) {
	store := memory.New()
	svc := app.NewService(store, fake.NewWithSessions(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	svc.SetVolumeStore(store) // M16 durable volumes
	svc.SetSnapshotStore(store)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		panic(err) // a 32 byte literal: New cannot refuse it
	}
	svc.SetSecrets(store, box)
	svc.SetAPIKeyStore(memory.NewAPIKeys())
	svc.SetTemplateRegistry(store, stubScanner{ok: true}) // M17 template registry
	reg := prometheus.NewRegistry()
	svc.SetObserver(metrics.New(reg))
	h := api.NewHandler(svc, memory.NewIdempotency())
	return api.NewRouter(h, auth.Middleware(stubAuth{}), reg), reg
}

func post(srv http.Handler, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func doGet(srv http.Handler, id, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+id, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func createSandbox(t *testing.T, srv http.Handler, key string) string {
	t.Helper()
	rec := post(srv, key, `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	return created.ID
}

func TestE2E_create_get_destroy(t *testing.T) {
	srv := newServer()

	rec := post(srv, "key-a", `{"image":"python:3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("missing X-Request-ID")
	}
	var created struct{ ID, State string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.State != "running" {
		t.Errorf("state = %q, want running", created.State)
	}

	if got := doGet(srv, created.ID, "key-a"); got.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", got.Code)
	}

	del := httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+created.ID, nil)
	del.Header.Set("Authorization", "Bearer key-a")
	drec := httptest.NewRecorder()
	srv.ServeHTTP(drec, del)
	if drec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", drec.Code)
	}
	if got := doGet(srv, created.ID, "key-a"); got.Code != http.StatusNotFound {
		t.Fatalf("GET after destroy = %d, want 404", got.Code)
	}
}

func TestE2E_unauthorized(t *testing.T) {
	srv := newServer()
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/sandboxes"},
		{http.MethodGet, "/v1/sandboxes/x"},
		{http.MethodDelete, "/v1/sandboxes/x"},
	} {
		// no key
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s (no key) = %d, want 401", tc.method, tc.path, rec.Code)
		}
		// invalid key
		req = httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer nope")
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s (bad key) = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestE2E_cross_tenant_get_and_delete_are_404(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")

	// tenant-b GET → 404 (US-05)
	if got := doGet(srv, id, "key-b"); got.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant GET = %d, want 404", got.Code)
	}
	// tenant-b DELETE → 404, and tenant-a's sandbox survives (US-05)
	del := httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+id, nil)
	del.Header.Set("Authorization", "Bearer key-b")
	drec := httptest.NewRecorder()
	srv.ServeHTTP(drec, del)
	if drec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant DELETE = %d, want 404", drec.Code)
	}
	if got := doGet(srv, id, "key-a"); got.Code != http.StatusOK {
		t.Errorf("tenant-a sandbox affected by cross-tenant delete: %d", got.Code)
	}
}

func TestE2E_malformed_json_is_400(t *testing.T) {
	rec := post(newServer(), "key-a", `{"image":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestE2E_idempotency proves TRD §6.3: same key + same payload replays the first
// response; same key + different payload → 409 conflict.
func TestE2E_idempotency(t *testing.T) {
	srv := newServer()

	first := postIdem(srv, "key-a", "idem-1", `{"image":"python:3"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d, want 201", first.Code)
	}
	var f struct{ ID string }
	_ = json.Unmarshal(first.Body.Bytes(), &f)

	// same key + same payload → replay identical id, no new sandbox
	replayRec := postIdem(srv, "key-a", "idem-1", `{"image":"python:3"}`)
	if replayRec.Code != http.StatusCreated {
		t.Fatalf("replay = %d, want 201", replayRec.Code)
	}
	var r struct{ ID string }
	_ = json.Unmarshal(replayRec.Body.Bytes(), &r)
	if r.ID != f.ID {
		t.Errorf("replay id = %q, want same as first %q", r.ID, f.ID)
	}
	if replayRec.Header().Get("Idempotent-Replay") != "true" {
		t.Error("missing Idempotent-Replay header on replay")
	}

	// same key + DIFFERENT payload → 409 conflict
	conflict := postIdem(srv, "key-a", "idem-1", `{"image":"node:20"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict = %d, want 409", conflict.Code)
	}
}

func postIdem(srv http.Handler, key, idemKey, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", idemKey)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestE2E_idempotency_concurrent_same_key_creates_one proves the reserve-before-act
// fix: N concurrent requests with the same key+payload create exactly ONE sandbox.
// All 201 responses carry the same id; the rest are 409 (in-flight). No double-create.
func TestE2E_idempotency_concurrent_same_key_creates_one(t *testing.T) {
	srv := newServer()
	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[string]struct{}{}
	codes := map[int]int{}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postIdem(srv, "key-a", "race-1", `{"image":"python:3"}`)
			mu.Lock()
			codes[rec.Code]++
			if rec.Code == http.StatusCreated {
				var b struct{ ID string }
				_ = json.Unmarshal(rec.Body.Bytes(), &b)
				ids[b.ID] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(ids) != 1 {
		t.Fatalf("distinct created ids = %d, want exactly 1 (no double-create); codes=%v", len(ids), codes)
	}
	if codes[http.StatusCreated]+codes[http.StatusConflict] != n {
		t.Errorf("codes=%v; every response must be 201 (replay/create) or 409 (in-flight)", codes)
	}
}

// TestE2E_idempotency_is_tenant_scoped proves tenant-b reusing tenant-a's key gets
// a fresh sandbox, never a replay of tenant-a's response (no cross-tenant leak).
func TestE2E_idempotency_is_tenant_scoped(t *testing.T) {
	srv := newServer()
	a := postIdem(srv, "key-a", "shared", `{"image":"python:3"}`)
	var aBody struct{ ID string }
	_ = json.Unmarshal(a.Body.Bytes(), &aBody)

	b := postIdem(srv, "key-b", "shared", `{"image":"python:3"}`)
	if b.Code != http.StatusCreated {
		t.Fatalf("tenant-b = %d, want 201", b.Code)
	}
	if b.Header().Get("Idempotent-Replay") == "true" {
		t.Fatal("tenant-b got a REPLAY of tenant-a's idempotency record — cross-tenant leak")
	}
	var bBody struct {
		ID       string `json:"id"`
		TenantID string `json:"tenant_id"`
	}
	_ = json.Unmarshal(b.Body.Bytes(), &bBody)
	if bBody.ID == aBody.ID || bBody.TenantID != "tenant-b" {
		t.Errorf("tenant-b response leaked tenant-a data: %+v", bBody)
	}
}

func TestE2E_malformed_json_with_idem_key_stores_nothing(t *testing.T) {
	srv := newServer()
	// malformed body + idem key → 400, and NO reservation persisted...
	if rec := postIdem(srv, "key-a", "idem-bad", `{"image":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed = %d, want 400", rec.Code)
	}
	// ...so a subsequent valid request with the same key is a normal create (not replay/conflict).
	rec := postIdem(srv, "key-a", "idem-bad", `{"image":"python:3"}`)
	if rec.Code != http.StatusCreated || rec.Header().Get("Idempotent-Replay") == "true" {
		t.Errorf("after malformed, valid request = %d (replay=%q), want fresh 201",
			rec.Code, rec.Header().Get("Idempotent-Replay"))
	}
}

// TestE2E_metrics_endpoint_reflects_activity proves the observability baseline:
// after a create the /metrics endpoint exposes the incremented counter (public,
// no auth) so ops can scrape provisioning throughput and latency.
func TestE2E_metrics_endpoint_reflects_activity(t *testing.T) {
	srv, _ := newServerWithMetrics()
	rec := post(srv, "key-a", `{"image":"python:3"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", rec.Code)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// A successful exec (echo) → result="ok"; a non-zero exit (cat missing file) →
	// result="nonzero_exit" — proving the two exec label paths fire through real HTTP.
	base := "/v1/sandboxes/" + created.ID + "/executions"
	if r := req(srv, http.MethodPost, base, "key-a", []byte(`{"cmd":["echo","hi"]}`)); r.Code != http.StatusOK {
		t.Fatalf("exec echo = %d, want 200", r.Code)
	}
	if r := req(srv, http.MethodPost, base, "key-a", []byte(`{"cmd":["cat","/workspace/nope"]}`)); r.Code != http.StatusOK {
		t.Fatalf("exec cat-missing = %d, want 200 (US-02 non-zero exit)", r.Code)
	}

	mreq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mrec := httptest.NewRecorder()
	srv.ServeHTTP(mrec, mreq)
	if mrec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", mrec.Code)
	}
	body := mrec.Body.String()
	for _, want := range []string{
		"loopsandbox_sandboxes_created_total 1",
		`loopsandbox_executions_total{result="ok"} 1`,
		`loopsandbox_executions_total{result="nonzero_exit"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q; body:\n%s", want, body)
		}
	}
}

// TestE2E_templates_lists_official_catalog proves GET /v1/templates is public and
// returns the built-in catalog (≥3 vetted images) for sandbox-image discovery.
func TestE2E_templates_lists_official_catalog(t *testing.T) {
	srv := newServer()
	// M17: listing templates requires a key (it merges the tenant's own + public templates
	// with the built-in catalog).
	req := httptest.NewRequest(http.MethodGet, "/v1/templates", nil)
	req.Header.Set("Authorization", "Bearer key-a")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/templates = %d, want 200", rec.Code)
	}
	var body struct {
		Templates []struct {
			Name, Image, Description string
		} `json:"templates"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Templates) < 3 {
		t.Errorf("templates = %d, want >= 3", len(body.Templates))
	}
}

// stubPlanUsage wires a fixed plan + usage for the quota E2E.
type stubPlanUsage struct {
	tier  tcplan.Plan
	value int64
}

func (s stubPlanUsage) PlanFor(context.Context, string) (tcplan.Plan, error) { return s.tier, nil }
func (s stubPlanUsage) Value(context.Context, string, string, int64) (int64, error) {
	return s.value, nil
}

// TestE2E_quota_exceeded_returns_402 proves plan enforcement through the real
// handler: an over-cap tenant gets HTTP 402 with the LoopScale ExceededError context, the
// quota_denied metric moves, and an over-cap create with an Idempotency-Key RELEASES
// the reservation (a retry re-evaluates → 402 again, never a stuck 409-in-progress).
func TestE2E_quota_exceeded_returns_402(t *testing.T) {
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	reg := prometheus.NewRegistry()
	svc.SetObserver(metrics.New(reg))
	// starter creations cap = 50; usage already at 50 → next create is 402.
	su := stubPlanUsage{tier: tcplan.PlanStarter, value: 50}
	svc.SetEnforcer(enforcement.New(su), su)
	h := api.NewHandler(svc, memory.NewIdempotency())
	srv := api.NewRouter(h, auth.Middleware(stubAuth{}), reg)

	rec := post(srv, "key-a", `{"image":"python:3"}`)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("over-cap create = %d, want 402; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Code != "quota_exceeded" {
		t.Errorf("error code = %q, want quota_exceeded", body.Error.Code)
	}

	// The quota_denied counter must reflect the denial (billing/ops visibility).
	mrec := httptest.NewRecorder()
	srv.ServeHTTP(mrec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(mrec.Body.String(), `loopsandbox_quota_denied_total{metric="sandbox_creations"} 1`) {
		t.Errorf("/metrics missing quota_denied for sandbox_creations; body:\n%s", mrec.Body.String())
	}

	// H1: an over-cap create with an Idempotency-Key must RELEASE the reservation so
	// a retry re-evaluates (402), not get stuck as in-progress (409).
	if r := postIdem(srv, "key-a", "idem-q", `{"image":"python:3"}`); r.Code != http.StatusPaymentRequired {
		t.Fatalf("keyed over-cap create = %d, want 402", r.Code)
	}
	if r := postIdem(srv, "key-a", "idem-q", `{"image":"python:3"}`); r.Code != http.StatusPaymentRequired {
		t.Errorf("retry after 402 = %d, want 402 (reservation released, not stuck 409)", r.Code)
	}
}

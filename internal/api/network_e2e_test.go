package api_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

func newNetworkServer(driver *fake.Driver) http.Handler {
	svc := app.NewService(memory.New(), driver, clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	return api.NewRouter(api.NewHandler(svc, memory.NewIdempotency()), auth.Middleware(stubAuth{}), nil)
}

func send(srv http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

type networkJSON struct {
	AllowOut []string `json:"allow_out"`
	DenyOut  []string `json:"deny_out"`
}

type sandboxJSON struct {
	ID      string       `json:"id"`
	Network *networkJSON `json:"network"`
}

const restricted = `{"image":"busybox:latest","network":{"allow_out":["pypi.org"],"deny_out":["0.0.0.0/0"]}}`

func TestCreateWithNetworkRulesReachesTheDriverAndIsReturned(t *testing.T) {
	driver := fake.New().EnableNetwork()
	srv := newNetworkServer(driver)

	created := decode[sandboxJSON](t, post(srv, "key-a", restricted), http.StatusCreated)
	if created.Network == nil || len(created.Network.AllowOut) != 1 || created.Network.AllowOut[0] != "pypi.org" ||
		len(created.Network.DenyOut) != 1 || created.Network.DenyOut[0] != "0.0.0.0/0" {
		t.Errorf("create response network = %+v", created.Network)
	}
	if got := driver.NetworkPolicy(runtime.SandboxID(created.ID)); len(got.AllowOut) != 1 || got.AllowOut[0] != "pypi.org" || len(got.DenyOut) != 1 {
		t.Errorf("the driver received %+v", got)
	}
	fetched := decode[sandboxJSON](t, doGet(srv, created.ID, "key-a"), http.StatusOK)
	if fetched.Network == nil || fetched.Network.AllowOut[0] != "pypi.org" {
		t.Errorf("GET network = %+v", fetched.Network)
	}
}

func TestSandboxWithoutRulesHasNoNetworkField(t *testing.T) {
	srv := newNetworkServer(fake.New().EnableNetwork())

	created := decode[sandboxJSON](t, post(srv, "key-a", `{"image":"busybox:latest"}`), http.StatusCreated)

	if created.Network != nil {
		t.Errorf("network = %+v, want it absent", created.Network)
	}
}

func TestCreateWithInvalidNetworkRulesIs400AndCreatesNothing(t *testing.T) {
	srv := newNetworkServer(fake.New().EnableNetwork())

	for name, body := range map[string]string{
		"host name in deny_out": `{"image":"busybox:latest","network":{"deny_out":["example.com"]}}`,
		"malformed CIDR":        `{"image":"busybox:latest","network":{"allow_out":["10.0.0.0/99"]}}`,
		"with a template":       `{"template":"python","network":{"deny_out":["0.0.0.0/0"]}}`,
	} {
		if rec := post(srv, "key-a", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	list := send(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")
	if got := decode[struct {
		Sandboxes []sandboxJSON `json:"sandboxes"`
	}](t, list, http.StatusOK); len(got.Sandboxes) != 0 {
		t.Errorf("%d sandboxes exist after refused creations", len(got.Sandboxes))
	}
}

func TestNetworkRulesOnARuntimeWithoutNetworksAre501(t *testing.T) {
	srv := newNetworkServer(fake.New())

	if rec := post(srv, "key-a", restricted); rec.Code != http.StatusNotImplemented {
		t.Errorf("create with rules = %d, want 501: %s", rec.Code, rec.Body.String())
	}
	id := createSandbox(t, srv, "key-a")
	if rec := send(srv, http.MethodPut, "/v1/sandboxes/"+id+"/network", "key-a", `{"deny_out":["0.0.0.0/0"]}`); rec.Code != http.StatusNotImplemented {
		t.Errorf("update = %d, want 501", rec.Code)
	}
	if rec := send(srv, http.MethodGet, "/v1/sandboxes/"+id+"/network/denials", "key-a", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("denials = %d, want 501", rec.Code)
	}
}

func TestUpdateNetworkReplacesTheRules(t *testing.T) {
	driver := fake.New().EnableNetwork()
	srv := newNetworkServer(driver)
	id := decode[sandboxJSON](t, post(srv, "key-a", restricted), http.StatusCreated).ID

	rec := send(srv, http.MethodPut, "/v1/sandboxes/"+id+"/network", "key-a", `{"allow_out":["*.example.org"],"deny_out":["10.0.0.0/8","0.0.0.0/0"]}`)
	if got := decode[networkJSON](t, rec, http.StatusOK); len(got.AllowOut) != 1 || got.AllowOut[0] != "*.example.org" || len(got.DenyOut) != 2 {
		t.Errorf("update response = %+v", got)
	}
	if got := driver.NetworkPolicy(runtime.SandboxID(id)); got.AllowOut[0] != "*.example.org" {
		t.Errorf("the driver has %+v", got)
	}
	if got := decode[sandboxJSON](t, doGet(srv, id, "key-a"), http.StatusOK); got.Network == nil || got.Network.AllowOut[0] != "*.example.org" {
		t.Errorf("GET after update = %+v", got.Network)
	}

	// Empty lists restore the default policy, and the field goes away.
	if rec := send(srv, http.MethodPut, "/v1/sandboxes/"+id+"/network", "key-a", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("update to no rules = %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode[sandboxJSON](t, doGet(srv, id, "key-a"), http.StatusOK); got.Network != nil {
		t.Errorf("GET after clearing = %+v, want no network field", got.Network)
	}
}

func TestUpdateNetworkRefusesInvalidRulesAndAnotherTenant(t *testing.T) {
	driver := fake.New().EnableNetwork()
	srv := newNetworkServer(driver)
	id := decode[sandboxJSON](t, post(srv, "key-a", restricted), http.StatusCreated).ID

	if rec := send(srv, http.MethodPut, "/v1/sandboxes/"+id+"/network", "key-a", `{"deny_out":["example.com"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid rules = %d, want 400", rec.Code)
	}
	if rec := send(srv, http.MethodPut, "/v1/sandboxes/"+id+"/network", "key-b", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("another tenant = %d, want 404", rec.Code)
	}
	if rec := send(srv, http.MethodGet, "/v1/sandboxes/"+id+"/network/denials", "key-b", ""); rec.Code != http.StatusNotFound {
		t.Errorf("another tenant's denials = %d, want 404", rec.Code)
	}
	if got := driver.NetworkPolicy(runtime.SandboxID(id)); len(got.AllowOut) != 1 || got.AllowOut[0] != "pypi.org" {
		t.Errorf("the rules changed to %+v", got)
	}
}

func TestNetworkDenialsAreListedOldestFirstWithALimit(t *testing.T) {
	driver := fake.New().EnableNetwork()
	srv := newNetworkServer(driver)
	id := decode[sandboxJSON](t, post(srv, "key-a", restricted), http.StatusCreated).ID
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	driver.RecordDenial(runtime.SandboxID(id), runtime.NetworkDenial{Time: at, Destination: "1.1.1.1:443", Host: "a.example", Reason: "denied by deny_out"})
	driver.RecordDenial(runtime.SandboxID(id), runtime.NetworkDenial{Time: at.Add(time.Second), Destination: "10.0.0.1:80", Reason: "private address"})

	type denial struct {
		Time, Destination, Host, Reason string
	}
	type body struct {
		Denials []denial `json:"denials"`
	}
	all := decode[body](t, send(srv, http.MethodGet, "/v1/sandboxes/"+id+"/network/denials", "key-a", ""), http.StatusOK)
	if len(all.Denials) != 2 || all.Denials[0].Host != "a.example" || all.Denials[0].Time != "2026-11-27T10:00:00Z" || all.Denials[1].Destination != "10.0.0.1:80" {
		t.Errorf("denials = %+v", all.Denials)
	}
	last := decode[body](t, send(srv, http.MethodGet, "/v1/sandboxes/"+id+"/network/denials?limit=1", "key-a", ""), http.StatusOK)
	if len(last.Denials) != 1 || last.Denials[0].Destination != "10.0.0.1:80" {
		t.Errorf("limit=1 = %+v, want the most recent", last.Denials)
	}
	for _, bad := range []string{"0", "-1", "1001", "x"} {
		if rec := send(srv, http.MethodGet, "/v1/sandboxes/"+id+"/network/denials?limit="+bad, "key-a", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%s: status = %d, want 400", bad, rec.Code)
		}
	}
}

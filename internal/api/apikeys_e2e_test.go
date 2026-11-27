package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

type apiKeyBody struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Prefix    string   `json:"prefix"`
	Scopes    []string `json:"scopes"`
	RevokedAt string   `json:"revoked_at"`
	Key       string   `json:"key"`
}

type apiKeyListBody struct {
	APIKeys []apiKeyBody `json:"api_keys"`
}

// newKeyServer serves the API with the real API key authenticator over
// in-memory stores, and returns a bootstrap key of tenant-a that holds both
// scopes, as an operator would create with sandboxctl.
func newKeyServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	keys, tenants, store := memory.NewAPIKeys(), memory.NewTenants(), memory.New()
	now := time.Unix(0, 0).UTC()
	for _, id := range []string{"tenant-a", "tenant-b"} {
		tn, err := domain.NewTenant(id, id, domain.PlanStarter, now)
		if err != nil {
			t.Fatalf("NewTenant: %v", err)
		}
		if err := tenants.SaveTenant(context.Background(), tn); err != nil {
			t.Fatalf("SaveTenant: %v", err)
		}
	}
	raw, key, err := domain.GenerateAPIKey("key_bootstrap", "tenant-a", "bootstrap",
		[]string{domain.ScopeSandboxExecute, domain.ScopeAPIKeysManage}, now)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if err := keys.SaveAPIKey(context.Background(), key); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}
	svc := app.NewService(store, fake.New(), clock.Fixed{T: now}, api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	svc.SetAPIKeyStore(keys)
	h := api.NewHandler(svc, nil)
	return api.NewRouter(h, auth.Middleware(auth.NewAPIKeyAuthenticator(keys, tenants)), nil), raw
}

func createKey(t *testing.T, srv http.Handler, caller, body string) apiKeyBody {
	t.Helper()
	return decode[apiKeyBody](t, call(srv, http.MethodPost, "/v1/api-keys", caller, body), http.StatusCreated)
}

func TestCreatedKeyAuthenticatesAndItsSecretIsShownOnce(t *testing.T) {
	srv, admin := newKeyServer(t)

	created := createKey(t, srv, admin, `{"name":"ci"}`)
	rec := post(srv, created.Key, `{}`)
	list := call(srv, http.MethodGet, "/v1/api-keys", admin, "")

	if !strings.HasPrefix(created.Key, "lsb_") || created.Name != "ci" {
		t.Fatalf("created = %+v, want a key starting with lsb_ named ci", created)
	}
	if len(created.Scopes) != 1 || created.Scopes[0] != "sandbox:execute" {
		t.Errorf("default scopes = %v, want [sandbox:execute]", created.Scopes)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("create sandbox with the new key = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(list.Body.String(), created.Key) || strings.Contains(list.Body.String(), `"key"`) {
		t.Errorf("the list response exposes a secret: %s", list.Body.String())
	}
}

func TestCreateKeyResponseIsNotCacheable(t *testing.T) {
	srv, admin := newKeyServer(t)

	rec := call(srv, http.MethodPost, "/v1/api-keys", admin, `{"name":"ci"}`)

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestRevokedKeyIsRefusedOnTheNextRequest(t *testing.T) {
	srv, admin := newKeyServer(t)
	created := createKey(t, srv, admin, `{"name":"ci"}`)
	if rec := post(srv, created.Key, `{}`); rec.Code != http.StatusCreated {
		t.Fatalf("key does not work before the revoke: %d", rec.Code)
	}

	revoke := call(srv, http.MethodDelete, "/v1/api-keys/"+created.ID, admin, "")
	after := post(srv, created.Key, `{}`)

	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204 (%s)", revoke.Code, revoke.Body.String())
	}
	if after.Code != http.StatusUnauthorized {
		t.Errorf("request with the revoked key = %d, want 401", after.Code)
	}
}

func TestListShowsRevokedKeysWithTheirRevocationTime(t *testing.T) {
	srv, admin := newKeyServer(t)
	created := createKey(t, srv, admin, `{"name":"ci"}`)
	call(srv, http.MethodDelete, "/v1/api-keys/"+created.ID, admin, "")

	list := decode[apiKeyListBody](t, call(srv, http.MethodGet, "/v1/api-keys", admin, ""), http.StatusOK)

	byID := map[string]apiKeyBody{}
	for _, k := range list.APIKeys {
		byID[k.ID] = k
	}
	if len(list.APIKeys) != 2 || byID[created.ID].RevokedAt == "" || byID["key_bootstrap"].RevokedAt != "" {
		t.Errorf("list = %+v, want the bootstrap key active and %s revoked", list.APIKeys, created.ID)
	}
}

func TestRevokingTwiceAnswers204BothTimes(t *testing.T) {
	srv, admin := newKeyServer(t)
	created := createKey(t, srv, admin, `{"name":"ci"}`)

	first := call(srv, http.MethodDelete, "/v1/api-keys/"+created.ID, admin, "")
	second := call(srv, http.MethodDelete, "/v1/api-keys/"+created.ID, admin, "")

	if first.Code != http.StatusNoContent || second.Code != http.StatusNoContent {
		t.Errorf("revokes = %d and %d, want 204 and 204", first.Code, second.Code)
	}
}

func TestRevokingAnUnknownKeyIs404(t *testing.T) {
	srv, admin := newKeyServer(t)

	rec := call(srv, http.MethodDelete, "/v1/api-keys/key_nope", admin, "")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestKeyWithoutTheManageScopeCannotUseTheKeyRoutes(t *testing.T) {
	srv, admin := newKeyServer(t)
	plain := createKey(t, srv, admin, `{"name":"plain"}`)
	cases := map[string]struct{ method, path, body string }{
		"list":   {http.MethodGet, "/v1/api-keys", ""},
		"create": {http.MethodPost, "/v1/api-keys", `{"name":"x"}`},
		"revoke": {http.MethodDelete, "/v1/api-keys/key_bootstrap", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, c.method, c.path, plain.Key, c.body)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
	if rec := post(srv, admin, `{}`); rec.Code != http.StatusCreated {
		t.Errorf("the bootstrap key was affected by a refused revoke: %d", rec.Code)
	}
}

func TestKeyCannotGrantAScopeItDoesNotHold(t *testing.T) {
	srv := newServer()

	rec := call(srv, http.MethodPost, "/v1/api-keys", "key-keys-only", `{"name":"x","scopes":["sandbox:execute"]}`)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestKeyWithoutTheSandboxScopeCannotUseSandboxRoutes(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)
	cases := map[string]struct{ method, path, body string }{
		"create sandbox": {http.MethodPost, "/v1/sandboxes", `{}`},
		"list sandboxes": {http.MethodGet, "/v1/sandboxes", ""},
		"get sandbox":    {http.MethodGet, "/v1/sandboxes/" + sb.ID, ""},
		"exec":           {http.MethodPost, "/v1/sandboxes/" + sb.ID + "/executions", `{"cmd":["true"]}`},
		"destroy":        {http.MethodDelete, "/v1/sandboxes/" + sb.ID, ""},
		"list volumes":   {http.MethodGet, "/v1/volumes", ""},
	}
	for _, key := range []string{"key-keys-only", "key-noscope"} {
		for name, c := range cases {
			t.Run(key+"/"+name, func(t *testing.T) {
				rec := call(srv, c.method, c.path, key, c.body)

				if rec.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
				}
			})
		}
	}
	if rec := doGet(srv, sb.ID, "key-a"); rec.Code != http.StatusOK {
		t.Errorf("sandbox is gone after refused requests: %d", rec.Code)
	}
}

func TestCreateKeyRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"no name":        `{}`,
		"name too long":  `{"name":"` + strings.Repeat("n", 65) + `"}`,
		"unknown scope":  `{"name":"x","scopes":["root"]}`,
		"repeated scope": `{"name":"x","scopes":["sandbox:execute","sandbox:execute"]}`,
		"malformed JSON": `{"name":`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newServer()

			rec := call(srv, http.MethodPost, "/v1/api-keys", "key-admin", body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestKeysOfAnotherTenantAreInvisibleAndCannotBeRevoked(t *testing.T) {
	srv := newServer()
	created := createKey(t, srv, "key-admin", `{"name":"a-key"}`)

	list := decode[apiKeyListBody](t, call(srv, http.MethodGet, "/v1/api-keys", "key-admin-b", ""), http.StatusOK)
	revoke := call(srv, http.MethodDelete, "/v1/api-keys/"+created.ID, "key-admin-b", "")
	mine := decode[apiKeyListBody](t, call(srv, http.MethodGet, "/v1/api-keys", "key-admin", ""), http.StatusOK)

	if len(list.APIKeys) != 0 {
		t.Errorf("tenant-b sees %d keys of tenant-a", len(list.APIKeys))
	}
	if revoke.Code != http.StatusNotFound {
		t.Errorf("cross-tenant revoke = %d, want 404", revoke.Code)
	}
	if len(mine.APIKeys) != 1 || mine.APIKeys[0].RevokedAt != "" {
		t.Errorf("tenant-a's key after a cross-tenant revoke = %+v, want it active", mine.APIKeys)
	}
}

func TestKeyRoutesAnswer501WithoutAKeyStore(t *testing.T) {
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	srv := api.NewRouter(api.NewHandler(svc, nil), auth.Middleware(stubAuth{}), nil)

	rec := call(srv, http.MethodGet, "/v1/api-keys", "key-admin", "")

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 (%s)", rec.Code, rec.Body.String())
	}
}

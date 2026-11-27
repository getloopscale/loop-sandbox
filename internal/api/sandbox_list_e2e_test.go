package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type sandboxBody struct {
	ID        string            `json:"id"`
	TenantID  string            `json:"tenant_id"`
	State     string            `json:"state"`
	ExpiresAt string            `json:"expires_at"`
	Metadata  map[string]string `json:"metadata"`
	Env       map[string]string `json:"env"`
}

type sandboxListBody struct {
	Sandboxes  []sandboxBody `json:"sandboxes"`
	NextCursor string        `json:"next_cursor"`
}

func call(srv http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) T {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, wantStatus, rec.Body.String())
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return out
}

func createWith(t *testing.T, srv http.Handler, key, body string) sandboxBody {
	t.Helper()
	return decode[sandboxBody](t, post(srv, key, body), http.StatusCreated)
}

func listSandboxes(t *testing.T, srv http.Handler, key, query string) sandboxListBody {
	t.Helper()
	return decode[sandboxListBody](t, call(srv, http.MethodGet, "/v1/sandboxes"+query, key, ""), http.StatusOK)
}

func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Message
}

func TestCreatedSandboxReturnsItsMetadataAndEnv(t *testing.T) {
	srv := newServer()

	created := createWith(t, srv, "key-a", `{"metadata":{"team":"search"},"env":{"MODE":"test"}}`)
	got := decode[sandboxBody](t, doGet(srv, created.ID, "key-a"), http.StatusOK)

	if created.Metadata["team"] != "search" || created.Env["MODE"] != "test" {
		t.Errorf("create response: metadata=%v env=%v", created.Metadata, created.Env)
	}
	if got.Metadata["team"] != "search" || got.Env["MODE"] != "test" {
		t.Errorf("get response: metadata=%v env=%v", got.Metadata, got.Env)
	}
}

func TestCreateWithInvalidAttributesIs400NamingTheField(t *testing.T) {
	cases := map[string]struct{ body, field string }{
		"metadata key with a space":  {`{"metadata":{"my key":"v"}}`, "metadata"},
		"env name starting a digit":  {`{"env":{"1VAR":"v"}}`, "env"},
		"env name with equals sign":  {`{"env":{"A=B":"v"}}`, "env"},
		"metadata value far too big": {`{"metadata":{"k":"` + strings.Repeat("v", 300) + `"}}`, "metadata"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newServer()

			rec := post(srv, "key-a", c.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if msg := errorMessage(t, rec); !strings.HasPrefix(msg, c.field+":") {
				t.Errorf("message = %q, want it to start with %q", msg, c.field+":")
			}
			if n := len(listSandboxes(t, srv, "key-a", "").Sandboxes); n != 0 {
				t.Errorf("a rejected create left %d sandboxes", n)
			}
		})
	}
}

func TestListReturnsOnlyTheCallersSandboxes(t *testing.T) {
	srv := newServer()
	mine := createWith(t, srv, "key-a", `{}`)
	createWith(t, srv, "key-b", `{}`)

	list := listSandboxes(t, srv, "key-a", "")

	if len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != mine.ID {
		t.Errorf("list = %+v, want only %s", list.Sandboxes, mine.ID)
	}
}

func TestListOfATenantWithoutSandboxesIsAnEmptyArray(t *testing.T) {
	srv := newServer()

	rec := call(srv, http.MethodGet, "/v1/sandboxes", "key-a", "")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sandboxes":[]`) {
		t.Errorf("status = %d body = %s, want 200 with an empty array", rec.Code, rec.Body.String())
	}
}

func TestListPagesThroughEverySandboxOnce(t *testing.T) {
	srv := newServer()
	want := map[string]bool{}
	for i := 0; i < 7; i++ {
		want[createWith(t, srv, "key-a", `{}`).ID] = true
	}

	seen := map[string]int{}
	pages := 0
	query := "?limit=3"
	for {
		list := listSandboxes(t, srv, "key-a", query)
		pages++
		for _, sb := range list.Sandboxes {
			seen[sb.ID]++
		}
		if list.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not end")
		}
		query = "?limit=3&cursor=" + url.QueryEscape(list.NextCursor)
	}

	if pages != 3 {
		t.Errorf("pages = %d, want 3 (3+3+1)", pages)
	}
	for id := range want {
		if seen[id] != 1 {
			t.Errorf("sandbox %s seen %d times, want 1", id, seen[id])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("saw %d sandboxes, want %d", len(seen), len(want))
	}
}

func TestListFiltersByMetadata(t *testing.T) {
	srv := newServer()
	search := createWith(t, srv, "key-a", `{"metadata":{"team":"search","env":"prod"}}`)
	createWith(t, srv, "key-a", `{"metadata":{"team":"ads","env":"prod"}}`)
	createWith(t, srv, "key-a", `{}`)

	one := listSandboxes(t, srv, "key-a", "?metadata=team%3Dsearch&metadata=env%3Dprod")
	two := listSandboxes(t, srv, "key-a", "?metadata=env%3Dprod")

	if len(one.Sandboxes) != 1 || one.Sandboxes[0].ID != search.ID {
		t.Errorf("team=search,env=prod matched %+v, want only %s", one.Sandboxes, search.ID)
	}
	if len(two.Sandboxes) != 2 {
		t.Errorf("env=prod matched %d sandboxes, want 2", len(two.Sandboxes))
	}
}

func TestListFiltersByState(t *testing.T) {
	srv := newServer()
	running := createWith(t, srv, "key-a", `{}`)
	paused := createWith(t, srv, "key-a", `{}`)
	if rec := call(srv, http.MethodPost, "/v1/sandboxes/"+paused.ID+"/pause", "key-a", ""); rec.Code/100 != 2 {
		t.Fatalf("pause status = %d (%s)", rec.Code, rec.Body.String())
	}

	onlyPaused := listSandboxes(t, srv, "key-a", "?state=paused")
	both := listSandboxes(t, srv, "key-a", "?state=paused,running")

	if len(onlyPaused.Sandboxes) != 1 || onlyPaused.Sandboxes[0].ID != paused.ID {
		t.Errorf("state=paused matched %+v, want only %s", onlyPaused.Sandboxes, paused.ID)
	}
	if len(both.Sandboxes) != 2 {
		t.Errorf("state=paused,running matched %d, want 2 (running is %s)", len(both.Sandboxes), running.ID)
	}
}

func TestListRejectsInvalidQueryParameters(t *testing.T) {
	cases := map[string]string{
		"limit of zero":         "?limit=0",
		"limit above the cap":   "?limit=201",
		"limit not a number":    "?limit=many",
		"unknown state":         "?state=sleeping",
		"metadata without '='":  "?metadata=team",
		"cursor not from here":  "?cursor=not-a-cursor",
		"cursor of another API": "?cursor=e30",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newServer()

			rec := call(srv, http.MethodGet, "/v1/sandboxes"+query, "key-a", "")

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestListWithoutCredentialsIs401(t *testing.T) {
	srv := newServer()

	rec := call(srv, http.MethodGet, "/v1/sandboxes", "", "")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestTimeoutReplacesTheExpiry(t *testing.T) {
	srv := newServer() // the clock is fixed at the Unix epoch
	sb := createWith(t, srv, "key-a", `{"ttl_seconds":60}`)

	updated := decode[sandboxBody](t,
		call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/timeout", "key-a", `{"ttl_seconds":600}`), http.StatusOK)
	got := decode[sandboxBody](t, doGet(srv, sb.ID, "key-a"), http.StatusOK)

	if sb.ExpiresAt != "1970-01-01T00:01:00Z" {
		t.Fatalf("initial expires_at = %s, want 1970-01-01T00:01:00Z", sb.ExpiresAt)
	}
	if updated.ExpiresAt != "1970-01-01T00:10:00Z" || got.ExpiresAt != updated.ExpiresAt {
		t.Errorf("expires_at after timeout = %s (get: %s), want 1970-01-01T00:10:00Z", updated.ExpiresAt, got.ExpiresAt)
	}
}

func TestTimeoutAboveTheMaximumIsCappedAtOneHour(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)

	updated := decode[sandboxBody](t,
		call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/timeout", "key-a", `{"ttl_seconds":86400}`), http.StatusOK)

	if updated.ExpiresAt != "1970-01-01T01:00:00Z" {
		t.Errorf("expires_at = %s, want 1970-01-01T01:00:00Z", updated.ExpiresAt)
	}
}

func TestTimeoutRejectsANonPositiveValue(t *testing.T) {
	for _, body := range []string{`{"ttl_seconds":0}`, `{"ttl_seconds":-5}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			srv := newServer()
			sb := createWith(t, srv, "key-a", `{}`)

			rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/timeout", "key-a", body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTimeoutOnAnotherTenantsSandboxIs404(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{"ttl_seconds":60}`)

	rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/timeout", "key-b", `{"ttl_seconds":600}`)
	got := decode[sandboxBody](t, doGet(srv, sb.ID, "key-a"), http.StatusOK)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got.ExpiresAt != sb.ExpiresAt {
		t.Errorf("expires_at changed to %s by another tenant", got.ExpiresAt)
	}
}

func TestForkInheritsMetadataAndEnv(t *testing.T) {
	srv := newServer()
	parent := createWith(t, srv, "key-a", `{"metadata":{"team":"search"},"env":{"MODE":"test"}}`)

	rec := call(srv, http.MethodPost, fmt.Sprintf("/v1/sandboxes/%s/fork", parent.ID), "key-a", "")
	fork := decode[sandboxBody](t, rec, http.StatusCreated)

	if fork.Metadata["team"] != "search" || fork.Env["MODE"] != "test" {
		t.Errorf("fork metadata=%v env=%v, want the parent's", fork.Metadata, fork.Env)
	}
}

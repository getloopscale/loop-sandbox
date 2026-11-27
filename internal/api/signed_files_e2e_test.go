package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
	"github.com/getloopscale/loop-sandbox/internal/signedurl"
)

// signedServer is an API with signed file URLs on and a clock the test moves.
type signedServer struct {
	http.Handler
	now time.Time
}

func newSignedServer(t *testing.T, publicURL string) *signedServer {
	t.Helper()
	s := &signedServer{now: time.Unix(1_700_000_000, 0).UTC()}
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: s.now},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	h := api.NewHandler(svc, nil)
	signer, err := signedurl.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	h.SetURLSigner(signer, publicURL, func() time.Time { return s.now })
	s.Handler = api.NewRouter(h, auth.Middleware(stubAuth{}), nil)
	return s
}

type fileURL struct {
	URL       string `json:"url"`
	Method    string `json:"method"`
	ExpiresAt string `json:"expires_at"`
}

// path is the URL without scheme and host, which is what the test server routes.
func (u fileURL) path() string {
	return u.URL[strings.Index(u.URL, "/v1/"):]
}

func issue(t *testing.T, srv http.Handler, id, body string) fileURL {
	t.Helper()
	return decode[fileURL](t, call(srv, http.MethodPost, "/v1/sandboxes/"+id+"/file-urls", "key-a", body), http.StatusCreated)
}

func TestSignedUploadStoresTheFileWithoutAnAPIKey(t *testing.T) {
	srv := newSignedServer(t, "")
	sb := createWith(t, srv, "key-a", `{}`)
	u := issue(t, srv, sb.ID, `{"path":"/workspace/in/data.csv","operation":"upload"}`)

	rec := call(srv, http.MethodPut, u.path(), "", "a,b\n1,2\n")

	if u.Method != http.MethodPut || rec.Code != http.StatusNoContent {
		t.Fatalf("method = %s, upload = %d (%s); want PUT and 204", u.Method, rec.Code, rec.Body.String())
	}
	listing := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files?path=/workspace/in", "key-a", "")
	stored := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/in/data.csv", "key-a", "")
	if !strings.Contains(listing.Body.String(), "data.csv") || stored.Body.String() != "a,b\n1,2\n" {
		t.Errorf("listing = %s, stored = %q; want data.csv with the uploaded bytes", listing.Body.String(), stored.Body.String())
	}
}

func TestSignedDownloadReturnsTheFileAsAnAttachment(t *testing.T) {
	srv := newSignedServer(t, "")
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/out/page.html", "<script>alert(1)</script>")
	u := issue(t, srv, sb.ID, `{"path":"/workspace/out/page.html","operation":"download"}`)

	rec := call(srv, http.MethodGet, u.path(), "", "")

	if u.Method != http.MethodGet || rec.Code != http.StatusOK || rec.Body.String() != "<script>alert(1)</script>" {
		t.Fatalf("method = %s, download = %d %q", u.Method, rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Content-Type") != "application/octet-stream" || h.Get("Content-Disposition") != `attachment; filename="page.html"` ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v, want an octet-stream attachment that is not sniffed or cached", h)
	}
}

func TestSignedURLExpires(t *testing.T) {
	srv := newSignedServer(t, "")
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/a.txt", "x")
	u := issue(t, srv, sb.ID, `{"path":"/workspace/a.txt","operation":"download","expires_in_seconds":60}`)
	srv.now = srv.now.Add(59 * time.Second)
	before := call(srv, http.MethodGet, u.path(), "", "")

	srv.now = srv.now.Add(time.Second)
	after := call(srv, http.MethodGet, u.path(), "", "")

	if before.Code != http.StatusOK || after.Code != http.StatusForbidden {
		t.Errorf("one second before the expiry = %d, at the expiry = %d; want 200 and 403", before.Code, after.Code)
	}
}

func TestSignedURLDefaultsToFiveMinutes(t *testing.T) {
	srv := newSignedServer(t, "")
	sb := createWith(t, srv, "key-a", `{}`)

	u := issue(t, srv, sb.ID, `{"path":"/workspace/a.txt","operation":"upload"}`)

	if want := srv.now.Add(5 * time.Minute).Format(time.RFC3339); u.ExpiresAt != want {
		t.Errorf("expires_at = %s, want %s", u.ExpiresAt, want)
	}
}

func TestSignedURLIsRefusedWhenAlteredOrUsedForTheOtherOperation(t *testing.T) {
	srv := newSignedServer(t, "")
	sb := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, sb.ID, "/workspace/a.txt", "keep")
	down := issue(t, srv, sb.ID, `{"path":"/workspace/a.txt","operation":"download"}`)
	up := issue(t, srv, sb.ID, `{"path":"/workspace/a.txt","operation":"upload"}`)
	payload, sig, _ := strings.Cut(strings.TrimPrefix(up.path(), "/v1/signed-files/"), ".")
	otherPayload, _, _ := strings.Cut(strings.TrimPrefix(down.path(), "/v1/signed-files/"), ".")
	cases := map[string]struct{ method, path string }{
		"a download URL used to upload":   {http.MethodPut, down.path()},
		"an upload URL used to download":  {http.MethodGet, up.path()},
		"another payload, same signature": {http.MethodPut, "/v1/signed-files/" + otherPayload + "." + sig},
		"a changed signature":             {http.MethodPut, "/v1/signed-files/" + payload + "." + flipFirst(sig)},
		"no signature":                    {http.MethodPut, "/v1/signed-files/" + payload},
		"garbage":                         {http.MethodGet, "/v1/signed-files/not-a-token"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, c.method, c.path, "", "overwritten")

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
	if got := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/a.txt", "key-a", ""); got.Body.String() != "keep" {
		t.Errorf("the file was changed by a refused request: %q", got.Body.String())
	}
}

func TestSignedURLStopsWorkingWhenTheSandboxIsDestroyed(t *testing.T) {
	srv := newSignedServer(t, "")
	sb := createWith(t, srv, "key-a", `{}`)
	u := issue(t, srv, sb.ID, `{"path":"/workspace/a.txt","operation":"upload"}`)
	if rec := call(srv, http.MethodDelete, "/v1/sandboxes/"+sb.ID, "key-a", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("destroy = %d", rec.Code)
	}

	rec := call(srv, http.MethodPut, u.path(), "", "late")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestCreateFileURLRejectsInvalidRequests(t *testing.T) {
	srv := newSignedServer(t, "")
	sb := createWith(t, srv, "key-a", `{}`)
	cases := map[string]struct {
		key, body string
		want      int
	}{
		"unknown operation":   {"key-a", `{"path":"/workspace/a","operation":"delete"}`, http.StatusBadRequest},
		"no operation":        {"key-a", `{"path":"/workspace/a"}`, http.StatusBadRequest},
		"path outside":        {"key-a", `{"path":"/etc/passwd","operation":"download"}`, http.StatusBadRequest},
		"dot-dot":             {"key-a", `{"path":"/workspace/../etc/passwd","operation":"download"}`, http.StatusBadRequest},
		"the workspace root":  {"key-a", `{"path":"/workspace","operation":"upload"}`, http.StatusBadRequest},
		"expiry over an hour": {"key-a", `{"path":"/workspace/a","operation":"upload","expires_in_seconds":3601}`, http.StatusBadRequest},
		"negative expiry":     {"key-a", `{"path":"/workspace/a","operation":"upload","expires_in_seconds":-1}`, http.StatusBadRequest},
		"malformed JSON":      {"key-a", `{"path":`, http.StatusBadRequest},
		"another tenant":      {"key-b", `{"path":"/workspace/a","operation":"upload"}`, http.StatusNotFound},
		"without the scope":   {"key-noscope", `{"path":"/workspace/a","operation":"upload"}`, http.StatusForbidden},
		"without credentials": {"", `{"path":"/workspace/a","operation":"upload"}`, http.StatusUnauthorized},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/file-urls", c.key, c.body)

			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestFileURLUsesTheConfiguredPublicAddress(t *testing.T) {
	srv := newSignedServer(t, "https://sandbox.example.org/")
	sb := createWith(t, srv, "key-a", `{}`)

	u := issue(t, srv, sb.ID, `{"path":"/workspace/a.txt","operation":"download"}`)

	if !strings.HasPrefix(u.URL, "https://sandbox.example.org/v1/signed-files/") {
		t.Errorf("url = %s, want it under the configured address", u.URL)
	}
}

func TestSignedFileRoutesWithoutASigningKey(t *testing.T) {
	srv := newServer()
	sb := createWith(t, srv, "key-a", `{}`)

	create := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/file-urls", "key-a", `{"path":"/workspace/a","operation":"upload"}`)
	use := call(srv, http.MethodPut, "/v1/signed-files/anything.at-all", "", "x")

	if create.Code != http.StatusNotImplemented || !strings.Contains(create.Body.String(), "LOOPSANDBOX_URL_SIGNING_KEY") {
		t.Errorf("create = %d %s, want 501 naming the variable", create.Code, create.Body.String())
	}
	if use.Code != http.StatusForbidden {
		t.Errorf("use = %d, want 403", use.Code)
	}
}

// flipFirst changes the first character of s to another one of the base64url
// alphabet. The first character carries six bits of data, so the result always
// decodes to other bytes.
func flipFirst(s string) string {
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

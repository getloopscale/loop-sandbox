package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

type capabilitiesBody struct {
	Driver       string `json:"driver"`
	Capabilities []struct {
		Name      string `json:"name"`
		Supported bool   `json:"supported"`
	} `json:"capabilities"`
}

func getCapabilities(t *testing.T, srv http.Handler, key string) (*httptest.ResponseRecorder, capabilitiesBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var body capabilitiesBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode capabilities: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, body
}

func TestCapabilitiesReportsDriverAndEveryCapability(t *testing.T) {
	srv := newServer()

	rec, body := getCapabilities(t, srv, "key-a")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if body.Driver != "fake" {
		t.Errorf("driver = %q, want fake", body.Driver)
	}
	got := map[string]bool{}
	for _, c := range body.Capabilities {
		got[c.Name] = c.Supported
	}
	for _, name := range []string{"exec", "files", "exec_stream", "sessions", "session_stream", "pty", "fork", "pause", "volumes", "browser", "template_build"} {
		if _, ok := got[name]; !ok {
			t.Errorf("capability %q missing from the response", name)
		}
	}
	if !got["exec"] || !got["files"] {
		t.Errorf("exec and files must be supported by every driver, got %v", got)
	}
	if got["template_build"] {
		t.Error("template_build reported as supported by the fake driver, which cannot build templates")
	}
}

func TestCapabilitiesRequiresAuthentication(t *testing.T) {
	srv := newServer()

	rec, _ := getCapabilities(t, srv, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// newServerWithoutHostExecution builds a server on a fake driver that did not opt in
// to the capabilities that run on the host, which is how the fake runs in a binary.
func newServerWithoutHostExecution() http.Handler {
	store := memory.New()
	svc := app.NewService(store, fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	return api.NewRouter(api.NewHandler(svc, memory.NewIdempotency()), auth.Middleware(stubAuth{}), nil)
}

func TestRouteOfAnUnsupportedCapabilityAnswers501(t *testing.T) {
	routes := []struct {
		capability, method, path, body string
	}{
		{"sessions", http.MethodPost, "/sessions", `{}`},
		{"exec_stream", http.MethodPost, "/executions?stream=1", `{"cmd":["echo","x"]}`},
		{"pty", http.MethodGet, "/pty", ``},
		{"browser", http.MethodPost, "/browser/navigate", `{"url":"data:text/html,x"}`},
	}
	for _, route := range routes {
		t.Run(route.capability, func(t *testing.T) {
			srv := newServerWithoutHostExecution()
			sandboxID := createSandbox(t, srv, "key-a")

			req := httptest.NewRequest(route.method, "/v1/sandboxes/"+sandboxID+route.path, strings.NewReader(route.body))
			req.Header.Set("Authorization", "Bearer key-a")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotImplemented {
				t.Errorf("status = %d, want 501 for a capability the driver does not declare: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestVolumesAreUnsupportedWithoutAVolumeStore(t *testing.T) {
	srv := newServerWithoutHostExecution()

	_, body := getCapabilities(t, srv, "key-a")

	for _, c := range body.Capabilities {
		if c.Name == "volumes" && c.Supported {
			t.Error("volumes reported as supported by a deployment with no volume store wired")
		}
	}
}

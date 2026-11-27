package api

import (
	"net/http"

	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Middleware wraps a handler with an auth gate (standalone API-key or LoopScale
// forward-auth). The composition root chooses which; the router only applies it.
type Middleware = func(http.Handler) http.Handler

// Route is one HTTP route of the API. The table returned by Routes is the single
// definition of the surface: NewRouter registers it and the OpenAPI contract test
// compares it with api/openapi.yaml.
type Route struct {
	Method string
	// Path is a net/http ServeMux pattern path, e.g. "/v1/sandboxes/{id}/files/{path...}".
	Path string
	// Public routes skip the auth middleware.
	Public bool
	// Scope is the scope the caller's credential must carry. An authenticated
	// route that leaves it empty needs domain.ScopeSandboxExecute.
	Scope   string
	handler func(*Handler, http.ResponseWriter, *http.Request)
}

// RequiredScope returns the scope a caller needs for the route, or "" for a
// public route.
func (r Route) RequiredScope() string {
	switch {
	case r.Public:
		return ""
	case r.Scope != "":
		return r.Scope
	default:
		return domain.ScopeSandboxExecute
	}
}

// requireScope refuses, with 403, a caller whose credential lacks scope. It runs
// inside the auth middleware, so a principal is always present.
func requireScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := auth.PrincipalFrom(r.Context()); p == nil || !p.HasScope(scope) {
			writeError(w, http.StatusForbidden, "forbidden", "this credential lacks the scope "+scope)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MetricsRoute is served only when NewRouter receives a Prometheus registry.
var MetricsRoute = Route{Method: http.MethodGet, Path: "/metrics", Public: true}

// Routes returns every route NewRouter registers, except MetricsRoute.
func Routes() []Route {
	return []Route{
		{Method: http.MethodPost, Path: "/v1/sandboxes", handler: (*Handler).CreateSandbox},
		{Method: http.MethodGet, Path: "/v1/sandboxes", handler: (*Handler).ListSandboxes},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}", handler: (*Handler).GetSandbox},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/timeout", handler: (*Handler).SetSandboxTimeout},
		{Method: http.MethodDelete, Path: "/v1/sandboxes/{id}", handler: (*Handler).DeleteSandbox},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/executions", handler: (*Handler).CreateExecution},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/files", handler: (*Handler).ListFiles},
		{Method: http.MethodPut, Path: "/v1/sandboxes/{id}/files/{path...}", handler: (*Handler).PutFile},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/files/{path...}", handler: (*Handler).GetFile},
		{Method: http.MethodDelete, Path: "/v1/sandboxes/{id}/files/{path...}", handler: (*Handler).DeleteFile},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/file-info/{path...}", handler: (*Handler).FileInfo},
		{Method: http.MethodPut, Path: "/v1/sandboxes/{id}/directories/{path...}", handler: (*Handler).MakeDirectory},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/file-moves", handler: (*Handler).MoveFile},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/artifact-url/{path...}", handler: (*Handler).ArtifactURL},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/file-urls", handler: (*Handler).CreateFileURL},
		// The token in the path is the credential of these two.
		{Method: http.MethodGet, Path: "/v1/signed-files/{token}", Public: true, handler: (*Handler).SignedDownload},
		{Method: http.MethodPut, Path: "/v1/signed-files/{token}", Public: true, handler: (*Handler).SignedUpload},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/pty", handler: (*Handler).PTY},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/browser/navigate", handler: (*Handler).BrowserNavigate},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/browser/screenshot", handler: (*Handler).BrowserScreenshot},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/browser/dom", handler: (*Handler).BrowserDOM},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/browser/input", handler: (*Handler).BrowserInput},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/fork", handler: (*Handler).ForkSandbox},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/pause", handler: (*Handler).PauseSandbox},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/resume", handler: (*Handler).ResumeSandbox},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/snapshots", handler: (*Handler).CreateSnapshot},
		{Method: http.MethodGet, Path: "/v1/snapshots", handler: (*Handler).ListSnapshots},
		{Method: http.MethodGet, Path: "/v1/snapshots/{snapid}", handler: (*Handler).GetSnapshot},
		{Method: http.MethodDelete, Path: "/v1/snapshots/{snapid}", handler: (*Handler).DeleteSnapshot},
		{Method: http.MethodGet, Path: "/v1/secrets", Scope: domain.ScopeSecretsManage, handler: (*Handler).ListSecrets},
		{Method: http.MethodPut, Path: "/v1/secrets/{name}", Scope: domain.ScopeSecretsManage, handler: (*Handler).PutSecret},
		{Method: http.MethodDelete, Path: "/v1/secrets/{name}", Scope: domain.ScopeSecretsManage, handler: (*Handler).DeleteSecret},
		{Method: http.MethodPost, Path: "/v1/volumes", handler: (*Handler).CreateVolume},
		{Method: http.MethodGet, Path: "/v1/volumes", handler: (*Handler).ListVolumes},
		{Method: http.MethodDelete, Path: "/v1/volumes/{vid}", handler: (*Handler).DeleteVolume},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/logs", handler: (*Handler).SandboxLogs},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/metrics", handler: (*Handler).SandboxMetrics},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/watches", handler: (*Handler).StartWatch},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/watches/{wid}/events", handler: (*Handler).WatchEvents},
		{Method: http.MethodDelete, Path: "/v1/sandboxes/{id}/watches/{wid}", handler: (*Handler).StopWatch},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/processes", handler: (*Handler).StartProcess},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/processes", handler: (*Handler).ListProcesses},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/processes/{pid}", handler: (*Handler).GetProcess},
		{Method: http.MethodGet, Path: "/v1/sandboxes/{id}/processes/{pid}/output", handler: (*Handler).ProcessOutput},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/processes/{pid}/stdin", handler: (*Handler).WriteProcessStdin},
		{Method: http.MethodDelete, Path: "/v1/sandboxes/{id}/processes/{pid}/stdin", handler: (*Handler).CloseProcessStdin},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/processes/{pid}/signal", handler: (*Handler).SignalProcess},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/sessions", handler: (*Handler).StartSession},
		{Method: http.MethodPost, Path: "/v1/sandboxes/{id}/sessions/{sid}/execute", handler: (*Handler).RunCell},
		{Method: http.MethodDelete, Path: "/v1/sandboxes/{id}/sessions/{sid}", handler: (*Handler).CloseSession},
		{Method: http.MethodGet, Path: "/v1/api-keys", Scope: domain.ScopeAPIKeysManage, handler: (*Handler).ListAPIKeys},
		{Method: http.MethodPost, Path: "/v1/api-keys", Scope: domain.ScopeAPIKeysManage, handler: (*Handler).CreateAPIKey},
		{Method: http.MethodDelete, Path: "/v1/api-keys/{kid}", Scope: domain.ScopeAPIKeysManage, handler: (*Handler).RevokeAPIKey},
		{Method: http.MethodGet, Path: "/v1/capabilities", handler: (*Handler).Capabilities},
		{Method: http.MethodGet, Path: "/v1/templates", handler: (*Handler).Templates},
		{Method: http.MethodPost, Path: "/v1/templates", handler: (*Handler).RegisterTemplate},
		{Method: http.MethodGet, Path: "/v1/templates/{id}", handler: (*Handler).GetTemplate},
		{Method: http.MethodGet, Path: "/v1/healthz", Public: true, handler: (*Handler).Health},
		{Method: http.MethodGet, Path: "/v1/readyz", Public: true, handler: (*Handler).Ready},
	}
}

// NewRouter builds the /v1 mux from Routes. Non-public routes are wrapped by authMW.
// reg is the Prometheus registry to expose at /metrics (nil means no metrics
// endpoint). Uses the stdlib ServeMux (Go 1.22+ method+path patterns).
func NewRouter(h *Handler, authMW Middleware, reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	for _, route := range Routes() {
		serve := route.handler
		var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(h, w, r) })
		if !route.Public {
			handler = authMW(requireScope(route.RequiredScope(), handler))
		}
		mux.Handle(route.Method+" "+route.Path, handler)
	}
	if reg != nil {
		mux.Handle(MetricsRoute.Method+" "+MetricsRoute.Path, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	}
	return requestID(mux)
}

// requestID sets X-Request-ID on every response (TRD §7.1) before auth runs, so
// the id is available to handlers via the response header.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", rid)
		// Join the caller's trace when the request carries a traceparent header.
		next.ServeHTTP(w, r.WithContext(tracing.Extract(r.Context(), r.Header)))
	})
}

// PrefixedIDGen generates opaque prefixed identifiers (e.g. "sbx_<uuid>").
type PrefixedIDGen struct{ Prefix string }

// New returns a new prefixed id.
func (g PrefixedIDGen) New() string { return g.Prefix + uuid.NewString() }

package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// nonStreamDriver implements runtime.Driver but NOT runtime.StreamingExecDriver — it models a
// runtime that cannot stream (e.g. the container driver). The lifecycle methods are trivial
// because the ownership check reads the repo, not the driver, so RunExecStream short-circuits
// at the capability assert before any driver call.
type nonStreamDriver struct{}

func (nonStreamDriver) Name() string { return "nonstream" }
func (nonStreamDriver) Create(_ context.Context, req runtime.CreateRequest) (*runtime.Sandbox, error) {
	return &runtime.Sandbox{ID: req.SandboxID, TenantID: req.TenantID, State: runtime.StateRunning}, nil
}
func (nonStreamDriver) Inspect(_ context.Context, id runtime.SandboxID) (*runtime.Sandbox, error) {
	return &runtime.Sandbox{ID: id, State: runtime.StateRunning}, nil
}
func (nonStreamDriver) Destroy(context.Context, runtime.SandboxID) error { return nil }
func (nonStreamDriver) Exec(context.Context, runtime.SandboxID, runtime.ExecRequest) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}
func (nonStreamDriver) WriteFile(context.Context, runtime.SandboxID, string, io.Reader) error {
	return nil
}
func (nonStreamDriver) ReadFile(context.Context, runtime.SandboxID, string) (io.ReadCloser, error) {
	return nil, runtime.ErrFileNotFound
}
func (nonStreamDriver) DeleteFile(context.Context, runtime.SandboxID, string) error { return nil }
func (nonStreamDriver) ListFiles(context.Context, runtime.SandboxID, string) ([]runtime.FileEntry, error) {
	return nil, nil
}

// TestE2E_exec_stream_unsupported_runtime_returns_501: on a runtime that cannot stream, the
// streamed-exec endpoint must return HTTP 501 with the status committed BEFORE any body — NOT
// a 200 with the error buried in the SSE stream (M15 review HIGH-1 regression).
func TestE2E_exec_stream_unsupported_runtime_returns_501(t *testing.T) {
	svc := app.NewService(memory.New(), nonStreamDriver{}, clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	h := api.NewHandler(svc, memory.NewIdempotency())
	srv := api.NewRouter(h, auth.Middleware(stubAuth{}), nil)

	id := createSandbox(t, srv, "key-a")
	rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/executions?stream=1", "key-a",
		[]byte(`{"cmd":["echo","hi"]}`))

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
	// It is a real error response, not an SSE stream.
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q; a 501 must not be an SSE stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "not_implemented") {
		t.Errorf("body missing not_implemented code; got %s", rec.Body.String())
	}
}

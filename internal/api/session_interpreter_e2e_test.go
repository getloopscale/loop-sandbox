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
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// noInterpreterDriver is a driver with sessions whose sandboxes have no python3.
type noInterpreterDriver struct{ runtime.Driver }

func (noInterpreterDriver) StartSession(context.Context, runtime.SandboxID, string) error {
	return runtime.ErrNoInterpreter
}

func (noInterpreterDriver) ExecSession(context.Context, runtime.SandboxID, string, string) (*runtime.ExecResult, error) {
	return nil, runtime.ErrNoInterpreter
}

func (noInterpreterDriver) CloseSession(context.Context, runtime.SandboxID, string) error { return nil }

func TestStartSessionInAnImageWithoutPythonIs400AndSaysWhy(t *testing.T) {
	svc := app.NewService(memory.New(), noInterpreterDriver{fake.New()}, clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	srv := api.NewRouter(api.NewHandler(svc, nil), auth.Middleware(stubAuth{}), nil)
	sb := createWith(t, srv, "key-a", `{}`)

	rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/sessions", "key-a", "")

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "python3") {
		t.Errorf("status = %d body = %s, want 400 naming python3", rec.Code, rec.Body.String())
	}
}

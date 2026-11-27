// Command testserver runs the real LoopSandbox API router (fake driver with sessions +
// fork enabled, in-memory store) on a port, with a fixed API key. It exists ONLY to give
// the Python SDK a real, hermetic API to run its contract tests against — proving
// cross-SDK parity with the TS SDK (which is tested the same way). NOT for production.
package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

type fixedKeyAuth struct{}

func (fixedKeyAuth) Authenticate(_ context.Context, raw string) (*auth.Principal, error) {
	switch strings.TrimPrefix(strings.TrimSpace(raw), "Bearer ") {
	case "lsb_test_key":
		return &auth.Principal{TenantID: "tenant-a", KeyID: "k", Scopes: []string{"sandbox:execute"}}, nil
	case "lsb_other_key":
		return &auth.Principal{TenantID: "tenant-b", KeyID: "k2", Scopes: []string{"sandbox:execute"}}, nil
	default:
		return nil, auth.ErrUnauthorized
	}
}

type noAudit struct{}

func (noAudit) Record(_, _, _, _, _, _ string, _ time.Time) {}

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8099"
	}
	svc := app.NewService(memory.New(), fake.NewWithSessions(), clock.System{},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	h := api.NewHandler(svc, memory.NewIdempotency())
	router := api.NewRouter(h, auth.Middleware(fixedKeyAuth{}), nil)
	srv := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 5 * time.Second}
	_ = srv.ListenAndServe()
}

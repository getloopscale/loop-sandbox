package api_test

import (
	"encoding/json"
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
	"github.com/prometheus/client_golang/prometheus"
)

func TestCreateAtTheLiveSandboxLimitAnswers429WithTheLimit(t *testing.T) {
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	svc.SetLiveSandboxLimit(1)
	srv := api.NewRouter(api.NewHandler(svc, memory.NewIdempotency()), auth.Middleware(stubAuth{}), prometheus.NewRegistry())
	if rec := post(srv, "key-a", `{"image":"python:3"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	rec := post(srv, "key-a", `{"image":"python:3"}`)

	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusTooManyRequests || body.Error.Code != "live_sandbox_limit" ||
		!strings.Contains(body.Error.Message, "the limit is 1") {
		t.Fatalf("create at the limit = %d %q %q; want 429 live_sandbox_limit naming the limit of 1",
			rec.Code, body.Error.Code, body.Error.Message)
	}
}

package api_test

import (
	"context"
	"errors"
	"fmt"
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

type forkBatch struct {
	Sandboxes []sandboxBody `json:"sandboxes"`
}

// failingForkDriver is the fake driver with a Fork that fails from the failAt-th
// call on.
type failingForkDriver struct {
	*fake.Driver
	failAt, calls int
}

func (d *failingForkDriver) Fork(ctx context.Context, parent, fork runtime.SandboxID) (*runtime.Sandbox, error) {
	d.calls++
	if d.calls >= d.failAt {
		return nil, errors.New("no capacity for another fork")
	}
	return d.Driver.Fork(ctx, parent, fork)
}

func TestForkWithCountReturnsThatManyIndependentSandboxes(t *testing.T) {
	srv := newServer()
	parent := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, parent.ID, "/workspace/base.txt", "base")

	batch := decode[forkBatch](t, call(srv, http.MethodPost, "/v1/sandboxes/"+parent.ID+"/fork", "key-a", `{"count":3}`), http.StatusCreated)

	ids := map[string]bool{parent.ID: true}
	for _, sb := range batch.Sandboxes {
		ids[sb.ID] = true
		if got := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/base.txt", "key-a", ""); got.Body.String() != "base" {
			t.Errorf("fork %s read %q, want the parent's file", sb.ID, got.Body.String())
		}
	}
	if len(batch.Sandboxes) != 3 || len(ids) != 4 {
		t.Fatalf("got %d forks and %d distinct ids with the parent, want 3 and 4", len(batch.Sandboxes), len(ids))
	}
	putFile(t, srv, batch.Sandboxes[0].ID, "/workspace/only-here.txt", "x")
	if rec := call(srv, http.MethodGet, "/v1/sandboxes/"+batch.Sandboxes[1].ID+"/files/workspace/only-here.txt", "key-a", ""); rec.Code != http.StatusNotFound {
		t.Errorf("a write in one fork is visible in another: %d", rec.Code)
	}
}

func TestForkWithCountOneStillReturnsAList(t *testing.T) {
	srv := newServer()
	parent := createWith(t, srv, "key-a", `{}`)

	batch := decode[forkBatch](t, call(srv, http.MethodPost, "/v1/sandboxes/"+parent.ID+"/fork", "key-a", `{"count":1}`), http.StatusCreated)

	if len(batch.Sandboxes) != 1 {
		t.Errorf("got %d forks, want a list of 1", len(batch.Sandboxes))
	}
}

func TestForkRejectsACountOutOfRange(t *testing.T) {
	srv := newServer()
	parent := createWith(t, srv, "key-a", `{}`)
	for name, body := range map[string]string{"zero": `{"count":0}`, "negative": `{"count":-1}`, "over the maximum": `{"count":17}`, "malformed": `{"count":`} {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, http.MethodPost, "/v1/sandboxes/"+parent.ID+"/fork", "key-a", body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAFailedForkBatchLeavesNoSandboxBehind(t *testing.T) {
	driver := &failingForkDriver{Driver: fake.NewWithSessions(), failAt: 3}
	svc := app.NewService(memory.New(), driver, clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	srv := api.NewRouter(api.NewHandler(svc, nil), auth.Middleware(stubAuth{}), nil)
	parent := createWith(t, srv, "key-a", `{}`)

	rec := call(srv, http.MethodPost, "/v1/sandboxes/"+parent.ID+"/fork", "key-a", `{"count":4}`)

	if rec.Code < 500 {
		t.Fatalf("status = %d, want a server error (%s)", rec.Code, rec.Body.String())
	}
	listing := call(srv, http.MethodGet, "/v1/sandboxes?state=running", "key-a", "").Body.String()
	if got := strings.Count(listing, `"id":"sbx_`); got != 1 || !strings.Contains(listing, fmt.Sprintf("%q", parent.ID)) {
		t.Errorf("running sandboxes after the failed batch = %d, want only the parent: %s", got, listing)
	}
}

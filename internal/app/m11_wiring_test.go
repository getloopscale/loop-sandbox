package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/cells"
	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// fakeArtifactStore records the args it was called with and returns a canned URL.
type fakeArtifactStore struct {
	gotTenant, gotSandbox, gotPath string
	url                            string
}

func (f *fakeArtifactStore) PresignArtifact(_ context.Context, tenantID, sandboxID, path string, _ time.Duration) (string, error) {
	f.gotTenant, f.gotSandbox, f.gotPath = tenantID, sandboxID, path
	return f.url, nil
}

// TestPresignArtifactURL_owner_gets_url wires a real ArtifactStore into the service and
// proves the caller path: an owner gets a URL keyed to THEIR tenant + sandbox, and the
// metric fires.
func TestPresignArtifactURL_owner_gets_url(t *testing.T) {
	svc, _, _ := newService()
	fs := &fakeArtifactStore{url: "https://s3.example/presigned"}
	svc.SetArtifactStore(fs)
	obs := &recObserver{}
	svc.SetObserver(obs)

	sb, err := svc.CreateSandbox(context.Background(), actorA, "img", time.Minute)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	url, err := svc.PresignArtifactURL(context.Background(), actorA, sb.ID, "/workspace/out.bin", time.Minute)
	if err != nil {
		t.Fatalf("PresignArtifactURL: %v", err)
	}
	if url != fs.url {
		t.Errorf("url = %q, want %q", url, fs.url)
	}
	if fs.gotTenant != actorA.TenantID || fs.gotSandbox != sb.ID {
		t.Errorf("store keyed by (%s,%s), want (%s,%s)", fs.gotTenant, fs.gotSandbox, actorA.TenantID, sb.ID)
	}
	if obs.artifactURLs != 1 {
		t.Errorf("ArtifactURLIssued fired %d times, want 1", obs.artifactURLs)
	}
}

// Negative case: a different tenant cannot presign someone else's sandbox — the
// ownership check returns not-found (never a URL leaking cross-tenant existence).
func TestPresignArtifactURL_cross_tenant_is_not_found(t *testing.T) {
	svc, _, _ := newService()
	svc.SetArtifactStore(&fakeArtifactStore{url: "leak"})

	sb, err := svc.CreateSandbox(context.Background(), actorA, "img", time.Minute)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	_, err = svc.PresignArtifactURL(context.Background(), actorB, sb.ID, "/workspace/out.bin", time.Minute)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant presign err = %v, want ErrNotFound", err)
	}
}

// Negative case: with no store wired, the service returns a typed unset error (handler
// maps it to 501), not a panic.
func TestPresignArtifactURL_unset_store(t *testing.T) {
	svc, _, _ := newService()
	sb, _ := svc.CreateSandbox(context.Background(), actorA, "img", time.Minute)
	_, err := svc.PresignArtifactURL(context.Background(), actorA, sb.ID, "/workspace/f", time.Minute)
	if !errors.Is(err, app.ErrArtifactStoreUnset) {
		t.Fatalf("err = %v, want ErrArtifactStoreUnset", err)
	}
}

// TestCreateSandbox_routes_to_cell wires a real cells.Router and proves CreateSandbox
// records the routed cell as a metric.
func TestCreateSandbox_routes_to_cell(t *testing.T) {
	svc, _, _ := newService()
	router, err := cells.NewRouter([]string{"cell-a", "cell-b", "cell-c"})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	svc.SetCellRouter(router)
	obs := &recObserver{}
	svc.SetObserver(obs)

	if _, err := svc.CreateSandbox(context.Background(), actorA, "img", time.Minute); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if len(obs.cellsRouted) != 1 {
		t.Fatalf("CellRouted fired %d times, want 1", len(obs.cellsRouted))
	}
	want := router.CellFor(actorA.TenantID)
	if obs.cellsRouted[0] != want {
		t.Errorf("routed to %q, want %q (deterministic)", obs.cellsRouted[0], want)
	}
}

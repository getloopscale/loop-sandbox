package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/api"
	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// stubArtifactStore returns a canned presigned URL and records the tenant it keyed by.
type stubArtifactStore struct{ url string }

func (s stubArtifactStore) PresignArtifact(_ context.Context, _, _, _ string, _ time.Duration) (string, error) {
	return s.url, nil
}

func serverWithArtifactStore(store app.ArtifactStore) http.Handler {
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()},
		api.PrefixedIDGen{Prefix: "sbx_"}, noAudit{}, nil, nil)
	if store != nil {
		svc.SetArtifactStore(store)
	}
	h := api.NewHandler(svc, memory.NewIdempotency())
	return api.NewRouter(h, auth.Middleware(stubAuth{}), nil)
}

// TestE2E_artifact_url_owner_gets_presigned_url: the owner receives a 200 with the URL.
func TestE2E_artifact_url_owner_gets_presigned_url(t *testing.T) {
	srv := serverWithArtifactStore(stubArtifactStore{url: "https://s3.example/signed?X-Amz-Expires=600"})
	id := createSandbox(t, srv, "key-a")

	rec := req(srv, http.MethodGet, "/v1/sandboxes/"+id+"/artifact-url/workspace/out.bin", "key-a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET artifact-url = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		URL              string `json:"url"`
		ExpiresInSeconds int    `json:"expires_in_seconds"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.URL == "" || body.ExpiresInSeconds <= 0 {
		t.Errorf("body = %+v, want a url + positive expiry", body)
	}
}

// Cross-tenant: key-b (tenant-b) asking for key-a's sandbox artifact → 404, never a URL.
func TestE2E_artifact_url_cross_tenant_is_404(t *testing.T) {
	srv := serverWithArtifactStore(stubArtifactStore{url: "LEAK"})
	id := createSandbox(t, srv, "key-a")

	rec := req(srv, http.MethodGet, "/v1/sandboxes/"+id+"/artifact-url/workspace/out.bin", "key-b", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant artifact-url = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); contains(body, "LEAK") {
		t.Errorf("cross-tenant response leaked the URL: %s", body)
	}
}

// Store not configured → 501 not_implemented (feature not deployed), not a panic.
func TestE2E_artifact_url_unconfigured_is_501(t *testing.T) {
	srv := serverWithArtifactStore(nil)
	id := createSandbox(t, srv, "key-a")

	rec := req(srv, http.MethodGet, "/v1/sandboxes/"+id+"/artifact-url/workspace/out.bin", "key-a", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured artifact-url = %d, want 501; body=%s", rec.Code, rec.Body.String())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestE2E_fork_clones_and_isolates: fork a sandbox over HTTP; the fork gets the parent's
// files but writes diverge; cross-tenant fork is 404.
func TestE2E_fork_clones_and_isolates(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	// parent writes a file
	if rec := req(srv, http.MethodPut, "/v1/sandboxes/"+id+"/files/workspace/a.txt", "key-a", []byte("parent")); rec.Code != http.StatusNoContent {
		t.Fatalf("write parent = %d", rec.Code)
	}
	// fork
	rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/fork", "key-a", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fork = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var fork struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &fork)
	if fork.ID == "" || fork.ID == id {
		t.Fatalf("fork id = %q, want a new id", fork.ID)
	}
	// fork inherited the parent's file
	if get := req(srv, http.MethodGet, "/v1/sandboxes/"+fork.ID+"/files/workspace/a.txt", "key-a", nil); get.Body.String() != "parent" {
		t.Errorf("fork file = %q, want 'parent' (not inherited)", get.Body.String())
	}
	// write to the fork → parent unaffected
	req(srv, http.MethodPut, "/v1/sandboxes/"+fork.ID+"/files/workspace/b.txt", "key-a", []byte("fork"))
	if get := req(srv, http.MethodGet, "/v1/sandboxes/"+id+"/files/workspace/b.txt", "key-a", nil); get.Code == http.StatusOK {
		t.Errorf("parent sees fork's file, want isolated (got %d)", get.Code)
	}
}

// Cross-tenant fork → 404 (US-05).
func TestE2E_fork_cross_tenant_is_404(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/fork", "key-b", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant fork = %d, want 404", rec.Code)
	}
}

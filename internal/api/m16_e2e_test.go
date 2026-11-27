package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestE2E_pause_resume: a running sandbox can be paused and resumed; the state reflects it.
func TestE2E_pause_resume(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")

	if rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/pause", "key-a", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Pausing again is an invalid transition (already paused) → 409.
	if rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/pause", "key-a", nil); rec.Code != http.StatusConflict {
		t.Errorf("double pause = %d, want 409", rec.Code)
	}
	if rec := doGet(srv, id, "key-a"); rec.Code == http.StatusOK {
		var sb struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &sb)
		if sb.State != "paused" {
			t.Errorf("state = %q, want paused", sb.State)
		}
	}
	if rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/resume", "key-a", nil); rec.Code != http.StatusOK {
		t.Fatalf("resume = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestE2E_pause_cross_tenant_is_404(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	if rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/pause", "key-b", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant pause = %d, want 404", rec.Code)
	}
}

// TestE2E_volume_data_survives_sandbox_destroy: write a file to a volume mount, destroy the
// sandbox, attach the volume to a NEW sandbox, and read the file back — the durable-volume
// promise (M16). Uses the fake driver's durable backing store.
func TestE2E_volume_data_survives_sandbox_destroy(t *testing.T) {
	srv := newServer()
	id1 := createSandbox(t, srv, "key-a")

	// Create + attach a 5 GiB volume named "data" to sandbox 1.
	rec := req(srv, http.MethodPost, "/v1/volumes", "key-a",
		[]byte(`{"sandbox_id":"`+id1+`","name":"data","size_gb":5}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create volume = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var vol struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &vol)

	// Write a file INTO the volume mount (/workspace/data/greeting).
	if rec := req(srv, http.MethodPut, "/v1/sandboxes/"+id1+"/files/workspace/data/greeting", "key-a", []byte("durable-hello")); rec.Code != http.StatusNoContent {
		t.Fatalf("write to volume = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	// List shows the volume attached to sandbox 1.
	if rec := req(srv, http.MethodGet, "/v1/volumes?sandbox_id="+id1, "key-a", nil); rec.Code != http.StatusOK {
		t.Fatalf("list volumes = %d, want 200", rec.Code)
	}

	// Destroy sandbox 1.
	if rec := req(srv, http.MethodDelete, "/v1/sandboxes/"+id1, "key-a", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("destroy sandbox = %d, want 204", rec.Code)
	}

	// Attach the SAME volume (re-create with same name → same backing) to a new sandbox and
	// read the file back. In the fake, re-attaching by name reuses the durable backing store.
	id2 := createSandbox(t, srv, "key-a")
	if rec := req(srv, http.MethodPost, "/v1/volumes", "key-a",
		[]byte(`{"sandbox_id":"`+id2+`","name":"data","size_gb":5}`)); rec.Code != http.StatusCreated {
		t.Fatalf("re-attach volume = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	rec = req(srv, http.MethodGet, "/v1/sandboxes/"+id2+"/files/workspace/data/greeting", "key-a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read from re-attached volume = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "durable-hello" {
		t.Errorf("volume data = %q, want durable-hello (data did NOT survive destroy)", got)
	}
}

func TestE2E_volume_cross_tenant_delete_is_404(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	rec := req(srv, http.MethodPost, "/v1/volumes", "key-a",
		[]byte(`{"sandbox_id":"`+id+`","name":"data","size_gb":1}`))
	var vol struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &vol)
	// tenant-b cannot delete tenant-a's volume — not-found (indistinguishable).
	if rec := req(srv, http.MethodDelete, "/v1/volumes/"+vol.ID, "key-b", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant delete = %d, want 404", rec.Code)
	}
}

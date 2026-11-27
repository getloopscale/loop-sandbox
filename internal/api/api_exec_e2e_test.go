package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(srv http.Handler, method, path, key string, body []byte) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

func TestE2E_file_upload_exec_download_delete(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	base := "/v1/sandboxes/" + id

	// upload
	if rec := req(srv, http.MethodPut, base+"/files/workspace/main.txt", "key-a", []byte("hello file")); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT file = %d, want 204", rec.Code)
	}
	// exec cat → sees the content (fake driver interprets echo/cat)
	execBody, _ := json.Marshal(map[string]any{"cmd": []string{"cat", "/workspace/main.txt"}})
	rec := req(srv, http.MethodPost, base+"/executions", "key-a", execBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST exec = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var ex struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ex)
	if ex.ExitCode != 0 || ex.Stdout != "hello file" {
		t.Errorf("exec = %+v, want exit 0 stdout 'hello file'", ex)
	}
	// download → content + checksum header
	get := req(srv, http.MethodGet, base+"/files/workspace/main.txt", "key-a", nil)
	if get.Code != http.StatusOK || get.Body.String() != "hello file" {
		t.Fatalf("GET file = %d body=%q", get.Code, get.Body.String())
	}
	if get.Header().Get("X-Checksum-SHA256") == "" {
		t.Error("missing checksum header")
	}
	// list
	list := req(srv, http.MethodGet, base+"/files?path=/workspace", "key-a", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d", list.Code)
	}
	// delete
	if del := req(srv, http.MethodDelete, base+"/files/workspace/main.txt", "key-a", nil); del.Code != http.StatusNoContent {
		t.Fatalf("DELETE file = %d, want 204", del.Code)
	}
	if after := req(srv, http.MethodGet, base+"/files/workspace/main.txt", "key-a", nil); after.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", after.Code)
	}
}

// A path outside /workspace reaches the handler and is rejected by the boundary
// validation (400). Traversal via ".." is additionally cleaned by net/http's mux
// (unit-tested directly in domain/path_test.go).
func TestE2E_out_of_workspace_path_is_rejected(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	rec := req(srv, http.MethodPut, "/v1/sandboxes/"+id+"/files/etc/passwd", "key-a", []byte("x"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("out-of-workspace PUT = %d, want 400", rec.Code)
	}
}

func TestE2E_exec_cross_tenant_is_404(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	execBody, _ := json.Marshal(map[string]any{"cmd": []string{"echo", "hi"}})
	// tenant-b execs in tenant-a's sandbox → 404 (US-05)
	rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/executions", "key-b", execBody)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant exec = %d, want 404", rec.Code)
	}
}

func TestE2E_exec_missing_cmd_is_400(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/executions", "key-a", []byte(`{"cmd":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty cmd = %d, want 400", rec.Code)
	}
}

// US-02 at the HTTP boundary: a failing command is a 200 with the exit code, not
// a transport error.
func TestE2E_exec_nonzero_exit_is_200(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	body, _ := json.Marshal(map[string]any{"cmd": []string{"cat", "/workspace/does-not-exist"}})
	rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/executions", "key-a", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("failing exec = %d, want 200 (US-02)", rec.Code)
	}
	var ex struct {
		ExitCode int `json:"exit_code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ex)
	if ex.ExitCode == 0 {
		t.Errorf("exit_code = 0, want non-zero for a failing command")
	}
}

func TestE2E_get_missing_file_is_404(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	rec := req(srv, http.MethodGet, "/v1/sandboxes/"+id+"/files/workspace/never-created", "key-a", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET missing file = %d, want 404", rec.Code)
	}
}

func TestE2E_exec_on_missing_sandbox_is_404(t *testing.T) {
	srv := newServer()
	body, _ := json.Marshal(map[string]any{"cmd": []string{"echo", "x"}})
	rec := req(srv, http.MethodPost, "/v1/sandboxes/sbx_does_not_exist/executions", "key-a", body)
	if rec.Code != http.StatusNotFound {
		t.Errorf("exec on missing sandbox = %d, want 404", rec.Code)
	}
}

func TestE2E_upload_over_cap_is_413(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	big := bytes.Repeat([]byte("a"), (32<<20)+1) // maxFileBytes + 1
	rec := req(srv, http.MethodPut, "/v1/sandboxes/"+id+"/files/workspace/big.bin", "key-a", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload = %d, want 413", rec.Code)
	}
}

func TestE2E_get_file_checksum_is_correct(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	base := "/v1/sandboxes/" + id
	_ = req(srv, http.MethodPut, base+"/files/workspace/f.txt", "key-a", []byte("hello file"))
	rec := req(srv, http.MethodGet, base+"/files/workspace/f.txt", "key-a", nil)
	want := sha256.Sum256([]byte("hello file"))
	if got := rec.Header().Get("X-Checksum-SHA256"); got != hex.EncodeToString(want[:]) {
		t.Errorf("checksum = %q, want %x", got, want)
	}
}

package api_test

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

// TestE2E_session_shares_state_over_http is the M12 acceptance: over the REST API, a
// variable set in one cell is visible in a later cell of the same session (the fake
// driver runs a real python3 kernel, so this is genuine state sharing, hermetic).
func TestE2E_session_shares_state_over_http(t *testing.T) {
	requirePython(t)
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	base := "/v1/sandboxes/" + id

	// open a session
	rec := req(srv, http.MethodPost, base+"/sessions", "key-a", []byte(`{}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST sessions = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var s struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	if s.SessionID == "" {
		t.Fatal("no session_id returned")
	}
	sbase := base + "/sessions/" + s.SessionID

	// cell 1: x = 41
	if rec := req(srv, http.MethodPost, sbase+"/execute", "key-a", []byte(`{"code":"x = 41"}`)); rec.Code != http.StatusOK {
		t.Fatalf("cell1 = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// cell 2: print(x + 1) — sees the shared state
	rec = req(srv, http.MethodPost, sbase+"/execute", "key-a", []byte(`{"code":"print(x + 1)"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("cell2 = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var cell struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cell)
	if cell.ExitCode != 0 || strings.TrimSpace(cell.Stdout) != "42" {
		t.Errorf("cell2 = %+v, want exit 0 stdout '42' (state not shared over HTTP)", cell)
	}

	// close
	if rec := req(srv, http.MethodDelete, sbase, "key-a", nil); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE session = %d, want 204", rec.Code)
	}
}

// A cell that raises returns 200 with exit_code=1 (program error ≠ transport error).
func TestE2E_session_cell_exception_is_200_exit1(t *testing.T) {
	requirePython(t)
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	sbase := "/v1/sandboxes/" + id + "/sessions"
	rec := req(srv, http.MethodPost, sbase, "key-a", []byte(`{}`))
	var s struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &s)

	rec = req(srv, http.MethodPost, sbase+"/"+s.SessionID+"/execute", "key-a", []byte(`{"code":"raise ValueError('boom')"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("cell = %d, want 200 (program error is not transport error)", rec.Code)
	}
	var cell struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cell)
	if cell.ExitCode != 1 || !strings.Contains(cell.Stderr, "ValueError: boom") {
		t.Errorf("cell = %+v, want exit 1 + ValueError in stderr", cell)
	}
}

// Cross-tenant: key-b cannot open a session on key-a's sandbox → 404 (US-05).
func TestE2E_session_cross_tenant_is_404(t *testing.T) {
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/sessions", "key-b", []byte(`{}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant session = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestE2E_session_stream_sse proves the streaming endpoint (?stream=1) delivers partial
// stdout as Server-Sent Events plus a terminal done event.
func TestE2E_session_stream_sse(t *testing.T) {
	requirePython(t)
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	sbase := "/v1/sandboxes/" + id + "/sessions"
	rec := req(srv, http.MethodPost, sbase, "key-a", []byte(`{}`))
	var s struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &s)

	rec = req(srv, http.MethodPost, sbase+"/"+s.SessionID+"/execute?stream=1", "key-a",
		[]byte(`{"code":"for i in range(3):\n    print('row', i)"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream execute = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	stdoutEvents := strings.Count(body, "event: stdout")
	if stdoutEvents < 3 {
		t.Errorf("got %d stdout SSE events, want ≥3 (not incremental); body=%q", stdoutEvents, body)
	}
	if !strings.Contains(body, "event: done") {
		t.Errorf("missing terminal 'event: done'; body=%q", body)
	}
	// The output is fragmented across chunks (print emits several write() calls per line —
	// that fragmentation IS the incremental streaming), so assert the tokens, not contiguous
	// "row N": reassemble the stdout from the SSE data payloads and check the full text.
	var reassembled strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var m struct {
				Data string `json:"data"`
			}
			if json.Unmarshal([]byte(data), &m) == nil {
				reassembled.WriteString(m.Data)
			}
		}
	}
	for _, want := range []string{"row 0", "row 1", "row 2"} {
		if !strings.Contains(reassembled.String(), want) {
			t.Errorf("reassembled stream missing %q; got %q", want, reassembled.String())
		}
	}
}

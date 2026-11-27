package api_test

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// TestE2E_exec_stream_sse: a command that prints multiple lines streams them as SSE stdout
// events as it runs, ending with a done event (M15 streamed exec).
func TestE2E_exec_stream_sse(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	srv := newServer()
	id := createSandbox(t, srv, "key-a")
	rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/executions?stream=1", "key-a",
		[]byte(`{"cmd":["sh","-c","for i in 1 2 3; do echo line$i; done"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream exec = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if n := strings.Count(body, "event: stdout"); n < 3 {
		t.Errorf("got %d stdout events, want ≥3; body=%q", n, body)
	}
	if !strings.Contains(body, "event: done") {
		t.Errorf("missing done event; body=%q", body)
	}
	for _, w := range []string{"line1", "line2", "line3"} {
		if !strings.Contains(body, w) {
			t.Errorf("stream missing %q", w)
		}
	}
}

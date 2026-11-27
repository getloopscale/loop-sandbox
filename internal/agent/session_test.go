package agent

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(pythonBin); err != nil {
		t.Skipf("%s not available", pythonBin)
	}
}

// dispatch drives the handler like Serve would, but in-process.
func dispatch(h *Handler, req *agentproto.Request) *agentproto.Response { return h.handle(req) }

// TestSession_shares_state_across_cells is the M12 core: a variable set in one cell is
// visible in a later cell of the SAME session (persistent namespace).
func TestSession_shares_state_across_cells(t *testing.T) {
	requirePython(t)
	h := New(t.TempDir())
	defer h.CloseSessions()

	if r := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionStart, SessionID: "s1"}); r.Err != "" {
		t.Fatalf("start: %s", r.Err)
	}
	if r := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "s1", Code: "x = 41"}); r.Err != "" {
		t.Fatalf("cell1: %s", r.Err)
	}
	r := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "s1", Code: "print(x + 1)"})
	if r.Err != "" {
		t.Fatalf("cell2: %s", r.Err)
	}
	if got := strings.TrimSpace(string(r.Stdout)); got != "42" {
		t.Errorf("cell2 stdout = %q, want 42 (state not shared)", got)
	}
	if r.ExitCode != 0 {
		t.Errorf("exit = %d, want 0; stderr=%q", r.ExitCode, r.Stderr)
	}
}

// A Python exception in a cell is a NORMAL result (exit 1 + traceback in stderr), not an
// agent failure or a hang — and the session survives for the next cell.
func TestSession_cell_exception_is_reported_not_fatal(t *testing.T) {
	requirePython(t)
	h := New(t.TempDir())
	defer h.CloseSessions()
	dispatch(h, &agentproto.Request{Op: agentproto.OpSessionStart, SessionID: "s"})

	r := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "s", Code: "raise ValueError('boom')"})
	if r.Err != "" {
		t.Fatalf("agent-level err (should be a cell result): %s", r.Err)
	}
	if r.ExitCode != 1 || !strings.Contains(string(r.Stderr), "ValueError: boom") {
		t.Errorf("exit=%d stderr=%q, want exit 1 + ValueError traceback", r.ExitCode, r.Stderr)
	}
	// Session still alive for the next cell.
	r2 := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "s", Code: "print('alive')"})
	if strings.TrimSpace(string(r2.Stdout)) != "alive" {
		t.Errorf("session dead after a cell exception: %q %q", r2.Stdout, r2.Err)
	}
}

// exec on an unknown/closed session is a typed error, never a hang.
func TestSession_exec_unknown_session_errors(t *testing.T) {
	h := New(t.TempDir())
	r := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "nope", Code: "1"})
	if r.Err == "" || !strings.Contains(r.Err, "unknown session") {
		t.Errorf("err = %q, want 'unknown session'", r.Err)
	}
}

// Concurrent cells on ONE session serialize (the per-session mutex) — no interleaved
// state corruption. Two goroutines each increment a shared counter 50x; the final value
// must be exactly 100.
func TestSession_concurrent_cells_serialize(t *testing.T) {
	requirePython(t)
	h := New(t.TempDir())
	defer h.CloseSessions()
	dispatch(h, &agentproto.Request{Op: agentproto.OpSessionStart, SessionID: "c"})
	dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "c", Code: "n = 0"})

	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "c", Code: "n = n + 1"})
			}
		}()
	}
	wg.Wait()
	r := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "c", Code: "print(n)"})
	if got := strings.TrimSpace(string(r.Stdout)); got != "100" {
		t.Errorf("counter = %q, want 100 (cells not serialized → lost updates)", got)
	}
}

// close terminates the kernel; a subsequent exec is a typed error.
func TestSession_close_then_exec_errors(t *testing.T) {
	requirePython(t)
	h := New(t.TempDir())
	dispatch(h, &agentproto.Request{Op: agentproto.OpSessionStart, SessionID: "z"})
	if r := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionClose, SessionID: "z"}); r.Err != "" {
		t.Fatalf("close: %s", r.Err)
	}
	r := dispatch(h, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: "z", Code: "1"})
	if r.Err == "" {
		t.Error("exec after close succeeded, want typed error")
	}
}

// TestSession_stream_emits_incremental_chunks proves partial output arrives AS THE CELL
// RUNS: a cell that prints several lines yields multiple onChunk calls (not one buffered
// blob), and the final result carries the exit code.
func TestSession_stream_emits_incremental_chunks(t *testing.T) {
	requirePython(t)
	h := New(t.TempDir())
	defer h.CloseSessions()
	if err := h.StartSession("st"); err != nil {
		t.Fatalf("start: %v", err)
	}
	var chunks []string
	stderr, exit, err := h.ExecSessionStream("st", "for i in range(3):\n    print('line', i)", func(stream, data string) {
		chunks = append(chunks, data)
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if exit != 0 {
		t.Errorf("exit=%d stderr=%q, want 0", exit, stderr)
	}
	// print() writes the text then the newline separately → several chunks; the key
	// assertion is that output came in MULTIPLE frames, not one buffered blob.
	if len(chunks) < 3 {
		t.Errorf("got %d chunks (%q), want ≥3 incremental chunks", len(chunks), chunks)
	}
	joined := strings.Join(chunks, "")
	for _, want := range []string{"line 0", "line 1", "line 2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("streamed output missing %q; got %q", want, joined)
		}
	}
}

// TestSession_runaway_cell_times_out proves a `while True` cell does NOT hang forever: the
// per-cell timer kills the kernel and returns a typed timeout (never a permanent hang).
func TestSession_runaway_cell_times_out(t *testing.T) {
	requirePython(t)
	old := CellTimeout
	CellTimeout = 400 * time.Millisecond
	defer func() { CellTimeout = old }()

	h := New(t.TempDir())
	defer h.CloseSessions()
	if err := h.StartSession("run"); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan *agentproto.Response, 1)
	go func() { done <- h.sessions.exec("run", "while True:\n    pass") }()
	select {
	case r := <-done:
		if r.Err == "" || !strings.Contains(r.Err, "timeout") {
			t.Errorf("runaway err = %q, want a timeout error", r.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runaway cell HUNG — timeout did not fire (BLOCKER)")
	}
}

//go:build integration

package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func TestExec_echo_returns_stdout_and_exit(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("exec")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"echo", "hello"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "hello" {
		t.Errorf("exec = exit %d stdout %q, want 0 'hello'", res.ExitCode, res.Stdout)
	}

	// A failing command returns a non-zero exit, NOT a Go error (US-02).
	fail, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"sh", "-c", "exit 7"}})
	if err != nil {
		t.Fatalf("Exec(exit 7) unexpected error: %v", err)
	}
	if fail.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", fail.ExitCode)
	}
}

func TestExec_file_roundtrip_real(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("file")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const content = "print('hi from sandbox')\n"
	if err := d.WriteFile(ctx, id, "/workspace/main.py", bytes.NewBufferString(content)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// cat inside the container sees the written file.
	res, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"cat", "/workspace/main.py"}})
	if err != nil {
		t.Fatalf("Exec cat: %v", err)
	}
	if string(res.Stdout) != content {
		t.Errorf("cat = %q, want %q", res.Stdout, content)
	}
	// ReadFile round-trips the content.
	rc, err := d.ReadFile(ctx, id, "/workspace/main.py")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != content {
		t.Errorf("ReadFile = %q, want %q", got, content)
	}
	// ListFiles shows it.
	entries, err := d.ListFiles(ctx, id, "/workspace")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "main.py" {
			found = true
		}
	}
	if !found {
		t.Errorf("main.py not in listing %+v", entries)
	}
	// DeleteFile then ReadFile → not found.
	if err := d.DeleteFile(ctx, id, "/workspace/main.py"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := d.ReadFile(ctx, id, "/workspace/main.py"); err != runtime.ErrFileNotFound {
		t.Errorf("ReadFile after delete = %v, want ErrFileNotFound", err)
	}
}

// TestExec_network_is_denied proves deny-all: a network egress attempt from inside
// the sandbox fails because the container has no network (NetworkMode=none). We
// assert wget is PRESENT (rc != 127) and that egress FAILED (rc != 0) — so a
// missing tool can't masquerade as "network denied".
func TestExec_network_is_denied(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("net")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"sh", "-c", "wget -T 3 -q -O- http://1.1.1.1; echo rc=$?"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	out := string(res.Stdout)
	if strings.Contains(out, "rc=0") {
		t.Errorf("network egress unexpectedly succeeded: %q", out)
	}
	if strings.Contains(out, "rc=127") {
		t.Fatalf("wget missing — test would false-positive; output %q", out)
	}
	if !strings.Contains(out, "rc=") {
		t.Errorf("no exit code captured: %q", out)
	}
}

// TestExec_command_injection_is_inert proves the argv discipline: a file path
// containing shell metacharacters is a literal filename, never executed. Writing
// to "/workspace/a; touch pwned" must NOT create "/workspace/pwned".
func TestExec_command_injection_is_inert(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("inj")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := d.WriteFile(ctx, id, "/workspace/a; touch pwned", bytes.NewBufferString("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// The injection target must NOT exist — the ';' was part of the filename.
	if _, err := d.ReadFile(ctx, id, "/workspace/pwned"); err != runtime.ErrFileNotFound {
		t.Errorf("command injection executed: /workspace/pwned exists (err=%v)", err)
	}
}

// TestExec_timeout_is_enforced proves a hung command is bounded: sleep 5 with a
// 1s timeout returns quickly (well under 5s), not after the full sleep.
func TestExec_timeout_is_enforced(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("to")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	start := time.Now()
	_, _ = d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"sleep", "5"}, Timeout: time.Second})
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("exec ran %v — timeout not enforced (expected ~1s)", elapsed)
	}
}

func TestExecStreamDeliversEachStreamSeparatelyAndTheExitCode(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("stream")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := map[string]string{}

	res, err := d.ExecStream(ctx, id, runtime.ExecRequest{Cmd: []string{"sh", "-c", "echo out; echo err >&2; exit 4"}},
		func(c runtime.StreamChunk) { got[c.Stream] += string(c.Data) })

	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if res.ExitCode != 4 || got["stdout"] != "out\n" || got["stderr"] != "err\n" {
		t.Errorf("exit=%d stdout=%q stderr=%q, want 4, out and err", res.ExitCode, got["stdout"], got["stderr"])
	}
}

func TestExecStreamDeliversOutputWhileTheCommandRuns(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("stream-live")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var firstAt time.Time

	_, err := d.ExecStream(ctx, id, runtime.ExecRequest{Cmd: []string{"sh", "-c", "echo first; sleep 2; echo second"}},
		func(runtime.StreamChunk) {
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
		})
	ended := time.Now()

	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if firstAt.IsZero() || ended.Sub(firstAt) < time.Second {
		t.Errorf("first chunk arrived %v before the end, want at least 1s", ended.Sub(firstAt))
	}
}

func TestExecStreamTimeoutIsATimeoutError(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("stream-timeout")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: id, TenantID: "t", Image: testImage}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := d.ExecStream(ctx, id, runtime.ExecRequest{Cmd: []string{"sleep", "30"}, Timeout: time.Second},
		func(runtime.StreamChunk) {})

	if !errors.Is(err, runtime.ErrTimeout) {
		t.Errorf("ExecStream err = %v, want ErrTimeout", err)
	}
}

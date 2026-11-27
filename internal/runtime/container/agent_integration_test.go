//go:build integration

package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

const pythonImage = "python:3.12-slim"

// newAgentDriver returns a driver whose sandboxes run an lsb-agent built from
// this tree.
func newAgentDriver(t *testing.T) *Driver {
	t.Helper()
	d, _ := newDriver(t)
	d.allowed[pythonImage] = true
	bin := filepath.Join(t.TempDir(), "lsb-agent")
	build := exec.Command("go", "build", "-o", bin, "github.com/getloopscale/loop-sandbox/cmd/lsb-agent")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build lsb-agent: %v\n%s", err, out)
	}
	if err := d.EnableAgent(context.Background(), bin); err != nil {
		t.Fatalf("EnableAgent: %v", err)
	}
	return d
}

func agentSandbox(t *testing.T, d *Driver, image string) runtime.SandboxID {
	t.Helper()
	id := runtime.SandboxID("sbx_agent_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")))
	if _, err := d.Create(context.Background(), runtime.CreateRequest{SandboxID: id, TenantID: "tnt_test", Image: image}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), id) })
	return id
}

func TestSessionKeepsStateBetweenCells(t *testing.T) {
	d := newAgentDriver(t)
	id := agentSandbox(t, d, pythonImage)
	ctx := context.Background()
	if err := d.StartSession(ctx, id, "s1"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if _, err := d.ExecSession(ctx, id, "s1", "x = 41"); err != nil {
		t.Fatalf("first cell: %v", err)
	}

	res, err := d.ExecSession(ctx, id, "s1", "print(x + 1)")

	if err != nil || res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "42" {
		t.Errorf("res = %+v err = %v, want exit 0 and 42", res, err)
	}
}

func TestSessionRunsAsTheSandboxUser(t *testing.T) {
	d := newAgentDriver(t)
	id := agentSandbox(t, d, pythonImage)
	ctx := context.Background()
	if err := d.StartSession(ctx, id, "s1"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	res, err := d.ExecSession(ctx, id, "s1", "import os; print(os.getuid())")

	if err != nil || strings.TrimSpace(string(res.Stdout)) != "65534" {
		t.Errorf("res = %+v err = %v, want uid 65534", res, err)
	}
}

func TestExecStillWorksInASandboxThatRunsTheAgent(t *testing.T) {
	d := newAgentDriver(t)
	id := agentSandbox(t, d, testImage)

	res, err := d.Exec(context.Background(), id, runtime.ExecRequest{Cmd: []string{"sh", "-c", "echo hi > /workspace/f && cat /workspace/f"}})

	if err != nil || res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "hi" {
		t.Errorf("res = %+v err = %v, want hi", res, err)
	}
}

func TestOrphanedProcessIsReapedByTheAgent(t *testing.T) {
	d := newAgentDriver(t)
	id := agentSandbox(t, d, testImage)
	ctx := context.Background()
	// The inner sleep outlives its parent shell, is adopted by PID 1 and ends.
	if _, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"sh", "-c", "(sleep 0.2 &) ; exit 0"}}); err != nil {
		t.Fatalf("spawn orphan: %v", err)
	}

	res, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"sh", "-c", "sleep 1; grep -l '^State:.Z' /proc/[0-9]*/status | wc -l"}})

	if err != nil || strings.TrimSpace(string(res.Stdout)) != "0" {
		t.Errorf("zombie count = %q err = %v, want 0", res.Stdout, err)
	}
}

func TestSessionWithoutTheAgentIsRefused(t *testing.T) {
	d, _ := newDriver(t)

	err := d.StartSession(context.Background(), "sbx_any", "s1")

	if err == nil || d.CapabilityEnabled(runtime.CapSessions) {
		t.Errorf("err = %v, sessions enabled = %v; want an error and sessions disabled", err, d.CapabilityEnabled(runtime.CapSessions))
	}
}

func TestSessionInAnImageWithoutPythonIsNoInterpreter(t *testing.T) {
	d := newAgentDriver(t)
	id := agentSandbox(t, d, testImage)

	err := d.StartSession(context.Background(), id, "s1")

	if !errors.Is(err, runtime.ErrNoInterpreter) {
		t.Errorf("err = %v, want ErrNoInterpreter", err)
	}
}

func TestPTYResizeChangesTheSizeTheShellSees(t *testing.T) {
	d := newAgentDriver(t)
	id := agentSandbox(t, d, testImage)
	term, err := d.OpenPTY(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("OpenPTY: %v", err)
	}
	defer func() { _ = term.Close() }()
	if err := term.Resize(40, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	if _, err := term.Write([]byte("stty size\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	seen := make(chan string, 1)
	go func() {
		var out strings.Builder
		buf := make([]byte, 4096)
		for !strings.Contains(out.String(), "40 100") {
			n, rerr := term.Read(buf)
			if rerr != nil {
				break
			}
			out.Write(buf[:n])
		}
		seen <- out.String()
	}()
	select {
	case out := <-seen:
		if !strings.Contains(out, "40 100") {
			t.Errorf("terminal output = %q, want it to report 40 100", out)
		}
	case <-time.After(20 * time.Second):
		t.Error("the terminal never reported size 40 100")
	}
}

func TestClosingThePTYEndsItsShell(t *testing.T) {
	d := newAgentDriver(t)
	id := agentSandbox(t, d, testImage)
	ctx := context.Background()
	term, err := d.OpenPTY(ctx, id, nil)
	if err != nil {
		t.Fatalf("OpenPTY: %v", err)
	}
	if _, err := term.Write([]byte("exec sleep 4242\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	running := func() string {
		res, rerr := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"sh", "-c", "sleep 1; ps | grep -c '[s]leep 4242'"}})
		if rerr != nil {
			t.Fatalf("ps: %v", rerr)
		}
		return strings.TrimSpace(string(res.Stdout))
	}
	if got := running(); got != "1" {
		t.Fatalf("processes running sleep 4242 before the close = %q, want 1", got)
	}

	_ = term.Close()

	if got := running(); got != "0" {
		t.Errorf("processes running sleep 4242 after the close = %q, want 0", got)
	}
}

// A host that runs other workloads as the same unprivileged user, as a node shared
// with pods does. RLIMIT_NPROC counts every thread of a user id on the whole host, so a
// sandbox that carried one could not start its agent there: exec failed with EAGAIN.
func TestSandboxStartsOnAHostWhereItsUserAlreadyRunsManyProcesses(t *testing.T) {
	const crowd = "lsb-test-crowd"
	_ = exec.Command("docker", "rm", "-f", crowd).Run()
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", crowd, "--user", "65534:65534", "--pids-limit", "1024",
		"busybox:latest", "sh", "-c", "i=0; while [ $i -lt 600 ]; do sleep 600 & i=$((i+1)); done; wait").CombinedOutput()
	if err != nil {
		t.Fatalf("start the crowd of processes: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", crowd).Run() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		top, _ := exec.Command("docker", "top", crowd).Output()
		if strings.Count(string(top), "sleep 600") >= 600 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the crowd never reached 600 processes:\n%s", top)
		}
		time.Sleep(200 * time.Millisecond)
	}

	d := newAgentDriver(t)
	id := agentSandbox(t, d, pythonImage)

	res, err := d.Exec(context.Background(), id, runtime.ExecRequest{Cmd: []string{"echo", "started"}, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("exec in the sandbox: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "started" {
		t.Fatalf("exec in the sandbox: exit %d, %q%q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

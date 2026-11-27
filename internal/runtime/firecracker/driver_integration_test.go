//go:build firecracker

// These tests boot REAL Firecracker microVMs on the host KVM. They require:
//   - /dev/kvm accessible
//   - LOOPSANDBOX_FC_BIN, _KERNEL, _ROOTFS pointing at real assets
//     (firecracker binary, vmlinux, and the lsb-agent rootfs from scripts/fc-rootfs.sh)
//
// Run with:  go test -tags=firecracker ./internal/runtime/firecracker/ -v
package firecracker

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// restoreOne restores a single VM from the enabled warm pool's base snapshot,
// letting a test drive the warm path deterministically (not via the async filler).
func (d *Driver) restoreOne(ctx context.Context) (*runtime.Sandbox, *vm, error) {
	v, err := d.restore(ctx, d.pool.snap, nil)
	if err != nil {
		return nil, nil, err
	}
	return &runtime.Sandbox{ID: v.id, State: runtime.StateRunning}, v, nil
}

func testDriver(t *testing.T) *Driver {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not available")
	}
	if os.Getenv("LOOPSANDBOX_FC_BIN") == "" {
		t.Skip("firecracker assets not configured: LOOPSANDBOX_FC_BIN is unset")
	}
	// A short directory: t.TempDir embeds the test name, and a VM socket path under
	// it would pass the 107 byte limit of a unix socket.
	stateDir, err := os.MkdirTemp("", "lsb-fc-")
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	d, err := New(Config{
		FirecrackerBin: os.Getenv("LOOPSANDBOX_FC_BIN"),
		KernelPath:     os.Getenv("LOOPSANDBOX_FC_KERNEL"),
		RootfsPath:     os.Getenv("LOOPSANDBOX_FC_ROOTFS"),
		StateDir:       stateDir,
	})
	if err != nil {
		t.Fatalf("new firecracker driver: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestFirecracker_coldboot_exec_and_files(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()

	sb, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "fc-1", TenantID: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.State != runtime.StateRunning {
		t.Fatalf("state = %q, want running", sb.State)
	}

	// exec echo
	res, err := d.Exec(ctx, "fc-1", runtime.ExecRequest{Cmd: []string{"echo", "-n", "hello-microvm"}})
	if err != nil {
		t.Fatalf("Exec echo: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "hello-microvm" {
		t.Errorf("echo = exit %d out %q, want 0 'hello-microvm'", res.ExitCode, res.Stdout)
	}

	// write → cat (real subprocess reads the written file inside the VM)
	if err := d.WriteFile(ctx, "fc-1", "/workspace/f.txt", bytes.NewBufferString("in-vm-data")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cat, err := d.Exec(ctx, "fc-1", runtime.ExecRequest{Cmd: []string{"cat", "/workspace/f.txt"}})
	if err != nil || string(cat.Stdout) != "in-vm-data" {
		t.Fatalf("cat = %q (err %v), want 'in-vm-data'", cat.Stdout, err)
	}

	// ReadFile round-trip
	rc, err := d.ReadFile(ctx, "fc-1", "/workspace/f.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "in-vm-data" {
		t.Errorf("ReadFile = %q, want 'in-vm-data'", b)
	}

	// ListFiles
	ents, err := d.ListFiles(ctx, "fc-1", "/workspace")
	if err != nil || len(ents) != 1 || ents[0].Name != "f.txt" {
		t.Errorf("ListFiles = %+v (err %v), want [f.txt]", ents, err)
	}

	// Destroy → Inspect destroyed; second destroy idempotent
	if err := d.Destroy(ctx, "fc-1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if s, _ := d.Inspect(ctx, "fc-1"); s.State != runtime.StateDestroyed {
		t.Errorf("post-destroy state = %q, want destroyed", s.State)
	}
	if err := d.Destroy(ctx, "fc-1"); err != nil {
		t.Errorf("second Destroy not idempotent: %v", err)
	}
}

func TestFirecracker_exec_missing_file_and_nonzero_exit(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "fc-2"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// non-zero exit is a normal result (US-02), not a Go error
	res, err := d.Exec(ctx, "fc-2", runtime.ExecRequest{Cmd: []string{"cat", "/workspace/nope"}})
	if err != nil {
		t.Fatalf("Exec cat-missing returned Go error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("exit = 0, want non-zero for missing file")
	}
	// ReadFile of a missing path → ErrFileNotFound
	if _, err := d.ReadFile(ctx, "fc-2", "/workspace/nope"); err != runtime.ErrFileNotFound {
		t.Errorf("ReadFile missing = %v, want ErrFileNotFound", err)
	}
}

func TestFirecracker_warmpool_restore_is_ready(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if err := d.EnableWarmPool(ctx, 1); err != nil {
		t.Fatalf("EnableWarmPool: %v", err)
	}
	// Restore one VM directly and prove it is immediately usable.
	_, v, err := d.restoreOne(ctx)
	if err != nil {
		t.Fatalf("restoreOne: %v", err)
	}
	t.Cleanup(func() { _ = killVM(v) })
	d.adopt("warm-1", v)
	res, err := d.Exec(ctx, "warm-1", runtime.ExecRequest{Cmd: []string{"echo", "-n", "warm"}})
	if err != nil || string(res.Stdout) != "warm" {
		t.Fatalf("warm exec = %q (err %v), want 'warm'", res.Stdout, err)
	}
}

func TestFirecracker_exec_timeout_maps_to_ErrTimeout(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "fc-3"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := d.Exec(ctx, "fc-3", runtime.ExecRequest{Cmd: []string{"sleep", "5"}, Timeout: 300 * time.Millisecond})
	if err != runtime.ErrTimeout {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
}

// B1: a caller that passes NO timeout must still be bounded by MaxExecTimeout, so a
// wedged/hostile guest can never hang the control-plane thread forever.
func TestFirecracker_exec_zero_timeout_is_bounded_by_ceiling(t *testing.T) {
	d := testDriver(t)
	d.cfg.MaxExecTimeout = 300 * time.Millisecond // tighten the ceiling for the test
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "fc-b1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Timeout: 0 (unset) → the ceiling applies, not "infinite".
	_, err := d.Exec(ctx, "fc-b1", runtime.ExecRequest{Cmd: []string{"sleep", "5"}, Timeout: 0})
	if err != runtime.ErrTimeout {
		t.Errorf("err = %v, want ErrTimeout (ceiling must bound a zero-timeout exec)", err)
	}
}

func TestFirecracker_exec_output_truncated_flag(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "fc-tr"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := d.Exec(ctx, "fc-tr", runtime.ExecRequest{Cmd: []string{"head", "-c", "9437184", "/dev/zero"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Truncated || len(res.Stdout) != 8<<20 {
		t.Errorf("truncated=%v len=%d, want true & 8MiB cap", res.Truncated, len(res.Stdout))
	}
}

func TestFirecracker_exec_on_unknown_sandbox_is_not_found(t *testing.T) {
	d := testDriver(t)
	_, err := d.Exec(context.Background(), "does-not-exist", runtime.ExecRequest{Cmd: []string{"echo"}})
	if err != runtime.ErrSandboxNotFound {
		t.Errorf("err = %v, want ErrSandboxNotFound", err)
	}
	if !strings.Contains(d.Name(), "firecracker") {
		t.Errorf("Name = %q", d.Name())
	}
}

//go:build firecracker

package firecracker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func shell(t *testing.T, d *Driver, id runtime.SandboxID, script string) *runtime.ExecResult {
	t.Helper()
	res, err := d.Exec(context.Background(), id, runtime.ExecRequest{Cmd: []string{"sh", "-c", script}, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("exec %q in %s: %v", script, id, err)
	}
	return res
}

func createVM(t *testing.T, d *Driver, id runtime.SandboxID) {
	t.Helper()
	if _, err := d.Create(context.Background(), runtime.CreateRequest{SandboxID: id}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), id) })
}

func TestFirecracker_volume_attached_to_a_running_sandbox_is_mounted(t *testing.T) {
	d := testDriver(t)
	createVM(t, d, "vol-run")

	if err := d.AttachVolume(context.Background(), "vol-run", "vol_mounted", "data", 1); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	res := shell(t, d, "vol-run", "grep -c ' /workspace/data ext4 ' /proc/mounts")
	if strings.TrimSpace(string(res.Stdout)) != "1" {
		t.Errorf("/workspace/data is not an ext4 mount: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestFirecracker_volume_data_survives_destroy_and_reattaches_to_another_sandbox(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	createVM(t, d, "vol-first")
	if err := d.AttachVolume(ctx, "vol-first", "vol_durable", "data", 1); err != nil {
		t.Fatalf("attach to the first sandbox: %v", err)
	}
	shell(t, d, "vol-first", "printf durable > /workspace/data/note.txt")
	if err := d.Destroy(ctx, "vol-first"); err != nil { // abrupt kill, no unmount
		t.Fatalf("destroy the first sandbox: %v", err)
	}
	createVM(t, d, "vol-second")

	if err := d.AttachVolume(ctx, "vol-second", "vol_durable", "data", 1); err != nil {
		t.Fatalf("attach to the second sandbox: %v", err)
	}

	res := shell(t, d, "vol-second", "cat /workspace/data/note.txt")
	if string(res.Stdout) != "durable" {
		t.Errorf("second sandbox read %q (stderr %q), want %q", res.Stdout, res.Stderr, "durable")
	}
}

func TestFirecracker_detached_volume_is_unmounted_and_its_slot_is_free(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	createVM(t, d, "vol-detach")
	if err := d.AttachVolume(ctx, "vol-detach", "vol_a", "data", 1); err != nil {
		t.Fatalf("attach: %v", err)
	}

	if err := d.DetachVolume(ctx, "vol-detach", "vol_a"); err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}

	res := shell(t, d, "vol-detach", "grep -c ' /workspace/data ' /proc/mounts; test -e /workspace/data; echo exists=$?")
	if got := string(res.Stdout); got != "0\nexists=1\n" {
		t.Errorf("after detach: %q, want no mount and no mount point", got)
	}
	if err := d.AttachVolume(ctx, "vol-detach", "vol_b", "other", 1); err != nil {
		t.Errorf("attaching another volume to the freed slot: %v", err)
	}
}

func TestFirecracker_second_volume_on_one_sandbox_is_a_conflict(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	createVM(t, d, "vol-two")
	if err := d.AttachVolume(ctx, "vol-two", "vol_one", "one", 1); err != nil {
		t.Fatalf("attach the first volume: %v", err)
	}

	err := d.AttachVolume(ctx, "vol-two", "vol_two", "two", 1)

	if !errors.Is(err, runtime.ErrVolumeConflict) {
		t.Errorf("second attach = %v, want ErrVolumeConflict", err)
	}
}

func TestFirecracker_fork_of_a_sandbox_with_a_volume_is_a_conflict_and_the_parent_keeps_running(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	createVM(t, d, "vol-parent")
	if err := d.AttachVolume(ctx, "vol-parent", "vol_forked", "data", 1); err != nil {
		t.Fatalf("attach: %v", err)
	}

	_, err := d.Fork(ctx, "vol-parent", "vol-child")

	if !errors.Is(err, runtime.ErrVolumeConflict) {
		t.Errorf("Fork = %v, want ErrVolumeConflict", err)
	}
	if res := shell(t, d, "vol-parent", "echo alive"); string(res.Stdout) != "alive\n" {
		t.Errorf("parent after the refused fork: %q", res.Stdout)
	}
}

func TestFirecracker_volume_stays_mounted_and_durable_across_pause_and_resume(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	createVM(t, d, "vol-pause")
	if err := d.AttachVolume(ctx, "vol-pause", "vol_paused", "data", 1); err != nil {
		t.Fatalf("attach: %v", err)
	}
	shell(t, d, "vol-pause", "printf before > /workspace/data/a.txt")
	if err := d.Pause(ctx, "vol-pause"); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	if err := d.Resume(ctx, "vol-pause"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	res := shell(t, d, "vol-pause", "cat /workspace/data/a.txt && printf after > /workspace/data/b.txt && cat /workspace/data/b.txt")
	if string(res.Stdout) != "beforeafter" {
		t.Errorf("after resume: stdout=%q stderr=%q, want %q", res.Stdout, res.Stderr, "beforeafter")
	}
}

// A fork must not share a writable block device with its parent: a guest that
// writes its raw volume slot would otherwise leak data into the other sandbox.
func TestFirecracker_fork_does_not_share_the_volume_slot_with_its_parent(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	createVM(t, d, "slot-parent")
	if _, err := d.Fork(ctx, "slot-parent", "slot-child"); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(ctx, "slot-child") })

	wrote := shell(t, d, "slot-child", "printf LEAKED | dd of=/dev/vdb bs=1 conv=fsync 2>/dev/null; head -c 6 /dev/vdb")
	if string(wrote.Stdout) != "LEAKED" {
		t.Fatalf("the writer could not write its own slot (read back %q), so the test proves nothing", wrote.Stdout)
	}

	res := shell(t, d, "slot-parent", "head -c 6 /dev/vdb | tr -d '\\000' | wc -c")
	if strings.TrimSpace(string(res.Stdout)) != "0" {
		t.Errorf("the parent read %s non-zero bytes the fork wrote to its slot (stderr %q)", strings.TrimSpace(string(res.Stdout)), res.Stderr)
	}
}

func TestFirecracker_warm_pool_sandboxes_do_not_share_the_volume_slot(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if err := d.EnableWarmPool(ctx, 2); err != nil {
		t.Fatalf("EnableWarmPool: %v", err)
	}
	_, first, err := d.restoreOne(ctx)
	if err != nil {
		t.Fatalf("restore the first VM: %v", err)
	}
	t.Cleanup(func() { _ = killVM(first) })
	_, second, err := d.restoreOne(ctx)
	if err != nil {
		t.Fatalf("restore the second VM: %v", err)
	}
	t.Cleanup(func() { _ = killVM(second) })
	d.adopt("warm-first", first)
	d.adopt("warm-second", second)

	wrote := shell(t, d, "warm-first", "printf LEAKED | dd of=/dev/vdb bs=1 conv=fsync 2>/dev/null; head -c 6 /dev/vdb")
	if string(wrote.Stdout) != "LEAKED" {
		t.Fatalf("the writer could not write its own slot (read back %q), so the test proves nothing", wrote.Stdout)
	}

	res := shell(t, d, "warm-second", "head -c 6 /dev/vdb | tr -d '\\000' | wc -c")
	if strings.TrimSpace(string(res.Stdout)) != "0" {
		t.Errorf("the second warm VM read %s non-zero bytes the first wrote to its slot", strings.TrimSpace(string(res.Stdout)))
	}
}

func TestFirecracker_pty_resize_changes_the_size_the_shell_sees(t *testing.T) {
	d := testDriver(t)
	createVM(t, d, "pty-resize")
	pty, err := d.OpenPTY(context.Background(), "pty-resize", nil)
	if err != nil {
		t.Fatalf("OpenPTY: %v", err)
	}
	defer func() { _ = pty.Close() }()

	if err := pty.Resize(40, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if _, err := pty.Write([]byte("stty size\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	output := make(chan string, 1)
	go func() {
		var seen strings.Builder
		buf := make([]byte, 4096)
		for !strings.Contains(seen.String(), "40 100") {
			n, rerr := pty.Read(buf)
			seen.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		output <- seen.String()
	}()
	select {
	case got := <-output:
		if !strings.Contains(got, "40 100") {
			t.Errorf("terminal output %q does not report size 40 100", got)
		}
	case <-time.After(15 * time.Second):
		t.Error("the terminal did not report size 40 100 within 15s")
	}
}

func TestNewRejectsAStateDirWhoseSocketPathsWouldBeTooLong(t *testing.T) {
	cfg := testDriver(t).cfg
	cfg.StateDir = "/tmp/" + strings.Repeat("d", 80)

	_, err := New(cfg)

	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("New = %v, want a state-dir-too-long error", err)
	}
}

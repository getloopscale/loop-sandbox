//go:build integration

package container

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func TestDriver_pause_freezes_the_sandbox_and_resume_keeps_its_state(t *testing.T) {
	d, cli := newDriver(t)
	ctx := context.Background()
	id := uid("pause")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{
		SandboxID: id, TenantID: "tenant-a", Image: testImage,
		CPUNanos: 1_000_000_000, MemoryBytes: 256 << 20, PidsLimit: 128,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := d.WriteFile(ctx, id, "/workspace/kept.txt", strings.NewReader("before the pause")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var pd runtime.PauseDriver = d
	if err := pd.Pause(ctx, id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	info, err := cli.ContainerInspect(ctx, containerName(id))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !info.State.Paused {
		t.Fatalf("container state = %q, want paused", info.State.Status)
	}
	execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := d.Exec(execCtx, id, runtime.ExecRequest{Cmd: []string{"true"}}); err == nil {
		t.Fatal("Exec in a paused sandbox succeeded, want an error")
	}

	if err := pd.Resume(ctx, id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	res, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"cat", "/workspace/kept.txt"}})
	if err != nil {
		t.Fatalf("Exec after resume: %v", err)
	}
	if got := string(res.Stdout); got != "before the pause" {
		t.Fatalf("file after resume = %q, want the content written before the pause", got)
	}
}

func TestDriver_pause_of_an_unknown_sandbox_is_not_found(t *testing.T) {
	d, _ := newDriver(t)
	err := d.Pause(context.Background(), uid("ghost"))
	if err == nil || !strings.Contains(err.Error(), runtime.ErrSandboxNotFound.Error()) {
		t.Fatalf("Pause of an unknown sandbox = %v, want %v", err, runtime.ErrSandboxNotFound)
	}
}

func TestDriver_destroy_removes_a_paused_sandbox(t *testing.T) {
	d, cli := newDriver(t)
	ctx := context.Background()
	id := uid("pausekill")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{
		SandboxID: id, TenantID: "tenant-a", Image: testImage,
		CPUNanos: 1_000_000_000, MemoryBytes: 256 << 20, PidsLimit: 128,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := d.Pause(ctx, id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy of a paused sandbox: %v", err)
	}
	if _, err := cli.ContainerInspect(ctx, containerName(id)); err == nil {
		t.Fatal("the container still exists after Destroy")
	}
}

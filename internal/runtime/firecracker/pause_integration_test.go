//go:build firecracker

package firecracker

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// TestFirecracker_pause_resume_preserves_state: write in-VM state, pause the microVM
// (snapshot RAM to durable storage + kill the process → compute released), resume it
// (restore into a fresh process), and read the state back — proving hibernation preserves
// the sandbox's in-memory state on a REAL microVM (M16).
func TestFirecracker_pause_resume_preserves_state(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "pz1", TenantID: "t"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// /workspace is an in-guest tmpfs → its contents live in guest RAM, so surviving a
	// snapshot/restore proves the RAM (not just a disk) was captured.
	if err := d.WriteFile(ctx, "pz1", "/workspace/state", bytes.NewBufferString("before-pause")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := d.Pause(ctx, "pz1"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// While paused there is no live VM: an exec/file op must fail (compute released).
	if _, err := d.ReadFile(ctx, "pz1", "/workspace/state"); err == nil {
		t.Errorf("ReadFile on a paused sandbox should fail (no live VM)")
	}

	if err := d.Resume(ctx, "pz1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	rc, err := d.ReadFile(ctx, "pz1", "/workspace/state")
	if err != nil {
		t.Fatalf("ReadFile after resume: %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "before-pause" {
		t.Errorf("state after resume = %q, want 'before-pause' (in-memory state did NOT survive)", b)
	}
	if err := d.Destroy(ctx, "pz1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}

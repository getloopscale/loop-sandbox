//go:build firecracker

// Real fork of a running Firecracker microVM (M13): the fork inherits the parent's state
// at the fork instant, and subsequent writes on each side are isolated.
package firecracker

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func readFile(t *testing.T, d *Driver, id runtime.SandboxID, path string) string {
	t.Helper()
	rc, err := d.ReadFile(context.Background(), id, path)
	if err != nil {
		return "<missing>"
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestFirecracker_fork_isolates_state(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "parent", TenantID: "t"}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	// Parent state at fork time.
	if err := d.WriteFile(ctx, "parent", "/workspace/marker.txt", bytes.NewReader([]byte("at-fork"))); err != nil {
		t.Fatalf("write parent: %v", err)
	}

	fork, err := d.Fork(ctx, "parent", "fork")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if fork.State != runtime.StateRunning {
		t.Fatalf("fork state = %v, want running", fork.State)
	}

	// 1. The fork INHERITED the parent's state at the fork instant.
	if got := readFile(t, d, "fork", "/workspace/marker.txt"); strings.TrimSpace(got) != "at-fork" {
		t.Errorf("fork marker = %q, want 'at-fork' (state not inherited)", got)
	}

	// 2. A write to the PARENT after the fork does NOT appear in the fork (isolated).
	if err := d.WriteFile(ctx, "parent", "/workspace/marker.txt", bytes.NewReader([]byte("parent-only"))); err != nil {
		t.Fatalf("write parent post-fork: %v", err)
	}
	if got := readFile(t, d, "fork", "/workspace/marker.txt"); strings.TrimSpace(got) != "at-fork" {
		t.Errorf("fork marker = %q after parent write, want unchanged 'at-fork' (isolation breached)", got)
	}

	// 3. A write to the FORK does NOT appear in the parent (isolated the other way).
	if err := d.WriteFile(ctx, "fork", "/workspace/forkonly.txt", bytes.NewReader([]byte("fork"))); err != nil {
		t.Fatalf("write fork: %v", err)
	}
	if got := readFile(t, d, "parent", "/workspace/forkonly.txt"); got != "<missing>" {
		t.Errorf("parent sees fork-only file = %q, want <missing> (isolation breached)", got)
	}

	// 4. The parent kept its own post-fork write.
	if got := readFile(t, d, "parent", "/workspace/marker.txt"); strings.TrimSpace(got) != "parent-only" {
		t.Errorf("parent marker = %q, want 'parent-only'", got)
	}
	_ = d.Destroy(ctx, "fork")
	_ = d.Destroy(ctx, "parent")
}

// TestFirecracker_concurrent_forks_of_same_parent proves the per-parent serialization
// (HIGH-1 fix): two concurrent forks of the same running VM both succeed without
// corrupting each other's snapshot.
func TestFirecracker_concurrent_forks_of_same_parent(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "cp", TenantID: "t"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := d.WriteFile(ctx, "cp", "/workspace/m.txt", bytes.NewReader([]byte("base"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	errs := make(chan error, 2)
	for _, fid := range []runtime.SandboxID{"f1", "f2"} {
		go func(fid runtime.SandboxID) {
			_, err := d.Fork(ctx, "cp", fid)
			errs <- err
		}(fid)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent fork failed: %v", err)
		}
	}
	for _, fid := range []runtime.SandboxID{"f1", "f2"} {
		if got := readFile(t, d, fid, "/workspace/m.txt"); strings.TrimSpace(got) != "base" {
			t.Errorf("fork %s inherited %q, want 'base'", fid, got)
		}
		_ = d.Destroy(ctx, fid)
	}
	_ = d.Destroy(ctx, "cp")
}

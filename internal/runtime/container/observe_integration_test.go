//go:build integration

package container

import (
	"bytes"
	"context"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func TestDriver_metrics_report_how_full_the_workspace_is(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("disk")
	t.Cleanup(func() { _ = d.Destroy(ctx, id) })
	if _, err := d.Create(ctx, runtime.CreateRequest{
		SandboxID: id, TenantID: "tenant-a", Image: testImage,
		CPUNanos: 1_000_000_000, MemoryBytes: 256 << 20, PidsLimit: 128,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	const written = 1 << 20
	if err := d.WriteFile(ctx, id, "/workspace/blob.bin", bytes.NewReader(make([]byte, written))); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m, err := d.SandboxMetrics(ctx, id)
	if err != nil {
		t.Fatalf("SandboxMetrics: %v", err)
	}
	if m.DiskUsedBytes < written || m.DiskLimitBytes <= m.DiskUsedBytes {
		t.Fatalf("disk = %d used of %d, want at least %d used and a larger limit", m.DiskUsedBytes, m.DiskLimitBytes, written)
	}
}

func TestDriver_metrics_of_a_paused_sandbox_come_without_the_disk(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uid("diskpause")
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

	m, err := d.SandboxMetrics(ctx, id)
	if err != nil {
		t.Fatalf("SandboxMetrics of a paused sandbox: %v", err)
	}
	if m.MemoryLimitBytes == 0 || m.DiskLimitBytes != 0 {
		t.Fatalf("metrics = %+v, want a memory limit and no disk figure", m)
	}
}

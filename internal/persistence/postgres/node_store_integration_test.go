//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
)

// A heartbeat inserts a node; ListReady returns it for its cell only.
func TestNodeStore_heartbeat_then_list_ready(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	reg := postgres.NewNodeStore(pool)
	now := time.Now().UTC()

	n := &domain.Node{ID: "node-a", CellID: "cell-0", Address: "10.0.0.1:9000",
		CpuCount: 8, CpuAllocated: 2, CpuPercent: 25, State: domain.NodeReady, LastHeartbeat: now}
	if err := reg.Heartbeat(ctx, n); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// A node in another cell must NOT appear in cell-0's ready set (multi-tenant cell isolation).
	other := &domain.Node{ID: "node-b", CellID: "cell-1", CpuCount: 8, State: domain.NodeReady, LastHeartbeat: now}
	if err := reg.Heartbeat(ctx, other); err != nil {
		t.Fatalf("heartbeat other: %v", err)
	}

	got, err := reg.ListReady(ctx, "cell-0")
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	if len(got) != 1 || got[0].ID != "node-a" {
		t.Fatalf("ListReady(cell-0) = %+v, want exactly node-a", got)
	}
	if got[0].CpuCount != 8 || got[0].CpuAllocated != 2 || got[0].CpuPercent != 25 {
		t.Errorf("capacity round-trip wrong: %+v", got[0])
	}
}

// A second heartbeat updates capacity + liveness; it must not create a duplicate row.
func TestNodeStore_heartbeat_upserts_capacity(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	reg := postgres.NewNodeStore(pool)
	now := time.Now().UTC()

	n := &domain.Node{ID: "node-a", CellID: "cell-0", CpuCount: 8, CpuAllocated: 1, State: domain.NodeReady, LastHeartbeat: now}
	if err := reg.Heartbeat(ctx, n); err != nil {
		t.Fatalf("heartbeat 1: %v", err)
	}
	n.CpuAllocated = 5
	n.CpuPercent = 60
	n.LastHeartbeat = now.Add(10 * time.Second)
	if err := reg.Heartbeat(ctx, n); err != nil {
		t.Fatalf("heartbeat 2: %v", err)
	}
	got, err := reg.ListReady(ctx, "cell-0")
	if err != nil {
		t.Fatalf("list ready: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 node (upsert, not duplicate), got %d", len(got))
	}
	if got[0].CpuAllocated != 5 || got[0].CpuPercent != 60 {
		t.Errorf("capacity not updated: %+v", got[0])
	}
}

// SetState(draining) removes a node from the ready set; a heartbeat must NOT clear the drain.
func TestNodeStore_drain_excludes_and_survives_heartbeat(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	reg := postgres.NewNodeStore(pool)
	now := time.Now().UTC()

	n := &domain.Node{ID: "node-a", CellID: "cell-0", CpuCount: 8, State: domain.NodeReady, LastHeartbeat: now}
	if err := reg.Heartbeat(ctx, n); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := reg.SetState(ctx, "node-a", domain.NodeDraining); err != nil {
		t.Fatalf("set draining: %v", err)
	}
	if got, _ := reg.ListReady(ctx, "cell-0"); len(got) != 0 {
		t.Fatalf("draining node must not be ready, got %+v", got)
	}
	// A heartbeat from a draining node keeps it draining (operator intent wins over liveness).
	n.LastHeartbeat = now.Add(5 * time.Second)
	if err := reg.Heartbeat(ctx, n); err != nil {
		t.Fatalf("heartbeat while draining: %v", err)
	}
	if got, _ := reg.ListReady(ctx, "cell-0"); len(got) != 0 {
		t.Errorf("heartbeat must not clear an operator drain, got %+v", got)
	}
}

// SetState on an unknown node is a typed ErrNotFound (fail-fast, no silent no-op).
func TestNodeStore_setstate_unknown_node_errnotfound(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	reg := postgres.NewNodeStore(pool)
	if err := reg.SetState(ctx, "ghost", domain.NodeDraining); err == nil {
		t.Errorf("SetState(unknown) = nil, want ErrNotFound")
	}
}

var _ domain.Registry = (*postgres.NodeStore)(nil)

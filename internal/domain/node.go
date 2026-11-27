package domain

import (
	"context"
	"time"
)

// NodeState is the health state of a worker node (M21). Mirrors the E2B node status machine
// (`e2b-infra` nodemanager/status.go): a node is placeable only while ready AND fresh.
type NodeState string

const (
	NodeReady     NodeState = "ready"     // accepting placements
	NodeDraining  NodeState = "draining"  // finish existing work, no NEW placements
	NodeUnhealthy NodeState = "unhealthy" // heartbeat lost or self-reported unhealthy
)

// Node is a worker host that runs sandboxes (M21). Capacity fields drive best-of-K placement;
// LastHeartbeat drives liveness (a node whose heartbeat is stale is treated unhealthy).
type Node struct {
	ID            string
	CellID        string
	Address       string  // where the API/worker reaches this node
	CpuCount      int64   // total vCPUs
	CpuAllocated  int64   // vCPUs already reserved by sandboxes
	CpuPercent    float64 // live CPU utilisation 0..100
	State         NodeState
	LastHeartbeat time.Time
}

// Stale reports whether the node's last heartbeat is older than thr as of now.
func (n *Node) Stale(now time.Time, thr time.Duration) bool {
	return now.Sub(n.LastHeartbeat) > thr
}

// Placeable reports whether a new sandbox may be placed on this node: ready AND not stale.
// A draining or unhealthy node is never placeable (drain ≠ kill — existing work continues).
func (n *Node) Placeable(now time.Time, thr time.Duration) bool {
	return n.State == NodeReady && !n.Stale(now, thr)
}

// Resources is the capacity a sandbox requests for placement (M21). CPU is the M21 signal;
// memory/GPU dimensions are a follow-up (M23+).
type Resources struct {
	CPUs int64
}

// Registry is the node-membership port (M21, DIP): the domain lists ready nodes and workers
// heartbeat through it. The PG-backed impl is the source of truth (no Nomad/Consul — ADR-1).
// A node that belongs to another cell MUST NOT appear in ListReady for a different cell.
type Registry interface {
	// ListReady returns the ready, non-stale nodes of a cell (the placement candidate set).
	ListReady(ctx context.Context, cellID string) ([]*Node, error)
	// Heartbeat upserts a node's liveness + capacity (a worker calls this periodically).
	Heartbeat(ctx context.Context, n *Node) error
	// SetState transitions a node's state (e.g. an operator draining it).
	SetState(ctx context.Context, id string, state NodeState) error
}

// Placer picks a node for a new sandbox within a cell (M21, DIP). Implemented by the best-of-K
// scheduler. Returns ErrNoCapacity when no ready node can host the request.
type Placer interface {
	Place(ctx context.Context, cellID string, req Resources) (*Node, error)
}

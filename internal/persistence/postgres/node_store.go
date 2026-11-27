package postgres

import (
	"context"
	"fmt"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NodeStore is the PostgreSQL-backed worker-node registry (M21). It satisfies domain.Registry;
// the domain never imports pgx. PG is the placement source of truth (no Nomad/Consul — ADR-1),
// so there is a single, transactional view of node membership (anti-split-brain).
type NodeStore struct {
	pool *pgxpool.Pool
}

// NewNodeStore returns a NodeStore over an existing pool.
func NewNodeStore(pool *pgxpool.Pool) *NodeStore { return &NodeStore{pool: pool} }

// ListReady returns the ready nodes of a cell (the placement candidate set). Heartbeat
// staleness is applied by the Placer (which owns the threshold), so this returns every node
// whose stored state is 'ready' for the cell — a draining/unhealthy node is excluded here.
func (s *NodeStore) ListReady(ctx context.Context, cellID string) ([]*domain.Node, error) {
	const q = `
		SELECT id, cell_id, address, cpu_count, cpu_allocated, cpu_percent, state, last_heartbeat
		FROM nodes
		WHERE cell_id = $1 AND state = 'ready'
		ORDER BY id`
	rows, err := s.pool.Query(ctx, q, cellID)
	if err != nil {
		return nil, fmt.Errorf("list ready nodes: %w", err)
	}
	defer rows.Close()
	var out []*domain.Node
	for rows.Next() {
		var n domain.Node
		var state string
		if err := rows.Scan(&n.ID, &n.CellID, &n.Address, &n.CpuCount, &n.CpuAllocated,
			&n.CpuPercent, &state, &n.LastHeartbeat); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		n.State = domain.NodeState(state)
		out = append(out, &n)
	}
	return out, rows.Err()
}

// Heartbeat upserts a node's capacity + liveness. On first heartbeat the node is 'ready'. On a
// repeat heartbeat the stored state is preserved when 'draining' (operator intent wins over
// liveness) but an 'unhealthy' node self-heals back to 'ready' — a fresh heartbeat proves the
// node recovered. Capacity/utilisation/address/last_heartbeat are always refreshed.
func (s *NodeStore) Heartbeat(ctx context.Context, n *domain.Node) error {
	cell := n.CellID
	if cell == "" {
		cell = domain.DefaultCell
	}
	const q = `
		INSERT INTO nodes (id, cell_id, address, cpu_count, cpu_allocated, cpu_percent, state, last_heartbeat)
		VALUES ($1, $2, $3, $4, $5, $6, 'ready', $7)
		ON CONFLICT (id) DO UPDATE SET
			cell_id        = EXCLUDED.cell_id,
			address        = EXCLUDED.address,
			cpu_count      = EXCLUDED.cpu_count,
			cpu_allocated  = EXCLUDED.cpu_allocated,
			cpu_percent    = EXCLUDED.cpu_percent,
			last_heartbeat = EXCLUDED.last_heartbeat,
			state          = CASE WHEN nodes.state = 'draining' THEN 'draining' ELSE 'ready' END`
	_, err := s.pool.Exec(ctx, q, n.ID, cell, n.Address, n.CpuCount, n.CpuAllocated, n.CpuPercent, n.LastHeartbeat)
	if err != nil {
		return fmt.Errorf("heartbeat node: %w", err)
	}
	return nil
}

// SetState transitions a node's state (e.g. an operator draining it, or a reconciler marking a
// stale node unhealthy). An unknown node is a typed ErrNotFound (fail-fast, no silent no-op).
func (s *NodeStore) SetState(ctx context.Context, id string, state domain.NodeState) error {
	const q = `UPDATE nodes SET state = $2 WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id, string(state))
	if err != nil {
		return fmt.Errorf("set node state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

var _ domain.Registry = (*NodeStore)(nil)

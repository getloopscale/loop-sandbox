// Package cells shards tenants across cells for horizontal scale (M11, B10). A cell
// is an isolated slice of the platform (its own worker pool, scheduler, and optional
// DB/broker). Routing is deterministic (a tenant always maps to the same cell) and
// STABLE under rebalance: adding or removing a cell remaps only a small fraction of
// tenants — achieved with Rendezvous (Highest-Random-Weight) hashing, NOT modulo
// (which reshuffles nearly everything when the cell count changes).
package cells

import (
	"fmt"

	"github.com/cespare/xxhash/v2"
)

// GPUCellID is the isolated cell GPU sandboxes are pinned to (M19), kept separate from CPU
// cells so device passthrough never shares a cell with a CPU sandbox.
const GPUCellID = "cell-gpu"

// Router maps a tenant to a cell using rendezvous hashing over the cell set.
type Router struct {
	cells   []string
	gpuCell string
}

// GPUCell returns the isolated GPU cell (M19).
func (r *Router) GPUCell() string {
	if r.gpuCell == "" {
		return GPUCellID
	}
	return r.gpuCell
}

// NewRouter builds a router over the given cell ids (must be non-empty, unique).
func NewRouter(cellIDs []string) (*Router, error) {
	if len(cellIDs) == 0 {
		return nil, fmt.Errorf("cells: at least one cell is required")
	}
	seen := map[string]bool{}
	for _, c := range cellIDs {
		if c == "" {
			return nil, fmt.Errorf("cells: empty cell id")
		}
		if seen[c] {
			return nil, fmt.Errorf("cells: duplicate cell id %q", c)
		}
		seen[c] = true
	}
	cp := append([]string(nil), cellIDs...)
	return &Router{cells: cp}, nil
}

// CellFor returns the cell a tenant is assigned to. Deterministic: the highest-weight
// (tenant, cell) pair wins, so the same tenant always maps to the same cell for a
// given cell set, and only ~1/N of tenants move when a cell is added/removed.
func (r *Router) CellFor(tenantID string) string {
	var best string
	var bestScore uint64
	for _, cell := range r.cells {
		s := weight(tenantID, cell)
		if s > bestScore || (s == bestScore && cell > best) {
			bestScore = s
			best = cell
		}
	}
	return best
}

// (No Cells() accessor yet: nothing enumerates the cell set — the router's only job so
// far is CellFor. Add it back with a real caller when a health/metrics surface needs it.)

// weight is the rendezvous score for a (tenant, cell) pair. A single xxhash over
// tenantID\x00cell is already well-distributed (avalanche), so adjacent cell ids yield
// uncorrelated scores — no second mixing pass needed.
func weight(tenantID, cell string) uint64 {
	var buf []byte
	buf = append(buf, tenantID...)
	buf = append(buf, 0) // delimiter so "a"+"bc" != "ab"+"c"
	buf = append(buf, cell...)
	return xxhash.Sum64(buf)
}

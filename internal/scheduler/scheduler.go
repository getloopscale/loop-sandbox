// Package scheduler implements capacity-aware "best-of-K" (power-of-K-choices) sandbox
// placement across worker nodes (M21). It ports E2B's proven algorithm
// (e2b-infra packages/api/internal/orchestrator/placement/placement_best_of_K.go): sample K
// ready nodes at random, score each by capacity (folding in in-flight reservations), pick the
// lowest. Random-K sampling beats a global least-busy scan under stale heartbeats — it avoids
// the thundering-herd hotspot where every concurrent placement targets the same "emptiest" node.
// The Placer satisfies the domain.Placer port (DIP): the app depends on the interface, not this.
package scheduler

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// Config tunes the placement algorithm. Defaults mirror E2B (placement_best_of_K.go:26-32).
type Config struct {
	K         int           // candidates sampled per placement (power-of-K-choices)
	R         float64       // over-commit ratio: usable capacity = R × CpuCount
	Alpha     float64       // weight on live CPU utilisation in the score
	Staleness time.Duration // heartbeat age past which a node is not placeable
}

// DefaultConfig returns the E2B defaults (K=3, R=4, Alpha=0.5, 90s staleness).
func DefaultConfig() Config {
	return Config{K: 3, R: 4, Alpha: 0.5, Staleness: 90 * time.Second}
}

// Placer picks a node for a new sandbox within a cell. Concurrency-safe: the
// candidate-filter + sample + reserve decision runs under one mutex so two concurrent
// placements cannot over-book a node (anti-double-book — see TestPlace_concurrent_no_double_book).
type Placer struct {
	reg   domain.Registry
	cfg   Config
	clock clock.Clock

	mu         sync.Mutex       // guards inProgress + rng + the placement decision
	inProgress map[string]int64 // nodeID → vCPUs reserved but not yet reflected in a heartbeat
	rng        *rand.Rand
}

// New builds a Placer over a node registry. The RNG is seeded deterministically: uniform
// sampling needs no entropy, and a fixed seed keeps placement benchmarks reproducible.
func New(reg domain.Registry, cfg Config, clk clock.Clock) *Placer {
	if cfg.K <= 0 || cfg.R <= 0 {
		cfg = DefaultConfig()
	}
	return &Placer{
		reg:        reg,
		cfg:        cfg,
		clock:      clk,
		inProgress: make(map[string]int64),
		rng:        rand.New(rand.NewSource(1)),
	}
}

// ScoreNode computes a node's placement score for req (LOWER is better). Mirrors
// placement_best_of_K.go:35-61: (cpuRequested + reserved + Alpha·usageAvg) / (R·CpuCount),
// where reserved folds the committed allocation and the in-flight reservations. A node with
// zero CPUs (mis-reported) scores +Inf so it is never chosen.
func (p *Placer) ScoreNode(n *domain.Node, req domain.Resources) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scoreLocked(n, req)
}

// scoreLocked is ScoreNode's body; the caller must hold p.mu (it reads inProgress).
func (p *Placer) scoreLocked(n *domain.Node, req domain.Resources) float64 {
	if n.CpuCount <= 0 {
		return math.Inf(1)
	}
	reserved := n.CpuAllocated + p.inProgress[n.ID]
	usageAvg := n.CpuPercent / 100.0
	return (float64(req.CPUs) + float64(reserved) + p.cfg.Alpha*usageAvg) / (p.cfg.R * float64(n.CpuCount))
}

// Reserve records an in-flight reservation for a node (exported for the app, which reserves at
// placement and releases once the create window closes). Place also calls it under the lock.
func (p *Placer) Reserve(nodeID string, cpus int64) {
	p.mu.Lock()
	p.inProgress[nodeID] += cpus
	p.mu.Unlock()
}

// Release drops an in-flight reservation once the create completes — the worker's next
// heartbeat then reports the real allocation. Clamped at zero (never negative).
func (p *Placer) Release(nodeID string, cpus int64) {
	p.mu.Lock()
	p.inProgress[nodeID] -= cpus
	if p.inProgress[nodeID] <= 0 {
		delete(p.inProgress, nodeID)
	}
	p.mu.Unlock()
}

// hasRoomLocked reports whether the node can host req under the over-committed capacity, given
// its committed allocation + in-flight reservations. Caller holds p.mu.
func (p *Placer) hasRoomLocked(n *domain.Node, req domain.Resources) bool {
	if n.CpuCount <= 0 {
		return false
	}
	reserved := n.CpuAllocated + p.inProgress[n.ID]
	return float64(reserved+req.CPUs) <= p.cfg.R*float64(n.CpuCount)
}

// Place picks a node for a sandbox in cell (best-of-K) and reserves its capacity. Returns
// domain.ErrNoCapacity when no ready, fresh node has room. The caller releases the reservation
// (via Release) after the create completes, whether it succeeded or failed.
func (p *Placer) Place(ctx context.Context, cellID string, req domain.Resources) (*domain.Node, error) {
	nodes, err := p.reg.ListReady(ctx, cellID)
	if err != nil {
		return nil, err
	}
	now := p.clock.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Candidates: ready + fresh + has room (in-flight reservations folded in).
	cands := make([]*domain.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Placeable(now, p.cfg.Staleness) && p.hasRoomLocked(n, req) {
			cands = append(cands, n)
		}
	}
	if len(cands) == 0 {
		return nil, domain.ErrNoCapacity
	}

	best := p.argminOfKSampleLocked(cands, req)
	if best == nil { // defensive: every candidate scored +Inf (all zero-CPU) — treat as no capacity.
		return nil, domain.ErrNoCapacity
	}
	// Reserve under the same lock so a concurrent Place sees it (no double-book).
	p.inProgress[best.ID] += req.CPUs
	return best, nil
}

// argminOfKSampleLocked samples ≤K distinct candidates uniformly at random and returns the
// lowest-scoring one. Caller holds p.mu. Mirrors placement_best_of_K.go:93-122 (K-sample) +
// :142-191 (argmin). Uses a partial Fisher-Yates shuffle to draw K distinct indices in O(K).
func (p *Placer) argminOfKSampleLocked(cands []*domain.Node, req domain.Resources) *domain.Node {
	k := p.cfg.K
	if k > len(cands) {
		k = len(cands)
	}
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	var best *domain.Node
	bestScore := math.Inf(1)
	for i := 0; i < k; i++ {
		j := i + p.rng.Intn(len(idx)-i)
		idx[i], idx[j] = idx[j], idx[i]
		n := cands[idx[i]]
		if s := p.scoreLocked(n, req); s < bestScore {
			bestScore = s
			best = n
		}
	}
	return best
}

var _ domain.Placer = (*Placer)(nil)

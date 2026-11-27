package scheduler_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/scheduler"
)

var st0 = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

// fakeRegistry returns a fixed ready-node set for a cell.
type fakeRegistry struct {
	mu    sync.Mutex
	nodes map[string][]*domain.Node // cell → nodes
}

func (r *fakeRegistry) ListReady(_ context.Context, cell string) ([]*domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Node, 0, len(r.nodes[cell]))
	for _, n := range r.nodes[cell] {
		cp := *n
		out = append(out, &cp) // copy so the placer's reservation doesn't mutate our fixtures
	}
	return out, nil
}
func (r *fakeRegistry) Heartbeat(context.Context, *domain.Node) error            { return nil }
func (r *fakeRegistry) SetState(context.Context, string, domain.NodeState) error { return nil }

func node(id string, cpuCount, cpuAllocated int64, pct float64) *domain.Node {
	return &domain.Node{ID: id, CellID: "cell-0", CpuCount: cpuCount, CpuAllocated: cpuAllocated,
		CpuPercent: pct, State: domain.NodeReady, LastHeartbeat: st0}
}

func newPlacer(nodes []*domain.Node) *scheduler.Placer {
	reg := &fakeRegistry{nodes: map[string][]*domain.Node{"cell-0": nodes}}
	return scheduler.New(reg, scheduler.DefaultConfig(), clock.Fixed{T: st0})
}

// A more-loaded node scores worse (higher).
func TestBestOfK_score_monotonic_in_load(t *testing.T) {
	p := newPlacer(nil)
	light := node("a", 8, 1, 5)
	heavy := node("b", 8, 6, 90)
	if p.ScoreNode(heavy, domain.Resources{CPUs: 1}) <= p.ScoreNode(light, domain.Resources{CPUs: 1}) {
		t.Errorf("heavier node must score worse (higher)")
	}
}

// An in-progress reservation makes a node score worse (anti-double-book).
func TestBestOfK_pending_scores_worse(t *testing.T) {
	p := newPlacer(nil)
	n := node("a", 8, 1, 10)
	base := p.ScoreNode(n, domain.Resources{CPUs: 1})
	p.Reserve("a", 4) // 4 vCPUs in flight
	if p.ScoreNode(n, domain.Resources{CPUs: 1}) <= base {
		t.Errorf("a node with in-progress reservations must score worse")
	}
}

// Power-of-K-choices: over many placements, lower-loaded nodes are chosen more often.
func TestBestOfK_power_of_k_choices(t *testing.T) {
	nodes := []*domain.Node{
		node("busy", 8, 7, 90), node("mid", 8, 4, 50),
		node("idle1", 8, 0, 2), node("idle2", 8, 0, 3), node("idle3", 8, 1, 5),
	}
	p := newPlacer(nodes)
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		n, err := p.Place(context.Background(), "cell-0", domain.Resources{CPUs: 1})
		if err != nil {
			t.Fatalf("place %d: %v", i, err)
		}
		counts[n.ID]++
		p.Release(n.ID, 1) // release so we keep probing the base distribution
	}
	idle := counts["idle1"] + counts["idle2"] + counts["idle3"]
	if idle <= counts["busy"] {
		t.Errorf("idle nodes (%d) should be chosen more than the busy node (%d): %v", idle, counts["busy"], counts)
	}
}

func TestPlace_no_ready_nodes_returns_ErrNoCapacity(t *testing.T) {
	p := newPlacer([]*domain.Node{})
	if _, err := p.Place(context.Background(), "cell-0", domain.Resources{CPUs: 1}); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("empty cell err = %v, want ErrNoCapacity", err)
	}
}

func TestPlace_full_cluster_returns_ErrNoCapacity(t *testing.T) {
	p := newPlacer([]*domain.Node{node("a", 2, 8, 100)}) // capacity R*2=8, already 8 allocated
	if _, err := p.Place(context.Background(), "cell-0", domain.Resources{CPUs: 1}); !errors.Is(err, domain.ErrNoCapacity) {
		t.Errorf("full cluster err = %v, want ErrNoCapacity", err)
	}
}

// K goroutines placing on a 1-node cluster with room for exactly 2 must not over-book.
func TestPlace_concurrent_no_double_book(t *testing.T) {
	p := newPlacer([]*domain.Node{node("a", 1, 0, 0)}) // capacity R*1=4; req=2 → room for 2
	const goroutines = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, full := 0, 0
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.Place(context.Background(), "cell-0", domain.Resources{CPUs: 2})
			mu.Lock()
			if err == nil {
				ok++
			} else if errors.Is(err, domain.ErrNoCapacity) {
				full++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if ok != 2 {
		t.Errorf("placed %d, want exactly 2 (capacity=4, req=2); double-book detected", ok)
	}
	if ok+full != goroutines {
		t.Errorf("ok(%d)+full(%d) != %d", ok, full, goroutines)
	}
}

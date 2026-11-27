package cells_test

import (
	"fmt"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/cells"
)

func mustRouter(t *testing.T, ids ...string) *cells.Router {
	t.Helper()
	r, err := cells.NewRouter(ids)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func TestCellFor_is_deterministic(t *testing.T) {
	r := mustRouter(t, "cell-a", "cell-b", "cell-c")
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("tnt-%d", i)
		first, second := r.CellFor(id), r.CellFor(id)
		if first != second {
			t.Fatalf("CellFor(%s) not deterministic", id)
		}
	}
}

func TestCellFor_balances_across_cells(t *testing.T) {
	r := mustRouter(t, "cell-a", "cell-b", "cell-c", "cell-d")
	counts := map[string]int{}
	const n = 40_000
	for i := 0; i < n; i++ {
		counts[r.CellFor(fmt.Sprintf("tnt-%d", i))]++
	}
	if len(counts) != 4 {
		t.Fatalf("used %d cells, want 4", len(counts))
	}
	// each cell should hold roughly n/4; allow ±20% skew.
	exp := n / 4
	for cell, c := range counts {
		if c < exp*8/10 || c > exp*12/10 {
			t.Errorf("cell %s has %d tenants, want ~%d (±20%%)", cell, c, exp)
		}
	}
}

// Rendezvous stability (B10): adding a cell must remap only ~1/(N+1) tenants — NOT
// reshuffle everything (which modulo hashing would).
func TestCellFor_stable_when_adding_a_cell(t *testing.T) {
	before := mustRouter(t, "cell-a", "cell-b", "cell-c")
	after := mustRouter(t, "cell-a", "cell-b", "cell-c", "cell-d")

	const n = 40_000
	moved := 0
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("tnt-%d", i)
		if before.CellFor(id) != after.CellFor(id) {
			moved++
		}
	}
	// ideal ≈ n/4 (25%) move to the new cell; assert well under a modulo-style ~75%.
	frac := float64(moved) / float64(n)
	if frac > 0.35 {
		t.Errorf("adding a cell remapped %.1f%% of tenants, want ≈25%% (rendezvous stability)", frac*100)
	}
	if frac < 0.15 {
		t.Errorf("adding a cell remapped only %.1f%%, want ≈25%% (new cell should take a share)", frac*100)
	}
}

func TestNewRouter_validates(t *testing.T) {
	if _, err := cells.NewRouter(nil); err == nil {
		t.Error("empty cell set accepted")
	}
	if _, err := cells.NewRouter([]string{"a", "a"}); err == nil {
		t.Error("duplicate cell accepted")
	}
	if _, err := cells.NewRouter([]string{"a", ""}); err == nil {
		t.Error("empty cell id accepted")
	}
}

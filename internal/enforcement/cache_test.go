package enforcement_test

import (
	"context"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/enforcement"
)

// countingReader counts inner reads.
type countingReader struct{ hits int }

func (c *countingReader) Value(context.Context, string, string, int64) (int64, error) {
	c.hits++
	return 42, nil
}

func TestCachedUsageReader_serves_from_cache_within_ttl(t *testing.T) {
	inner := &countingReader{}
	clk := time.Unix(0, 0)
	cache := enforcement.NewCachedUsageReader(inner, time.Second, func() time.Time { return clk })

	if v, _ := cache.Value(context.Background(), "t", "m", 0); v != 42 {
		t.Fatalf("first = %d, want 42", v)
	}
	// second read within TTL → served from cache, no new inner hit.
	if v, _ := cache.Value(context.Background(), "t", "m", 0); v != 42 {
		t.Fatalf("second = %d, want 42", v)
	}
	if inner.hits != 1 {
		t.Errorf("inner hits = %d, want 1 (cached within TTL)", inner.hits)
	}

	// advance past TTL → read-through again.
	clk = clk.Add(2 * time.Second)
	_, _ = cache.Value(context.Background(), "t", "m", 0)
	if inner.hits != 2 {
		t.Errorf("inner hits = %d, want 2 (TTL expired)", inner.hits)
	}
}

func TestCachedUsageReader_keys_by_tenant_metric_period(t *testing.T) {
	inner := &countingReader{}
	clk := time.Unix(0, 0)
	cache := enforcement.NewCachedUsageReader(inner, time.Hour, func() time.Time { return clk })

	_, _ = cache.Value(context.Background(), "t1", "m", 0)
	_, _ = cache.Value(context.Background(), "t2", "m", 0)  // different tenant → new hit
	_, _ = cache.Value(context.Background(), "t1", "m2", 0) // different metric → new hit
	if inner.hits != 3 {
		t.Errorf("inner hits = %d, want 3 (distinct keys)", inner.hits)
	}
}

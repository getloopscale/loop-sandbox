//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/metering"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
)

type recSink struct {
	mu   sync.Mutex
	seen map[int64]int
}

func (s *recSink) Emit(_ context.Context, batch []metering.MeterInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[int64]int{}
	}
	for _, m := range batch {
		s.seen[m.EventID]++
	}
	return nil
}

// failSink always fails — used to prove a sink failure leaves the outbox intact.
type failSink struct{}

func (failSink) Emit(context.Context, []metering.MeterInput) error { return errors.New("sink down") }

// fixedPeriod pins the billing period so the test never straddles a month boundary.
var fixedNow = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

// TestMeteringOutbox_claim_and_delete_drains_exactly_once proves M8 metering vs REAL
// PostgreSQL: each Increment atomically appends a meter_event; the claim-and-delete
// Drain removes them exactly once and a second Drain finds nothing (no re-emit, no
// loss). A sink failure leaves the rows for a later pass.
func TestMeteringOutbox_claim_and_delete_drains_exactly_once(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	store := postgres.New(pool)
	start, end := domain.CurrentPeriod(fixedNow)

	for i := 0; i < 3; i++ {
		if err := store.Increment(ctx, "tenant-a", domain.MetricSandboxCreations, 1, start, end); err != nil {
			t.Fatalf("Increment: %v", err)
		}
	}
	if v, _ := store.Value(ctx, "tenant-a", domain.MetricSandboxCreations, start); v != 3 {
		t.Fatalf("counter = %d, want 3", v)
	}

	// A sink failure must NOT remove the rows (they survive for retry).
	if _, err := metering.NewEmitter(store, failSink{}, 10).RunOnce(ctx); err == nil {
		t.Fatal("expected sink failure")
	}

	sink := &recSink{}
	n, err := metering.NewEmitter(store, sink, 2).Drain(ctx)
	if err != nil || n != 3 {
		t.Fatalf("drain = %d (err %v), want 3", n, err)
	}
	if len(sink.seen) != 3 {
		t.Errorf("distinct emitted = %d, want 3", len(sink.seen))
	}
	// Second drain: outbox emptied → nothing (no re-emit).
	n2, err := metering.NewEmitter(store, sink, 10).Drain(ctx)
	if err != nil || n2 != 0 {
		t.Errorf("second drain = %d (err %v), want 0", n2, err)
	}
	for id, c := range sink.seen {
		if c != 1 {
			t.Errorf("event %d delivered %d times, want 1", id, c)
		}
	}
}

// TestIncrement_outbox_is_atomic_with_counter proves the tx atomicity: a failing
// meter_events insert (forced by dropping the table) must roll back the counter bump
// too — never a counted-but-unbilled unit.
func TestIncrement_outbox_is_atomic_with_counter(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	store := postgres.New(pool)
	start, end := domain.CurrentPeriod(fixedNow)

	// Drop the outbox table so the second statement of the tx fails.
	if _, err := pool.Exec(ctx, `DROP TABLE meter_events`); err != nil {
		t.Fatalf("drop meter_events: %v", err)
	}
	if err := store.Increment(ctx, "tenant-a", domain.MetricSandboxCreations, 1, start, end); err == nil {
		t.Fatal("Increment succeeded despite missing outbox table, want error")
	}
	// The counter bump must have rolled back with the failed outbox append.
	if v, _ := store.Value(ctx, "tenant-a", domain.MetricSandboxCreations, start); v != 0 {
		t.Errorf("counter = %d after rolled-back Increment, want 0 (atomic)", v)
	}
}

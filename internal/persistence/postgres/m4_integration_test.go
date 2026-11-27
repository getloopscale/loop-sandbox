//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
	"github.com/getloopscale/loop-sandbox/internal/queue"
	"github.com/getloopscale/loop-sandbox/internal/reconcile"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

type recAudit struct {
	mu sync.Mutex
	n  int
}

func (a *recAudit) Record(_, _, _, _, _, _ string, _ time.Time) { a.mu.Lock(); a.n++; a.mu.Unlock() }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestReconciler_is_restart_resistant proves TTL is enforced even after a control-
// plane restart: the expired sandbox is written by one store, then a FRESH
// reconciler (new store on the same DB — the "restarted" process) destroys it,
// because the work list lives in PostgreSQL, not memory.
func TestReconciler_is_restart_resistant(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Process 1: persist a running sandbox that is already expired.
	store1 := postgres.New(pool)
	sb, _ := domain.NewSandbox("sbx_exp", "tenant-a", "busybox", time.Minute, now.Add(-2*time.Minute))
	_ = sb.TransitionTo(domain.StateRunning, now.Add(-2*time.Minute))
	if err := store1.SaveTenant(ctx, mustTenant(now)); err != nil {
		t.Fatalf("SaveTenant: %v", err)
	}
	if err := store1.Save(ctx, sb); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Process 2 ("restart"): a brand-new reconciler + queue on the same DB drive the
	// full producer→consumer path (enqueue destroy → dequeue → destroy).
	store2 := postgres.New(pool)
	aud := &recAudit{}
	rec := reconcile.New(store2, fake.New(), aud, clock.System{}, quietLog())
	var seq int
	q := queue.NewPG(pool, func() string { seq++; return "j" + string(rune('a'+seq)) })

	if n, err := rec.EnqueueExpired(ctx, q); err != nil || n != 1 {
		t.Fatalf("EnqueueExpired = %d (err %v), want 1", n, err)
	}
	job, ack, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := rec.HandleJob(ctx, job); err != nil {
		t.Fatalf("HandleJob: %v", err)
	}
	_ = ack(ctx)

	if _, err := store2.Get(ctx, "tenant-a", "sbx_exp"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expired sandbox survived restart+reconcile: %v", err)
	}
	if aud.n != 1 {
		t.Errorf("audit events = %d, want 1 (sandbox.expired)", aud.n)
	}
}

// TestJobQueue_blocks_then_wakes_on_enqueue proves the PUSH semantics (not polling):
// Dequeue blocks on an empty queue and returns quickly after an Enqueue NOTIFY,
// well under the safety-poll interval.
func TestJobQueue_blocks_then_wakes_on_enqueue(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	var seq int
	q := queue.NewPG(pool, func() string { seq++; return "w" + string(rune('a'+seq)) })

	got := make(chan time.Duration, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		start := time.Now()
		j, ack, err := q.Dequeue(cctx)
		if err == nil {
			_ = ack(ctx)
			_ = j
		}
		got <- time.Since(start)
	}()

	// Ensure the consumer is blocked (nothing to claim yet).
	time.Sleep(300 * time.Millisecond)
	enqAt := time.Now()
	if err := q.Enqueue(ctx, queue.Job{Kind: "x"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case elapsed := <-got:
		if elapsed < 250*time.Millisecond {
			t.Errorf("Dequeue returned in %v — did not actually block on the empty queue", elapsed)
		}
		if woke := time.Since(enqAt); woke > 900*time.Millisecond {
			t.Errorf("woke %v after enqueue — NOTIFY push not working (fell back to safety poll)", woke)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Dequeue did not wake after enqueue")
	}
}

func mustTenant(now time.Time) *domain.Tenant {
	tn, _ := domain.NewTenant("tenant-a", "acme", domain.PlanPro, now)
	return tn
}

// TestJobQueue_push_and_skiplocked proves the queue is push (Dequeue returns after
// an Enqueue NOTIFY) and that two concurrent consumers never get the same job
// (FOR UPDATE SKIP LOCKED).
func TestJobQueue_push_and_skiplocked(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	var seq int
	q := queue.NewPG(pool, func() string { seq++; return "job-" + string(rune('a'+seq)) })

	if err := q.Enqueue(ctx, queue.Job{Kind: "destroy", Payload: []byte(`{"id":"1"}`)}); err != nil {
		t.Fatalf("Enqueue 1: %v", err)
	}
	if err := q.Enqueue(ctx, queue.Job{Kind: "destroy", Payload: []byte(`{"id":"2"}`)}); err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}

	// Two concurrent consumers must get two DISTINCT jobs (no double delivery).
	results := make(chan dequeued, 2)
	for i := 0; i < 2; i++ {
		go func() {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			j, ack, err := q.Dequeue(cctx)
			if err == nil {
				_ = ack(ctx)
			}
			results <- dequeued{j, err}
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("Dequeue: %v", r.err)
		}
		if seen[r.job.ID] {
			t.Fatalf("same job delivered twice: %s (SKIP LOCKED broken)", r.job.ID)
		}
		seen[r.job.ID] = true
	}
	if len(seen) != 2 {
		t.Errorf("distinct jobs = %d, want 2", len(seen))
	}
}

type dequeued struct {
	job queue.Job
	err error
}

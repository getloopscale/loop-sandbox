package reconcile_test

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
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/queue"
	"github.com/getloopscale/loop-sandbox/internal/reconcile"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

type fakeAudit struct {
	mu      sync.Mutex
	actions []string
}

func (f *fakeAudit) Record(_, _, action, _, _, _ string, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action)
}

// fakeQueue is a minimal in-memory JobQueue for unit tests (dedup on job id).
type fakeQueue struct {
	mu   sync.Mutex
	jobs []queue.Job
	seen map[string]bool
}

func newFakeQueue() *fakeQueue { return &fakeQueue{seen: map[string]bool{}} }

func (q *fakeQueue) Enqueue(_ context.Context, j queue.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.seen[j.ID] {
		return nil // idempotent dedup
	}
	q.seen[j.ID] = true
	q.jobs = append(q.jobs, j)
	return nil
}
func (q *fakeQueue) Dequeue(context.Context) (queue.Job, queue.AckFunc, error) { panic("unused") }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func makeExpired(repo *memory.Store, id, tenant string, t0 time.Time) {
	sb, _ := domain.NewSandbox(id, tenant, "", time.Minute, t0)
	_ = sb.TransitionTo(domain.StateRunning, t0)
	_ = repo.Save(context.Background(), sb)
}

func TestEnqueueExpired_enqueues_only_expired_and_dedups(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0).UTC()
	repo := memory.New()
	makeExpired(repo, "sb-exp", "tenant-a", t0)
	live, _ := domain.NewSandbox("sb-live", "tenant-a", "", time.Hour, t0)
	_ = live.TransitionTo(domain.StateRunning, t0)
	_ = repo.Save(ctx, live)

	q := newFakeQueue()
	rec := reconcile.New(repo, fake.New(), &fakeAudit{}, clock.Fixed{T: t0.Add(2 * time.Minute)}, quiet())
	n, err := rec.EnqueueExpired(ctx, q)
	if err != nil || n != 1 {
		t.Fatalf("EnqueueExpired = %d (err %v), want 1", n, err)
	}
	// Second pass dedups (same job id) → 0 new visible jobs.
	_, _ = rec.EnqueueExpired(ctx, q)
	if len(q.jobs) != 1 {
		t.Errorf("queue has %d jobs, want 1 (dedup)", len(q.jobs))
	}
}

func TestHandleJob_destroys_expired_and_audits(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0).UTC()
	repo := memory.New()
	makeExpired(repo, "sb-exp", "tenant-a", t0)
	aud := &fakeAudit{}
	rec := reconcile.New(repo, fake.New(), aud, clock.Fixed{T: t0.Add(2 * time.Minute)}, quiet())

	job := queue.Job{ID: "destroy-sb-exp", Kind: "sandbox.destroy", Payload: []byte(`{"tenant_id":"tenant-a","sandbox_id":"sb-exp"}`)}
	if err := rec.HandleJob(ctx, job); err != nil {
		t.Fatalf("HandleJob: %v", err)
	}
	if _, err := repo.Get(ctx, "tenant-a", "sb-exp"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("sandbox not destroyed: %v", err)
	}
	if len(aud.actions) != 1 || aud.actions[0] != "sandbox.expired" {
		t.Errorf("audit = %v, want [sandbox.expired]", aud.actions)
	}
	// Idempotent: handling again is a no-op (already gone).
	if err := rec.HandleJob(ctx, job); err != nil {
		t.Errorf("second HandleJob: %v", err)
	}
}

// TestHandleJob_spares_extended_sandbox proves the extend-race fix: if the sandbox
// was extended after the job was enqueued, the consumer does NOT destroy it.
func TestHandleJob_spares_extended_sandbox(t *testing.T) {
	ctx := context.Background()
	t0 := time.Unix(1000, 0).UTC()
	repo := memory.New()
	// Sandbox now expires far in the future (extended).
	sb, _ := domain.NewSandbox("sb", "t", "", time.Hour, t0)
	_ = sb.TransitionTo(domain.StateRunning, t0)
	_ = repo.Save(ctx, sb)

	rec := reconcile.New(repo, fake.New(), &fakeAudit{}, clock.Fixed{T: t0.Add(time.Minute)}, quiet())
	job := queue.Job{ID: "destroy-sb", Kind: "sandbox.destroy", Payload: []byte(`{"tenant_id":"t","sandbox_id":"sb"}`)}
	if err := rec.HandleJob(ctx, job); err != nil {
		t.Fatalf("HandleJob: %v", err)
	}
	if _, err := repo.Get(ctx, "t", "sb"); err != nil {
		t.Errorf("extended sandbox was wrongly destroyed: %v", err)
	}
}

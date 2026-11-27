package worker_test

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
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
	"github.com/getloopscale/loop-sandbox/internal/worker"
)

type noAudit struct{}

func (noAudit) Record(_, _, _, _, _, _ string, _ time.Time) {}

// memQueue is an in-memory JobQueue: Enqueue dedups on the job id, Dequeue blocks
// until a job arrives or the context ends.
type memQueue struct {
	mu   sync.Mutex
	seen map[string]bool
	jobs chan queue.Job
}

func newMemQueue() *memQueue {
	return &memQueue{seen: map[string]bool{}, jobs: make(chan queue.Job, 16)}
}

func (q *memQueue) Enqueue(_ context.Context, j queue.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.seen[j.ID] {
		return nil
	}
	q.seen[j.ID] = true
	q.jobs <- j
	return nil
}

func (q *memQueue) Dequeue(ctx context.Context) (queue.Job, queue.AckFunc, error) {
	select {
	case j := <-q.jobs:
		return j, func(context.Context) error { return nil }, nil
	case <-ctx.Done():
		return queue.Job{}, nil, ctx.Err()
	}
}

type countingReaper struct {
	mu    sync.Mutex
	calls int
}

func (r *countingReaper) ReclaimStuck(context.Context, time.Duration) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return 0, nil
}

func (r *countingReaper) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runningSandbox stores a running sandbox with the given TTL, created at t0, in
// both the repository and the driver.
func runningSandbox(t *testing.T, repo *memory.Store, driver *fake.Driver, id string, ttl time.Duration, t0 time.Time) {
	t.Helper()
	ctx := context.Background()
	sb, err := domain.NewSandbox(id, "tenant-a", "", ttl, t0)
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}
	if err := sb.TransitionTo(domain.StateRunning, t0); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := repo.Save(ctx, sb); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := driver.Create(ctx, runtime.CreateRequest{SandboxID: runtime.SandboxID(id)}); err != nil {
		t.Fatalf("driver create: %v", err)
	}
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func startWorker(t *testing.T, deps worker.Deps) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx, deps) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker.Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker.Run did not return after its context was cancelled")
		}
	}
}

func TestRunDestroysASandboxWhoseTTLExpired(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	repo, driver := memory.New(), fake.New()
	runningSandbox(t, repo, driver, "sb-expired", time.Minute, t0)

	stop := startWorker(t, worker.Deps{
		Repo: repo, Driver: driver, Audit: noAudit{}, Clock: clock.Fixed{T: t0.Add(2 * time.Minute)},
		Queue: newMemQueue(), Logger: quiet(), Interval: 10 * time.Millisecond,
	})
	defer stop()

	eventually(t, "the expired sandbox to be destroyed at the runtime", func() bool {
		sb, err := driver.Inspect(context.Background(), "sb-expired")
		return err == nil && sb.State == runtime.StateDestroyed
	})
}

func TestRunKeepsASandboxWhoseTTLHasNotExpired(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	repo, driver := memory.New(), fake.New()
	runningSandbox(t, repo, driver, "sb-expired", time.Minute, t0)
	runningSandbox(t, repo, driver, "sb-live", time.Hour, t0)

	stop := startWorker(t, worker.Deps{
		Repo: repo, Driver: driver, Audit: noAudit{}, Clock: clock.Fixed{T: t0.Add(2 * time.Minute)},
		Queue: newMemQueue(), Logger: quiet(), Interval: 10 * time.Millisecond,
	})
	defer stop()

	// The expired sandbox going away proves a full reconcile pass ran over both.
	eventually(t, "the expired sandbox to be destroyed", func() bool {
		sb, err := driver.Inspect(context.Background(), "sb-expired")
		return err == nil && sb.State == runtime.StateDestroyed
	})
	live, err := driver.Inspect(context.Background(), "sb-live")
	if err != nil || live.State != runtime.StateRunning {
		t.Errorf("live sandbox: state=%v err=%v, want running", live, err)
	}
}

func TestRunCallsTheStuckJobReaperPeriodically(t *testing.T) {
	reaper := &countingReaper{}

	stop := startWorker(t, worker.Deps{
		Repo: memory.New(), Driver: fake.New(), Audit: noAudit{}, Clock: clock.System{},
		Queue: newMemQueue(), Reaper: reaper, Logger: quiet(), Interval: 10 * time.Millisecond,
	})
	defer stop()

	eventually(t, "the reaper to be called twice", func() bool { return reaper.count() >= 2 })
}

func TestRunRejectsMissingDependencies(t *testing.T) {
	err := worker.Run(context.Background(), worker.Deps{Logger: quiet()})

	if !errors.Is(err, worker.ErrMissingDependency) {
		t.Errorf("Run = %v, want ErrMissingDependency", err)
	}
}

// Package reconcile enforces sandbox TTL and reaps orphans. It is a system actor
// (no tenant context) and idempotent: destroying an already-gone sandbox is a
// no-op, so it is safe to run concurrently and safe after a control-plane restart
// (the work list lives in PostgreSQL, not in memory).
//
// TTL enforcement is push-driven: the producer finds expired sandboxes and
// enqueues idempotent destroy jobs; the JobQueue's SKIP LOCKED claim guarantees a
// single consumer per sandbox (no duplicate destroy/audit), and the consumer
// re-checks expiry before destroying (so a just-extended sandbox is spared).
package reconcile

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/queue"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// batchSize bounds the work per producer pass.
const batchSize = 100

// jobKindDestroy is the queue job kind for a TTL-expired destroy.
const jobKindDestroy = "sandbox.destroy"

// AuditRecorder records the expiry (US-04). Same shape as app.AuditRecorder.
type AuditRecorder interface {
	Record(tenantID, actorID, action, resourceType, resourceID, requestID string, now time.Time)
}

// destroyPayload is the queue job body for a destroy.
type destroyPayload struct {
	TenantID  string `json:"tenant_id"`
	SandboxID string `json:"sandbox_id"`
}

// Reconciler destroys expired sandboxes and reaps orphaned runtime resources.
type Reconciler struct {
	repo   domain.Repository
	driver runtime.Driver
	audit  AuditRecorder
	clock  clock.Clock
	log    *slog.Logger
}

// New wires the reconciler.
func New(repo domain.Repository, driver runtime.Driver, audit AuditRecorder, clk clock.Clock, log *slog.Logger) *Reconciler {
	return &Reconciler{repo: repo, driver: driver, audit: audit, clock: clk, log: log}
}

// EnqueueExpired finds expired sandboxes and enqueues one idempotent destroy job
// each (job id keyed by sandbox, so re-enqueuing across ticks is a no-op). Returns
// the number enqueued.
func (r *Reconciler) EnqueueExpired(ctx context.Context, q queue.JobQueue) (int, error) {
	now := r.clock.Now()
	expired, err := r.repo.ListExpired(ctx, now, batchSize)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sb := range expired {
		payload, _ := json.Marshal(destroyPayload{TenantID: sb.TenantID, SandboxID: sb.ID})
		job := queue.Job{ID: "destroy-" + sb.ID, Kind: jobKindDestroy, Payload: payload}
		if err := q.Enqueue(ctx, job); err != nil {
			r.log.Error("reconcile: enqueue destroy failed", "sandbox", sb.ID, "error", err)
			continue
		}
		n++
	}
	return n, nil
}

// HandleJob processes one dequeued job. For a destroy it re-checks the sandbox is
// still present AND still expired (so a concurrently-extended sandbox is spared),
// then destroys idempotently and audits. Unknown kinds are ignored (acked).
func (r *Reconciler) HandleJob(ctx context.Context, job queue.Job) error {
	if job.Kind != jobKindDestroy {
		return nil
	}
	var p destroyPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil // malformed job — ack and drop rather than loop forever
	}
	sb, err := r.repo.Get(ctx, p.TenantID, p.SandboxID)
	if err != nil {
		return nil // already gone — nothing to do
	}
	if !sb.Expired(r.clock.Now()) {
		return nil // extended after enqueue — spare it (extend-race)
	}
	if err := r.driver.Destroy(ctx, runtime.SandboxID(sb.ID)); err != nil {
		return err // leave the job reclaimable; consumer retries
	}
	if err := r.repo.Delete(ctx, sb.TenantID, sb.ID); err != nil {
		return err
	}
	r.audit.Record(sb.TenantID, "system:reconciler", "sandbox.expired", "sandbox", sb.ID, "", r.clock.Now())
	return nil
}

// ReconcileOrphans reaps managed runtime resources with no live DB record. It is
// FAIL-SAFE: a runtime sandbox with no tenant label is skipped (never destroyed),
// because destroying on incomplete information could kill a live, just-created
// sandbox. Only runs when the driver supports orphan listing.
func (r *Reconciler) ReconcileOrphans(ctx context.Context) (int, error) {
	lister, ok := r.driver.(runtime.OrphanLister)
	if !ok {
		return 0, nil
	}
	orphans, err := lister.ListOrphans(ctx)
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, o := range orphans {
		if o.TenantID == "" {
			r.log.Warn("reconcile: runtime sandbox without tenant label — skipping (fail-safe)", "sandbox", o.ID)
			continue
		}
		if _, err := r.repo.Get(ctx, o.TenantID, string(o.ID)); err == nil {
			continue // has a live record — not an orphan
		}
		if err := r.driver.Destroy(ctx, o.ID); err != nil {
			r.log.Error("reconcile: orphan destroy failed", "sandbox", o.ID, "error", err)
			continue
		}
		reaped++
	}
	return reaped, nil
}

// RunProducer enqueues expired-destroy jobs on interval until ctx is cancelled.
func (r *Reconciler) RunProducer(ctx context.Context, q queue.JobQueue, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := r.EnqueueExpired(ctx, q); err != nil {
				r.log.Error("reconcile: enqueue expired", "error", err)
			} else if n > 0 {
				r.log.Info("reconcile: enqueued destroy jobs", "count", n)
			}
			if n, err := r.ReconcileOrphans(ctx); err != nil {
				r.log.Error("reconcile: orphans", "error", err)
			} else if n > 0 {
				r.log.Info("reconcile: reaped orphans", "count", n)
			}
		}
	}
}

// RunConsumer dequeues and processes jobs until ctx is cancelled.
func (r *Reconciler) RunConsumer(ctx context.Context, q queue.JobQueue) {
	for {
		job, ack, err := q.Dequeue(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.log.Error("reconcile: dequeue", "error", err)
			time.Sleep(time.Second)
			continue
		}
		if err := r.HandleJob(ctx, job); err != nil {
			r.log.Error("reconcile: handle job", "job", job.ID, "error", err)
			continue // do NOT ack — leave reclaimable (at-least-once)
		}
		if err := ack(ctx); err != nil {
			r.log.Error("reconcile: ack", "job", job.ID, "error", err)
		}
	}
}

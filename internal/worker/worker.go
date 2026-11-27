// Package worker runs the background loops of the control plane: it destroys
// sandboxes whose TTL expired, reaps runtime resources that no record points to,
// recovers jobs a crashed consumer abandoned, and drains usage events.
//
// The loops run in the standalone worker binary or inside the API process. They
// must run in the process that owns the runtime driver when the driver keeps its
// sandboxes in process memory (see runtime.ProcessLocal): another process could
// not destroy them.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/queue"
	"github.com/getloopscale/loop-sandbox/internal/reconcile"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// StuckReaper recovers jobs claimed by a consumer that died before acking. Only a
// queue without its own redelivery needs one.
type StuckReaper interface {
	ReclaimStuck(ctx context.Context, olderThan time.Duration) (int64, error)
}

// MeterDrainer ships buffered usage events to the billing sink.
type MeterDrainer interface {
	Drain(ctx context.Context) (int, error)
}

// IdlePauser pauses the sandboxes that went past their idle limit. Only the
// process that serves the requests knows which sandboxes are in use, so the
// dependency is set only there.
type IdlePauser interface {
	PauseIdleSandboxes(ctx context.Context) (int, error)
}

// Deps are the collaborators of the worker loops. Reaper, Meter and Idle are optional.
type Deps struct {
	Repo     domain.Repository
	Driver   runtime.Driver
	Audit    reconcile.AuditRecorder
	Clock    clock.Clock
	Queue    queue.JobQueue
	Reaper   StuckReaper
	Meter    MeterDrainer
	Idle     IdlePauser
	Logger   *slog.Logger
	Interval time.Duration
}

// DefaultInterval is how often the periodic loops run when Deps.Interval is unset.
const DefaultInterval = 10 * time.Second

// ErrMissingDependency is returned by Run when a required collaborator is nil.
var ErrMissingDependency = errors.New("worker: missing dependency")

// Run starts the loops and blocks until ctx is cancelled and all of them returned.
func Run(ctx context.Context, d Deps) error {
	if d.Repo == nil || d.Driver == nil || d.Audit == nil || d.Clock == nil || d.Queue == nil || d.Logger == nil {
		return ErrMissingDependency
	}
	interval := d.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	rec := reconcile.New(d.Repo, d.Driver, d.Audit, d.Clock, d.Logger)
	d.Logger.Info("worker loops started", "driver", d.Driver.Name(), "interval", interval.String())

	var wg sync.WaitGroup
	start := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	// Producer: find expired sandboxes, enqueue idempotent destroy jobs, reap orphans.
	start(func() { rec.RunProducer(ctx, d.Queue, interval) })
	// Consumer: dequeue, destroy (re-checking the expiry), ack.
	start(func() { rec.RunConsumer(ctx, d.Queue) })
	if d.Idle != nil {
		start(func() {
			every(ctx, interval, func() {
				n, err := d.Idle.PauseIdleSandboxes(ctx)
				if err != nil {
					d.Logger.Error("pause idle sandboxes", "error", err, "paused", n)
				} else if n > 0 {
					d.Logger.Info("paused idle sandboxes", "count", n)
				}
			})
		})
	}
	if d.Reaper != nil {
		start(func() {
			every(ctx, interval, func() {
				n, err := d.Reaper.ReclaimStuck(ctx, 0)
				switch {
				case err != nil:
					d.Logger.Error("reclaim stuck jobs", "error", err)
				case n > 0:
					d.Logger.Warn("reclaimed stuck jobs", "count", n)
				}
			})
		})
	}
	if d.Meter != nil {
		start(func() {
			every(ctx, interval, func() {
				n, err := d.Meter.Drain(ctx)
				switch {
				case err != nil:
					d.Logger.Error("meter drain", "error", err)
				case n > 0:
					d.Logger.Info("metered usage events", "count", n)
				}
			})
		})
	}
	wg.Wait()
	d.Logger.Info("worker loops stopped")
	return nil
}

func every(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

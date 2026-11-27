// Command worker runs the LoopSandbox background loops as their own process: TTL
// enforcement, orphan reaping, stuck-job recovery and usage metering. The work list
// lives in PostgreSQL, so the loops keep enforcing TTLs across a restart.
//
// It is for runtime drivers whose sandboxes another process can reach (container).
// A driver that keeps sandboxes in process memory (firecracker) runs these loops
// inside the API process instead; see internal/worker.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getloopscale/loop-sandbox/internal/audit"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/metering"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/container"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
	"github.com/getloopscale/loop-sandbox/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	interval, err := intervalFromEnv()
	if err != nil {
		return err
	}
	driver, err := selectDriver(envOr("LOOPSANDBOX_DRIVER", "fake"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tracesOn, shutdownTraces, err := tracing.Setup(ctx, "loopsandbox-worker")
	if err != nil {
		return fmt.Errorf("set up tracing: %w", err)
	}
	// Flush buffered spans on the way out; ctx is already cancelled by then.
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTraces(flushCtx); err != nil {
			logger.Error("flush traces", "error", err)
		}
	}()
	logger.Info("tracing", "otlp_export", tracesOn)

	dsn := envOr("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/loopsandbox?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	store := postgres.New(pool)
	auditSink := audit.NewSink(store, 256, logger)
	defer auditSink.Close()
	// Runtime resources carry the id of this deployment, so the orphan reaper
	// leaves alone those of another deployment on the same host.
	if scoped, ok := driver.(runtime.InstanceScoped); ok {
		id, err := store.DeploymentID(ctx)
		if err != nil {
			return fmt.Errorf("%w (the API applies the schema migrations; start it first)", err)
		}
		scoped.SetInstance(id)
	}

	usageSink, err := metering.SinkFromEnv(os.Getenv, logger)
	if err != nil {
		return err
	}
	q, reaper, closeQueue, err := worker.QueueFromEnv(ctx, pool, logger)
	if err != nil {
		return fmt.Errorf("build job queue: %w", err)
	}
	defer closeQueue()

	return worker.Run(ctx, worker.Deps{
		Repo: store, Driver: driver, Audit: auditSink, Clock: clock.System{},
		Queue: q, Reaper: reaper,
		// Drain the meter_events outbox to the billing sink off the request path
		// (claim-and-delete, at-least-once).
		Meter:    metering.NewEmitter(store, usageSink, 500),
		Logger:   logger,
		Interval: interval,
	})
}

func intervalFromEnv() (time.Duration, error) {
	v := os.Getenv("LOOPSANDBOX_RECONCILE_INTERVAL")
	if v == "" {
		return worker.DefaultInterval, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("LOOPSANDBOX_RECONCILE_INTERVAL=%q: want a positive duration such as 10s", v)
	}
	return d, nil
}

// selectDriver picks the runtime driver. An unknown name is an error: falling back
// to the fake driver would make the worker report sandboxes destroyed while they
// keep running.
func selectDriver(name string) (runtime.Driver, error) {
	switch name {
	case "container":
		return container.NewFromEnv(envOr("LOOPSANDBOX_SANDBOX_IMAGE", "busybox:latest"))
	case "fake":
		return fake.New(), nil
	case "firecracker":
		return nil, fmt.Errorf("the firecracker driver keeps its microVMs in the API process; " +
			"run the API with LOOPSANDBOX_DRIVER=firecracker and no separate worker, it runs these loops itself")
	default:
		return nil, fmt.Errorf("unknown driver %q (want fake|container)", name)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

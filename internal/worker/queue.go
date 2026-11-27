package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/getloopscale/loop-sandbox/internal/queue"
)

// QueueFromEnv builds the job queue selected by LOOPSANDBOX_QUEUE: "pg" (the
// default; PostgreSQL with SKIP LOCKED and LISTEN/NOTIFY) or "nats" (JetStream, one
// stream per cell). It returns the queue, the reaper the PostgreSQL queue needs
// (nil for NATS, which redelivers unacked jobs itself) and a function that releases
// the queue's connection.
func QueueFromEnv(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (queue.JobQueue, StuckReaper, func(), error) {
	switch backend := envOr("LOOPSANDBOX_QUEUE", "pg"); backend {
	case "pg":
		pg := queue.NewPG(pool, func() string { return "job_" + uuid.NewString() })
		return pg, pg, func() {}, nil
	case "nats":
		cell := envOr("LOOPSANDBOX_CELL", "cell-0")
		replicas, err := strconv.Atoi(envOr("LOOPSANDBOX_NATS_REPLICAS", "1"))
		if err != nil || replicas < 1 {
			return nil, nil, nil, fmt.Errorf("LOOPSANDBOX_NATS_REPLICAS=%q: want a positive integer", os.Getenv("LOOPSANDBOX_NATS_REPLICAS"))
		}
		nc, err := nats.Connect(envOr("LOOPSANDBOX_NATS_URL", nats.DefaultURL))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("connect nats: %w", err)
		}
		js, err := jetstream.New(nc)
		if err != nil {
			nc.Close()
			return nil, nil, nil, fmt.Errorf("open jetstream: %w", err)
		}
		// One replica on a single node; 3 or 5 on a cluster, so losing a node does
		// not lose the queue.
		q, err := queue.NewNATSQueue(ctx, js, cell, queue.WithReplicas(replicas))
		if err != nil {
			nc.Close()
			return nil, nil, nil, fmt.Errorf("create nats queue for cell %s: %w", cell, err)
		}
		logger.Info("job queue: NATS JetStream per cell", "cell", cell, "replicas", replicas)
		return q, nil, nc.Close, nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown queue backend %q (want pg|nats)", backend)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

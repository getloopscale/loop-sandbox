//go:build integration

package queue_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/getloopscale/loop-sandbox/internal/queue"
	"github.com/getloopscale/loop-sandbox/migrations"
)

func setupPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("loopsandbox"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, stmt := range migrations.Ordered() {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return pool
}

// TestPGQueue_partitions_jobs_by_cell proves migration 0006: after attaching a per-cell
// partition, a job enqueued for that cell physically lands in ITS partition (not the
// default) — write distribution by cell (B2). Enqueue/dequeue/dedup all still work
// against the partitioned table.
func TestPGQueue_partitions_jobs_by_cell(t *testing.T) {
	pool := setupPG(t)
	ctx := context.Background()

	// Attach an explicit partition for cell-a (cells are provisioned deliberately).
	if _, err := pool.Exec(ctx, `CREATE TABLE jobs_cell_a PARTITION OF jobs FOR VALUES IN ('cell-a')`); err != nil {
		t.Fatalf("attach partition: %v", err)
	}

	q := queue.NewPG(pool, func() string { return "generated" })
	if err := q.Enqueue(ctx, queue.Job{ID: "j-a", CellID: "cell-a", Kind: "destroy"}); err != nil {
		t.Fatalf("enqueue cell-a: %v", err)
	}
	if err := q.Enqueue(ctx, queue.Job{ID: "j-0", CellID: "cell-0", Kind: "destroy"}); err != nil {
		t.Fatalf("enqueue cell-0: %v", err)
	}

	// cell-a's job physically lives in jobs_cell_a; cell-0's in the default partition.
	if n := countIn(t, pool, "jobs_cell_a"); n != 1 {
		t.Errorf("jobs_cell_a has %d rows, want 1 (cell-a not routed to its partition)", n)
	}
	if n := countIn(t, pool, "jobs_default"); n != 1 {
		t.Errorf("jobs_default has %d rows, want 1 (cell-0 job)", n)
	}
	// And no cell-a row leaked into the default partition.
	var leaked int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs_default WHERE cell_id='cell-a'`).Scan(&leaked)
	if leaked != 0 {
		t.Errorf("cell-a leaked into default partition (%d rows)", leaked)
	}

	// Dedup still works on the partitioned PK (cell_id, id).
	if err := q.Enqueue(ctx, queue.Job{ID: "j-a", CellID: "cell-a", Kind: "destroy"}); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if n := countIn(t, pool, "jobs_cell_a"); n != 1 {
		t.Errorf("after re-enqueue jobs_cell_a has %d rows, want 1 (dedup broken)", n)
	}

	// Dequeue drains across partitions and ack deletes.
	got, ack, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if got.ID != "j-a" && got.ID != "j-0" {
		t.Errorf("dequeued unexpected job %q", got.ID)
	}
	if err := ack(ctx); err != nil {
		t.Errorf("ack: %v", err)
	}
}

// TestMigrations_are_idempotent is the regression test for the crash-loop BLOCKER: the
// runner re-applies every file on each boot, so applying the whole set TWICE must not
// error (0006 must no-op once jobs is already partitioned).
func TestMigrations_are_idempotent(t *testing.T) {
	pool := setupPG(t) // first apply happens in setup
	ctx := context.Background()

	// Second apply — must not error (previously failed at CREATE TABLE jobs_default).
	for _, stmt := range migrations.Ordered() {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("re-applying migrations failed (not idempotent): %v", err)
		}
	}
	// jobs is still a partitioned table after the re-run.
	var relkind string
	if err := pool.QueryRow(ctx, `SELECT relkind FROM pg_class WHERE relname='jobs'`).Scan(&relkind); err != nil {
		t.Fatalf("inspect jobs: %v", err)
	}
	if relkind != "p" {
		t.Errorf("jobs relkind = %q after re-migrate, want 'p' (partitioned)", relkind)
	}
}

func countIn(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

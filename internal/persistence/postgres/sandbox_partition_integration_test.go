//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
	"github.com/getloopscale/loop-sandbox/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSandboxes_partition_by_cell proves migration 0007: a sandbox saved with a routed
// cell_id physically lands in that cell's partition, round-trips its cell_id on read,
// and the tenant-scoped Get/Delete still work across partitions.
func TestSandboxes_partition_by_cell(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	store := postgres.New(pool)

	// Attach an explicit partition for cell-a.
	if _, err := pool.Exec(ctx, `CREATE TABLE sandboxes_cell_a PARTITION OF sandboxes FOR VALUES IN ('cell-a')`); err != nil {
		t.Fatalf("attach partition: %v", err)
	}

	now := time.Now().UTC()
	sbA, _ := domain.NewSandbox("sb-a", "tenant-a", "img", time.Minute, now)
	sbA.CellID = "cell-a"
	sb0, _ := domain.NewSandbox("sb-0", "tenant-0", "img", time.Minute, now) // CellID defaults to cell-0
	if err := store.Save(ctx, sbA); err != nil {
		t.Fatalf("save sb-a: %v", err)
	}
	if err := store.Save(ctx, sb0); err != nil {
		t.Fatalf("save sb-0: %v", err)
	}

	// Physical routing: sb-a in its partition, sb-0 in the default.
	if n := count(t, pool, "sandboxes_cell_a"); n != 1 {
		t.Errorf("sandboxes_cell_a has %d rows, want 1 (cell-a not routed)", n)
	}
	if n := count(t, pool, "sandboxes_default"); n != 1 {
		t.Errorf("sandboxes_default has %d rows, want 1 (cell-0)", n)
	}

	// Round-trip: Get returns the persisted cell_id.
	got, err := store.Get(ctx, "tenant-a", "sb-a")
	if err != nil {
		t.Fatalf("get sb-a: %v", err)
	}
	if got.CellID != "cell-a" {
		t.Errorf("Get cell_id = %q, want cell-a", got.CellID)
	}

	// Upsert (state transition) keeps the same partition key.
	sbA.State = domain.StateRunning
	if err := store.Save(ctx, sbA); err != nil {
		t.Fatalf("re-save sb-a: %v", err)
	}
	if n := count(t, pool, "sandboxes_cell_a"); n != 1 {
		t.Errorf("after upsert sandboxes_cell_a has %d rows, want 1 (upsert made a dup)", n)
	}

	// Tenant-scoped Delete works across partitions.
	if err := store.Delete(ctx, "tenant-a", "sb-a"); err != nil {
		t.Fatalf("delete sb-a: %v", err)
	}
	if _, err := store.Get(ctx, "tenant-a", "sb-a"); err != domain.ErrNotFound {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
}

// TestSandboxMigrations_idempotent guards the crash-loop class (0007 must no-op on a
// second full apply once sandboxes is partitioned).
func TestSandboxMigrations_idempotent(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	for _, stmt := range migrations.Ordered() {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("re-applying migrations failed (not idempotent): %v", err)
		}
	}
	var relkind string
	if err := pool.QueryRow(ctx, `SELECT relkind FROM pg_class WHERE relname='sandboxes'`).Scan(&relkind); err != nil {
		t.Fatalf("inspect sandboxes: %v", err)
	}
	if relkind != "p" {
		t.Errorf("sandboxes relkind = %q after re-migrate, want 'p' (partitioned)", relkind)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

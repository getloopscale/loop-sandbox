//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
	"github.com/getloopscale/loop-sandbox/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// setupPostgres returns a pool on a fresh database of the shared PostgreSQL server
// (see emptyPostgres) with every migration applied.
func setupPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := emptyPostgres(t)
	for _, stmt := range migrations.Ordered() {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("apply migration: %v", err)
		}
	}
	return pool
}

func TestPostgresStore_save_get_delete(t *testing.T) {
	pool := setupPostgres(t)
	store := postgres.New(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	sb := &domain.Sandbox{
		ID: "sbx_1", TenantID: "tenant-a", Image: "python:3",
		State: domain.StateRunning, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Save(ctx, sb); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Get(ctx, "tenant-a", "sbx_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != domain.StateRunning || got.Image != "python:3" {
		t.Errorf("got = %+v", got)
	}

	if err := store.Delete(ctx, "tenant-a", "sbx_1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "tenant-a", "sbx_1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get after delete err = %v, want ErrNotFound", err)
	}
}

// TestPostgresStore_save_upserts_state proves the ON CONFLICT DO UPDATE branch
// against real SQL — the create flow re-saves the same key (pending → running),
// so a broken upsert target would corrupt every sandbox. The memory store (a map
// overwrite) cannot catch this.
func TestPostgresStore_save_upserts_state(t *testing.T) {
	pool := setupPostgres(t)
	store := postgres.New(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	sb := &domain.Sandbox{ID: "sbx_up", TenantID: "tenant-a", State: domain.StatePending, CreatedAt: now, UpdatedAt: now}
	if err := store.Save(ctx, sb); err != nil {
		t.Fatalf("Save pending: %v", err)
	}
	sb.State = domain.StateRunning
	sb.UpdatedAt = now.Add(time.Second)
	if err := store.Save(ctx, sb); err != nil {
		t.Fatalf("Save running (upsert): %v", err)
	}

	got, err := store.Get(ctx, "tenant-a", "sbx_up")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != domain.StateRunning {
		t.Errorf("upserted State = %q, want running", got.State)
	}
}

func TestPostgresStore_cross_tenant_is_not_found(t *testing.T) {
	pool := setupPostgres(t)
	store := postgres.New(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	_ = store.Save(ctx, &domain.Sandbox{
		ID: "sbx_x", TenantID: "tenant-a", State: domain.StateRunning,
		CreatedAt: now, UpdatedAt: now,
	})
	// tenant-b must not see it (US-05).
	if _, err := store.Get(ctx, "tenant-b", "sbx_x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant Get err = %v, want ErrNotFound", err)
	}
}

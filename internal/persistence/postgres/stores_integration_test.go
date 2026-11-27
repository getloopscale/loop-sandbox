//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
)

func TestPostgres_tenant_and_apikey_resolve(t *testing.T) {
	pool := setupPostgres(t)
	store := postgres.New(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	tn, _ := domain.NewTenant("tenant-a", "acme", domain.PlanPro, now)
	if err := store.SaveTenant(ctx, tn); err != nil {
		t.Fatalf("SaveTenant: %v", err)
	}
	raw, key, _ := domain.GenerateAPIKey("k1", "tenant-a", "ci", []string{"sandbox:execute"}, now)
	if err := store.SaveAPIKey(ctx, key); err != nil {
		t.Fatalf("SaveAPIKey: %v", err)
	}

	// resolve by the raw key (hash lookup)
	got, err := store.ResolveByRawKey(ctx, raw)
	if err != nil {
		t.Fatalf("ResolveByRawKey: %v", err)
	}
	if got.TenantID != "tenant-a" || got.ID != "k1" || !got.HasScope("sandbox:execute") {
		t.Errorf("resolved = %+v", got)
	}
	// unknown key → ErrNotFound
	if _, err := store.ResolveByRawKey(ctx, "lsb_unknown"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown key err = %v, want ErrNotFound", err)
	}
	// revoke → still resolves but marked revoked (auth layer rejects)
	if err := store.Revoke(ctx, "tenant-a", "k1", time.Now().UTC()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, _ = store.ResolveByRawKey(ctx, raw)
	if !got.Revoked() {
		t.Error("key should be revoked after Revoke")
	}
}

func TestPostgres_idempotency_and_cost(t *testing.T) {
	pool := setupPostgres(t)
	store := postgres.New(pool)
	ctx := context.Background()

	now := time.Now().Unix()
	// First Reserve wins (Claimed).
	res, err := store.Reserve(ctx, "tenant-a", "idem-1", "h1", now)
	if err != nil || res.Outcome != domain.Claimed {
		t.Fatalf("first Reserve = %v (err %v), want Claimed", res.Outcome, err)
	}
	// Same key, same hash, not yet completed → InProgress.
	if r2, _ := store.Reserve(ctx, "tenant-a", "idem-1", "h1", now); r2.Outcome != domain.InProgress {
		t.Errorf("in-flight Reserve = %v, want InProgress", r2.Outcome)
	}
	// Same key, DIFFERENT hash → Conflict.
	if r3, _ := store.Reserve(ctx, "tenant-a", "idem-1", "h2", now); r3.Outcome != domain.Conflict {
		t.Errorf("different-payload Reserve = %v, want Conflict", r3.Outcome)
	}
	// Complete then Reserve same hash → Replay with the stored body.
	if err := store.Complete(ctx, "tenant-a", "idem-1", 201, []byte(`{"id":"x"}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	r4, _ := store.Reserve(ctx, "tenant-a", "idem-1", "h1", now)
	if r4.Outcome != domain.Replay || r4.Record.Status != 201 || string(r4.Record.Body) != `{"id":"x"}` {
		t.Errorf("post-complete Reserve = %+v, want Replay 201", r4)
	}
	// Cross-tenant: tenant-b with the same key is a fresh claim (no leak of tenant-a's body).
	if rb, _ := store.Reserve(ctx, "tenant-b", "idem-1", "h1", now); rb.Outcome != domain.Claimed {
		t.Errorf("cross-tenant Reserve = %v, want Claimed (isolated)", rb.Outcome)
	}

	start, end := domain.CurrentPeriod(time.Now())
	_ = store.Increment(ctx, "tenant-a", domain.MetricSandboxCreations, 1, start, end)
	_ = store.Increment(ctx, "tenant-a", domain.MetricSandboxCreations, 2, start, end)
	v, err := store.Value(ctx, "tenant-a", domain.MetricSandboxCreations, start)
	if err != nil || v != 3 {
		t.Errorf("cost value = %d (err %v), want 3", v, err)
	}
}

func TestPostgres_execution_and_template_persist(t *testing.T) {
	pool := setupPostgres(t)
	store := postgres.New(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	ex, _ := domain.NewExecution("ex1", "sbx1", "tenant-a", now)
	if err := store.SaveExecution(ctx, ex); err != nil {
		t.Fatalf("SaveExecution: %v", err)
	}
	if got, err := store.GetExecution(ctx, "tenant-a", "ex1"); err != nil || got.SandboxID != "sbx1" {
		t.Errorf("GetExecution = %+v, err %v", got, err)
	}
	if _, err := store.GetExecution(ctx, "tenant-b", "ex1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant execution err = %v, want ErrNotFound", err)
	}

	tpl, _ := domain.NewTemplate("py", "tenant-a", "python", "python:3", now)
	if err := store.SaveTemplate(ctx, tpl); err != nil {
		t.Fatalf("SaveTemplate: %v", err)
	}
	if got, err := store.GetTemplate(ctx, "tenant-a", "py"); err != nil || got.ImageRef != "python:3" {
		t.Errorf("GetTemplate = %+v, err %v", got, err)
	}
}

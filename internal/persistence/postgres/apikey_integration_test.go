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

// keyStore returns a store with tenant-a and tenant-b, each holding the keys named.
func keyStore(t *testing.T, keysByTenant map[string][]string) *postgres.Store {
	t.Helper()
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		tn, err := domain.NewTenant(tenant, tenant, domain.PlanStarter, base)
		if err != nil {
			t.Fatalf("NewTenant: %v", err)
		}
		if err := store.SaveTenant(ctx, tn); err != nil {
			t.Fatalf("SaveTenant: %v", err)
		}
		for i, id := range keysByTenant[tenant] {
			_, key, err := domain.GenerateAPIKey(id, tenant, id, []string{domain.ScopeSandboxExecute}, base.Add(time.Duration(i)*time.Second))
			if err != nil {
				t.Fatalf("GenerateAPIKey: %v", err)
			}
			if err := store.SaveAPIKey(ctx, key); err != nil {
				t.Fatalf("SaveAPIKey: %v", err)
			}
		}
	}
	return store
}

func TestListAPIKeysReturnsOnlyTheTenantsKeysOldestFirst(t *testing.T) {
	store := keyStore(t, map[string][]string{"tenant-a": {"k_first", "k_second"}, "tenant-b": {"k_other"}})

	got, err := store.ListAPIKeys(context.Background(), "tenant-a")

	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(got) != 2 || got[0].ID != "k_first" || got[1].ID != "k_second" {
		t.Errorf("ListAPIKeys = %+v, want k_first then k_second", got)
	}
	if got[0].Scopes[0] != domain.ScopeSandboxExecute || got[0].Revoked() {
		t.Errorf("first key = %+v, want an active key with the sandbox scope", got[0])
	}
}

func TestRevokeKeepsTheFirstRevocationTime(t *testing.T) {
	store := keyStore(t, map[string][]string{"tenant-a": {"k1"}})
	ctx := context.Background()
	first := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	if err := store.Revoke(ctx, "tenant-a", "k1", first); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if err := store.Revoke(ctx, "tenant-a", "k1", first.Add(time.Hour)); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	got, err := store.ListAPIKeys(ctx, "tenant-a")

	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if got[0].RevokedAt == nil || !got[0].RevokedAt.Equal(first) {
		t.Errorf("revoked_at = %v, want %v", got[0].RevokedAt, first)
	}
}

func TestRevokeOfAnotherTenantsKeyIsNotFoundAndLeavesItActive(t *testing.T) {
	store := keyStore(t, map[string][]string{"tenant-a": {"k1"}})
	ctx := context.Background()

	err := store.Revoke(ctx, "tenant-b", "k1", time.Now().UTC())
	got, lerr := store.ListAPIKeys(ctx, "tenant-a")

	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Revoke err = %v, want ErrNotFound", err)
	}
	if lerr != nil || got[0].Revoked() {
		t.Errorf("tenant-a's key after a cross-tenant revoke: revoked=%v err=%v", got[0].Revoked(), lerr)
	}
}

func TestRevokeOfAnUnknownKeyIsNotFound(t *testing.T) {
	store := keyStore(t, nil)

	err := store.Revoke(context.Background(), "tenant-a", "k_nope", time.Now().UTC())

	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Revoke err = %v, want ErrNotFound", err)
	}
}

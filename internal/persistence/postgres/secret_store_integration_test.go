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

func secretRecord(tenant, name, ciphertext string, at time.Time) *domain.Secret {
	return &domain.Secret{TenantID: tenant, Name: name, Ciphertext: []byte(ciphertext), KeyID: "key-1", CreatedAt: at, UpdatedAt: at}
}

func TestSecretStorePutInsertsThenReplacesKeepingTheCreationTime(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()

	created, err := store.PutSecret(ctx, secretRecord("tenant-a", "TOKEN", "first", fixedNow))
	replaced, err2 := store.PutSecret(ctx, secretRecord("tenant-a", "TOKEN", "second", fixedNow.Add(time.Hour)))

	got, getErr := store.GetSecret(ctx, "tenant-a", "TOKEN")
	if err != nil || err2 != nil || getErr != nil || !created || replaced {
		t.Fatalf("put: %v %v, get: %v, created %v then %v; want inserted then replaced", err, err2, getErr, created, replaced)
	}
	if string(got.Ciphertext) != "second" || !got.CreatedAt.Equal(fixedNow) || !got.UpdatedAt.Equal(fixedNow.Add(time.Hour)) {
		t.Errorf("got %q created %v updated %v; want second, the first creation time and the later update", got.Ciphertext, got.CreatedAt, got.UpdatedAt)
	}
}

func TestSecretStoreListsOnlyTheTenantOrderedByName(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	for _, s := range []*domain.Secret{
		secretRecord("tenant-a", "ZETA", "z", fixedNow), secretRecord("tenant-a", "ALPHA", "a", fixedNow),
		secretRecord("tenant-b", "OTHER", "o", fixedNow),
	} {
		if _, err := store.PutSecret(ctx, s); err != nil {
			t.Fatalf("PutSecret: %v", err)
		}
	}

	got, err := store.ListSecrets(ctx, "tenant-a")

	if err != nil || len(got) != 2 || got[0].Name != "ALPHA" || got[1].Name != "ZETA" {
		t.Errorf("list = %+v err = %v; want ALPHA then ZETA", got, err)
	}
}

func TestSecretStoreHidesAndKeepsASecretOfAnotherTenant(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	if _, err := store.PutSecret(ctx, secretRecord("tenant-a", "TOKEN", "v", fixedNow)); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	_, getErr := store.GetSecret(ctx, "tenant-b", "TOKEN")
	delErr := store.DeleteSecret(ctx, "tenant-b", "TOKEN")

	if !errors.Is(getErr, domain.ErrNotFound) || !errors.Is(delErr, domain.ErrNotFound) {
		t.Errorf("get: %v, delete: %v; want ErrNotFound for both", getErr, delErr)
	}
	if _, err := store.GetSecret(ctx, "tenant-a", "TOKEN"); err != nil {
		t.Errorf("the secret is gone for its owner: %v", err)
	}
}

func TestSecretStoreDeleteRemovesTheSecret(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	if _, err := store.PutSecret(ctx, secretRecord("tenant-a", "TOKEN", "v", fixedNow)); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	err := store.DeleteSecret(ctx, "tenant-a", "TOKEN")

	if _, getErr := store.GetSecret(ctx, "tenant-a", "TOKEN"); err != nil || !errors.Is(getErr, domain.ErrNotFound) {
		t.Errorf("delete: %v, get afterwards: %v; want nil and ErrNotFound", err, getErr)
	}
	if again := store.DeleteSecret(ctx, "tenant-a", "TOKEN"); !errors.Is(again, domain.ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", again)
	}
}

func TestSandboxStoreKeepsTheNamesOfItsSecrets(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	sb := idleSandbox(t, "sbx_secrets", 0, fixedNow)
	sb.Secrets = []string{"TOKEN", "OTHER"}
	if err := store.Save(ctx, sb); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Get(ctx, "tenant-a", "sbx_secrets")

	if err != nil || len(got.Secrets) != 2 || got.Secrets[0] != "TOKEN" || got.Secrets[1] != "OTHER" {
		t.Errorf("secrets = %v err = %v; want TOKEN and OTHER", got.Secrets, err)
	}
}

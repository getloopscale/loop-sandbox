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

func snapshotRecord(tenant, id, name string, at time.Time) *domain.Snapshot {
	return &domain.Snapshot{
		ID: id, TenantID: tenant, Name: name, SandboxID: "sbx_1", Image: "busybox:latest",
		Metadata: map[string]string{"team": "search"}, Env: map[string]string{"MODE": "test"},
		SizeBytes: 4096, CreatedAt: at,
	}
}

func TestSnapshotStoreReturnsWhatWasSaved(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	want := snapshotRecord("tenant-a", "snap_1", "base", fixedNow)
	if err := store.SaveSnapshot(ctx, want); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	got, err := store.GetSnapshot(ctx, "tenant-a", "snap_1")

	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if got.Name != want.Name || got.SandboxID != want.SandboxID || got.Image != want.Image || got.SizeBytes != want.SizeBytes ||
		!got.CreatedAt.Equal(want.CreatedAt) || got.Metadata["team"] != "search" || got.Env["MODE"] != "test" {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSnapshotStoreRefusesASecondSnapshotWithTheNameInTheSameTenantOnly(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	if err := store.SaveSnapshot(ctx, snapshotRecord("tenant-a", "snap_1", "base", fixedNow)); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	sameTenant := store.SaveSnapshot(ctx, snapshotRecord("tenant-a", "snap_2", "base", fixedNow))
	otherTenant := store.SaveSnapshot(ctx, snapshotRecord("tenant-b", "snap_3", "base", fixedNow))

	if !errors.Is(sameTenant, domain.ErrAlreadyExists) || otherTenant != nil {
		t.Errorf("same tenant: %v, other tenant: %v; want ErrAlreadyExists and nil", sameTenant, otherTenant)
	}
}

func TestSnapshotStoreListsOnlyTheTenantOldestFirst(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	for _, s := range []*domain.Snapshot{
		snapshotRecord("tenant-a", "snap_new", "new", fixedNow.Add(time.Hour)),
		snapshotRecord("tenant-a", "snap_old", "old", fixedNow),
		snapshotRecord("tenant-b", "snap_other", "other", fixedNow),
	} {
		if err := store.SaveSnapshot(ctx, s); err != nil {
			t.Fatalf("SaveSnapshot: %v", err)
		}
	}

	got, err := store.ListSnapshots(ctx, "tenant-a")

	if err != nil || len(got) != 2 || got[0].ID != "snap_old" || got[1].ID != "snap_new" {
		t.Errorf("list = %+v err = %v; want snap_old then snap_new", got, err)
	}
}

func TestSnapshotStoreHidesAndKeepsASnapshotOfAnotherTenant(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	if err := store.SaveSnapshot(ctx, snapshotRecord("tenant-a", "snap_1", "base", fixedNow)); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	_, getErr := store.GetSnapshot(ctx, "tenant-b", "snap_1")
	delErr := store.DeleteSnapshot(ctx, "tenant-b", "snap_1")

	if !errors.Is(getErr, domain.ErrNotFound) || delErr != nil {
		t.Errorf("get as another tenant: %v, delete: %v; want ErrNotFound and nil", getErr, delErr)
	}
	if _, err := store.GetSnapshot(ctx, "tenant-a", "snap_1"); err != nil {
		t.Errorf("the snapshot is gone for its owner after another tenant's delete: %v", err)
	}
}

func TestSnapshotStoreDeleteRemovesTheSnapshotAndFreesItsName(t *testing.T) {
	store := postgres.New(setupPostgres(t))
	ctx := context.Background()
	if err := store.SaveSnapshot(ctx, snapshotRecord("tenant-a", "snap_1", "base", fixedNow)); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	err := store.DeleteSnapshot(ctx, "tenant-a", "snap_1")

	if _, getErr := store.GetSnapshot(ctx, "tenant-a", "snap_1"); err != nil || !errors.Is(getErr, domain.ErrNotFound) {
		t.Errorf("delete: %v, get afterwards: %v; want nil and ErrNotFound", err, getErr)
	}
	if err := store.SaveSnapshot(ctx, snapshotRecord("tenant-a", "snap_2", "base", fixedNow)); err != nil {
		t.Errorf("the name is still taken after the delete: %v", err)
	}
}

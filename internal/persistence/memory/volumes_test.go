package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
)

func TestVolumeStore_roundtrip_and_tenant_isolation(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	now := time.Unix(0, 0).UTC()
	v, _ := domain.NewVolume("vol-1", "tenant-a", "cell-0", "data", 5, now)
	_ = v.AttachTo("sb-1")
	if err := s.SaveVolume(ctx, v); err != nil {
		t.Fatalf("SaveVolume: %v", err)
	}

	got, err := s.GetVolume(ctx, "tenant-a", "vol-1")
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if got.SizeGB != 5 || got.SandboxID != "sb-1" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}

	// Cross-tenant is indistinguishable from not-found (US-05).
	if _, err := s.GetVolume(ctx, "tenant-b", "vol-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-tenant GetVolume err = %v, want ErrNotFound", err)
	}

	// List by sandbox is tenant-scoped.
	vs, err := s.ListVolumes(ctx, "tenant-a", "sb-1")
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(vs) != 1 || vs[0].ID != "vol-1" {
		t.Errorf("ListVolumes = %+v, want [vol-1]", vs)
	}
	if other, _ := s.ListVolumes(ctx, "tenant-b", "sb-1"); len(other) != 0 {
		t.Errorf("cross-tenant ListVolumes leaked %d volumes", len(other))
	}
}

func TestVolumeStore_delete_is_idempotent(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	now := time.Unix(0, 0).UTC()
	v, _ := domain.NewVolume("vol-1", "tenant-a", "cell-0", "data", 1, now)
	_ = s.SaveVolume(ctx, v)
	if err := s.DeleteVolume(ctx, "tenant-a", "vol-1"); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	// second delete is a no-op, not an error
	if err := s.DeleteVolume(ctx, "tenant-a", "vol-1"); err != nil {
		t.Fatalf("idempotent DeleteVolume: %v", err)
	}
	if _, err := s.GetVolume(ctx, "tenant-a", "vol-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
}

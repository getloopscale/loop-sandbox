package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// ErrVolumesUnsupported is returned when durable volumes are not available — either the
// volume store is unwired or the runtime cannot attach volumes (M16).
var ErrVolumesUnsupported = errors.New("durable volumes not supported by this deployment")

// CreateVolume provisions a durable named volume and attaches it to the actor's sandbox
// (M16). The volume's data survives sandbox destroy and can be re-attached later. It is
// cell-pinned to the sandbox's cell (B2 model) and counts against the tenant's storage
// quota. Cross-tenant sandbox access is not-found.
func (s *Service) CreateVolume(ctx context.Context, actor Actor, sandboxID, name string, sizeGB int64) (*domain.Volume, error) {
	ctx, end := tracing.Start(ctx, "volume.create", actor.TenantID, sandboxID)
	defer end()
	// Validate the name at the boundary: it becomes a guest mount path segment AND a kernel
	// cmdline token, so an unsafe name (whitespace / slash / traversal) must be rejected here
	// (fail-fast) before it is persisted or reaches the runtime (M16 review HIGH).
	if !domain.ValidVolumeName(name) {
		return nil, domain.ErrInvalidArgument
	}
	// Serialize same-name creates so two concurrent requests cannot double-provision a volume
	// (the get-create-save is not atomic across stores; M16 review MEDIUM).
	unlock := s.locks.lock("vol:" + actor.TenantID + ":" + name)
	defer unlock()
	sb, err := s.useSandbox(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	vd, ok := s.driver.(runtime.VolumeDriver)
	if !ok || !s.supports(runtime.CapVolumes) {
		return nil, ErrVolumesUnsupported
	}
	now := s.clock.Now()

	// Named-volume model: a volume named `name` that already exists is RE-ATTACHED to this
	// sandbox (its durable data is preserved — the whole point of a volume). Only a genuinely
	// new volume is provisioned + charged against the storage quota.
	if existing, gerr := s.vols.GetVolumeByName(ctx, actor.TenantID, name); gerr == nil {
		if existing.CellID != sb.CellID {
			// A volume is cell-pinned; it can only attach to a sandbox in the same cell.
			return nil, domain.ErrInvalidArgument
		}
		if err := existing.AttachTo(sandboxID); err != nil {
			return nil, err // already attached to a different live sandbox
		}
		rctx, rend := tracing.Child(ctx, "runtime.volume.attach")
		aerr := vd.AttachVolume(rctx, runtime.SandboxID(sandboxID), existing.ID, existing.Name, existing.SizeGB)
		rend()
		if aerr != nil {
			return nil, fmt.Errorf("attach volume: %w", s.mapRuntimeErr(aerr))
		}
		if err := s.vols.SaveVolume(ctx, existing); err != nil {
			_ = vd.DetachVolume(ctx, runtime.SandboxID(sandboxID), existing.ID)
			return nil, err
		}
		s.audit.Record(actor.TenantID, actor.KeyID, "volume.attach", "volume", existing.ID, actor.RequestID, now)
		return existing, nil
	}

	// New volume: enforce the storage quota (live gauge = current provisioned GB + this volume
	// ≤ cap), summed from the volumes table so it can't desync across billing periods.
	if err := s.enforceVolumeStorage(ctx, actor.TenantID, sizeGB, now); err != nil {
		return nil, err
	}
	vol, err := domain.NewVolume("vol_"+s.ids.New(), actor.TenantID, sb.CellID, name, sizeGB, now)
	if err != nil {
		return nil, err
	}
	if err := vol.AttachTo(sandboxID); err != nil {
		return nil, err
	}
	rctx, rend := tracing.Child(ctx, "runtime.volume.attach")
	aerr := vd.AttachVolume(rctx, runtime.SandboxID(sandboxID), vol.ID, name, sizeGB)
	rend()
	if aerr != nil {
		return nil, fmt.Errorf("attach volume: %w", s.mapRuntimeErr(aerr))
	}
	if err := s.vols.SaveVolume(ctx, vol); err != nil {
		// Backing store attached but not persisted — detach + delete the data to avoid an
		// orphan the tenant is not billed for and cannot see.
		_ = vd.DetachVolume(ctx, runtime.SandboxID(sandboxID), vol.ID)
		_ = vd.DeleteVolumeData(ctx, vol.ID)
		return nil, err
	}
	// Storage usage is a live gauge derived from the volumes table (TotalSizeGB) — no separate
	// counter to increment, so it can never drift from the source of truth (M16 review).
	s.audit.Record(actor.TenantID, actor.KeyID, "volume.create", "volume", vol.ID, actor.RequestID, now)
	return vol, nil
}

// ListVolumes returns the volumes attached to the actor's sandbox (M16). Cross-tenant
// access is not-found.
func (s *Service) ListVolumes(ctx context.Context, actor Actor, sandboxID string) ([]*domain.Volume, error) {
	if _, err := s.ownedSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	if s.vols == nil {
		return nil, ErrVolumesUnsupported
	}
	return s.vols.ListVolumes(ctx, actor.TenantID, sandboxID)
}

// DeleteVolume detaches and destroys a durable volume's backing store, releasing its
// storage quota (M16). Cross-tenant access is not-found (the volume store is tenant-scoped).
func (s *Service) DeleteVolume(ctx context.Context, actor Actor, volumeID string) error {
	ctx, end := tracing.Start(ctx, "volume.delete", actor.TenantID, volumeID)
	defer end()
	if s.vols == nil {
		return ErrVolumesUnsupported
	}
	vol, err := s.vols.GetVolume(ctx, actor.TenantID, volumeID)
	if err != nil {
		return err // ErrNotFound for unknown / cross-tenant
	}
	// Refuse to delete a volume that is still attached to a live sandbox — unlinking the
	// backing image out from under a running guest would silently lose data on VM teardown
	// (M16 review MEDIUM). The caller must destroy/detach the sandbox first.
	if vol.SandboxID != "" {
		return domain.ErrInvalidTransition
	}
	now := s.clock.Now()
	if vd, ok := s.driver.(runtime.VolumeDriver); ok {
		if derr := vd.DeleteVolumeData(ctx, vol.ID); derr != nil {
			return fmt.Errorf("delete volume data: %w", derr)
		}
	}
	if err := s.vols.DeleteVolume(ctx, actor.TenantID, volumeID); err != nil {
		return err
	}
	// Storage usage is a live gauge (TotalSizeGB) — deleting the row releases it, no counter
	// to decrement (so the gauge can never go negative; M16 review).
	s.audit.Record(actor.TenantID, actor.KeyID, "volume.delete", "volume", vol.ID, actor.RequestID, now)
	return nil
}

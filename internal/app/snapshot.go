package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// Snapshot errors.
var (
	// ErrSnapshotsUnsupported is returned when the deployment has no snapshot
	// store or the runtime cannot take snapshots.
	ErrSnapshotsUnsupported = errors.New("snapshots not supported by this deployment")
	// ErrTooManySnapshots is returned when the tenant already keeps
	// domain.MaxSnapshotsPerTenant snapshots.
	ErrTooManySnapshots = errors.New("too many snapshots")
	// ErrSnapshotNameTaken is returned when the tenant has a snapshot with the name.
	ErrSnapshotNameTaken = errors.New("snapshot name already in use")
)

// SetSnapshotStore enables named snapshots (composition root only).
func (s *Service) SetSnapshotStore(store domain.SnapshotStore) { s.snaps = store }

func (s *Service) snapshotDriver() (runtime.SnapshotDriver, error) {
	sd, ok := s.driver.(runtime.SnapshotDriver)
	if !ok || s.snaps == nil || !s.supports(runtime.CapSnapshots) {
		return nil, ErrSnapshotsUnsupported
	}
	return sd, nil
}

// CreateSnapshot saves the state of a running sandbox of the actor under name.
// The sandbox keeps running. The name is unique within the tenant.
func (s *Service) CreateSnapshot(ctx context.Context, actor Actor, sandboxID, name string) (*domain.Snapshot, error) {
	ctx, end := tracing.Start(ctx, "snapshot.create", actor.TenantID, sandboxID)
	defer end()
	if !domain.ValidSnapshotName(name) {
		return nil, &domain.ValidationError{Field: "name", Reason: "must be 1 to 63 lower-case letters, digits, dots, underscores or hyphens, starting with a letter or digit"}
	}
	sb, err := s.useSandbox(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	sd, err := s.snapshotDriver()
	if err != nil {
		return nil, err
	}
	if sb.State != domain.StateRunning {
		return nil, fmt.Errorf("%w: sandbox %s is %s, and only a running sandbox can be snapshotted", domain.ErrInvalidTransition, sandboxID, sb.State)
	}
	// One snapshot request per tenant at a time: the count and the name are
	// checked and then written, and two requests must not both pass the check.
	unlock := s.locks.lock("snap:" + actor.TenantID)
	defer unlock()
	existing, err := s.snaps.ListSnapshots(ctx, actor.TenantID)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	if len(existing) >= domain.MaxSnapshotsPerTenant {
		return nil, ErrTooManySnapshots
	}
	for _, other := range existing {
		if other.Name == name {
			return nil, ErrSnapshotNameTaken
		}
	}
	now := s.clock.Now()
	snap := &domain.Snapshot{
		ID: "snap_" + s.ids.New(), TenantID: actor.TenantID, Name: name, SandboxID: sandboxID,
		Image: sb.Image, Metadata: sb.Metadata, Env: sb.Env, CreatedAt: now,
	}
	rctx, rend := tracing.Child(ctx, "runtime.snapshot.create")
	size, err := sd.CreateSnapshot(rctx, runtime.SandboxID(sandboxID), snap.ID)
	rend()
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", s.mapRuntimeErr(err))
	}
	snap.SizeBytes = size
	if err := s.snaps.SaveSnapshot(ctx, snap); err != nil {
		// The state was written and the record was not: remove the state, which
		// no one could find or delete.
		if derr := sd.DeleteSnapshot(context.WithoutCancel(ctx), snap.ID); derr != nil {
			err = errors.Join(err, fmt.Errorf("remove the state of snapshot %s: %w", snap.ID, derr))
		}
		if errors.Is(err, domain.ErrAlreadyExists) {
			return nil, ErrSnapshotNameTaken // another replica took the name first
		}
		return nil, err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "snapshot.create", "snapshot", snap.ID, actor.RequestID, now)
	return snap, nil
}

// ListSnapshots returns the snapshots of the actor's tenant, oldest first.
func (s *Service) ListSnapshots(ctx context.Context, actor Actor) ([]*domain.Snapshot, error) {
	if s.snaps == nil {
		return nil, ErrSnapshotsUnsupported
	}
	return s.snaps.ListSnapshots(ctx, actor.TenantID)
}

// GetSnapshot returns one snapshot of the actor's tenant.
func (s *Service) GetSnapshot(ctx context.Context, actor Actor, snapshotID string) (*domain.Snapshot, error) {
	if s.snaps == nil {
		return nil, ErrSnapshotsUnsupported
	}
	return s.snaps.GetSnapshot(ctx, actor.TenantID, snapshotID)
}

// DeleteSnapshot removes a snapshot and its state. Sandboxes started from it
// keep running.
func (s *Service) DeleteSnapshot(ctx context.Context, actor Actor, snapshotID string) error {
	ctx, end := tracing.Start(ctx, "snapshot.delete", actor.TenantID, snapshotID)
	defer end()
	sd, err := s.snapshotDriver()
	if err != nil {
		return err
	}
	snap, err := s.snaps.GetSnapshot(ctx, actor.TenantID, snapshotID)
	if err != nil {
		return err
	}
	// The state first: when it fails the record stays and the delete can be tried again.
	if err := sd.DeleteSnapshot(ctx, snap.ID); err != nil {
		return fmt.Errorf("delete snapshot: %w", s.mapRuntimeErr(err))
	}
	if err := s.snaps.DeleteSnapshot(ctx, actor.TenantID, snap.ID); err != nil {
		return err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "snapshot.delete", "snapshot", snap.ID, actor.RequestID, s.clock.Now())
	return nil
}

// createFromSnapshot starts a sandbox from a snapshot of the actor's tenant. It
// counts as a creation for the quota. Labels and environment given in attrs
// replace those recorded in the snapshot; when attrs has none, the snapshot's apply.
func (s *Service) createFromSnapshot(ctx context.Context, actor Actor, snapshotID string, ttl time.Duration, attrs sandboxAttributes) (*domain.Sandbox, error) {
	ctx, end := tracing.Start(ctx, "sandbox.create_from_snapshot", actor.TenantID, snapshotID)
	defer end()
	sd, err := s.snapshotDriver()
	if err != nil {
		return nil, err
	}
	snap, err := s.snaps.GetSnapshot(ctx, actor.TenantID, snapshotID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, &domain.ValidationError{Field: "snapshot", Reason: "no snapshot with this id"}
		}
		return nil, err
	}
	now := s.clock.Now()
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricSandboxCreations, 1, now); err != nil {
		return nil, err
	}
	sb, err := domain.NewSandbox(s.ids.New(), actor.TenantID, snap.Image, clampTTL(ttl), now)
	if err != nil {
		return nil, err
	}
	metadata, env := snap.Metadata, snap.Env
	if attrs.metadata != nil {
		metadata = attrs.metadata
	}
	if attrs.env != nil {
		env = attrs.env
	}
	if err := sb.SetAttributes(metadata, env); err != nil {
		return nil, err
	}
	attrs.apply(sb, now)
	if s.router != nil {
		sb.CellID = s.router.CellFor(actor.TenantID)
	}
	rctx, rend := tracing.Child(ctx, "runtime.snapshot.restore")
	_, rerr := sd.RestoreSnapshot(rctx, snap.ID, runtime.SandboxID(sb.ID))
	rend()
	if rerr != nil {
		return nil, fmt.Errorf("restore snapshot: %w", s.mapRuntimeErr(rerr))
	}
	if err := sb.TransitionTo(domain.StateRunning, now); err != nil {
		return nil, err
	}
	if err := s.saveTraced(ctx, sb, "persist sandbox from snapshot"); err != nil {
		if derr := s.driver.Destroy(context.WithoutCancel(ctx), runtime.SandboxID(sb.ID)); derr != nil {
			err = errors.Join(err, fmt.Errorf("destroy the restored sandbox %s: %w", sb.ID, derr))
		}
		return nil, err
	}
	s.meter(ctx, actor.TenantID, domain.MetricSandboxCreations, now)
	s.obs.SandboxCreated(0)
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.create", "sandbox", sb.ID, actor.RequestID, now)
	return sb, nil
}

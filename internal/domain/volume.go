package domain

import (
	"context"
	"regexp"
	"time"
)

// volumeNameRE constrains a volume name to a strict, safe charset. The name becomes a guest
// mount path segment (/workspace/<name>) AND part of the guest kernel command line
// (lsb.vol=…), so it MUST NOT contain whitespace, slashes, dots (traversal), or control
// characters — otherwise a malicious name could inject kernel args or escape the mount point.
var volumeNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidVolumeName reports whether name is a safe volume name.
func ValidVolumeName(name string) bool { return volumeNameRE.MatchString(name) }

// Volume is a named, durable block of storage (M16). Unlike the ephemeral sandbox
// filesystem, a volume's data survives sandbox destroy and can be re-attached to a new
// sandbox. It is tenant-scoped and cell-pinned (like a sandbox) so it stays in the same
// partition (B2 model). A volume is mounted in at most one sandbox at a time.
type Volume struct {
	ID       string
	TenantID string
	// CellID pins the volume to a cell shard; IMMUTABLE after creation (same rationale
	// as Sandbox.CellID — the partitioned PK does not enforce cross-partition uniqueness).
	CellID string
	// Name is the human label / mount identifier (e.g. "data"); the guest mounts it at
	// /workspace/<name>.
	Name string
	// SizeGB is the provisioned size in whole gibibytes (the quota/billing unit).
	SizeGB int64
	// SandboxID is the sandbox the volume is currently attached to, or "" when detached.
	SandboxID string
	CreatedAt time.Time
}

// NewVolume constructs a detached volume. Fail-fast on missing ids or a non-positive size.
func NewVolume(id, tenantID, cellID, name string, sizeGB int64, now time.Time) (*Volume, error) {
	if id == "" || tenantID == "" || cellID == "" || sizeGB <= 0 || !ValidVolumeName(name) {
		return nil, ErrInvalidArgument
	}
	return &Volume{
		ID:        id,
		TenantID:  tenantID,
		CellID:    cellID,
		Name:      name,
		SizeGB:    sizeGB,
		CreatedAt: now,
	}, nil
}

// AttachTo binds the volume to a sandbox. A volume already attached to a DIFFERENT
// sandbox cannot be attached again (it is mounted in at most one sandbox at a time);
// re-attaching to the same sandbox is idempotent.
func (v *Volume) AttachTo(sandboxID string) error {
	if v.SandboxID != "" && v.SandboxID != sandboxID {
		return ErrInvalidTransition
	}
	v.SandboxID = sandboxID
	return nil
}

// Detach unbinds the volume from its sandbox. The data is unaffected (that is the
// whole point of a volume). Idempotent.
func (v *Volume) Detach() { v.SandboxID = "" }

// VolumeStore persists volumes (tenant-scoped, cell-aware). A volume that belongs to
// another tenant MUST be indistinguishable from a non-existent one (ErrNotFound, US-05).
// Volume names are unique per tenant (the named-volume model): GetVolumeByName resolves a
// name to its stable volume so re-attaching by name reuses the SAME durable backing.
type VolumeStore interface {
	SaveVolume(ctx context.Context, v *Volume) error
	GetVolume(ctx context.Context, tenantID, id string) (*Volume, error)
	GetVolumeByName(ctx context.Context, tenantID, name string) (*Volume, error)
	ListVolumes(ctx context.Context, tenantID, sandboxID string) ([]*Volume, error)
	// TotalSizeGB is the tenant's total provisioned storage across ALL volumes — the live
	// source of truth for the storage quota (a gauge that can never go negative or straddle
	// billing periods, unlike a period-scoped counter).
	TotalSizeGB(ctx context.Context, tenantID string) (int64, error)
	DeleteVolume(ctx context.Context, tenantID, id string) error
}

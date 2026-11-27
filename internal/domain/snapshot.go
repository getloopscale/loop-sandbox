package domain

import (
	"context"
	"regexp"
	"time"
)

// MaxSnapshotsPerTenant bounds how many snapshots one tenant may keep. A
// snapshot holds the whole memory of a sandbox, so its count is what bounds the
// disk a tenant can take.
const MaxSnapshotsPerTenant = 20

var snapshotNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidSnapshotName reports whether name can label a snapshot: lower-case
// letters, digits, dot, underscore and hyphen, at most 63 characters.
func ValidSnapshotName(name string) bool { return snapshotNameRE.MatchString(name) }

// Snapshot is the saved memory and disk state of a sandbox, from which new
// sandboxes can start. It belongs to one tenant and outlives the sandbox it was
// taken from.
type Snapshot struct {
	ID       string
	TenantID string
	// Name is unique within the tenant.
	Name string
	// SandboxID is the sandbox the snapshot was taken from, which may no longer exist.
	SandboxID string
	// Image, Metadata and Env are those of the source sandbox when the snapshot
	// was taken; a sandbox started from the snapshot gets them.
	Image    string
	Metadata map[string]string
	Env      map[string]string
	// SizeBytes is what the snapshot takes on the driver's storage.
	SizeBytes int64
	CreatedAt time.Time
}

// SnapshotStore persists snapshot records. Every method is scoped to a tenant:
// another tenant's snapshot is not found.
type SnapshotStore interface {
	// SaveSnapshot inserts the record. It returns ErrAlreadyExists when the
	// tenant has a snapshot with the same name.
	SaveSnapshot(ctx context.Context, s *Snapshot) error
	GetSnapshot(ctx context.Context, tenantID, id string) (*Snapshot, error)
	// ListSnapshots returns the tenant's snapshots, oldest first.
	ListSnapshots(ctx context.Context, tenantID string) ([]*Snapshot, error)
	DeleteSnapshot(ctx context.Context, tenantID, id string) error
}

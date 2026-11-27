// Package domain holds the business entities, the sandbox state machine, and the
// ports (Repository) the domain requires. It imports NO infrastructure and NO
// concrete runtime (DIP / rules/architecture.md).
package domain

import "time"

// State is the domain lifecycle state of a sandbox (subset of TRD §5.4 for M0).
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StatePaused    State = "paused" // M16: snapshotted to durable storage, compute released
	StateDestroyed State = "destroyed"
)

// validTransitions encodes the state machine. M16 adds paused: a running sandbox can
// be paused (snapshot → storage) and a paused sandbox resumed (running) or destroyed.
// A pending sandbox has nothing to snapshot, so pending → paused is rejected.
var validTransitions = map[State]map[State]bool{
	StatePending:   {StateRunning: true, StateDestroyed: true},
	StateRunning:   {StatePaused: true, StateDestroyed: true},
	StatePaused:    {StateRunning: true, StateDestroyed: true},
	StateDestroyed: {}, // terminal
}

// Sandbox is the aggregate root. It carries tenant identity (M1) and a mandatory
// TTL (M4): every sandbox is ephemeral and has an expiry the reconciler enforces.
type Sandbox struct {
	ID       string
	TenantID string
	// CellID is the cell shard (M11 B2 partition key); defaults to "cell-0". IMMUTABLE
	// after creation: the partitioned PK (cell_id, tenant_id, id) does not enforce global
	// (tenant_id, id) uniqueness across partitions, so a sandbox MUST keep its cell for
	// its whole life. Never re-route an existing sandbox to a different cell (that would
	// create a second row). Sandbox ids are globally unique, so distinct sandboxes never
	// collide regardless of cell.
	CellID string
	Image  string
	// GPU, when non-empty, marks this as a GPU-backed sandbox (M19): scheduled onto a GPU cell
	// and metered on the premium GPU tier.
	GPU string
	// Metadata are caller-supplied labels, returned as given and usable as list
	// filters. Env is the default environment of every command run in the sandbox.
	// Both are set at creation (SetAttributes) and never change afterwards.
	Metadata map[string]string
	Env      map[string]string
	// Secrets are the names of the tenant's secrets the sandbox receives as
	// environment variables. Names only; set at creation.
	Secrets []string
	// NetworkAllowOut and NetworkDenyOut are the sandbox's outbound rules. Both
	// empty means the default policy of the runtime.
	NetworkAllowOut []string
	NetworkDenyOut  []string
	State           State
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ExpiresAt       time.Time
	// AutoPauseAfter, when positive, is how long the sandbox may go without a
	// request that uses it before the idle loop pauses it. Set at creation.
	AutoPauseAfter time.Duration
	// LastActiveAt is the last time a request used the sandbox.
	LastActiveAt time.Time
	// AutoPaused is true while the sandbox is paused by the idle loop. The next
	// request that uses it resumes it; a pause the caller asked for stays.
	AutoPaused bool
}

// The bounds of AutoPauseAfter. The floor keeps a sandbox from being paused and
// resumed over and over: each pause writes its whole memory to disk.
const (
	MinAutoPause = 10 * time.Second
	MaxAutoPause = 24 * time.Hour
)

// Idle reports whether the sandbox is running, has an idle limit and has gone
// that long without use.
func (s *Sandbox) Idle(now time.Time) bool {
	return s.State == StateRunning && s.AutoPauseAfter > 0 && !now.Before(s.LastActiveAt.Add(s.AutoPauseAfter))
}

// DefaultCell is the single-cell default a sandbox belongs to until a router assigns
// it a shard (keeps the partition key non-empty).
const DefaultCell = "cell-0"

// NewSandbox constructs a pending sandbox with a mandatory TTL. Validation is
// fail-fast at the boundary; a non-positive ttl is rejected.
func NewSandbox(id, tenantID, image string, ttl time.Duration, now time.Time) (*Sandbox, error) {
	if id == "" || tenantID == "" || ttl <= 0 {
		return nil, ErrInvalidArgument
	}
	return &Sandbox{
		ID:        id,
		TenantID:  tenantID,
		CellID:    DefaultCell,
		Image:     image,
		State:     StatePending,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(ttl),
	}, nil
}

// Expired reports whether the sandbox has passed its TTL as of now.
func (s *Sandbox) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
}

// TransitionTo moves the sandbox to next, enforcing the state machine.
// Invalid transitions return ErrInvalidTransition (typed, no silent corruption).
func (s *Sandbox) TransitionTo(next State, now time.Time) error {
	allowed, ok := validTransitions[s.State]
	if !ok || !allowed[next] {
		return ErrInvalidTransition
	}
	s.State = next
	s.UpdatedAt = now
	return nil
}

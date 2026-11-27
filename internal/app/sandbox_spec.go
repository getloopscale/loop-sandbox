package app

import (
	"context"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// SandboxSpec is what a caller asks for when creating a sandbox.
type SandboxSpec struct {
	// Image is the image reference. Template, when set, names a registered
	// template and replaces Image.
	Image    string
	Template string
	// Snapshot, when set, is the id of a snapshot to start from. It cannot be
	// combined with Image, Template or GPU.
	Snapshot string
	// AutoPause, when positive, pauses the sandbox after that long without a
	// request that uses it; the next one resumes it.
	AutoPause time.Duration
	// Secrets are the names of the tenant's secrets the sandbox receives as
	// environment variables of its commands.
	Secrets []string
	// GPU, when set, asks for a GPU-backed sandbox.
	GPU string
	// TTL is the lifetime; zero means DefaultTTL, and it is capped at MaxTTL.
	TTL time.Duration
	// Metadata are labels returned as given and usable as list filters.
	Metadata map[string]string
	// Env is the default environment of every command run in the sandbox.
	Env map[string]string
}

// CreateSandboxFromSpec provisions a sandbox as described by spec. Metadata and env
// are validated before any quota is spent or anything is provisioned.
func (s *Service) CreateSandboxFromSpec(ctx context.Context, actor Actor, spec SandboxSpec) (*domain.Sandbox, error) {
	// A GPU template would drop the GPU and skip its quota gate, so the two
	// cannot be combined.
	if spec.Template != "" && spec.GPU != "" {
		return nil, &domain.ValidationError{Field: "template", Reason: "cannot be combined with gpu"}
	}
	if err := domain.ValidateMetadata(spec.Metadata); err != nil {
		return nil, err
	}
	if err := domain.ValidateEnv(spec.Env); err != nil {
		return nil, err
	}
	if err := s.validateAutoPause(spec.AutoPause); err != nil {
		return nil, err
	}
	if err := s.validateSandboxSecrets(ctx, actor, spec.Secrets); err != nil {
		return nil, err
	}
	attrs := sandboxAttributes{metadata: spec.Metadata, env: spec.Env, autoPause: spec.AutoPause, secrets: spec.Secrets}
	if spec.Snapshot != "" {
		if spec.Image != "" || spec.Template != "" || spec.GPU != "" {
			return nil, &domain.ValidationError{Field: "snapshot", Reason: "cannot be combined with image, template or gpu"}
		}
		return s.createFromSnapshot(ctx, actor, spec.Snapshot, spec.TTL, attrs)
	}
	if spec.Template != "" {
		return s.createFromTemplate(ctx, actor, spec.Template, spec.TTL, attrs)
	}
	return s.createSandbox(ctx, actor, spec.Image, "", spec.GPU, spec.TTL, attrs)
}

// sandboxAttributes carries the caller-supplied labels and environment through
// the create path.
type sandboxAttributes struct {
	metadata map[string]string
	env      map[string]string
	// autoPause is the idle limit; zero means the sandbox never pauses itself.
	autoPause time.Duration
	// secrets are the names of the secrets the sandbox receives.
	secrets []string
}

// apply sets on a new sandbox the attributes that are not validated by
// SetAttributes.
func (a sandboxAttributes) apply(sb *domain.Sandbox, now time.Time) {
	sb.AutoPauseAfter, sb.LastActiveAt = a.autoPause, now
	sb.Secrets = append([]string(nil), a.secrets...)
}

// clampTTL applies the default and the ceiling every sandbox lifetime obeys.
func clampTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return DefaultTTL
	}
	if ttl > MaxTTL {
		return MaxTTL
	}
	return ttl
}

// SetSandboxTTL makes the actor's sandbox expire ttl from now, replacing its
// current expiry. The lifetime can be shortened or extended, and is capped at
// MaxTTL like the lifetime given at creation.
func (s *Service) SetSandboxTTL(ctx context.Context, actor Actor, id string, ttl time.Duration) (*domain.Sandbox, error) {
	ctx, end := tracing.Start(ctx, "sandbox.set_ttl", actor.TenantID, id)
	defer end()
	if ttl <= 0 {
		return nil, &domain.ValidationError{Field: "ttl_seconds", Reason: "must be positive"}
	}
	// Serialized with destroy and pause on the same sandbox, so the expiry is not
	// written back over a row that was just deleted.
	unlock := s.locks.lock("sb:" + id)
	defer unlock()
	sb, err := s.ownedSandbox(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	if err := sb.SetTTL(clampTTL(ttl), now); err != nil {
		return nil, err
	}
	if err := s.saveTraced(ctx, sb, "persist sandbox ttl"); err != nil {
		return nil, err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.set_ttl", "sandbox", id, actor.RequestID, now)
	return sb, nil
}

// Limits of one page of ListSandboxes.
const (
	DefaultSandboxPageSize = 50
	MaxSandboxPageSize     = 200
)

// SandboxPage is one page of a tenant's sandboxes. Next is set when more
// sandboxes follow; passing it as SandboxQuery.After returns the next page.
type SandboxPage struct {
	Sandboxes []*domain.Sandbox
	Next      *domain.SandboxCursor
}

// ListSandboxes returns one page of the actor's sandboxes, newest first. A
// q.Limit of zero means DefaultSandboxPageSize.
func (s *Service) ListSandboxes(ctx context.Context, actor Actor, q domain.SandboxQuery) (*SandboxPage, error) {
	ctx, end := tracing.Start(ctx, "sandbox.list", actor.TenantID, "")
	defer end()
	if q.Limit == 0 {
		q.Limit = DefaultSandboxPageSize
	}
	if q.Limit < 0 || q.Limit > MaxSandboxPageSize {
		return nil, &domain.ValidationError{Field: "limit", Reason: "must be between 1 and 200"}
	}
	for _, st := range q.States {
		if !domain.ValidState(st) {
			return nil, &domain.ValidationError{Field: "state", Reason: "unknown state " + string(st)}
		}
	}
	if err := domain.ValidateMetadata(q.Metadata); err != nil {
		return nil, err
	}
	// One extra row tells whether another page exists without a second query.
	pageSize := q.Limit
	q.Limit = pageSize + 1
	rows, err := s.repo.List(ctx, actor.TenantID, q)
	if err != nil {
		return nil, err
	}
	page := &SandboxPage{Sandboxes: rows}
	if len(rows) > pageSize {
		page.Sandboxes = rows[:pageSize]
		last := page.Sandboxes[pageSize-1]
		page.Next = &domain.SandboxCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

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

// ErrForkUnsupported is returned when the runtime cannot fork a running sandbox (M13).
var ErrForkUnsupported = errors.New("fork not supported by this runtime")

// MaxForkCount is the most forks one request may ask for.
const MaxForkCount = 16

// ForkSandbox clones a running sandbox owned by the actor into a NEW isolated sandbox
// (M13). The fork inherits the parent's state at the fork instant; subsequent writes are
// isolated. The fork is a first-class sandbox: new id, same tenant, persisted, TTL-bound.
func (s *Service) ForkSandbox(ctx context.Context, actor Actor, parentID string) (*domain.Sandbox, error) {
	forks, err := s.ForkSandboxes(ctx, actor, parentID, 1)
	if err != nil {
		return nil, err
	}
	return forks[0], nil
}

// ForkSandboxes clones a running sandbox count times. Each fork is independent
// of the parent and of the others. It is all or nothing: the quota is checked
// for the whole batch before the first fork, and when one fork fails the ones
// already made are destroyed and the error is returned.
func (s *Service) ForkSandboxes(ctx context.Context, actor Actor, parentID string, count int) ([]*domain.Sandbox, error) {
	ctx, end := tracing.Start(ctx, "sandbox.fork", actor.TenantID, parentID)
	defer end()
	if count < 1 || count > MaxForkCount {
		return nil, &domain.ValidationError{Field: "count", Reason: fmt.Sprintf("must be between 1 and %d", MaxForkCount)}
	}
	parent, err := s.useSandbox(ctx, actor, parentID)
	if err != nil {
		return nil, err
	}
	fd, ok := s.driver.(runtime.ForkDriver)
	ok = ok && s.supports(runtime.CapFork)
	if !ok {
		return nil, ErrForkUnsupported
	}
	// A fork counts as a creation for quota (it materializes a new sandbox).
	now := s.clock.Now()
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricSandboxCreations, int64(count), now); err != nil {
		return nil, err
	}
	forks := make([]*domain.Sandbox, 0, count)
	for range count {
		fork, err := s.forkOnce(ctx, actor, fd, parent, now)
		if err != nil {
			// The caller asked for count sandboxes and gets none: a partial batch
			// would leave sandboxes it has no id for. The request may be gone, so
			// the cleanup does not use its context.
			for _, made := range forks {
				if derr := s.DestroySandbox(context.WithoutCancel(ctx), actor, made.ID); derr != nil {
					err = errors.Join(err, fmt.Errorf("destroy fork %s after a failed batch: %w", made.ID, derr))
				}
			}
			return nil, err
		}
		forks = append(forks, fork)
	}
	return forks, nil
}

// forkOnce makes one fork of parent, persists it and records it. The caller has
// checked ownership, capability and quota.
func (s *Service) forkOnce(ctx context.Context, actor Actor, fd runtime.ForkDriver, parent *domain.Sandbox, now time.Time) (*domain.Sandbox, error) {
	parentID := parent.ID
	ttl := parent.ExpiresAt.Sub(now)
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	fork, err := domain.NewSandbox(s.ids.New(), actor.TenantID, parent.Image, ttl, now)
	if err != nil {
		return nil, err
	}
	// The fork is a copy of the parent, so it carries the parent's labels and
	// default environment.
	if err := fork.SetAttributes(parent.Metadata, parent.Env); err != nil {
		return nil, err
	}
	fork.AutoPauseAfter, fork.LastActiveAt = parent.AutoPauseAfter, now
	fork.Secrets = append([]string(nil), parent.Secrets...)
	if s.router != nil {
		fork.CellID = s.router.CellFor(actor.TenantID)
	}
	rctx, rend := tracing.Child(ctx, "runtime.fork")
	_, ferr := fd.Fork(rctx, runtime.SandboxID(parentID), runtime.SandboxID(fork.ID))
	rend()
	if ferr != nil {
		return nil, fmt.Errorf("fork sandbox: %w", s.mapRuntimeErr(ferr))
	}
	if err := fork.TransitionTo(domain.StateRunning, now); err != nil {
		return nil, err
	}
	if err := s.saveTraced(ctx, fork, "persist forked sandbox"); err != nil {
		// The runtime fork exists but we couldn't persist — compensate by destroying it.
		_ = s.driver.Destroy(ctx, runtime.SandboxID(fork.ID))
		return nil, err
	}
	// Meter the fork as a creation ONLY now that it succeeded (a failed fork is not charged).
	s.meter(ctx, actor.TenantID, domain.MetricSandboxCreations, now)
	s.obs.SandboxCreated(0)
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.fork", "sandbox", fork.ID, actor.RequestID, now)
	return fork, nil
}

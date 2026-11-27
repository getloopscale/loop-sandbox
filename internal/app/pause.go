package app

import (
	"context"
	"errors"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// ErrPauseUnsupported is returned when the runtime cannot pause/resume a sandbox (M16).
var ErrPauseUnsupported = errors.New("pause/resume not supported by this runtime")

// PauseSandbox snapshots a running sandbox to durable storage and releases its compute
// (M16). The sandbox transitions running → paused; a long task can hibernate without
// holding compute. Cross-tenant access is not-found. Pausing a non-running sandbox is a
// typed invalid-transition error (mapped to 409).
func (s *Service) PauseSandbox(ctx context.Context, actor Actor, sandboxID string) error {
	ctx, end := tracing.Start(ctx, "sandbox.pause", actor.TenantID, sandboxID)
	defer end()
	unlock := s.locks.lock("sb:" + sandboxID)
	defer unlock()
	sb, err := s.ownedSandbox(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	pd, ok := s.driver.(runtime.PauseDriver)
	ok = ok && s.supports(runtime.CapPause)
	if !ok {
		return ErrPauseUnsupported
	}
	now := s.clock.Now()
	// Validate the transition BEFORE touching the runtime (fail-fast, no partial snapshot).
	if err := sb.TransitionTo(domain.StatePaused, now); err != nil {
		return err
	}
	rctx, rend := tracing.Child(ctx, "runtime.pause")
	perr := pd.Pause(rctx, runtime.SandboxID(sandboxID))
	rend()
	if perr != nil {
		return s.mapRuntimeErr(perr)
	}
	if err := s.saveTraced(ctx, sb, "persist paused sandbox"); err != nil {
		// Snapshot exists but state not persisted — try to resume so the sandbox is not
		// stranded paused-but-recorded-running. Best-effort compensation.
		_ = pd.Resume(ctx, runtime.SandboxID(sandboxID))
		return err
	}
	s.meter(ctx, actor.TenantID, domain.MetricSandboxPauses, now)
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.pause", "sandbox", sandboxID, actor.RequestID, now)
	return nil
}

// ResumeSandbox restores a paused sandbox from its snapshot and resumes execution (M16).
// The sandbox transitions paused → running. Cross-tenant access is not-found; resuming a
// non-paused sandbox is a typed invalid-transition error (409).
func (s *Service) ResumeSandbox(ctx context.Context, actor Actor, sandboxID string) (*domain.Sandbox, error) {
	ctx, end := tracing.Start(ctx, "sandbox.resume", actor.TenantID, sandboxID)
	defer end()
	unlock := s.locks.lock("sb:" + sandboxID)
	defer unlock()
	sb, err := s.ownedSandbox(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	pd, ok := s.driver.(runtime.PauseDriver)
	ok = ok && s.supports(runtime.CapPause)
	if !ok {
		return nil, ErrPauseUnsupported
	}
	now := s.clock.Now()
	if err := sb.TransitionTo(domain.StateRunning, now); err != nil {
		return nil, err
	}
	rctx, rend := tracing.Child(ctx, "runtime.resume")
	rerr := pd.Resume(rctx, runtime.SandboxID(sandboxID))
	rend()
	if rerr != nil {
		return nil, s.mapRuntimeErr(rerr)
	}
	// A resume the caller asked for also ends a pause made by the idle loop, and
	// the idle limit counts from now.
	sb.AutoPaused, sb.LastActiveAt = false, now
	if err := s.saveTraced(ctx, sb, "persist resumed sandbox"); err != nil {
		return nil, err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.resume", "sandbox", sandboxID, actor.RequestID, now)
	return sb, nil
}

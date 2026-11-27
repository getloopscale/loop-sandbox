package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// idleBatch bounds the sandboxes one pass of the idle loop looks at.
const idleBatch = 100

// inUse counts, per sandbox, the requests that are using it right now. The idle
// loop does not pause a sandbox with a request in flight, however long it takes.
type inUse struct {
	mu sync.Mutex
	n  map[string]int
}

func (u *inUse) begin(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.n == nil {
		u.n = map[string]int{}
	}
	u.n[id]++
}

func (u *inUse) end(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.n[id]--; u.n[id] <= 0 {
		delete(u.n, id)
	}
}

func (u *inUse) busy(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n[id] > 0
}

// validateAutoPause checks the idle limit of a create request.
func (s *Service) validateAutoPause(limit time.Duration) error {
	if limit == 0 {
		return nil
	}
	if limit < domain.MinAutoPause || limit > domain.MaxAutoPause {
		return &domain.ValidationError{Field: "auto_pause_seconds", Reason: fmt.Sprintf("must be between %d and %d",
			int(domain.MinAutoPause/time.Second), int(domain.MaxAutoPause/time.Second))}
	}
	if _, ok := s.driver.(runtime.PauseDriver); !ok || !s.supports(runtime.CapPause) {
		return &domain.ValidationError{Field: "auto_pause_seconds", Reason: "this runtime cannot pause a sandbox"}
	}
	if _, ok := s.repo.(domain.ActivityStore); !ok {
		return &domain.ValidationError{Field: "auto_pause_seconds", Reason: "this deployment does not record sandbox activity"}
	}
	return nil
}

// useSandbox resolves the actor's sandbox for an operation that runs inside it.
// For a sandbox with an idle limit it also resumes the sandbox when the idle
// loop paused it, records the activity, and holds the sandbox in use until ctx
// is done. ctx must therefore be the context of the request: a context that
// never ends would keep the sandbox from ever pausing.
func (s *Service) useSandbox(ctx context.Context, actor Actor, id string) (*domain.Sandbox, error) {
	sb, err := s.ownedSandbox(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if sb.AutoPauseAfter <= 0 {
		return sb, nil
	}
	activity, ok := s.repo.(domain.ActivityStore)
	if !ok {
		return sb, nil
	}
	// In use first, then the lock: the idle loop checks the count under the same
	// lock, so it either sees this request or has finished pausing before the
	// state is read below.
	s.using.begin(id)
	context.AfterFunc(ctx, func() {
		s.using.end(id)
		// The request is over, so its context is too; the limit counts from now.
		if err := activity.TouchSandbox(context.WithoutCancel(ctx), actor.TenantID, id, s.clock.Now()); err != nil {
			slog.Error("record sandbox activity", "sandbox", id, "tenant", actor.TenantID, "error", err)
		}
	})
	unlock := s.locks.lock("sb:" + id)
	defer unlock()
	sb, err = s.ownedSandbox(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	if sb.State == domain.StatePaused && sb.AutoPaused {
		if err := s.wake(ctx, actor, sb, now); err != nil {
			return nil, err
		}
		return sb, nil
	}
	if err := activity.TouchSandbox(ctx, actor.TenantID, id, now); err != nil {
		return nil, fmt.Errorf("record sandbox activity: %w", err)
	}
	sb.LastActiveAt = now
	return sb, nil
}

// wake resumes a sandbox the idle loop paused. The caller holds the sandbox lock.
func (s *Service) wake(ctx context.Context, actor Actor, sb *domain.Sandbox, now time.Time) error {
	ctx, end := tracing.Start(ctx, "sandbox.wake", actor.TenantID, sb.ID)
	defer end()
	pd, ok := s.driver.(runtime.PauseDriver)
	if !ok {
		return ErrPauseUnsupported
	}
	if err := sb.TransitionTo(domain.StateRunning, now); err != nil {
		return err
	}
	if err := pd.Resume(ctx, runtime.SandboxID(sb.ID)); err != nil {
		return fmt.Errorf("resume the idle sandbox %s: %w", sb.ID, s.mapRuntimeErr(err))
	}
	sb.AutoPaused = false
	sb.LastActiveAt = now
	if err := s.saveTraced(ctx, sb, "persist woken sandbox"); err != nil {
		return err
	}
	s.audit.Record(actor.TenantID, "system:idle", "sandbox.wake", "sandbox", sb.ID, actor.RequestID, now)
	return nil
}

// PauseIdleSandboxes pauses the running sandboxes that went past their idle
// limit and have no request in flight, and returns how many it paused. One
// sandbox that fails to pause does not stop the others; the failures come back
// joined.
func (s *Service) PauseIdleSandboxes(ctx context.Context) (int, error) {
	activity, ok := s.repo.(domain.ActivityStore)
	if !ok {
		return 0, nil
	}
	pd, ok := s.driver.(runtime.PauseDriver)
	if !ok || !s.supports(runtime.CapPause) {
		return 0, nil
	}
	idle, err := activity.ListIdle(ctx, s.clock.Now(), idleBatch)
	if err != nil {
		return 0, fmt.Errorf("list idle sandboxes: %w", err)
	}
	var (
		paused int
		errs   []error
	)
	for _, candidate := range idle {
		ok, err := s.pauseIfIdle(ctx, pd, candidate.TenantID, candidate.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("sandbox %s: %w", candidate.ID, err))
			continue
		}
		if ok {
			paused++
		}
	}
	return paused, errors.Join(errs...)
}

func (s *Service) pauseIfIdle(ctx context.Context, pd runtime.PauseDriver, tenantID, id string) (bool, error) {
	ctx, end := tracing.Start(ctx, "sandbox.auto_pause", tenantID, id)
	defer end()
	unlock := s.locks.lock("sb:" + id)
	defer unlock()
	if s.using.busy(id) {
		return false, nil
	}
	// Read again under the lock: the sandbox may have been used, paused or
	// destroyed since it was listed.
	sb, err := s.repo.Get(ctx, tenantID, id)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := s.clock.Now()
	if !sb.Idle(now) {
		return false, nil
	}
	if err := sb.TransitionTo(domain.StatePaused, now); err != nil {
		return false, err
	}
	if err := pd.Pause(ctx, runtime.SandboxID(id)); err != nil {
		return false, s.mapRuntimeErr(err)
	}
	sb.AutoPaused = true
	if err := s.saveTraced(ctx, sb, "persist auto-paused sandbox"); err != nil {
		// The runtime paused it and the record says running: resume, so the two agree.
		if rerr := pd.Resume(context.WithoutCancel(ctx), runtime.SandboxID(id)); rerr != nil {
			err = errors.Join(err, fmt.Errorf("resume after the failed save: %w", rerr))
		}
		return false, err
	}
	s.meter(ctx, tenantID, domain.MetricSandboxPauses, now)
	s.audit.Record(tenantID, "system:idle", "sandbox.pause", "sandbox", id, "", now)
	return true, nil
}

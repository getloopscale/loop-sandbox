package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/getloopscale/loop-sandbox/internal/domain"

	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// ErrSessionsUnsupported is returned when the configured runtime driver does not support
// stateful code-interpreter sessions (M12).
var ErrSessionsUnsupported = errors.New("sessions not supported by this runtime")

// sessionDriver returns the driver as a SessionDriver or ErrSessionsUnsupported.
func (s *Service) sessionDriver() (runtime.SessionDriver, error) {
	sd, ok := s.driver.(runtime.SessionDriver)
	ok = ok && s.supports(runtime.CapSessions)
	if !ok {
		return nil, ErrSessionsUnsupported
	}
	return sd, nil
}

// StartSession opens a stateful interpreter session in the actor's sandbox and returns
// its id. Cross-tenant access is not-found (US-05).
func (s *Service) StartSession(ctx context.Context, actor Actor, sandboxID string) (string, error) {
	ctx, end := tracing.Start(ctx, "session.start", actor.TenantID, sandboxID)
	defer end()
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return "", err
	}
	sd, err := s.sessionDriver()
	if err != nil {
		return "", err
	}
	sessionID := "ses_" + s.ids.New()
	if err := sd.StartSession(ctx, runtime.SandboxID(sandboxID), sessionID); err != nil {
		if errors.Is(err, runtime.ErrNoInterpreter) {
			return "", &domain.ValidationError{Field: "image", Reason: "the sandbox image has no python3, which a code session needs"}
		}
		return "", fmt.Errorf("start session: %w", err)
	}
	now := s.clock.Now()
	s.audit.Record(actor.TenantID, actor.KeyID, "session.start", "session", sessionID, actor.RequestID, now)
	return sessionID, nil
}

// RunCell executes one code cell in the session; state from prior cells is visible. A
// cell that raises is a normal result (ExitCode 1), not an error. Charged like an exec.
func (s *Service) RunCell(ctx context.Context, actor Actor, sandboxID, sessionID, code string) (*runtime.ExecResult, error) {
	ctx, end := tracing.Start(ctx, "session.exec", actor.TenantID, sandboxID)
	defer end()
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricSandboxExecutions, 1, s.clock.Now()); err != nil {
		return nil, err
	}
	sd, err := s.sessionDriver()
	if err != nil {
		return nil, err
	}
	rctx, rend := tracing.Child(ctx, "runtime.session_exec")
	res, err := sd.ExecSession(rctx, runtime.SandboxID(sandboxID), sessionID, code)
	rend()
	if err != nil {
		s.obs.ExecObserved("error")
		return nil, s.mapRuntimeErr(err)
	}
	if res.ExitCode == 0 {
		s.obs.ExecObserved("ok")
	} else {
		s.obs.ExecObserved("nonzero_exit")
	}
	return res, nil
}

// ErrStreamingUnsupported is returned when the runtime supports sessions but not streaming.
var ErrStreamingUnsupported = errors.New("session streaming not supported by this runtime")

// RunCellStream executes a cell, forwarding partial output to onChunk as it runs, and
// returns the final result. Same ownership/quota/tracing contract as RunCell.
func (s *Service) RunCellStream(ctx context.Context, actor Actor, sandboxID, sessionID, code string, onChunk func(runtime.StreamChunk)) (*runtime.ExecResult, error) {
	ctx, end := tracing.Start(ctx, "session.exec_stream", actor.TenantID, sandboxID)
	defer end()
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricSandboxExecutions, 1, s.clock.Now()); err != nil {
		return nil, err
	}
	ssd, ok := s.driver.(runtime.StreamingSessionDriver)
	ok = ok && s.supports(runtime.CapSessionStream)
	if !ok {
		if _, err := s.sessionDriver(); err != nil {
			return nil, err // sessions not supported at all
		}
		return nil, ErrStreamingUnsupported
	}
	rctx, rend := tracing.Child(ctx, "runtime.session_exec_stream")
	res, err := ssd.ExecSessionStream(rctx, runtime.SandboxID(sandboxID), sessionID, code, onChunk)
	rend()
	if err != nil {
		s.obs.ExecObserved("error")
		return nil, s.mapRuntimeErr(err)
	}
	if res.ExitCode == 0 {
		s.obs.ExecObserved("ok")
	} else {
		s.obs.ExecObserved("nonzero_exit")
	}
	return res, nil
}

// CloseSession terminates a session's interpreter (idempotent).
func (s *Service) CloseSession(ctx context.Context, actor Actor, sandboxID, sessionID string) error {
	ctx, end := tracing.Start(ctx, "session.close", actor.TenantID, sandboxID)
	defer end()
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return err
	}
	sd, err := s.sessionDriver()
	if err != nil {
		return err
	}
	if err := sd.CloseSession(ctx, runtime.SandboxID(sandboxID), sessionID); err != nil {
		return fmt.Errorf("close session: %w", err)
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "session.close", "session", sessionID, actor.RequestID, s.clock.Now())
	return nil
}

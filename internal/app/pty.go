package app

import (
	"context"
	"errors"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// ErrPTYUnsupported is returned when the runtime cannot open an interactive PTY (M15).
var ErrPTYUnsupported = errors.New("interactive PTY not supported by this runtime")

// OpenPTY opens an interactive terminal into the actor's sandbox (M15). Cross-tenant
// access is not-found. The caller pumps bytes to/from the returned PTY.
func (s *Service) OpenPTY(ctx context.Context, actor Actor, sandboxID string, cmd []string) (runtime.PTY, error) {
	ctx, end := tracing.Start(ctx, "sandbox.pty", actor.TenantID, sandboxID)
	defer end()
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	pd, ok := s.driver.(runtime.PTYDriver)
	ok = ok && s.supports(runtime.CapPTY)
	if !ok {
		return nil, ErrPTYUnsupported
	}
	pty, err := pd.OpenPTY(ctx, runtime.SandboxID(sandboxID), cmd)
	if err != nil {
		return nil, err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.pty", "sandbox", sandboxID, actor.RequestID, s.clock.Now())
	return pty, nil
}

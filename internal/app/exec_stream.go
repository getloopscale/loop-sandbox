package app

import (
	"context"
	"errors"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// ErrExecStreamUnsupported is returned when the runtime cannot stream exec output (M15).
var ErrExecStreamUnsupported = errors.New("streamed exec not supported by this runtime")

// RunExecStream runs a command in the actor's sandbox, forwarding stdout/stderr to onChunk
// as it is produced (M15 streamed exec). Same ownership/quota/audit contract as Exec: a
// streamed exec is a mutable, billed operation, so it records an Execution row and an audit
// event exactly like the non-streaming path. The capability assert happens BEFORE enforce so
// an unsupported runtime never consumes quota (and the caller can map it to 501 up front).
func (s *Service) RunExecStream(ctx context.Context, actor Actor, sandboxID string, req runtime.ExecRequest, onChunk func(runtime.StreamChunk)) (*runtime.ExecResult, error) {
	ctx, end := tracing.Start(ctx, "sandbox.exec_stream", actor.TenantID, sandboxID)
	defer end()
	sb, err := s.useSandbox(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateEnv(req.Env); err != nil {
		return nil, err
	}
	if req, err = s.withCommandEnv(ctx, sb, req); err != nil {
		return nil, err
	}
	sd, ok := s.driver.(runtime.StreamingExecDriver)
	ok = ok && s.supports(runtime.CapExecStream)
	if !ok {
		return nil, ErrExecStreamUnsupported
	}
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricSandboxExecutions, 1, s.clock.Now()); err != nil {
		return nil, err
	}
	now := s.clock.Now()
	execID := "exe_" + s.ids.New()
	if s.execs != nil {
		if ex, err := domain.NewExecution(execID, sandboxID, actor.TenantID, now); err == nil {
			_ = s.execs.SaveExecution(ctx, ex)
		}
	}
	rctx, rend := tracing.Child(ctx, "runtime.exec_stream")
	res, err := sd.ExecStream(rctx, runtime.SandboxID(sandboxID), req, onChunk)
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
	if s.execs != nil {
		if ex, e := domain.NewExecution(execID, sandboxID, actor.TenantID, now); e == nil {
			ex.Status = "completed"
			_ = s.execs.SaveExecution(ctx, ex)
		}
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.exec_stream", "sandbox", sandboxID, actor.RequestID, now)
	return res, nil
}

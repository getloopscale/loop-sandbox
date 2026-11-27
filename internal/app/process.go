package app

import (
	"context"
	"errors"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// Errors of the background process use cases. The API maps the first to 501 and
// the others to 409.
var (
	ErrProcessesUnsupported = errors.New("background processes not supported by this runtime")
	ErrProcessExited        = errors.New("the process has ended")
	ErrStdinClosed          = errors.New("the standard input of the process is closed")
	ErrTooManyProcesses     = errors.New("the sandbox is at its limit of running processes")
)

// Limits of the process use cases.
const (
	// MaxProcessOutputWait is the longest a read of process output may wait.
	MaxProcessOutputWait = 30 * time.Second
	// MaxProcessStdinBytes bounds one write to a process's standard input.
	MaxProcessStdinBytes = 1 << 20
)

// processSignals are the signal names a client may send.
var processSignals = map[string]bool{
	"HUP": true, "INT": true, "QUIT": true, "KILL": true, "TERM": true,
	"USR1": true, "USR2": true, "STOP": true, "CONT": true,
}

// processes resolves the actor's sandbox and the driver capability.
func (s *Service) processes(ctx context.Context, actor Actor, sandboxID string) (*domain.Sandbox, runtime.ProcessDriver, error) {
	sb, err := s.useSandbox(ctx, actor, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	pd, ok := s.driver.(runtime.ProcessDriver)
	if !ok || !s.supports(runtime.CapProcesses) {
		return nil, nil, ErrProcessesUnsupported
	}
	return sb, pd, nil
}

func (s *Service) mapProcessErr(err error) error {
	switch {
	case errors.Is(err, runtime.ErrProcessNotFound):
		return domain.ErrNotFound
	case errors.Is(err, runtime.ErrProcessExited):
		return ErrProcessExited
	case errors.Is(err, runtime.ErrStdinClosed):
		return ErrStdinClosed
	case errors.Is(err, runtime.ErrTooManyProcesses):
		return ErrTooManyProcesses
	default:
		return s.mapRuntimeErr(err)
	}
}

// StartProcess starts a command that keeps running after the request. It counts
// as an execution for quota. The sandbox's env applies, and the request's wins.
func (s *Service) StartProcess(ctx context.Context, actor Actor, sandboxID string, spec runtime.ProcessSpec) (*runtime.ProcessInfo, error) {
	sb, pd, err := s.processes(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	if len(spec.Cmd) == 0 || spec.Cmd[0] == "" {
		return nil, &domain.ValidationError{Field: "cmd", Reason: "must name a command"}
	}
	if err := domain.ValidateEnv(spec.Env); err != nil {
		return nil, err
	}
	if spec.WorkingDir != "" {
		clean, err := domain.ValidateSandboxPath(spec.WorkingDir)
		if err != nil {
			return nil, &domain.ValidationError{Field: "workdir", Reason: "must be a path under /workspace"}
		}
		spec.WorkingDir = clean
	}
	if spec.Env, err = s.commandEnv(ctx, sb, spec.Env); err != nil {
		return nil, err
	}
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricSandboxExecutions, 1, s.clock.Now()); err != nil {
		return nil, err
	}
	info, err := pd.StartProcess(ctx, runtime.SandboxID(sandboxID), spec)
	if err != nil {
		if errors.Is(err, runtime.ErrInvalidRequest) {
			return nil, &domain.ValidationError{Field: "cmd", Reason: "could not be started: " + err.Error()}
		}
		return nil, s.mapProcessErr(err)
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.process.start", "sandbox", sandboxID, actor.RequestID, s.clock.Now())
	return info, nil
}

// ListProcesses lists the processes started in the actor's sandbox, oldest first.
func (s *Service) ListProcesses(ctx context.Context, actor Actor, sandboxID string) ([]runtime.ProcessInfo, error) {
	_, pd, err := s.processes(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	list, err := pd.ListProcesses(ctx, runtime.SandboxID(sandboxID))
	if err != nil {
		return nil, s.mapProcessErr(err)
	}
	return list, nil
}

// GetProcess describes one process of the actor's sandbox.
func (s *Service) GetProcess(ctx context.Context, actor Actor, sandboxID string, pid int) (*runtime.ProcessInfo, error) {
	list, err := s.ListProcesses(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].PID == pid {
			return &list[i], nil
		}
	}
	return nil, domain.ErrNotFound
}

// ProcessOutput returns the output a process wrote after the cursor, waiting up
// to wait for some when there is none yet.
func (s *Service) ProcessOutput(ctx context.Context, actor Actor, sandboxID string, pid int, after int64, wait time.Duration) (*runtime.ProcessOutput, error) {
	_, pd, err := s.processes(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	if after < 0 {
		return nil, &domain.ValidationError{Field: "after", Reason: "must not be negative"}
	}
	if wait < 0 || wait > MaxProcessOutputWait {
		return nil, &domain.ValidationError{Field: "wait_ms", Reason: "must be between 0 and 30000"}
	}
	out, err := pd.ProcessOutput(ctx, runtime.SandboxID(sandboxID), pid, after, wait)
	if err != nil {
		return nil, s.mapProcessErr(err)
	}
	return out, nil
}

// WriteProcessStdin writes data to the standard input of a running process.
func (s *Service) WriteProcessStdin(ctx context.Context, actor Actor, sandboxID string, pid int, data []byte) error {
	_, pd, err := s.processes(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	if len(data) > MaxProcessStdinBytes {
		return &domain.ValidationError{Field: "body", Reason: "is larger than 1 MiB"}
	}
	return s.mapProcessErrOrNil(pd.WriteProcessStdin(ctx, runtime.SandboxID(sandboxID), pid, data))
}

// CloseProcessStdin closes the standard input of a process, which a command
// reading it sees as the end of its input. Closing twice is not an error.
func (s *Service) CloseProcessStdin(ctx context.Context, actor Actor, sandboxID string, pid int) error {
	_, pd, err := s.processes(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	return s.mapProcessErrOrNil(pd.CloseProcessStdin(ctx, runtime.SandboxID(sandboxID), pid))
}

// SignalProcess sends a signal to a running process and the processes it started.
func (s *Service) SignalProcess(ctx context.Context, actor Actor, sandboxID string, pid int, signal string) error {
	_, pd, err := s.processes(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	if !processSignals[signal] {
		return &domain.ValidationError{Field: "signal", Reason: "must be one of HUP, INT, QUIT, KILL, TERM, USR1, USR2, STOP, CONT"}
	}
	if err := pd.SignalProcess(ctx, runtime.SandboxID(sandboxID), pid, signal); err != nil {
		return s.mapProcessErr(err)
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.process.signal", "sandbox", sandboxID, actor.RequestID, s.clock.Now())
	return nil
}

func (s *Service) mapProcessErrOrNil(err error) error {
	if err == nil {
		return nil
	}
	return s.mapProcessErr(err)
}

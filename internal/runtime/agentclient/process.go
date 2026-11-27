// Package agentclient implements operations of a runtime
// driver on top of the in-sandbox agent. The container and Firecracker drivers
// differ only in how a request reaches the agent, which is the RoundTrip they
// supply.
package agentclient

import (
	"context"
	"fmt"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// RoundTrip sends one request to the agent of a sandbox and returns its answer.
type RoundTrip func(ctx context.Context, id runtime.SandboxID, req *agentproto.Request, timeout time.Duration) (*agentproto.Response, error)

// opTimeout bounds an exchange that does not wait for output.
const opTimeout = 30 * time.Second

// Client runs process operations through an agent.
type Client struct{ RoundTrip RoundTrip }

func (c Client) do(ctx context.Context, id runtime.SandboxID, req *agentproto.Request, timeout time.Duration) (*agentproto.Response, error) {
	resp, err := c.RoundTrip(ctx, id, req, timeout)
	if err != nil {
		return nil, err
	}
	switch resp.Err {
	case "":
		return resp, nil
	case agentproto.ErrNotFound:
		return nil, runtime.ErrProcessNotFound
	case agentproto.ErrProcessExited:
		return nil, runtime.ErrProcessExited
	case agentproto.ErrStdinClosed:
		return nil, runtime.ErrStdinClosed
	case agentproto.ErrTooManyProcesses:
		return nil, runtime.ErrTooManyProcesses
	case agentproto.ErrStartFailed:
		return nil, fmt.Errorf("%w: %s", runtime.ErrInvalidRequest, resp.Stderr)
	default:
		return nil, fmt.Errorf("agent %s: %s", req.Op, resp.Err)
	}
}

func infoOf(p *agentproto.ProcessInfo) runtime.ProcessInfo {
	return runtime.ProcessInfo{
		PID: p.PID, Cmd: p.Cmd, StartedAt: time.UnixMilli(p.StartedAt).UTC(),
		Running: p.Running, ExitCode: p.ExitCode,
	}
}

// StartProcess implements runtime.ProcessDriver.
func (c Client) StartProcess(ctx context.Context, id runtime.SandboxID, spec runtime.ProcessSpec) (*runtime.ProcessInfo, error) {
	resp, err := c.do(ctx, id, &agentproto.Request{Op: agentproto.OpProcStart, Cmd: spec.Cmd, Env: spec.Env, WorkDir: spec.WorkingDir}, opTimeout)
	if err != nil {
		return nil, err
	}
	if resp.Process == nil {
		return nil, fmt.Errorf("agent %s: answer without a process", agentproto.OpProcStart)
	}
	info := infoOf(resp.Process)
	return &info, nil
}

// ListProcesses implements runtime.ProcessDriver.
func (c Client) ListProcesses(ctx context.Context, id runtime.SandboxID) ([]runtime.ProcessInfo, error) {
	resp, err := c.do(ctx, id, &agentproto.Request{Op: agentproto.OpProcList}, opTimeout)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.ProcessInfo, 0, len(resp.Processes))
	for i := range resp.Processes {
		out = append(out, infoOf(&resp.Processes[i]))
	}
	return out, nil
}

// ProcessOutput implements runtime.ProcessDriver.
func (c Client) ProcessOutput(ctx context.Context, id runtime.SandboxID, pid int, after int64, wait time.Duration) (*runtime.ProcessOutput, error) {
	req := &agentproto.Request{Op: agentproto.OpProcOutput, PID: pid, After: after, WaitMS: int(wait.Milliseconds())}
	resp, err := c.do(ctx, id, req, wait+opTimeout)
	if err != nil {
		return nil, err
	}
	if resp.Process == nil {
		return nil, fmt.Errorf("agent %s: answer without a process", agentproto.OpProcOutput)
	}
	return &runtime.ProcessOutput{
		Stdout: resp.Stdout, Stderr: resp.Stderr, Next: resp.Next,
		Truncated: resp.Truncated, Process: infoOf(resp.Process),
	}, nil
}

// WriteProcessStdin implements runtime.ProcessDriver.
func (c Client) WriteProcessStdin(ctx context.Context, id runtime.SandboxID, pid int, data []byte) error {
	_, err := c.do(ctx, id, &agentproto.Request{Op: agentproto.OpProcStdin, PID: pid, Data: data}, opTimeout)
	return err
}

// CloseProcessStdin implements runtime.ProcessDriver.
func (c Client) CloseProcessStdin(ctx context.Context, id runtime.SandboxID, pid int) error {
	_, err := c.do(ctx, id, &agentproto.Request{Op: agentproto.OpProcCloseStdin, PID: pid}, opTimeout)
	return err
}

// SignalProcess implements runtime.ProcessDriver.
func (c Client) SignalProcess(ctx context.Context, id runtime.SandboxID, pid int, signal string) error {
	_, err := c.do(ctx, id, &agentproto.Request{Op: agentproto.OpProcSignal, PID: pid, Signal: signal}, opTimeout)
	return err
}

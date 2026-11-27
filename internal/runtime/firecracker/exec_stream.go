package firecracker

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// ExecStream runs a command in the microVM and hands each piece of its output to
// onChunk as the guest produces it. The result carries the exit code only.
//
// The exchange is bounded by req.Timeout (capped at MaxExecTimeout) and by ctx.
// Either one ending closes the connection, and the guest agent then kills the
// command and its process group.
func (d *Driver) ExecStream(ctx context.Context, id runtime.SandboxID, req runtime.ExecRequest, onChunk func(runtime.StreamChunk)) (*runtime.ExecResult, error) {
	v, err := d.lookup(id)
	if err != nil {
		return nil, err
	}
	if len(req.Cmd) == 0 {
		return nil, fmt.Errorf("%w: empty command", runtime.ErrInvalidRequest)
	}
	areq := &agentproto.Request{Op: agentproto.OpExecStream, Cmd: req.Cmd, Env: req.Env, WorkDir: req.WorkingDir}
	if req.Stdin != nil {
		b, rerr := io.ReadAll(io.LimitReader(req.Stdin, maxExecOutput))
		if rerr != nil {
			return nil, rerr
		}
		areq.StdinB64 = b
	}
	timeout := req.Timeout
	if timeout <= 0 || timeout > d.cfg.MaxExecTimeout {
		timeout = d.cfg.MaxExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := dialAgent(ctx, v.udsPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", runtime.ErrSandboxNotFound, err)
	}
	defer func() { _ = conn.Close() }()
	// A read blocked on a silent command does not see ctx; the deadline covers the
	// timeout and closing the connection covers a caller that went away.
	_ = conn.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	fail := func(err error) error {
		switch {
		case isTimeout(err) || ctx.Err() == context.DeadlineExceeded:
			return fmt.Errorf("%w: exec exceeded %s", runtime.ErrTimeout, timeout)
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			return err
		}
	}
	if err := agentproto.WriteFrame(conn, areq); err != nil {
		return nil, fail(err)
	}
	for {
		resp, err := agentproto.ReadResponse(conn)
		if err != nil {
			return nil, fail(err)
		}
		if resp.Stream != "" {
			onChunk(runtime.StreamChunk{Stream: resp.Stream, Data: resp.Data})
			continue
		}
		if resp.Err != "" {
			return nil, fmt.Errorf("%w: %s", runtime.ErrInvalidRequest, resp.Err)
		}
		return &runtime.ExecResult{ExitCode: resp.ExitCode}, nil
	}
}

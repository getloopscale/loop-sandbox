package firecracker

import (
	"context"
	"fmt"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// SessionDriver support (M12): the microVM's in-guest agent owns the persistent
// interpreter kernel; the driver just round-trips session ops over vsock (reusing the
// existing agent channel — no new transport).
var _ runtime.SessionDriver = (*Driver)(nil)

// StartSession asks the guest agent to spawn a kernel for sessionID.
func (d *Driver) StartSession(_ context.Context, id runtime.SandboxID, sessionID string) error {
	v, err := d.lookup(id)
	if err != nil {
		return err
	}
	resp, err := d.roundTrip(v, &agentproto.Request{Op: agentproto.OpSessionStart, SessionID: sessionID}, d.cfg.FileOpTimeout)
	if err != nil {
		return err
	}
	if resp.Err == agentproto.ErrNoInterpreter {
		return runtime.ErrNoInterpreter
	}
	if resp.Err != "" {
		return fmt.Errorf("firecracker session start: %s", resp.Err)
	}
	return nil
}

// ExecSession runs one cell in the guest kernel (state shared across cells).
func (d *Driver) ExecSession(ctx context.Context, id runtime.SandboxID, sessionID, code string) (*runtime.ExecResult, error) {
	v, err := d.lookup(id)
	if err != nil {
		return nil, err
	}
	timeout := d.cfg.MaxExecTimeout
	resp, err := d.roundTrip(v, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: sessionID, Code: code}, timeout)
	if err != nil {
		// A host-side timeout leaves the cell still running in the guest — kill the kernel
		// so a runaway does not keep burning guest CPU + corrupting session state (HIGH-1).
		// The session is invalidated; the client must open a new one.
		_ = d.CloseSession(ctx, id, sessionID)
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("firecracker session exec: %s", resp.Err)
	}
	return &runtime.ExecResult{ExitCode: resp.ExitCode, Stdout: resp.Stdout, Stderr: resp.Stderr}, nil
}

// CloseSession terminates the guest kernel for sessionID.
func (d *Driver) CloseSession(_ context.Context, id runtime.SandboxID, sessionID string) error {
	v, err := d.lookup(id)
	if err != nil {
		return err
	}
	resp, err := d.roundTrip(v, &agentproto.Request{Op: agentproto.OpSessionClose, SessionID: sessionID}, d.cfg.FileOpTimeout)
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return fmt.Errorf("firecracker session close: %s", resp.Err)
	}
	return nil
}

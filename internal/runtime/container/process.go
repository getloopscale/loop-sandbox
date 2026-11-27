package container

import (
	"context"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/agentclient"
)

var _ runtime.ProcessDriver = (*Driver)(nil)

func (d *Driver) agent() agentclient.Client { return agentclient.Client{RoundTrip: d.agentRoundTrip} }

// StartProcess implements runtime.ProcessDriver through the sandbox's agent.
func (d *Driver) StartProcess(ctx context.Context, id runtime.SandboxID, spec runtime.ProcessSpec) (*runtime.ProcessInfo, error) {
	return d.agent().StartProcess(ctx, id, spec)
}

// ListProcesses implements runtime.ProcessDriver.
func (d *Driver) ListProcesses(ctx context.Context, id runtime.SandboxID) ([]runtime.ProcessInfo, error) {
	return d.agent().ListProcesses(ctx, id)
}

// ProcessOutput implements runtime.ProcessDriver.
func (d *Driver) ProcessOutput(ctx context.Context, id runtime.SandboxID, pid int, after int64, wait time.Duration) (*runtime.ProcessOutput, error) {
	return d.agent().ProcessOutput(ctx, id, pid, after, wait)
}

// WriteProcessStdin implements runtime.ProcessDriver.
func (d *Driver) WriteProcessStdin(ctx context.Context, id runtime.SandboxID, pid int, data []byte) error {
	return d.agent().WriteProcessStdin(ctx, id, pid, data)
}

// CloseProcessStdin implements runtime.ProcessDriver.
func (d *Driver) CloseProcessStdin(ctx context.Context, id runtime.SandboxID, pid int) error {
	return d.agent().CloseProcessStdin(ctx, id, pid)
}

// SignalProcess implements runtime.ProcessDriver.
func (d *Driver) SignalProcess(ctx context.Context, id runtime.SandboxID, pid int, signal string) error {
	return d.agent().SignalProcess(ctx, id, pid, signal)
}

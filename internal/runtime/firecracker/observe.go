package firecracker

import (
	"context"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var (
	_ runtime.LogsDriver    = (*Driver)(nil)
	_ runtime.MetricsDriver = (*Driver)(nil)
)

// SandboxLogs implements runtime.LogsDriver through the guest agent.
func (d *Driver) SandboxLogs(ctx context.Context, id runtime.SandboxID, after int64, wait time.Duration) (*runtime.SandboxLogs, error) {
	return d.agent().SandboxLogs(ctx, id, after, wait)
}

// SandboxMetrics implements runtime.MetricsDriver with the guest's own counters:
// the microVM is the sandbox, so its CPU time and memory are the sandbox's.
func (d *Driver) SandboxMetrics(ctx context.Context, id runtime.SandboxID) (*runtime.SandboxMetrics, error) {
	return d.agent().SandboxMetrics(ctx, id)
}

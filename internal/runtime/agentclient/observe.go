package agentclient

import (
	"context"
	"fmt"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// SandboxLogs implements runtime.LogsDriver.
func (c Client) SandboxLogs(ctx context.Context, id runtime.SandboxID, after int64, wait time.Duration) (*runtime.SandboxLogs, error) {
	req := &agentproto.Request{Op: agentproto.OpLogs, After: after, WaitMS: int(wait.Milliseconds())}
	resp, err := c.RoundTrip(ctx, id, req, wait+opTimeout)
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("agent %s: %s", req.Op, resp.Err)
	}
	out := &runtime.SandboxLogs{Next: resp.Next, Truncated: resp.Truncated, Entries: make([]runtime.LogEntry, 0, len(resp.Logs))}
	for _, rec := range resp.Logs {
		stream := "stdout"
		if rec.Stderr {
			stream = "stderr"
		}
		out.Entries = append(out.Entries, runtime.LogEntry{PID: rec.PID, Stream: stream, Data: rec.Data, At: time.UnixMilli(rec.AtMS).UTC()})
	}
	return out, nil
}

// SandboxMetrics implements runtime.MetricsDriver for a sandbox that is a whole
// machine, where what the agent sees is what the sandbox uses.
func (c Client) SandboxMetrics(ctx context.Context, id runtime.SandboxID) (*runtime.SandboxMetrics, error) {
	resp, err := c.RoundTrip(ctx, id, &agentproto.Request{Op: agentproto.OpStats}, opTimeout)
	if err != nil {
		return nil, err
	}
	if resp.Err != "" || resp.Stats == nil {
		return nil, fmt.Errorf("agent %s: %s", agentproto.OpStats, resp.Err)
	}
	return &runtime.SandboxMetrics{
		CPUUsageNanos: resp.Stats.CPUNanos, MemoryUsedBytes: resp.Stats.MemoryUsed,
		MemoryLimitBytes: resp.Stats.MemoryTotal, At: time.Now().UTC(),
	}, nil
}

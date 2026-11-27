package container

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/containerd/errdefs"
	dcontainer "github.com/docker/docker/api/types/container"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var (
	_ runtime.LogsDriver    = (*Driver)(nil)
	_ runtime.MetricsDriver = (*Driver)(nil)
)

// SandboxLogs implements runtime.LogsDriver through the sandbox's agent.
func (d *Driver) SandboxLogs(ctx context.Context, id runtime.SandboxID, after int64, wait time.Duration) (*runtime.SandboxLogs, error) {
	return d.agent().SandboxLogs(ctx, id, after, wait)
}

// SandboxMetrics implements runtime.MetricsDriver with the container's cgroup
// counters, read by the Docker daemon. It needs no agent.
func (d *Driver) SandboxMetrics(ctx context.Context, id runtime.SandboxID) (*runtime.SandboxMetrics, error) {
	reader, err := d.cli.ContainerStatsOneShot(ctx, containerName(id))
	if errdefs.IsNotFound(err) {
		return nil, runtime.ErrSandboxNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("container stats: %w", err)
	}
	defer func() { _ = reader.Body.Close() }()
	var stats dcontainer.StatsResponse
	if err := json.NewDecoder(reader.Body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("container stats: decode: %w", err)
	}
	return metricsOf(&stats, time.Now().UTC()), nil
}

// metricsOf converts Docker's stats. Memory in use leaves out the page cache
// the kernel can drop, as `docker stats` does: "inactive_file" under cgroup v2,
// "total_inactive_file" under v1.
func metricsOf(stats *dcontainer.StatsResponse, at time.Time) *runtime.SandboxMetrics {
	used := stats.MemoryStats.Usage
	for _, key := range []string{"inactive_file", "total_inactive_file"} {
		if cache, ok := stats.MemoryStats.Stats[key]; ok && cache <= used {
			used -= cache
			break
		}
	}
	return &runtime.SandboxMetrics{
		CPUUsageNanos: stats.CPUStats.CPUUsage.TotalUsage, MemoryUsedBytes: used,
		MemoryLimitBytes: stats.MemoryStats.Limit, At: at,
	}
}

package container

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	m := metricsOf(&stats, time.Now().UTC())
	m.DiskUsedBytes, m.DiskLimitBytes = d.workspaceUse(ctx, id)
	return m, nil
}

// workspaceUseTimeout bounds the read of the workspace size, so a sandbox that
// does not answer costs the metrics a second and not the request.
const workspaceUseTimeout = 2 * time.Second

// workspaceUse asks the sandbox how full its workspace is. It answers zeros
// when the sandbox cannot run a command, which is the case while it is paused:
// the CPU and memory figures come from the daemon and are still good, so the
// metrics are returned without the disk instead of failing.
func (d *Driver) workspaceUse(ctx context.Context, id runtime.SandboxID) (used, total uint64) {
	ctx, cancel := context.WithTimeout(ctx, workspaceUseTimeout)
	defer cancel()
	res, err := d.Exec(ctx, id, runtime.ExecRequest{Cmd: []string{"df", "-Pk", defaultWorkDir}})
	if err != nil || res.ExitCode != 0 {
		return 0, 0
	}
	used, total, _ = workspaceUse(string(res.Stdout))
	return used, total
}

// workspaceUse reads the used and total bytes from the output of `df -Pk` for
// one path: a header line, then "filesystem blocks used available ...".
func workspaceUse(df string) (used, total uint64, ok bool) {
	lines := strings.Split(strings.TrimSpace(df), "\n")
	if len(lines) < 2 {
		return 0, 0, false
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 3 {
		return 0, 0, false
	}
	totalKB, errTotal := strconv.ParseUint(fields[1], 10, 64)
	usedKB, errUsed := strconv.ParseUint(fields[2], 10, 64)
	if errTotal != nil || errUsed != nil {
		return 0, 0, false
	}
	return usedKB << 10, totalKB << 10, true
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

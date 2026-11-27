package container

import (
	"testing"
	"time"

	dcontainer "github.com/docker/docker/api/types/container"
)

func TestMetricsOfLeavesTheDroppablePageCacheOutOfUsedMemory(t *testing.T) {
	cases := map[string]struct {
		stats map[string]uint64
		want  uint64
	}{
		"cgroup v2":               {map[string]uint64{"inactive_file": 300}, 700},
		"cgroup v1":               {map[string]uint64{"total_inactive_file": 200}, 800},
		"no cache figure":         {nil, 1000},
		"cache larger than usage": {map[string]uint64{"inactive_file": 5000}, 1000},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			stats := &dcontainer.StatsResponse{}
			stats.MemoryStats.Usage, stats.MemoryStats.Limit, stats.MemoryStats.Stats = 1000, 4000, c.stats
			stats.CPUStats.CPUUsage.TotalUsage = 42

			got := metricsOf(stats, time.Unix(1, 0))

			if got.MemoryUsedBytes != c.want || got.MemoryLimitBytes != 4000 || got.CPUUsageNanos != 42 {
				t.Errorf("metrics = %+v, want %d used of 4000 and 42ns", got, c.want)
			}
		})
	}
}

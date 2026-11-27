//go:build firecracker

package firecracker

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"

	rt "github.com/getloopscale/loop-sandbox/internal/runtime"
)

// samples is the number of provisioning measurements per path.
const samples = 20

// TestFirecracker_benchmark_provisioning measures create→ready latency on the cold
// path (kernel boot) and the warm path (snapshot restore). It reports p50/p95/p99 +
// mean±stddev and asserts the warm-path p95 is sub-second (M6 DoD). Honest scope:
// measured on THIS host's KVM, not a bare-metal .metal node (that validation is
// deferred to M10 / AWS); the mechanism and the reproducible harness are real.
func TestFirecracker_benchmark_provisioning(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not available")
	}
	d := testDriver(t)
	ctx := context.Background()

	// One warm-up iteration per path is discarded: the first boot/restore pays
	// one-time page-cache / asset-load costs that would skew the distribution.

	// --- cold path: full kernel boot each time ---
	cold := make([]time.Duration, 0, samples)
	for i := 0; i < samples+1; i++ {
		start := time.Now()
		v, err := d.coldBoot(ctx, sandboxID(i), "")
		if err != nil {
			t.Fatalf("coldBoot[%d]: %v", i, err)
		}
		if i > 0 { // discard warm-up
			cold = append(cold, time.Since(start))
		}
		_ = killVM(v)
	}

	// --- warm path: restore from a base snapshot ---
	snap, err := d.createSnapshot(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("createSnapshot: %v", err)
	}
	warm := make([]time.Duration, 0, samples)
	for i := 0; i < samples+1; i++ {
		start := time.Now()
		v, err := d.restore(ctx, snap, nil)
		if err != nil {
			t.Fatalf("restore[%d]: %v", i, err)
		}
		if i > 0 { // discard warm-up
			warm = append(warm, time.Since(start))
		}
		_ = killVM(v)
	}

	reportStats(t, "COLD (kernel boot)", cold)
	reportStats(t, "WARM (snapshot restore)", warm)

	warmP95 := percentile(warm, 95)
	warmP50 := percentile(warm, 50)
	t.Logf("host: %s/%s, %d CPUs, firecracker=%s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), d.cfg.FirecrackerBin)
	if warmP95 >= time.Second {
		t.Errorf("warm p95 = %s, want sub-second (M6 DoD)", warmP95)
	} else {
		t.Logf("PASS: warm-path p95 = %s (< 1s, M6 DoD met on this KVM host)", warmP95)
	}
	// M13 target: snapshot-restore provisioning p50 ≤ 150ms (E2B/Daytona-class).
	if warmP50 > 150*time.Millisecond {
		t.Errorf("warm p50 = %s, want ≤ 150ms (M13 sub-150ms provisioning target)", warmP50)
	} else {
		t.Logf("PASS: warm-path p50 = %s (≤ 150ms, M13 target met)", warmP50)
	}
}

func sandboxID(i int) rt.SandboxID { return rt.SandboxID(fmt.Sprintf("bench-%d", i)) }

func reportStats(t *testing.T, label string, ds []time.Duration) {
	t.Helper()
	mean, std := meanStd(ds)
	t.Logf("%-26s n=%d  p50=%-9s p95=%-9s p99=%-9s mean=%-9s stddev=%s",
		label, len(ds),
		percentile(ds, 50).Round(time.Millisecond),
		percentile(ds, 95).Round(time.Millisecond),
		percentile(ds, 99).Round(time.Millisecond),
		mean.Round(time.Millisecond), std.Round(time.Millisecond))
}

func percentile(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(float64(p)/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func meanStd(ds []time.Duration) (time.Duration, time.Duration) {
	if len(ds) == 0 {
		return 0, 0
	}
	var sum float64
	for _, d := range ds {
		sum += float64(d)
	}
	mean := sum / float64(len(ds))
	var varSum float64
	for _, d := range ds {
		varSum += (float64(d) - mean) * (float64(d) - mean)
	}
	return time.Duration(mean), time.Duration(math.Sqrt(varSum / float64(len(ds))))
}

//go:build firecracker

// Real Firecracker microVM DENSITY measurement on the host KVM (M11 B9). Boots N real
// microVMs, measures the actual host memory each firecracker process uses, and honestly
// extrapolates to 50k tenants + per-`.metal`-node density. The literal 50k run needs the
// AWS fleet; this measures the real per-VM footprint that the extrapolation rests on.
package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// firecrackerRSSKiB sums the resident memory (VmRSS) of every running firecracker process
// — the real host memory the microVMs occupy.
func firecrackerRSSKiB(t *testing.T) int64 {
	t.Helper()
	procs, _ := filepath.Glob("/proc/[0-9]*")
	var total int64
	for _, p := range procs {
		comm, err := os.ReadFile(filepath.Join(p, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "firecracker" {
			continue
		}
		status, err := os.ReadFile(filepath.Join(p, "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					if kib, err := strconv.ParseInt(f[1], 10, 64); err == nil {
						total += kib
					}
				}
			}
		}
	}
	return total
}

func TestFirecracker_density(t *testing.T) {
	const (
		memMiB = 128 // small sandbox guest — density-oriented
		n      = 30  // real microVMs to boot concurrently
	)
	d := testDriver(t)
	d.cfg.MemMiB = memMiB

	ctx := context.Background()
	baseline := firecrackerRSSKiB(t)

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := d.Create(ctx, runtime.CreateRequest{SandboxID: runtime.SandboxID(fmt.Sprintf("dens-%d", i)), TenantID: "t"})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	booted := 0
	for _, e := range errs {
		if e == nil {
			booted++
		}
	}
	if booted < n {
		t.Fatalf("only %d/%d microVMs booted (errs include: %v)", booted, n, firstErr(errs))
	}

	rss := firecrackerRSSKiB(t) - baseline
	perVMKiB := rss / int64(booted)
	perVMMiB := float64(perVMKiB) / 1024.0

	// Honest extrapolation. AWS m5.metal ≈ 384 GiB usable RAM (reserve 15% for host/OS).
	const metalRAMMiB = 384 * 1024
	usable := metalRAMMiB * 0.85
	perNode := int(usable / perVMMiB)
	nodesFor50k := (50000 + perNode - 1) / perNode

	t.Logf("DENSITY (real): booted %d microVMs @ %d MiB guest | measured host RSS/VM = %.1f MiB",
		booted, memMiB, perVMMiB)
	t.Logf("EXTRAPOLATION (honest, NOT a 50k run): ~%d microVMs per m5.metal (384 GiB, 85%% usable) "+
		"→ ~%d such nodes for 50k tenants. Real 50k validation still needs the AWS fleet.", perNode, nodesFor50k)

	// Sanity: per-VM host footprint must be bounded (guest mem + firecracker overhead),
	// not runaway. A microVM touching a fraction of its 128 MiB guest + ~a few MiB VMM
	// overhead should sit well under 200 MiB.
	if perVMMiB <= 0 || perVMMiB > float64(memMiB)+80 {
		t.Errorf("per-VM host RSS = %.1f MiB is outside the sane bound (0, %d]", perVMMiB, memMiB+80)
	}

	// Clean up.
	for i := 0; i < n; i++ {
		_ = d.Destroy(ctx, runtime.SandboxID(fmt.Sprintf("dens-%d", i)))
	}
}

func firstErr(errs []error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

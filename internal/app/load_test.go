//go:build load

// The load test asserts wall-clock latency SLOs under 200-way concurrency, so its
// result depends on how busy the machine is. It runs through `make load-test`, on a
// quiet machine, and stays out of the default `go test ./...` so that suite is
// deterministic.

package app_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// atomicIDs is a concurrency-safe id generator for the load harness (the seqIDs used
// elsewhere is single-goroutine).
type atomicIDs struct{ n atomic.Uint64 }

func (a *atomicIDs) New() string { return fmt.Sprintf("id-%d", a.n.Add(1)) }

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p / 100 * float64(len(sorted)-1))
	return sorted[i]
}

// TestLoad_provisioning_and_exec_SLOs drives many tenants concurrently through the FULL
// service path (quota gate → tracing → persistence → audit) and asserts latency SLOs +
// tenant fairness. It uses the fake runtime driver, so it measures OUR orchestration
// overhead and concurrency behaviour — NOT real microVM provisioning (which needs KVM /
// AWS metal at 50k scale; see docs/scale.md for the honest deferral).
func TestLoad_provisioning_and_exec_SLOs(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped in -short")
	}
	const (
		tenants      = 200
		opsPerTenant = 25
	)
	// Install a REAL TracerProvider so the tracing leg on the hot path is actually
	// exercised (not the OTel no-op) — this measures genuine span-creation overhead and
	// lets us assert every create+exec produced a span.
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	repo := memory.New()
	svc := app.NewService(repo, fake.New(), clock.System{}, &atomicIDs{}, noopAudit{}, nil, nil)

	var (
		mu        sync.Mutex
		createLat []time.Duration
		execLat   []time.Duration
		perTenant = make(map[string]int)
	)
	record := func(tenant string, create, exec time.Duration) {
		mu.Lock()
		createLat = append(createLat, create)
		execLat = append(execLat, exec)
		perTenant[tenant]++
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for tIdx := 0; tIdx < tenants; tIdx++ {
		wg.Add(1)
		go func(tIdx int) {
			defer wg.Done()
			actor := app.Actor{
				TenantID:  fmt.Sprintf("tenant-%d", tIdx),
				KeyID:     fmt.Sprintf("key-%d", tIdx),
				RequestID: fmt.Sprintf("req-%d", tIdx),
			}
			ctx := context.Background()
			for op := 0; op < opsPerTenant; op++ {
				t0 := time.Now()
				sb, err := svc.CreateSandbox(ctx, actor, "img", time.Minute)
				if err != nil {
					t.Errorf("tenant %d create: %v", tIdx, err)
					return
				}
				t1 := time.Now()
				if _, err := svc.Exec(ctx, actor, sb.ID, runtime.ExecRequest{Cmd: []string{"echo", "hi"}}); err != nil {
					t.Errorf("tenant %d exec: %v", tIdx, err)
					return
				}
				record(actor.TenantID, t1.Sub(t0), time.Since(t1))
			}
		}(tIdx)
	}
	wg.Wait()

	sort.Slice(createLat, func(i, j int) bool { return createLat[i] < createLat[j] })
	sort.Slice(execLat, func(i, j int) bool { return execLat[i] < execLat[j] })

	total := tenants * opsPerTenant
	if len(createLat) != total {
		t.Fatalf("recorded %d create samples, want %d", len(createLat), total)
	}

	createP95 := pct(createLat, 95)
	execP95 := pct(execLat, 95)
	t.Logf("N=%d ops across %d tenants | create p50=%v p95=%v | exec p50=%v p95=%v",
		total, tenants, pct(createLat, 50), createP95, pct(execLat, 50), execP95)

	// SLO: the control-plane orchestration path (quota+tracing+persist+audit) must stay
	// well under 50ms p95 even under 200-way concurrency. A regression that introduces a
	// global lock or O(n) scan would blow this.
	const sloP95 = 50 * time.Millisecond
	if createP95 > sloP95 {
		t.Errorf("create p95 = %v, exceeds SLO %v", createP95, sloP95)
	}
	if execP95 > sloP95 {
		t.Errorf("exec p95 = %v, exceeds SLO %v", execP95, sloP95)
	}

	// Fairness / no-noisy-neighbor: every tenant completed all its ops (none starved).
	for tenant, done := range perTenant {
		if done != opsPerTenant {
			t.Errorf("tenant %s completed %d/%d ops — starvation under load", tenant, done, opsPerTenant)
		}
	}
	if len(perTenant) != tenants {
		t.Errorf("only %d/%d tenants made progress", len(perTenant), tenants)
	}

	// The tracing leg is real (not no-op): at least the two root spans per op
	// (sandbox.create + sandbox.exec) — a no-op provider would record 0. The trace is a
	// tree (root + runtime/storage children), so the count exceeds 2*total.
	if got, min := len(rec.Ended()), 2*total; got < min {
		t.Errorf("recorded %d spans, want ≥ %d (≥ 2 roots/op) — tracing path not exercised", got, min)
	}
}

type noopAudit struct{}

func (noopAudit) Record(_, _, _, _, _, _ string, _ time.Time) {}

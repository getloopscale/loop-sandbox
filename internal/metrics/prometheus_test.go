package metrics_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/getloopscale/loop-sandbox/internal/metrics"
)

func TestPrometheus_counts_created_destroyed_and_execs(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	m.SandboxCreated(120 * time.Millisecond)
	m.SandboxCreated(80 * time.Millisecond)
	m.SandboxDestroyed()
	m.ExecObserved("ok")
	m.ExecObserved("ok")
	m.ExecObserved("timeout")

	if got := testutil.ToFloat64(m.Created()); got != 2 {
		t.Errorf("created_total = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.Destroyed()); got != 1 {
		t.Errorf("destroyed_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.Execs("ok")); got != 2 {
		t.Errorf("executions_total{result=ok} = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.Execs("timeout")); got != 1 {
		t.Errorf("executions_total{result=timeout} = %v, want 1", got)
	}
}

// The registry must expose the create-duration histogram (observability of latency,
// not just counts) so ops can alert on provisioning slowness (agent-critical).
func TestPrometheus_exposes_create_duration_histogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	m.SandboxCreated(50 * time.Millisecond)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, f := range families {
		if f.GetName() == "loopsandbox_sandbox_create_seconds" {
			found = true
			if f.GetMetric()[0].GetHistogram().GetSampleCount() != 1 {
				t.Errorf("histogram sample count = %d, want 1", f.GetMetric()[0].GetHistogram().GetSampleCount())
			}
		}
	}
	if !found {
		t.Error("loopsandbox_sandbox_create_seconds histogram not registered")
	}
}

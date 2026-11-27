// Package enforcement checks per-plan usage caps for sandbox metrics, reusing the
// LoopScale contracts/plan vocabulary (Plan, ExceededError, Unlimited, CurrentPeriod).
// The sandbox-specific caps live here (a local table) because the sandbox metrics
// are not yet in LoopScale contracts' capTable — adding them is a ratified cross-repo
// follow-up (M8 top risk #1). The types + the ExceededError → HTTP 402 mapping are
// the real LoopScale contract.
package enforcement

import (
	"context"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/plan"
)

// Sandbox-specific cost metrics (pending LoopScale contracts ratification).
const (
	MetricSandboxCreations  plan.CostMetric = "sandbox_creations"
	MetricSandboxExecutions plan.CostMetric = "sandbox_executions"
	// MetricVolumeStorageGB caps a tenant's total provisioned durable-volume storage (M16).
	MetricVolumeStorageGB plan.CostMetric = "volume_storage_gb"
	// MetricTemplateCreations caps how many templates a tenant may register in a period (M17).
	MetricTemplateCreations plan.CostMetric = "template_creations"
	// MetricGPUSandboxCreations caps GPU-backed sandbox creations (M19). GPU is a premium class:
	// lower tiers are capped at 0 (no GPU) so a GPU request is denied before scheduling.
	MetricGPUSandboxCreations plan.CostMetric = "gpu_sandbox_creations"
)

// sandboxCaps is the per-plan, per-metric cap table for a billing period. A plan
// absent from the table (Enterprise) or a metric absent for a plan is Unlimited.
// Values are placeholders pending billing ratification (documented in the plan).
var sandboxCaps = map[plan.Plan]map[plan.CostMetric]int64{
	plan.PlanStarter: {MetricSandboxCreations: 50, MetricSandboxExecutions: 500, MetricVolumeStorageGB: 10, MetricTemplateCreations: 20, MetricGPUSandboxCreations: 0},
	plan.PlanPro:     {MetricSandboxCreations: 500, MetricSandboxExecutions: 10_000, MetricVolumeStorageGB: 100, MetricTemplateCreations: 200, MetricGPUSandboxCreations: 20},
	plan.PlanTeam:    {MetricSandboxCreations: 5_000, MetricSandboxExecutions: 100_000, MetricVolumeStorageGB: 1_000, MetricTemplateCreations: 2_000, MetricGPUSandboxCreations: 200},
	// plan.PlanEnterprise omitted → Unlimited.
}

// CapFor returns the sandbox cap for (tier, metric), or plan.Unlimited when uncapped.
func CapFor(tier plan.Plan, metric plan.CostMetric) int64 {
	if caps, ok := sandboxCaps[tier]; ok {
		if c, ok := caps[metric]; ok {
			return c
		}
	}
	return plan.Unlimited
}

// UsageReader reads a tenant's accumulated usage for a metric in the period.
type UsageReader interface {
	Value(ctx context.Context, tenantID, metric string, periodStart int64) (int64, error)
}

// Enforcer decides whether an operation is within the tenant's plan caps.
type Enforcer struct {
	usage UsageReader
}

// New builds an Enforcer over a usage reader.
func New(usage UsageReader) *Enforcer { return &Enforcer{usage: usage} }

// Check returns a *plan.ExceededError (wrapping plan.ErrCostCapExceeded, mapped to
// HTTP 402 upstream) when charging `amount` of `metric` would exceed the tenant's
// cap for the current period. Unlimited plans/metrics always pass. A usage-read
// infra error does NOT block the operation (best-effort quota — a transient DB blip
// must not 402 a legitimate request); it returns nil.
func (e *Enforcer) Check(ctx context.Context, tenantID string, tier plan.Plan, metric plan.CostMetric, amount int64, now time.Time) error {
	cap := CapFor(tier, metric)
	if cap == plan.Unlimited {
		return nil
	}
	start, _ := plan.CurrentPeriod(now)
	current, err := e.usage.Value(ctx, tenantID, string(metric), start)
	if err != nil {
		return nil // best-effort: never fail a legit op on a usage-read error
	}
	if current+amount > cap {
		return &plan.ExceededError{
			TenantID: tenantID,
			Plan:     tier,
			Metric:   metric,
			Cap:      cap,
			Current:  current,
			Period:   now.UTC().Format("2006-01"),
		}
	}
	return nil
}

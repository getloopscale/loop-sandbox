package enforcement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/plan"

	"github.com/getloopscale/loop-sandbox/internal/enforcement"
)

// fakeUsage returns a fixed current usage (and optional error).
type fakeUsage struct {
	value int64
	err   error
}

func (f fakeUsage) Value(context.Context, string, string, int64) (int64, error) {
	return f.value, f.err
}

var now = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func TestCheck_under_cap_passes(t *testing.T) {
	e := enforcement.New(fakeUsage{value: 49})
	if err := e.Check(context.Background(), "t", plan.PlanStarter, enforcement.MetricSandboxCreations, 1, now); err != nil {
		t.Errorf("at 49+1 vs cap 50 → %v, want nil", err)
	}
}

func TestCheck_over_cap_returns_ExceededError_402(t *testing.T) {
	e := enforcement.New(fakeUsage{value: 50}) // starter cap = 50
	err := e.Check(context.Background(), "t", plan.PlanStarter, enforcement.MetricSandboxCreations, 1, now)
	if !errors.Is(err, plan.ErrCostCapExceeded) {
		t.Fatalf("err = %v, want ErrCostCapExceeded (→ HTTP 402)", err)
	}
	var ex *plan.ExceededError
	if !errors.As(err, &ex) {
		t.Fatalf("err is not *plan.ExceededError: %v", err)
	}
	if ex.Plan != plan.PlanStarter || ex.Cap != 50 || ex.Current != 50 || ex.Metric != enforcement.MetricSandboxCreations {
		t.Errorf("ExceededError = %+v, want starter/cap50/current50", ex)
	}
	if ex.Period != "2026-07" {
		t.Errorf("period = %q, want 2026-07", ex.Period)
	}
}

func TestCheck_per_plan_caps(t *testing.T) {
	for _, tc := range []struct {
		tier    plan.Plan
		current int64
		amount  int64
		blocked bool
	}{
		{plan.PlanStarter, 500, 1, true},         // starter exec cap 500
		{plan.PlanPro, 500, 1, false},            // pro exec cap 10k
		{plan.PlanPro, 10_000, 1, true},          // pro at cap
		{plan.PlanTeam, 50_000, 1, false},        // team exec cap 100k
		{plan.PlanEnterprise, 1 << 40, 1, false}, // enterprise unlimited
	} {
		e := enforcement.New(fakeUsage{value: tc.current})
		err := e.Check(context.Background(), "t", tc.tier, enforcement.MetricSandboxExecutions, tc.amount, now)
		if tc.blocked && err == nil {
			t.Errorf("%s at %d → nil, want blocked", tc.tier, tc.current)
		}
		if !tc.blocked && err != nil {
			t.Errorf("%s at %d → %v, want allowed", tc.tier, tc.current, err)
		}
	}
}

func TestCheck_enterprise_is_unlimited(t *testing.T) {
	e := enforcement.New(fakeUsage{value: 1 << 50})
	if err := e.Check(context.Background(), "t", plan.PlanEnterprise, enforcement.MetricSandboxCreations, 1_000_000, now); err != nil {
		t.Errorf("enterprise → %v, want nil (unlimited)", err)
	}
}

func TestCheck_usage_read_error_is_best_effort_allow(t *testing.T) {
	e := enforcement.New(fakeUsage{value: 0, err: errors.New("db down")})
	// A usage-read failure must NOT 402 a legitimate request.
	if err := e.Check(context.Background(), "t", plan.PlanStarter, enforcement.MetricSandboxCreations, 1, now); err != nil {
		t.Errorf("read error → %v, want nil (best-effort allow)", err)
	}
}

func TestCapFor_unknown_metric_is_unlimited(t *testing.T) {
	if c := enforcement.CapFor(plan.PlanStarter, plan.MetricEgressBytes); c != plan.Unlimited {
		t.Errorf("unknown sandbox metric cap = %d, want Unlimited", c)
	}
}

func TestCheck_zero_amount_at_exact_cap_passes(t *testing.T) {
	// A zero-cost probe at exactly the cap must NOT be denied (current+0 > cap is false).
	e := enforcement.New(fakeUsage{value: 50}) // starter cap = 50
	if err := e.Check(context.Background(), "t", plan.PlanStarter, enforcement.MetricSandboxCreations, 0, now); err != nil {
		t.Errorf("zero amount at cap → %v, want nil", err)
	}
}

func TestCheck_period_label_matches_month(t *testing.T) {
	e := enforcement.New(fakeUsage{value: 50})
	dec := time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	err := e.Check(context.Background(), "t", plan.PlanStarter, enforcement.MetricSandboxCreations, 1, dec)
	var ex *plan.ExceededError
	if errors.As(err, &ex) && ex.Period != "2026-12" {
		t.Errorf("period = %q, want 2026-12", ex.Period)
	}
}

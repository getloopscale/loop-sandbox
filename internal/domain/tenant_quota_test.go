package domain_test

import (
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

func TestNewTenant_defaults_to_active_starter(t *testing.T) {
	tn, err := domain.NewTenant("t1", "acme", "", t0)
	if err != nil {
		t.Fatalf("NewTenant: %v", err)
	}
	if !tn.Active() {
		t.Error("new tenant should be active")
	}
	if tn.Plan != domain.PlanStarter {
		t.Errorf("plan = %q, want starter default", tn.Plan)
	}
	if tn.CellID != "cell-0" {
		t.Errorf("cellID = %q, want cell-0", tn.CellID)
	}
}

func TestNewTenant_rejects_empty_name(t *testing.T) {
	if _, err := domain.NewTenant("t1", "", "", t0); err != domain.ErrInvalidArgument {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestTenant_suspended_is_not_active(t *testing.T) {
	tn, _ := domain.NewTenant("t1", "acme", domain.PlanPro, t0)
	tn.Status = domain.TenantSuspended
	if tn.Active() {
		t.Error("suspended tenant must not be active")
	}
}

func TestCurrentPeriod_is_utc_month(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	start, end := domain.CurrentPeriod(now)
	wantStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Unix()
	wantEnd := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix()
	if start != wantStart || end != wantEnd {
		t.Errorf("period = [%d,%d), want [%d,%d)", start, end, wantStart, wantEnd)
	}
}

func TestCostCounter_add(t *testing.T) {
	c := &domain.CostCounter{TenantID: "t1", Metric: domain.MetricSandboxCreations}
	c.Add(1)
	c.Add(2)
	if c.Value != 3 {
		t.Errorf("value = %d, want 3", c.Value)
	}
}

func TestNewAuditEvent_requires_tenant_and_action(t *testing.T) {
	if _, err := domain.NewAuditEvent("a1", "", "k1", "sandbox.create", "sandbox", "s1", "r1", t0); err != domain.ErrInvalidArgument {
		t.Errorf("missing tenant: err = %v, want ErrInvalidArgument", err)
	}
	ev, err := domain.NewAuditEvent("a1", "t1", "k1", "sandbox.create", "sandbox", "s1", "r1", t0)
	if err != nil {
		t.Fatalf("NewAuditEvent: %v", err)
	}
	if ev.ActorType != "api_key" {
		t.Errorf("actorType = %q, want api_key", ev.ActorType)
	}
}

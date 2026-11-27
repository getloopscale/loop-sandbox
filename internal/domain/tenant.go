package domain

import "time"

// TenantStatus is the lifecycle status of a tenant.
type TenantStatus string

const (
	TenantActive    TenantStatus = "active"
	TenantSuspended TenantStatus = "suspended"
)

// Plan is the tenant's plan tier. In M1 it is a local string; M7 maps it onto
// LoopScale contracts/plan.Plan (starter/pro/team/enterprise) for quota enforcement.
type Plan string

const (
	PlanStarter Plan = "starter"
	PlanPro     Plan = "pro"
	PlanTeam    Plan = "team"
)

// ValidPlan reports whether plan is one a tenant can be created on.
func ValidPlan(plan Plan) bool {
	return plan == PlanStarter || plan == PlanPro || plan == PlanTeam
}

// Tenant is the ownership boundary for every resource (US-05).
type Tenant struct {
	ID        string
	Name      string
	Plan      Plan
	Status    TenantStatus
	CellID    string
	CreatedAt time.Time
}

// NewTenant constructs an active tenant. Fail-fast on invalid input.
func NewTenant(id, name string, plan Plan, now time.Time) (*Tenant, error) {
	if id == "" || name == "" {
		return nil, ErrInvalidArgument
	}
	if plan == "" {
		plan = PlanStarter
	}
	return &Tenant{
		ID:        id,
		Name:      name,
		Plan:      plan,
		Status:    TenantActive,
		CellID:    "cell-0",
		CreatedAt: now,
	}, nil
}

// Active reports whether the tenant may perform operations.
func (t *Tenant) Active() bool { return t.Status == TenantActive }

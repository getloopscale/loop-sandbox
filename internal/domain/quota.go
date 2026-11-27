package domain

import "time"

// CostMetric is a metered quantity. M1 defines local names; M7 maps them onto
// LoopScale contracts/plan.CostMetric and ratifies the sandbox-specific ones with the
// contract owners (see ROADMAP Unresolved).
type CostMetric string

const (
	MetricSandboxCreations CostMetric = "sandbox_creations"
	MetricSandboxCPUSecond CostMetric = "sandbox_cpu_core_seconds"
	// MetricVolumeStorageGB is a live gauge of provisioned durable-volume storage (M16):
	// incremented by a volume's size on create, decremented on delete.
	MetricVolumeStorageGB CostMetric = "volume_storage_gb"
	// MetricSandboxPauses counts pause operations (M16).
	MetricSandboxPauses CostMetric = "sandbox_pauses"
	// MetricTemplateCreations counts template registrations (M17).
	MetricTemplateCreations CostMetric = "template_creations"
	// MetricGPUSandboxCreations counts GPU-backed sandbox creations (M19, premium tier).
	MetricGPUSandboxCreations CostMetric = "gpu_sandbox_creations"
)

// CostCounter is a per-tenant, per-metric usage counter for a billing period.
// It is the local quota foundation that M7 swaps for LoopScale enforcement.CapChecker
// without reshaping callers.
type CostCounter struct {
	TenantID    string
	Metric      CostMetric
	PeriodStart int64 // unix seconds (UTC month start)
	PeriodEnd   int64
	Value       int64
}

// CurrentPeriod returns the UTC calendar-month boundaries for now, mirroring
// LoopScale contracts/plan.CurrentPeriod so the M7 migration is mechanical.
func CurrentPeriod(now time.Time) (start, end int64) {
	u := now.UTC()
	s := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	e := s.AddDate(0, 1, 0)
	return s.Unix(), e.Unix()
}

// Add increments the counter value.
func (c *CostCounter) Add(n int64) { c.Value += n }

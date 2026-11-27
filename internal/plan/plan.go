// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 The LoopScale Authors

// Package plan holds the cost/plan VALUE VOCABULARY shared across the engine
// and cloud surfaces — plan tiers, cost metrics, the per-plan cap table, the
// cost-counter projection, and the cost-cap exceeded error. It is the
// contracts-bound surface (M3 target `LoopScale contracts/plan`) and MUST import
// nothing engine-internal (zero deps on circuitbreaker / enforcement /
// statemachine). Cap-enforcement LOGIC (CapChecker, CostStore, PlanLimiter,
// middleware) stays in pkg/enforcement (ISP — plan m1-decouple-in-place D2).
package plan

import (
	"errors"
	"fmt"
	"time"
)

// CostMetric identifies the kind of cost being tracked.
type CostMetric string

// Canonical cost metrics. Schema check constraint in migration_000017 pins
// these — adding a new metric requires both the constant + the SQL CHECK.
const (
	MetricEgressBytes  CostMetric = "egress_bytes"
	MetricDOCRPulls    CostMetric = "docr_pulls"
	MetricBuildMinutes CostMetric = "build_minutes"
	// Compute metrics (observe-only; plan metering-compute-loop T0.1).
	// Additive minor toward the v0.3.0 publish. The schema CHECK is pinned in
	// loop-runtime, api/internal/db/migrations/000046.
	MetricCPUCoreSeconds CostMetric = "cpu_core_seconds"
	MetricMemGBSeconds   CostMetric = "mem_gb_seconds"
)

// Plan identifies a subscription tier. Mirrors subscriptions.plan column.
type Plan string

// Canonical plan tiers. LoopScale's pricing per MEET-2026-05-12 D4.
const (
	PlanStarter    Plan = "starter"
	PlanPro        Plan = "pro"
	PlanTeam       Plan = "team"
	PlanEnterprise Plan = "enterprise"
)

// Unlimited is the sentinel value for "no cap" on a metric. Returned for
// Enterprise tier and for metrics not yet gated on a given plan.
const Unlimited int64 = -1

// capTable maps (plan, metric) → cap. Values are integers in metric units
// (egress_bytes: bytes/period, docr_pulls: count/period, build_minutes:
// minutes/period). Period is calendar month (UTC) — see CurrentPeriod().
//
// To raise a cap or add a metric, update both this table, CLAUDE.md
// § "Plan caps", and cli/src/commands/billing/usage.ts.
var capTable = map[Plan]map[CostMetric]int64{
	PlanStarter: {
		MetricEgressBytes:  100 * 1024 * 1024 * 1024, // 100 GB
		MetricDOCRPulls:    1_000,                    // 1k
		MetricBuildMinutes: 100,                      // 100 min
	},
	PlanPro: {
		MetricEgressBytes:  1024 * 1024 * 1024 * 1024, // 1 TB
		MetricDOCRPulls:    10_000,                    // 10k
		MetricBuildMinutes: 1_000,                     // 1000 min
	},
	PlanTeam: {
		MetricEgressBytes:  10 * 1024 * 1024 * 1024 * 1024, // 10 TB
		MetricDOCRPulls:    100_000,                        // 100k
		MetricBuildMinutes: 10_000,                         // 10000 min
	},
	PlanEnterprise: {
		MetricEgressBytes:  Unlimited,
		MetricDOCRPulls:    Unlimited,
		MetricBuildMinutes: Unlimited,
	},
}

// CapFor returns the cap for (plan, metric). Returns Unlimited (-1) if the
// plan or metric is unknown — fail-open is the safer default during rollout.
func CapFor(p Plan, metric CostMetric) int64 {
	planCaps, ok := capTable[p]
	if !ok {
		return Unlimited
	}
	c, ok := planCaps[metric]
	if !ok {
		return Unlimited
	}
	return c
}

// CostCounter is the read-side projection of the cost_counters row used by
// enforcement decisions.
type CostCounter struct {
	TenantID    string
	Plan        Plan
	Metric      CostMetric
	PeriodStart int64 // unix seconds
	PeriodEnd   int64
	Value       int64
}

// ErrCostCapExceeded is the sentinel returned when a tenant's counter has
// reached or exceeded its cap for the period.
var ErrCostCapExceeded = errors.New("cost cap exceeded")

// ExceededError carries the rich context expected by HTTP 402 responses.
// Wraps ErrCostCapExceeded so errors.Is works.
type ExceededError struct {
	TenantID string
	Plan     Plan
	Metric   CostMetric
	Cap      int64
	Current  int64
	Period   string // "2026-05" — calendar month, UTC
}

// Error implements error.
func (e *ExceededError) Error() string {
	return fmt.Sprintf("cost cap exceeded: tenant=%s plan=%s metric=%s cap=%d current=%d period=%s",
		e.TenantID, e.Plan, e.Metric, e.Cap, e.Current, e.Period)
}

// Unwrap returns the sentinel so errors.Is(err, ErrCostCapExceeded) holds.
func (e *ExceededError) Unwrap() error { return ErrCostCapExceeded }

// CurrentPeriod returns the start and end of the current calendar month (UTC),
// as unix seconds. Period boundary is minute 0 of day 1.
func CurrentPeriod(now time.Time) (start, end int64) {
	utc := now.UTC()
	startT := time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	endT := startT.AddDate(0, 1, 0)
	return startT.Unix(), endT.Unix()
}

// Package metrics is the Prometheus adapter for the app.Observer port. It is the
// only place that imports the Prometheus client — the domain and application layers
// depend on the small Observer interface, never on this concrete (DIP).
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus implements app.Observer by incrementing Prometheus collectors.
type Prometheus struct {
	created       prometheus.Counter
	destroyed     prometheus.Counter
	execs         *prometheus.CounterVec
	createDur     prometheus.Histogram
	quotaDenied   *prometheus.CounterVec
	quotaFailOpen *prometheus.CounterVec
	cellRouted    *prometheus.CounterVec
	artifactURLs  prometheus.Counter
}

// New registers the LoopSandbox collectors on reg and returns the observer.
// A dedicated registry (not the global default) is passed in by the composition
// root so tests get an isolated registry and there is no hidden global state.
func New(reg prometheus.Registerer) *Prometheus {
	p := &Prometheus{
		created: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopsandbox_sandboxes_created_total",
			Help: "Total sandboxes successfully created.",
		}),
		destroyed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopsandbox_sandboxes_destroyed_total",
			Help: "Total sandboxes destroyed (explicit destroy).",
		}),
		execs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loopsandbox_executions_total",
			Help: "Total command executions by result (ok|nonzero_exit|error|timeout). " +
				"nonzero_exit is the user's program exiting non-zero (US-02); error is a platform/runtime failure.",
		}, []string{"result"}),
		createDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "loopsandbox_sandbox_create_seconds",
			Help:    "Sandbox provisioning latency in seconds (agent-critical).",
			Buckets: prometheus.DefBuckets,
		}),
		quotaDenied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loopsandbox_quota_denied_total",
			Help: "Total operations denied by plan-quota enforcement, by metric.",
		}, []string{"metric"}),
		quotaFailOpen: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loopsandbox_quota_check_failopen_total",
			Help: "Total quota checks that could not run and allowed (fail-open), by metric.",
		}, []string{"metric"}),
		cellRouted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loopsandbox_cell_routed_total",
			Help: "Total tenant→cell routing decisions, by cell (M11 sharding distribution).",
		}, []string{"cell"}),
		artifactURLs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loopsandbox_artifact_urls_issued_total",
			Help: "Total pre-signed artifact download URLs issued (M11 FileTransport).",
		}),
	}
	reg.MustRegister(p.created, p.destroyed, p.execs, p.createDur, p.quotaDenied, p.quotaFailOpen, p.cellRouted, p.artifactURLs)
	return p
}

// SandboxCreated records one successful create and its provisioning latency.
func (p *Prometheus) SandboxCreated(d time.Duration) {
	p.created.Inc()
	p.createDur.Observe(d.Seconds())
}

// SandboxDestroyed records one explicit destroy.
func (p *Prometheus) SandboxDestroyed() { p.destroyed.Inc() }

// ExecObserved records one execution outcome by result label.
func (p *Prometheus) ExecObserved(result string) { p.execs.WithLabelValues(result).Inc() }

// QuotaDenied records one plan-quota denial by metric.
func (p *Prometheus) QuotaDenied(metric string) { p.quotaDenied.WithLabelValues(metric).Inc() }

// QuotaFailOpen records one quota check that could not run and allowed (fail-open),
// so ops can alert when the fail-open rate spikes (a silent-bypass signal).
func (p *Prometheus) QuotaFailOpen(metric string) { p.quotaFailOpen.WithLabelValues(metric).Inc() }

// CellRouted records one tenant→cell routing decision, so ops can see the live
// distribution of tenants across cells (M11 sharding).
func (p *Prometheus) CellRouted(cell string) { p.cellRouted.WithLabelValues(cell).Inc() }

// ArtifactURLIssued records one pre-signed artifact URL minted (M11 FileTransport).
func (p *Prometheus) ArtifactURLIssued() { p.artifactURLs.Inc() }

// Created / Destroyed / Execs expose collectors for assertion in tests.
func (p *Prometheus) Created() prometheus.Counter   { return p.created }
func (p *Prometheus) Destroyed() prometheus.Counter { return p.destroyed }
func (p *Prometheus) Execs(result string) prometheus.Counter {
	return p.execs.WithLabelValues(result)
}

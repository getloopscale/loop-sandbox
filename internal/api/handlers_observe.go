package api

import (
	"net/http"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
)

type logEntryResponse struct {
	PID    int    `json:"pid"`
	Stream string `json:"stream"`
	Data   string `json:"data"`
	At     string `json:"at"`
}

type sandboxLogsResponse struct {
	Entries   []logEntryResponse `json:"entries"`
	Next      int64              `json:"next"`
	Truncated bool               `json:"truncated"`
}

// SandboxLogs handles GET /v1/sandboxes/{id}/logs.
func (h *Handler) SandboxLogs(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	after, ok := queryInt(w, r, "after")
	if !ok {
		return
	}
	waitMS, ok := queryInt(w, r, "wait_ms")
	if !ok {
		return
	}
	// Bounded before the multiplication, so a huge value cannot overflow into a
	// valid duration.
	if waitMS > app.MaxLogsWait.Milliseconds() {
		waitMS = app.MaxLogsWait.Milliseconds() + 1
	}
	logs, err := h.svc.SandboxLogs(r.Context(), actor, r.PathValue("id"), after, time.Duration(waitMS)*time.Millisecond)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	out := sandboxLogsResponse{Next: logs.Next, Truncated: logs.Truncated, Entries: make([]logEntryResponse, 0, len(logs.Entries))}
	for _, e := range logs.Entries {
		out.Entries = append(out.Entries, logEntryResponse{PID: e.PID, Stream: e.Stream, Data: string(e.Data), At: e.At.Format(time.RFC3339Nano)})
	}
	writeJSON(w, http.StatusOK, out)
}

type sandboxMetricsResponse struct {
	CPUUsageNanos    uint64 `json:"cpu_usage_ns"`
	MemoryUsedBytes  uint64 `json:"memory_used_bytes"`
	MemoryLimitBytes uint64 `json:"memory_limit_bytes"`
	At               string `json:"at"`
}

// SandboxMetrics handles GET /v1/sandboxes/{id}/metrics.
func (h *Handler) SandboxMetrics(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	m, err := h.svc.SandboxMetrics(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxMetricsResponse{
		CPUUsageNanos: m.CPUUsageNanos, MemoryUsedBytes: m.MemoryUsedBytes,
		MemoryLimitBytes: m.MemoryLimitBytes, At: m.At.Format(time.RFC3339Nano),
	})
}

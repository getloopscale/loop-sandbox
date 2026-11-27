package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// observedDriver is a driver that answers logs and metrics from fixed data.
type observedDriver struct {
	runtime.Driver
	wait time.Duration
}

func (d *observedDriver) SandboxLogs(_ context.Context, _ runtime.SandboxID, after int64, wait time.Duration) (*runtime.SandboxLogs, error) {
	d.wait = wait
	return &runtime.SandboxLogs{Next: after + 4, Truncated: true, Entries: []runtime.LogEntry{
		{PID: 7, Stream: "stderr", Data: []byte("oops"), At: epoch},
	}}, nil
}

func (d *observedDriver) SandboxMetrics(context.Context, runtime.SandboxID) (*runtime.SandboxMetrics, error) {
	return &runtime.SandboxMetrics{CPUUsageNanos: 1500, MemoryUsedBytes: 2 << 20, MemoryLimitBytes: 512 << 20, At: epoch}, nil
}

func TestSandboxLogsReturnsEntriesAndTheCursor(t *testing.T) {
	d := &observedDriver{Driver: fake.New()}
	srv := processServer(d)
	sb := createWith(t, srv, "key-a", `{}`)

	rec := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/logs?after=10&wait_ms=100", "key-a", "")

	body := rec.Body.String()
	for _, want := range []string{`"pid":7`, `"stream":"stderr"`, `"data":"oops"`, `"next":14`, `"truncated":true`, `"at":"2023-11-14T22:13:20Z"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s lacks %s", body, want)
		}
	}
	if rec.Code != http.StatusOK || d.wait != 100*time.Millisecond {
		t.Errorf("status = %d, wait given to the driver = %s", rec.Code, d.wait)
	}
}

func TestSandboxLogsRejectsInvalidCursorsAndWaits(t *testing.T) {
	srv := processServer(&observedDriver{Driver: fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)
	for _, query := range []string{"after=-1", "after=x", "wait_ms=30001", "wait_ms=-5", "wait_ms=99999999999999999"} {
		t.Run(query, func(t *testing.T) {
			rec := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/logs?"+query, "key-a", "")

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSandboxMetricsReturnsCPUAndMemory(t *testing.T) {
	srv := processServer(&observedDriver{Driver: fake.New()})
	sb := createWith(t, srv, "key-a", `{}`)

	rec := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/metrics", "key-a", "")

	body := rec.Body.String()
	for _, want := range []string{`"cpu_usage_ns":1500`, `"memory_used_bytes":2097152`, `"memory_limit_bytes":536870912`, `"at":"2023-11-14T22:13:20Z"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s lacks %s", body, want)
		}
	}
}

func TestLogsAndMetricsAccessRules(t *testing.T) {
	observed := processServer(&observedDriver{Driver: fake.New()})
	plain := processServer(plainDriver{fake.New()})
	cases := map[string]struct {
		srv  http.Handler
		key  string
		want int
	}{
		"another tenant":               {observed, "key-b", http.StatusNotFound},
		"without the scope":            {observed, "key-noscope", http.StatusForbidden},
		"without credentials":          {observed, "", http.StatusUnauthorized},
		"a driver without the feature": {plain, "key-a", http.StatusNotImplemented},
	}
	for name, c := range cases {
		for _, route := range []string{"/logs", "/metrics"} {
			t.Run(name+route, func(t *testing.T) {
				sb := createWith(t, c.srv, "key-a", `{}`)

				rec := call(c.srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+route, c.key, "")

				if rec.Code != c.want {
					t.Errorf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
				}
			})
		}
	}
}

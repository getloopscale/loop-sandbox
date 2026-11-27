package tracing_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// collector is an in-process OTLP/HTTP trace receiver that records what arrives.
type collector struct {
	mu       sync.Mutex
	spans    map[string]string // span name -> service.name of its resource
	traceIDs map[string]string // span name -> hex trace id
}

func startCollector(t *testing.T) (*collector, string) {
	t.Helper()
	c := &collector{spans: map[string]string{}, traceIDs: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req collectorpb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, rs := range req.GetResourceSpans() {
			service := ""
			for _, kv := range rs.GetResource().GetAttributes() {
				if kv.GetKey() == "service.name" {
					service = kv.GetValue().GetStringValue()
				}
			}
			for _, ss := range rs.GetScopeSpans() {
				for _, span := range ss.GetSpans() {
					c.spans[span.GetName()] = service
					c.traceIDs[span.GetName()] = trace.TraceID(span.GetTraceId()).String()
				}
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func (c *collector) service(span string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.spans[span]
	return s, ok
}

// resetGlobalTracer keeps one test's provider from leaking into the next.
func resetGlobalTracer(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
}

func TestSetupExportsSpansToTheConfiguredCollector(t *testing.T) {
	resetGlobalTracer(t)
	received, endpoint := startCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	enabled, shutdown, err := tracing.Setup(context.Background(), "loopsandbox-api")
	if err != nil || !enabled {
		t.Fatalf("Setup = enabled %v, err %v; want enabled", enabled, err)
	}

	_, end := tracing.Start(context.Background(), "sandbox.create", "tenant-1", "")
	end()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown (flush): %v", err)
	}

	service, ok := received.service("sandbox.create")
	if !ok {
		t.Fatal("the collector did not receive the sandbox.create span")
	}
	if service != "loopsandbox-api" {
		t.Errorf("service.name = %q, want loopsandbox-api", service)
	}
}

func TestSetupWithoutAnEndpointLeavesExportOff(t *testing.T) {
	resetGlobalTracer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	enabled, _, err := tracing.Setup(context.Background(), "loopsandbox-api")

	if err != nil || enabled {
		t.Errorf("Setup = enabled %v, err %v; want disabled and no error", enabled, err)
	}
}

func TestSetupHonoursTheSDKDisabledSwitch(t *testing.T) {
	resetGlobalTracer(t)
	_, endpoint := startCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_SDK_DISABLED", "true")

	enabled, _, err := tracing.Setup(context.Background(), "loopsandbox-api")

	if err != nil || enabled {
		t.Errorf("Setup = enabled %v, err %v; want disabled and no error", enabled, err)
	}
}

func TestSetupRejectsAProtocolItCannotSpeak(t *testing.T) {
	resetGlobalTracer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")

	enabled, _, err := tracing.Setup(context.Background(), "loopsandbox-api")

	if err == nil || enabled {
		t.Errorf("Setup = enabled %v, err %v; want an error for the grpc protocol", enabled, err)
	}
}

func TestExtractJoinsTheTraceOfTheCaller(t *testing.T) {
	resetGlobalTracer(t)
	received, endpoint := startCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	_, shutdown, err := tracing.Setup(context.Background(), "loopsandbox-api")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	const callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	header := http.Header{"Traceparent": []string{"00-" + callerTrace + "-00f067aa0ba902b7-01"}}

	ctx := tracing.Extract(context.Background(), header)
	_, end := tracing.Start(ctx, "sandbox.get", "tenant-1", "sbx-1")
	end()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown (flush): %v", err)
	}

	received.mu.Lock()
	got := received.traceIDs["sandbox.get"]
	received.mu.Unlock()
	if got != callerTrace {
		t.Errorf("span trace id = %q, want the caller's %q", got, callerTrace)
	}
}

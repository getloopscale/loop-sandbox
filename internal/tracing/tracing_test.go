package tracing_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// TestStart_records_span_with_redacted_attributes proves the observability seam emits a
// real span carrying the operation + tenant/sandbox ids, and NOTHING resembling a secret.
func TestStart_records_span_with_redacted_attributes(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, end := tracing.Start(context.Background(), "sandbox.create", "tenant-42", "sbx-7")
	end()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "sandbox.create" {
		t.Errorf("span name = %q, want sandbox.create", span.Name())
	}

	attrs := map[string]string{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["tenant.id"] != "tenant-42" {
		t.Errorf("tenant.id = %q, want tenant-42", attrs["tenant.id"])
	}
	if attrs["sandbox.id"] != "sbx-7" {
		t.Errorf("sandbox.id = %q, want sbx-7", attrs["sandbox.id"])
	}
	// Redaction invariant: no attribute key or value may look like a credential.
	for k, v := range attrs {
		for _, banned := range []string{"secret", "token", "password", "api_key", "apikey", "credential"} {
			if containsFold(k, banned) || containsFold(v, banned) {
				t.Errorf("span attribute %q=%q leaks a credential-shaped value (%q)", k, v, banned)
			}
		}
	}
}

// TestStart_omits_empty_sandbox_id: on create there is no sandbox yet, so the attribute
// must be absent rather than empty-valued noise.
func TestStart_omits_empty_sandbox_id(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, end := tracing.Start(context.Background(), "sandbox.create", "tenant-1", "")
	end()

	for _, kv := range rec.Ended()[0].Attributes() {
		if string(kv.Key) == "sandbox.id" {
			t.Errorf("sandbox.id attribute present on create, want omitted")
		}
	}
}

// TestSetCell_tags_active_span proves the sharding-observability helper attaches the
// cell to the current span.
func TestSetCell_tags_active_span(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, end := tracing.Start(context.Background(), "sandbox.create", "tenant-9", "")
	tracing.SetCell(ctx, "cell-b")
	end()

	var cell string
	for _, kv := range rec.Ended()[0].Attributes() {
		if string(kv.Key) == "cell.id" {
			cell = kv.Value.AsString()
		}
	}
	if cell != "cell-b" {
		t.Errorf("cell.id = %q, want cell-b", cell)
	}
}

// Negative/edge: an empty tenant id must be omitted, not emitted as empty noise.
func TestStart_omits_empty_tenant_id(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, end := tracing.Start(context.Background(), "op", "", "")
	end()
	for _, kv := range rec.Ended()[0].Attributes() {
		if string(kv.Key) == "tenant.id" {
			t.Errorf("tenant.id attribute present when empty, want omitted")
		}
	}
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

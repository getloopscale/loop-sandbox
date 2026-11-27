// Package tracing is the OpenTelemetry instrumentation seam (M11 observability). It
// exposes a tiny wrapper so the application layer can open spans without importing the
// OTel surface everywhere. It attaches only non-secret identifiers (tenant id, sandbox
// id, cell) — it does NOT redact arbitrary data, so callers MUST NOT add secret
// attributes to the returned span.
package tracing

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "github.com/getloopscale/loop-sandbox"

// Start begins a span for op, attaching the non-secret tenant + sandbox identifiers.
// The returned end() closes the span. Empty ids are omitted (no empty-attribute noise).
func Start(ctx context.Context, op, tenantID, sandboxID string) (context.Context, func()) {
	var attrs []attribute.KeyValue
	if tenantID != "" {
		attrs = append(attrs, attribute.String("tenant.id", tenantID))
	}
	if sandboxID != "" {
		attrs = append(attrs, attribute.String("sandbox.id", sandboxID))
	}
	ctx, span := otel.Tracer(scopeName).Start(ctx, op, trace.WithAttributes(attrs...))
	return ctx, func() { span.End() }
}

// Child opens a nested span under the current span in ctx for an internal boundary
// (runtime driver, storage, db), so a request's trace is a tree — not a single span.
// The returned end() closes it. Attributes are inherited from the parent via context.
func Child(ctx context.Context, op string) (context.Context, func()) {
	ctx, span := otel.Tracer(scopeName).Start(ctx, op)
	return ctx, func() { span.End() }
}

// SetCell tags the current span with the cell a tenant routed to (M11 sharding
// observability). No-op when there is no active span in ctx.
func SetCell(ctx context.Context, cell string) {
	if cell == "" {
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("cell.id", cell))
}

// Extract returns ctx carrying the trace context of an incoming request (the W3C
// traceparent header), so the spans of this request join the caller's trace. A
// request with no trace context returns ctx unchanged.
func Extract(ctx context.Context, header http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(header))
}

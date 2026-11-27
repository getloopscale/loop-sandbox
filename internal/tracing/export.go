package tracing

import (
	"context"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Setup installs the W3C trace-context propagator and, when an OTLP endpoint is
// configured, a tracer provider that exports spans to it. It returns whether
// export is on and a shutdown function that flushes buffered spans; call it before
// the process exits.
//
// Configuration is the standard OpenTelemetry environment:
// OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT turns export on,
// OTEL_SDK_DISABLED=true turns it off, and OTEL_EXPORTER_OTLP_HEADERS,
// OTEL_TRACES_SAMPLER, OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES are honoured
// by the SDK. The transport is OTLP over HTTP with protobuf; any other
// OTEL_EXPORTER_OTLP_PROTOCOL is an error, so a gRPC-only collector address fails
// at startup instead of dropping every span.
//
// With no endpoint configured the spans stay no-ops and Setup costs nothing.
func Setup(ctx context.Context, serviceName string) (enabled bool, shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	noop := func(context.Context) error { return nil }
	if !exportConfigured() {
		return false, noop, nil
	}
	for _, key := range []string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		if p := os.Getenv(key); p != "" && p != "http/protobuf" {
			return false, noop, fmt.Errorf("%s=%q: only http/protobuf is supported", key, p)
		}
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return false, noop, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	// Later options win, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override
	// the name the binary passes.
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return false, noop, fmt.Errorf("build trace resource: %w", err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(provider)
	return true, provider.Shutdown, nil
}

func exportConfigured() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

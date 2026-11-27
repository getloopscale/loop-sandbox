package app_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// spanTree records spans and lets a test assert parent/child nesting by name.
func spanTree(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec
}

// TestCreateSandbox_emits_full_chain_trace proves the request trace is a TREE: the
// sandbox.create root has runtime.create + db.sandbox.save children (storage + runtime
// boundaries), all sharing one trace id — not a single flat span.
func TestCreateSandbox_emits_full_chain_trace(t *testing.T) {
	rec := spanTree(t)
	svc, _, _ := newService()

	if _, err := svc.CreateSandbox(context.Background(), actorA, "img", time.Minute); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	spans := rec.Ended()
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	root, ok := byName["sandbox.create"]
	if !ok {
		t.Fatal("no sandbox.create root span")
	}
	for _, child := range []string{"runtime.create", "db.sandbox.save"} {
		c, ok := byName[child]
		if !ok {
			t.Errorf("missing child span %q", child)
			continue
		}
		if c.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("span %q parent = %v, want root sandbox.create", child, c.Parent().SpanID())
		}
		if c.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Errorf("span %q trace id differs from root — not one trace", child)
		}
	}
}

// TestExec_emits_runtime_child_span proves the exec path nests a runtime.exec span under
// the sandbox.exec root.
func TestExec_emits_runtime_child_span(t *testing.T) {
	rec := spanTree(t)
	svc, _, _ := newService()
	sb, err := svc.CreateSandbox(context.Background(), actorA, "img", time.Minute)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := svc.Exec(context.Background(), actorA, sb.ID, runtime.ExecRequest{Cmd: []string{"echo", "hi"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	var root, child sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "sandbox.exec":
			root = s
		case "runtime.exec":
			child = s
		}
	}
	if root == nil || child == nil {
		t.Fatalf("missing spans: root=%v child=%v", root != nil, child != nil)
	}
	if child.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("runtime.exec parent != sandbox.exec")
	}
}

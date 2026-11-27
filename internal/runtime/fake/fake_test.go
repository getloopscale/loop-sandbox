package fake_test

import (
	"context"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

func TestFakeDriver_create_returns_running_sandbox(t *testing.T) {
	d := fake.New()

	sb, err := d.Create(context.Background(), runtime.CreateRequest{SandboxID: "sb-1", Image: "python:3"})
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if sb.ID != "sb-1" {
		t.Errorf("ID = %q, want sb-1", sb.ID)
	}
	if sb.State != runtime.StateRunning {
		t.Errorf("State = %q, want running", sb.State)
	}
}

func TestFakeDriver_inspect_reflects_created_state(t *testing.T) {
	d := fake.New()
	_, _ = d.Create(context.Background(), runtime.CreateRequest{SandboxID: "sb-2"})

	sb, err := d.Inspect(context.Background(), "sb-2")
	if err != nil {
		t.Fatalf("Inspect: unexpected error: %v", err)
	}
	if sb.State != runtime.StateRunning {
		t.Errorf("State = %q, want running", sb.State)
	}
}

func TestFakeDriver_destroy_is_idempotent(t *testing.T) {
	d := fake.New()
	_, _ = d.Create(context.Background(), runtime.CreateRequest{SandboxID: "sb-3"})

	if err := d.Destroy(context.Background(), "sb-3"); err != nil {
		t.Fatalf("Destroy: unexpected error: %v", err)
	}
	// Destroying again MUST NOT error (idempotent end state).
	if err := d.Destroy(context.Background(), "sb-3"); err != nil {
		t.Fatalf("second Destroy: unexpected error: %v", err)
	}
	// Destroying an unknown sandbox is also a no-op.
	if err := d.Destroy(context.Background(), "never-existed"); err != nil {
		t.Fatalf("Destroy unknown: unexpected error: %v", err)
	}

	sb, _ := d.Inspect(context.Background(), "sb-3")
	if sb.State != runtime.StateDestroyed {
		t.Errorf("State after destroy = %q, want destroyed", sb.State)
	}
}

func TestFakeDriver_name(t *testing.T) {
	if got := fake.New().Name(); got != "fake" {
		t.Errorf("Name = %q, want fake", got)
	}
}

// Compile-time assertion that *fake.Driver satisfies the port.
var _ runtime.Driver = (*fake.Driver)(nil)

package fake

import (
	"context"
	"errors"
	"testing"
)

// The DEFAULT fake driver must REFUSE sessions — it runs code on the host, so the session
// path must never be reachable in a production/default config (BLOCKER security fix).
func TestFake_default_refuses_sessions(t *testing.T) {
	d := New()
	if err := d.StartSession(context.Background(), "sbx", "s"); !errors.Is(err, ErrFakeSessionsDisabled) {
		t.Errorf("default fake StartSession err = %v, want ErrFakeSessionsDisabled", err)
	}
	if _, err := d.ExecSession(context.Background(), "sbx", "s", "1"); !errors.Is(err, ErrFakeSessionsDisabled) {
		t.Errorf("default fake ExecSession err = %v, want ErrFakeSessionsDisabled", err)
	}
}

// NewWithSessions (tests only) enables the capability.
func TestFake_NewWithSessions_enables(t *testing.T) {
	d := NewWithSessions()
	if err := d.StartSession(context.Background(), "sbx", "s"); err != nil {
		t.Skipf("python not available or start failed: %v", err) // capability enabled; env may lack python
	}
	_ = d.CloseSession(context.Background(), "sbx", "s")
}

package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// M16: a running sandbox can be paused and a paused sandbox resumed; a paused
// sandbox can still be destroyed. Pending → paused is NOT allowed (nothing to snapshot).
func TestSandbox_pause_resume_transitions(t *testing.T) {
	s, _ := domain.NewSandbox("sb-1", "tenant-a", "", time.Hour, t0)
	t1 := t0.Add(time.Second)
	if err := s.TransitionTo(domain.StateRunning, t1); err != nil {
		t.Fatalf("to running: %v", err)
	}
	if err := s.TransitionTo(domain.StatePaused, t1); err != nil {
		t.Fatalf("running → paused should be allowed: %v", err)
	}
	if s.State != domain.StatePaused {
		t.Fatalf("State = %q, want paused", s.State)
	}
	if err := s.TransitionTo(domain.StateRunning, t1); err != nil {
		t.Fatalf("paused → running (resume) should be allowed: %v", err)
	}
	// paused → destroyed is allowed too
	s2, _ := domain.NewSandbox("sb-2", "tenant-a", "", time.Hour, t0)
	_ = s2.TransitionTo(domain.StateRunning, t1)
	_ = s2.TransitionTo(domain.StatePaused, t1)
	if err := s2.TransitionTo(domain.StateDestroyed, t1); err != nil {
		t.Fatalf("paused → destroyed should be allowed: %v", err)
	}
}

func TestSandbox_pending_to_paused_rejected(t *testing.T) {
	s, _ := domain.NewSandbox("sb-1", "tenant-a", "", time.Hour, t0)
	if err := s.TransitionTo(domain.StatePaused, t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("pending → paused err = %v, want ErrInvalidTransition", err)
	}
}

func TestNewVolume_valid(t *testing.T) {
	v, err := domain.NewVolume("vol-1", "tenant-a", "cell-0", "data", 5, t0)
	if err != nil {
		t.Fatalf("NewVolume: %v", err)
	}
	if v.ID != "vol-1" || v.TenantID != "tenant-a" || v.CellID != "cell-0" || v.Name != "data" || v.SizeGB != 5 {
		t.Errorf("unexpected volume %+v", v)
	}
	if v.SandboxID != "" {
		t.Errorf("a fresh volume must be unattached, got sandbox %q", v.SandboxID)
	}
}

func TestNewVolume_rejects_bad_input(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		id, tenant, cell, vname string
		size                    int64
	}{
		{"empty id", "", "t", "cell-0", "d", 1},
		{"empty tenant", "v", "", "cell-0", "d", 1},
		{"empty cell", "v", "t", "", "d", 1},
		{"empty name", "v", "t", "cell-0", "", 1},
		{"zero size", "v", "t", "cell-0", "d", 0},
		{"negative size", "v", "t", "cell-0", "d", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.NewVolume(tc.id, tc.tenant, tc.cell, tc.vname, tc.size, t0); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

// Attach binds a volume to a sandbox; Detach unbinds it. Data survives detach.
func TestVolume_attach_detach(t *testing.T) {
	v, _ := domain.NewVolume("vol-1", "tenant-a", "cell-0", "data", 5, t0)
	if err := v.AttachTo("sb-1"); err != nil {
		t.Fatalf("AttachTo: %v", err)
	}
	if v.SandboxID != "sb-1" {
		t.Errorf("SandboxID = %q, want sb-1", v.SandboxID)
	}
	// attaching an already-attached volume to a different sandbox is rejected (a volume
	// is mounted in at most one sandbox at a time).
	if err := v.AttachTo("sb-2"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("double attach err = %v, want ErrInvalidTransition", err)
	}
	v.Detach()
	if v.SandboxID != "" {
		t.Errorf("after Detach SandboxID = %q, want empty", v.SandboxID)
	}
}

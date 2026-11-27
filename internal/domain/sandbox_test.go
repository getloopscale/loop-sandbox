package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

var t0 = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func TestNewSandbox_starts_pending(t *testing.T) {
	s, err := domain.NewSandbox("sb-1", "tenant-a", "python:3", time.Hour, t0)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	if s.State != domain.StatePending {
		t.Errorf("State = %q, want pending", s.State)
	}
}

func TestNewSandbox_rejects_empty_tenant(t *testing.T) {
	_, err := domain.NewSandbox("sb-1", "", "python:3", time.Hour, t0)
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
}

func TestSandbox_valid_transition_pending_to_running(t *testing.T) {
	s, _ := domain.NewSandbox("sb-1", "tenant-a", "", time.Hour, t0)
	t1 := t0.Add(time.Second)

	if err := s.TransitionTo(domain.StateRunning, t1); err != nil {
		t.Fatalf("TransitionTo running: %v", err)
	}
	if s.State != domain.StateRunning {
		t.Errorf("State = %q, want running", s.State)
	}
	if !s.UpdatedAt.Equal(t1) {
		t.Errorf("UpdatedAt = %v, want %v", s.UpdatedAt, t1)
	}
}

func TestSandbox_invalid_transition_is_rejected(t *testing.T) {
	s, _ := domain.NewSandbox("sb-1", "tenant-a", "", time.Hour, t0)
	_ = s.TransitionTo(domain.StateRunning, t0)
	_ = s.TransitionTo(domain.StateDestroyed, t0)

	// destroyed is terminal: any further transition must fail-fast, typed.
	err := s.TransitionTo(domain.StateRunning, t0)
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("err = %v, want ErrInvalidTransition", err)
	}
	if s.State != domain.StateDestroyed {
		t.Errorf("State mutated on invalid transition: %q", s.State)
	}
}

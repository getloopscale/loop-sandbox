package domain_test

import (
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

func TestNewSandbox_requires_positive_ttl(t *testing.T) {
	if _, err := domain.NewSandbox("s", "t", "", 0, t0); err != domain.ErrInvalidArgument {
		t.Errorf("ttl=0 err = %v, want ErrInvalidArgument", err)
	}
	if _, err := domain.NewSandbox("s", "t", "", -time.Second, t0); err != domain.ErrInvalidArgument {
		t.Errorf("ttl<0 err = %v, want ErrInvalidArgument", err)
	}
}

func TestSandbox_expired(t *testing.T) {
	s, _ := domain.NewSandbox("s", "t", "", time.Minute, t0)
	if s.Expired(t0.Add(30 * time.Second)) {
		t.Error("not expired before TTL")
	}
	if !s.Expired(t0.Add(2 * time.Minute)) {
		t.Error("expired after TTL")
	}
	if s.ExpiresAt != t0.Add(time.Minute) {
		t.Errorf("ExpiresAt = %v, want %v", s.ExpiresAt, t0.Add(time.Minute))
	}
}

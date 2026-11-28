package app

import (
	"context"
	"fmt"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// LiveSandboxLimitError refuses a create because the tenant already holds as many
// live sandboxes as the deployment allows.
type LiveSandboxLimitError struct {
	Limit int
	Live  int
}

func (e *LiveSandboxLimitError) Error() string {
	return fmt.Sprintf("this organization has %d live sandboxes and the limit is %d: kill one, or wait for one to expire, then create again",
		e.Live, e.Limit)
}

// liveStates are the states in which a sandbox holds memory on a host. A paused
// sandbox is among them: the container driver freezes its processes and keeps
// their memory, so a pause must not make room for another sandbox.
var liveStates = []domain.State{domain.StatePending, domain.StateRunning, domain.StatePaused}

// SetLiveSandboxLimit caps the live sandboxes of one tenant (composition root
// only). Zero or less removes the cap.
func (s *Service) SetLiveSandboxLimit(limit int) {
	s.liveLimit = limit
}

// reserveLiveSlot refuses the create when the tenant is at its cap. The caller
// holds the returned unlock until the new sandbox is saved, so two creates of
// one tenant cannot both take the last slot. The lock is local to this process:
// with several API replicas the cap can be passed by the creates in flight.
func (s *Service) reserveLiveSlot(ctx context.Context, tenantID string) (unlock func(), err error) {
	if s.liveLimit <= 0 {
		return func() {}, nil
	}
	unlock = s.locks.lock("live:" + tenantID)
	live, err := s.repo.List(ctx, tenantID, domain.SandboxQuery{States: liveStates, Limit: s.liveLimit})
	if err != nil {
		unlock()
		return nil, fmt.Errorf("count live sandboxes: %w", err)
	}
	if len(live) >= s.liveLimit {
		unlock()
		return nil, &LiveSandboxLimitError{Limit: s.liveLimit, Live: len(live)}
	}
	return unlock, nil
}

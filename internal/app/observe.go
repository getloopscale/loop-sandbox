package app

import (
	"context"
	"errors"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// Errors of the sandbox log and metrics use cases. The API maps them to 501.
var (
	ErrLogsUnsupported    = errors.New("sandbox logs not supported by this runtime")
	ErrMetricsUnsupported = errors.New("sandbox metrics not supported by this runtime")
)

// MaxLogsWait is the longest a read of sandbox logs may wait.
const MaxLogsWait = 30 * time.Second

// SandboxLogs returns what the background processes of the actor's sandbox wrote
// after the cursor, waiting up to wait for some when there is none yet.
func (s *Service) SandboxLogs(ctx context.Context, actor Actor, sandboxID string, after int64, wait time.Duration) (*runtime.SandboxLogs, error) {
	if _, err := s.ownedSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	ld, ok := s.driver.(runtime.LogsDriver)
	if !ok || !s.supports(runtime.CapLogs) {
		return nil, ErrLogsUnsupported
	}
	if after < 0 {
		return nil, &domain.ValidationError{Field: "after", Reason: "must not be negative"}
	}
	if wait < 0 || wait > MaxLogsWait {
		return nil, &domain.ValidationError{Field: "wait_ms", Reason: "must be between 0 and 30000"}
	}
	logs, err := ld.SandboxLogs(ctx, runtime.SandboxID(sandboxID), after, wait)
	if err != nil {
		return nil, s.mapRuntimeErr(err)
	}
	return logs, nil
}

// SandboxMetrics returns the CPU and memory use of the actor's sandbox.
func (s *Service) SandboxMetrics(ctx context.Context, actor Actor, sandboxID string) (*runtime.SandboxMetrics, error) {
	if _, err := s.ownedSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	md, ok := s.driver.(runtime.MetricsDriver)
	if !ok || !s.supports(runtime.CapMetrics) {
		return nil, ErrMetricsUnsupported
	}
	m, err := md.SandboxMetrics(ctx, runtime.SandboxID(sandboxID))
	if err != nil {
		return nil, s.mapRuntimeErr(err)
	}
	return m, nil
}

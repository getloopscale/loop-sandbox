package domain

import (
	"context"
	"time"
)

// Execution records a command run inside a sandbox. M1 persists the minimal,
// forward-compatible shape; M3 (command execution) fills in args/exit/streams.
type Execution struct {
	ID        string
	SandboxID string
	TenantID  string
	Status    string
	CreatedAt time.Time
}

// NewExecution constructs a pending execution. Fail-fast on missing ids.
func NewExecution(id, sandboxID, tenantID string, now time.Time) (*Execution, error) {
	if id == "" || sandboxID == "" || tenantID == "" {
		return nil, ErrInvalidArgument
	}
	return &Execution{ID: id, SandboxID: sandboxID, TenantID: tenantID, Status: "pending", CreatedAt: now}, nil
}

// ExecutionStore persists executions (tenant-scoped).
type ExecutionStore interface {
	SaveExecution(ctx context.Context, e *Execution) error
	GetExecution(ctx context.Context, tenantID, id string) (*Execution, error)
}

package container

import (
	"context"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.WatchDriver = (*Driver)(nil)

// StartWatch implements runtime.WatchDriver through the sandbox's agent.
func (d *Driver) StartWatch(ctx context.Context, id runtime.SandboxID, path string, recursive bool) (string, error) {
	return d.agent().StartWatch(ctx, id, path, recursive)
}

// WatchEvents implements runtime.WatchDriver.
func (d *Driver) WatchEvents(ctx context.Context, id runtime.SandboxID, watchID string, after int64, wait time.Duration) (*runtime.FileEvents, error) {
	return d.agent().WatchEvents(ctx, id, watchID, after, wait)
}

// StopWatch implements runtime.WatchDriver.
func (d *Driver) StopWatch(ctx context.Context, id runtime.SandboxID, watchID string) error {
	return d.agent().StopWatch(ctx, id, watchID)
}

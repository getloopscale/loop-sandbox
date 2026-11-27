package agentclient

import (
	"context"
	"fmt"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func (c Client) watchDo(ctx context.Context, id runtime.SandboxID, req *agentproto.Request, timeout time.Duration) (*agentproto.Response, error) {
	resp, err := c.RoundTrip(ctx, id, req, timeout)
	if err != nil {
		return nil, err
	}
	switch resp.Err {
	case "":
		return resp, nil
	case agentproto.ErrNotFound:
		if req.Op == agentproto.OpWatchStart {
			return nil, runtime.ErrFileNotFound
		}
		return nil, runtime.ErrWatchNotFound
	case agentproto.ErrInvalidPath:
		return nil, fmt.Errorf("%w: the path is outside /workspace", runtime.ErrInvalidRequest)
	case agentproto.ErrNotDirectory:
		return nil, runtime.ErrNotDirectory
	case agentproto.ErrTooManyWatches:
		return nil, runtime.ErrTooManyWatches
	default:
		return nil, fmt.Errorf("agent %s: %s", req.Op, resp.Err)
	}
}

// StartWatch implements runtime.WatchDriver.
func (c Client) StartWatch(ctx context.Context, id runtime.SandboxID, path string, recursive bool) (string, error) {
	resp, err := c.watchDo(ctx, id, &agentproto.Request{Op: agentproto.OpWatchStart, Path: path, Recursive: recursive}, opTimeout)
	if err != nil {
		return "", err
	}
	if resp.WatchID == "" {
		return "", fmt.Errorf("agent %s: answer without a watch id", agentproto.OpWatchStart)
	}
	return resp.WatchID, nil
}

// WatchEvents implements runtime.WatchDriver.
func (c Client) WatchEvents(ctx context.Context, id runtime.SandboxID, watchID string, after int64, wait time.Duration) (*runtime.FileEvents, error) {
	req := &agentproto.Request{Op: agentproto.OpWatchEvents, WatchID: watchID, After: after, WaitMS: int(wait.Milliseconds())}
	resp, err := c.watchDo(ctx, id, req, wait+opTimeout)
	if err != nil {
		return nil, err
	}
	out := &runtime.FileEvents{Next: resp.Next, Truncated: resp.Truncated, Closed: resp.Closed, Events: make([]runtime.FileEvent, 0, len(resp.Events))}
	for _, ev := range resp.Events {
		out.Events = append(out.Events, runtime.FileEvent{Type: ev.Type, Path: ev.Path, IsDir: ev.IsDir})
	}
	return out, nil
}

// StopWatch implements runtime.WatchDriver.
func (c Client) StopWatch(ctx context.Context, id runtime.SandboxID, watchID string) error {
	_, err := c.watchDo(ctx, id, &agentproto.Request{Op: agentproto.OpWatchStop, WatchID: watchID}, opTimeout)
	return err
}

package app

import (
	"context"
	"errors"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// Errors of the directory watch use cases. The API maps the first to 501 and the
// second to 409.
var (
	ErrFileWatchUnsupported = errors.New("directory watches not supported by this runtime")
	ErrTooManyWatches       = errors.New("the sandbox is at its limit of watches")
)

// MaxWatchEventsWait is the longest a read of watch events may wait.
const MaxWatchEventsWait = 30 * time.Second

func (s *Service) watches(ctx context.Context, actor Actor, sandboxID string) (runtime.WatchDriver, error) {
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		return nil, err
	}
	wd, ok := s.driver.(runtime.WatchDriver)
	if !ok || !s.supports(runtime.CapFileWatch) {
		return nil, ErrFileWatchUnsupported
	}
	return wd, nil
}

func (s *Service) mapWatchErr(err error) error {
	switch {
	case errors.Is(err, runtime.ErrWatchNotFound):
		return domain.ErrNotFound
	case errors.Is(err, runtime.ErrTooManyWatches):
		return ErrTooManyWatches
	case errors.Is(err, runtime.ErrNotDirectory):
		return &domain.ValidationError{Field: "path", Reason: "is not a directory"}
	default:
		return s.mapRuntimeErr(err)
	}
}

// StartWatch watches a directory of the actor's sandbox and returns the watch id
// and the cleaned path.
func (s *Service) StartWatch(ctx context.Context, actor Actor, sandboxID, dirPath string, recursive bool) (watchID, cleanPath string, err error) {
	wd, err := s.watches(ctx, actor, sandboxID)
	if err != nil {
		return "", "", err
	}
	clean, err := domain.ValidateSandboxPath(dirPath)
	if err != nil {
		return "", "", &domain.ValidationError{Field: "path", Reason: "must be a path under /workspace"}
	}
	id, err := wd.StartWatch(ctx, runtime.SandboxID(sandboxID), clean, recursive)
	if err != nil {
		return "", "", s.mapWatchErr(err)
	}
	return id, clean, nil
}

// WatchEvents returns the events of a watch after the cursor, waiting up to wait
// for one when there is none yet.
func (s *Service) WatchEvents(ctx context.Context, actor Actor, sandboxID, watchID string, after int64, wait time.Duration) (*runtime.FileEvents, error) {
	wd, err := s.watches(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	if after < 0 {
		return nil, &domain.ValidationError{Field: "after", Reason: "must not be negative"}
	}
	if wait < 0 || wait > MaxWatchEventsWait {
		return nil, &domain.ValidationError{Field: "wait_ms", Reason: "must be between 0 and 30000"}
	}
	events, err := wd.WatchEvents(ctx, runtime.SandboxID(sandboxID), watchID, after, wait)
	if err != nil {
		return nil, s.mapWatchErr(err)
	}
	return events, nil
}

// StopWatch ends a watch of the actor's sandbox.
func (s *Service) StopWatch(ctx context.Context, actor Actor, sandboxID, watchID string) error {
	wd, err := s.watches(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	if err := wd.StopWatch(ctx, runtime.SandboxID(sandboxID), watchID); err != nil {
		return s.mapWatchErr(err)
	}
	return nil
}

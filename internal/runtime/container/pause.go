package container

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.PauseDriver = (*Driver)(nil)

// Pause freezes every process of the sandbox with the cgroup freezer. The
// sandbox stops using CPU and keeps its memory and its files, so Resume
// continues exactly where it stopped. Unlike the microVM driver, nothing is
// written to disk: a paused container still holds its memory on the host, and
// it does not survive a restart of the host.
//
// The network guard of the sandbox keeps running, so the outbound rules stay
// in force for the moment the sandbox resumes.
func (d *Driver) Pause(ctx context.Context, id runtime.SandboxID) error {
	if err := d.cli.ContainerPause(ctx, containerName(id)); err != nil {
		return pauseErr("pause", err)
	}
	return nil
}

// Resume thaws a sandbox frozen by Pause.
func (d *Driver) Resume(ctx context.Context, id runtime.SandboxID) error {
	if err := d.cli.ContainerUnpause(ctx, containerName(id)); err != nil {
		return pauseErr("unpause", err)
	}
	return nil
}

func pauseErr(op string, err error) error {
	if errdefs.IsNotFound(err) {
		return fmt.Errorf("container %s: %w", op, runtime.ErrSandboxNotFound)
	}
	return fmt.Errorf("container %s: %w", op, err)
}

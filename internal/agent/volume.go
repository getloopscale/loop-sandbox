package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// VolumeDevice is the guest block device that backs the sandbox's volume slot: the
// second virtio-blk drive, after the root filesystem.
const VolumeDevice = "/dev/vdb"

func (h *Handler) volumeOp(name string, op func(dir string) error) *agentproto.Response {
	dir, err := h.volumeDir(name)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	if err := op(dir); err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	return &agentproto.Response{}
}

// volumeDir maps a volume name to its mount directory, a direct child of the
// workspace. The name rule (domain.ValidVolumeName) admits no separator or dot, so
// the result cannot leave the workspace.
func (h *Handler) volumeDir(name string) (string, error) {
	if !domain.ValidVolumeName(name) {
		return "", fmt.Errorf("invalid volume name %q", name)
	}
	return filepath.Join(h.root, name), nil
}

// mountVolume mounts the volume device at dir. It refuses a mount point that
// already holds something: mounting over a symlink would follow it out of the
// workspace, and mounting over a populated directory would hide the user's files.
func (h *Handler) mountVolume(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(dir, 0o755); err != nil {
			return fmt.Errorf("create mount point %s: %w", dir, err)
		}
	case err != nil:
		return fmt.Errorf("inspect mount point %s: %w", dir, err)
	case !info.IsDir():
		return fmt.Errorf("mount point %s exists and is not a directory", dir)
	default:
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			return fmt.Errorf("read mount point %s: %w", dir, rerr)
		}
		if len(entries) > 0 {
			return fmt.Errorf("mount point %s is not empty", dir)
		}
	}
	if err := h.mount(VolumeDevice, dir); err != nil {
		return fmt.Errorf("mount %s at %s: %w", VolumeDevice, dir, err)
	}
	return nil
}

// unmountVolume unmounts dir and removes the mount point.
func (h *Handler) unmountVolume(dir string) error {
	if err := h.unmount(dir); err != nil {
		return fmt.Errorf("unmount %s: %w", dir, err)
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove mount point %s: %w", dir, err)
	}
	return nil
}

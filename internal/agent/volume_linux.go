//go:build linux

package agent

import (
	"errors"
	"syscall"
)

// mountExt4 mounts device at dir. MS_SYNCHRONOUS sends every write straight to the
// block device: a sandbox VM is killed without a clean unmount, and data left in
// the guest page cache would be lost, which is the one thing a durable volume must
// not do.
func mountExt4(device, dir string) error {
	return syscall.Mount(device, dir, "ext4", syscall.MS_SYNCHRONOUS|syscall.MS_NOSUID|syscall.MS_NODEV, "")
}

// unmountDir unmounts dir. A busy mount (a process with its working directory
// inside) is detached lazily, so the host can always take the volume back. A
// directory that is not a mount point is already unmounted.
func unmountDir(dir string) error {
	err := syscall.Unmount(dir, 0)
	if errors.Is(err, syscall.EBUSY) {
		err = syscall.Unmount(dir, syscall.MNT_DETACH)
	}
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return err
}

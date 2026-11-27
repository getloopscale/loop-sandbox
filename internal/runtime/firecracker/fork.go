package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// ForkDriver support (M13): fork a RUNNING microVM by snapshotting its live state and
// restoring it into a new, isolated VM. The parent is paused only for the snapshot, then
// resumed. The fork has its own memory + writable overlay, so writes diverge.
var _ runtime.ForkDriver = (*Driver)(nil)

// Fork clones a running sandbox into a new one. The fork inherits the parent's state AS OF
// the fork instant; subsequent writes on either side are isolated.
func (d *Driver) Fork(ctx context.Context, parentID, forkID runtime.SandboxID) (*runtime.Sandbox, error) {
	parent, err := d.lookup(parentID)
	if err != nil {
		return nil, err
	}
	// Serialize forks of THE SAME parent: pause→snapshot→resume must not interleave with a
	// concurrent fork (which could resume the parent mid-snapshot or snapshot a running VM).
	parent.forkMu.Lock()
	defer parent.forkMu.Unlock()
	if parent.vol != nil {
		// The fork would mount the same image as the parent, and two guests writing
		// one ext4 image corrupt it.
		return nil, fmt.Errorf("%w: sandbox %s has volume %s attached; detach it before forking",
			runtime.ErrVolumeConflict, parentID, parent.vol.id)
	}

	// Snapshot into DRIVER-OWNED storage (not the parent's dir): the fork's mem_file is
	// mmapped for the fork's whole life, so it must NOT be destroyed when the PARENT is
	// destroyed. It is cleaned up with the FORK (killVM removes v.snapDir).
	snapDir, err := os.MkdirTemp(d.cfg.StateDir, "fork-snap-")
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(snapDir, "snapshot_file")
	memPath := filepath.Join(snapDir, "mem_file")

	if err := d.snapshotAndResume(parent, statePath, memPath); err != nil {
		_ = os.RemoveAll(snapDir)
		return nil, fmt.Errorf("fork: snapshot parent: %w", err)
	}

	forkVM, err := d.restore(ctx, &snapshot{statePath: statePath, memPath: memPath}, nil)
	if err != nil {
		_ = os.RemoveAll(snapDir)
		return nil, fmt.Errorf("fork: restore into new VM: %w", err)
	}
	// The VM state file is only needed for /snapshot/load; the mem_file remains mmapped.
	_ = os.Remove(statePath)
	forkVM.snapDir = snapDir // reaped when the fork is destroyed
	return d.adopt(forkID, forkVM), nil
}

// snapshotPaused writes a full snapshot of v, which the caller has already paused.
// The snapshot records the shared snapshot slot as the volume slot, never a path
// another VM could write. On return the slot points at v's own backing again,
// whether or not the snapshot succeeded.
func (d *Driver) snapshotPaused(v *vm, statePath, memPath string) error {
	api := v.api()
	if err := setSlotBacking(api, d.snapshotSlotPath()); err != nil {
		return fmt.Errorf("point the volume slot at the snapshot slot: %w", err)
	}
	snapErr := api.do("PUT", "/snapshot/create", map[string]any{
		"snapshot_path": statePath, "mem_file_path": memPath, "snapshot_type": "Full",
	})
	if err := setSlotBacking(api, v.slotBacking()); err != nil && snapErr == nil {
		snapErr = fmt.Errorf("point the volume slot back at %s: %w", v.slotBacking(), err)
	}
	return snapErr
}

// snapshotAndResume pauses v, snapshots it and resumes it. v is resumed even when
// the snapshot fails, so a failed fork never leaves the parent frozen.
func (d *Driver) snapshotAndResume(v *vm, statePath, memPath string) error {
	api := v.api()
	if err := api.do("PATCH", "/vm", map[string]any{"state": "Paused"}); err != nil {
		return fmt.Errorf("pause VM: %w", err)
	}
	snapErr := d.snapshotPaused(v, statePath, memPath)
	if err := api.do("PATCH", "/vm", map[string]any{"state": "Resumed"}); err != nil && snapErr == nil {
		snapErr = fmt.Errorf("resume VM: %w", err)
	}
	return snapErr
}

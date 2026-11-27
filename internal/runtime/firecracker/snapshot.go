package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.SnapshotDriver = (*Driver)(nil)

// Named snapshots live in StateDir/snapshots/<id>/ as the two files Firecracker
// writes: the VM state and the guest memory. A restored VM maps the memory file
// privately, so any number of sandboxes can start from one snapshot, and the
// files can be unlinked while those sandboxes run.
const (
	snapshotStateFile = "snapshot_file"
	snapshotMemFile   = "mem_file"
)

func (d *Driver) namedSnapshotDir(snapshotID string) string {
	return filepath.Join(d.cfg.StateDir, "snapshots", sanitize(snapshotID))
}

// CreateSnapshot pauses the sandbox for as long as the snapshot takes to write,
// then resumes it.
func (d *Driver) CreateSnapshot(_ context.Context, id runtime.SandboxID, snapshotID string) (int64, error) {
	v, err := d.lookup(id)
	if err != nil {
		return 0, err
	}
	// The same machinery as fork and pause: one at a time per VM.
	v.forkMu.Lock()
	defer v.forkMu.Unlock()
	if v.vol != nil {
		// A sandbox restored from the snapshot would mount the same image as this
		// one, and two guests writing one ext4 image corrupt it.
		return 0, fmt.Errorf("%w: sandbox %s has volume %s attached; detach it before taking a snapshot",
			runtime.ErrVolumeConflict, id, v.vol.id)
	}
	dir := d.namedSnapshotDir(snapshotID)
	if _, err := os.Stat(dir); err == nil {
		return 0, fmt.Errorf("snapshot %s already exists", snapshotID)
	}
	// Write into a temporary directory and rename it, so a snapshot directory
	// that exists is always complete.
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return 0, fmt.Errorf("create the snapshots directory: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "incoming-")
	if err != nil {
		return 0, fmt.Errorf("create a directory for snapshot %s: %w", snapshotID, err)
	}
	statePath, memPath := filepath.Join(tmp, snapshotStateFile), filepath.Join(tmp, snapshotMemFile)
	if err := d.snapshotAndResume(v, statePath, memPath); err != nil {
		_ = os.RemoveAll(tmp)
		return 0, fmt.Errorf("snapshot sandbox %s: %w", id, err)
	}
	var size int64
	for _, p := range []string{statePath, memPath} {
		info, err := os.Stat(p)
		if err != nil {
			_ = os.RemoveAll(tmp)
			return 0, fmt.Errorf("snapshot %s: %w", snapshotID, err)
		}
		size += info.Size()
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)
		return 0, fmt.Errorf("store snapshot %s: %w", snapshotID, err)
	}
	return size, nil
}

// RestoreSnapshot starts a new VM from the snapshot.
func (d *Driver) RestoreSnapshot(ctx context.Context, snapshotID string, newID runtime.SandboxID) (*runtime.Sandbox, error) {
	dir := d.namedSnapshotDir(snapshotID)
	snap := &snapshot{statePath: filepath.Join(dir, snapshotStateFile), memPath: filepath.Join(dir, snapshotMemFile)}
	if _, err := os.Stat(snap.statePath); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", runtime.ErrSnapshotNotFound, snapshotID)
	}
	v, err := d.restore(ctx, snap, nil)
	if err != nil {
		return nil, fmt.Errorf("restore snapshot %s: %w", snapshotID, err)
	}
	return d.adopt(newID, v), nil
}

// DeleteSnapshot removes the files of the snapshot.
func (d *Driver) DeleteSnapshot(_ context.Context, snapshotID string) error {
	if err := os.RemoveAll(d.namedSnapshotDir(snapshotID)); err != nil {
		return fmt.Errorf("delete snapshot %s: %w", snapshotID, err)
	}
	return nil
}

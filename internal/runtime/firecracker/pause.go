package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// PauseDriver support (M16): pause a running microVM by snapshotting its live state to
// DURABLE storage and killing the VM process (releasing all compute), then resume it later
// by restoring from that snapshot. Unlike a fork, pause/resume is the SAME sandbox — its
// in-memory state (open files, variables, running processes) survives the hibernation.
var _ runtime.PauseDriver = (*Driver)(nil)

// Pause snapshots the running sandbox to durable per-sandbox storage and stops its compute.
func (d *Driver) Pause(_ context.Context, id runtime.SandboxID) error {
	v, err := d.lookup(id)
	if err != nil {
		return err
	}
	// Serialize against forks/other pauses of THIS VM (shared live-snapshot machinery).
	v.forkMu.Lock()
	defer v.forkMu.Unlock()

	// Snapshot into a DURABLE dir (survives the VM process being killed). Keyed stably per
	// sandbox so it is easy to reason about and reap.
	pausedDir := filepath.Join(d.cfg.StateDir, "paused-"+sanitize(string(id)))
	if err := os.RemoveAll(pausedDir); err != nil { // clear any stale snapshot
		return err
	}
	if err := os.MkdirAll(pausedDir, 0o700); err != nil {
		return err
	}
	statePath := filepath.Join(pausedDir, "snapshot_file")
	memPath := filepath.Join(pausedDir, "mem_file")

	api := v.api()
	if err := api.do("PATCH", "/vm", map[string]any{"state": "Paused"}); err != nil {
		_ = os.RemoveAll(pausedDir)
		return fmt.Errorf("pause: freeze VM: %w", err)
	}
	if err := d.snapshotPaused(v, statePath, memPath); err != nil {
		// Snapshot failed — resume the VM so the sandbox is not left frozen, then report.
		_ = api.do("PATCH", "/vm", map[string]any{"state": "Resumed"})
		_ = os.RemoveAll(pausedDir)
		return fmt.Errorf("pause: snapshot: %w", err)
	}

	// Compute is now captured on disk — kill the VM process to release CPU/RAM. The prior
	// resume's mem-backend (if any) is no longer needed.
	prevSnap := v.snapDir
	d.mu.Lock()
	delete(d.vms, id)
	d.paused[id] = pausedDir
	if v.vol != nil {
		// The guest keeps the volume mounted inside the snapshot; Resume points the
		// slot back at the image. Nothing else writes the image meanwhile: the
		// volume stays bound to this sandbox.
		d.pausedVol[id] = v.vol
	}
	d.mu.Unlock()
	stopProc(v.cmd)
	if v.dir != "" {
		_ = os.RemoveAll(v.dir)
	}
	if prevSnap != "" && prevSnap != pausedDir {
		_ = os.RemoveAll(prevSnap)
	}
	return nil
}

// Resume restores a paused sandbox from its durable snapshot into a fresh VM process and
// resumes execution. The sandbox reappears in the running set with its state intact.
func (d *Driver) Resume(ctx context.Context, id runtime.SandboxID) error {
	d.mu.Lock()
	pausedDir, ok := d.paused[id]
	vol := d.pausedVol[id]
	d.mu.Unlock()
	if !ok {
		return runtime.ErrSandboxNotFound
	}
	statePath := filepath.Join(pausedDir, "snapshot_file")
	memPath := filepath.Join(pausedDir, "mem_file")

	v, err := d.restore(ctx, &snapshot{statePath: statePath, memPath: memPath}, vol)
	if err != nil {
		return fmt.Errorf("resume: restore VM: %w", err)
	}
	// The state file is only needed for /snapshot/load; the mem_file stays mmapped for the
	// VM's life, so keep pausedDir as the VM's snapDir (reaped on Destroy).
	_ = os.Remove(statePath)
	v.snapDir = pausedDir

	d.mu.Lock()
	delete(d.paused, id)
	delete(d.pausedVol, id)
	d.mu.Unlock()
	d.adopt(id, v)
	return nil
}

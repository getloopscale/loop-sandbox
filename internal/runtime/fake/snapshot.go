package fake

import (
	"context"
	"fmt"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.SnapshotDriver = (*Driver)(nil)

// CreateSnapshot keeps a copy of the sandbox's files under snapshotID.
func (d *Driver) CreateSnapshot(_ context.Context, id runtime.SandboxID, snapshotID string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sandboxes[id]; !ok {
		return 0, runtime.ErrSandboxNotFound
	}
	if _, ok := d.snapshots[snapshotID]; ok {
		return 0, fmt.Errorf("snapshot %s already exists", snapshotID)
	}
	if d.snapshots == nil {
		d.snapshots = map[string]map[string][]byte{}
	}
	var size int64
	cp := make(map[string][]byte, len(d.files[id]))
	for k, v := range d.files[id] {
		cp[k] = append([]byte(nil), v...)
		size += int64(len(v))
	}
	d.snapshots[snapshotID] = cp
	return size, nil
}

// RestoreSnapshot starts a sandbox with a copy of the snapshot's files.
func (d *Driver) RestoreSnapshot(_ context.Context, snapshotID string, newID runtime.SandboxID) (*runtime.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	files, ok := d.snapshots[snapshotID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", runtime.ErrSnapshotNotFound, snapshotID)
	}
	cp := make(map[string][]byte, len(files))
	for k, v := range files {
		cp[k] = append([]byte(nil), v...)
	}
	d.files[newID] = cp
	d.sandboxes[newID] = runtime.StateRunning
	return &runtime.Sandbox{ID: newID, State: runtime.StateRunning}, nil
}

// DeleteSnapshot forgets the snapshot.
func (d *Driver) DeleteSnapshot(_ context.Context, snapshotID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.snapshots, snapshotID)
	return nil
}

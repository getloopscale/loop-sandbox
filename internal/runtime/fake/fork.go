package fake

import (
	"context"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.ForkDriver = (*Driver)(nil)

// Fork clones a sandbox in-memory: the fork gets an isolated COPY of the parent's files
// (writes on either side diverge). Hermetic analogue of the microVM snapshot-fork.
func (d *Driver) Fork(_ context.Context, parentID, forkID runtime.SandboxID) (*runtime.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sandboxes[parentID]; !ok {
		return nil, runtime.ErrSandboxNotFound
	}
	cp := make(map[string][]byte, len(d.files[parentID]))
	for k, v := range d.files[parentID] {
		b := make([]byte, len(v))
		copy(b, v)
		cp[k] = b
	}
	d.files[forkID] = cp
	d.sandboxes[forkID] = runtime.StateRunning
	return &runtime.Sandbox{ID: forkID, State: runtime.StateRunning}, nil
}

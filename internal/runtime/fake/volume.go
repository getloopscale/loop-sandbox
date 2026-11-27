package fake

import (
	"context"
	"strings"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// backing returns the file map that path resolves to for sandbox id: a mounted volume's
// DURABLE store (volData, survives sandbox destroy) when path is under a mount prefix,
// else the sandbox's ephemeral per-sandbox files. Caller holds d.mu.
func (d *Driver) backing(id runtime.SandboxID, path string) map[string][]byte {
	for prefix, volID := range d.mounts[id] {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			if d.volData[volID] == nil {
				d.volData[volID] = make(map[string][]byte)
			}
			return d.volData[volID]
		}
	}
	if d.files[id] == nil {
		d.files[id] = make(map[string][]byte)
	}
	return d.files[id]
}

// AttachVolume mounts volume volID at /workspace/<name> in the sandbox. The backing store is
// created on first attach and REUSED on re-attach (so data survives sandbox destroy).
func (d *Driver) AttachVolume(_ context.Context, id runtime.SandboxID, volID, name string, _ int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.volData[volID] == nil {
		d.volData[volID] = make(map[string][]byte)
	}
	if d.mounts[id] == nil {
		d.mounts[id] = make(map[string]string)
	}
	d.mounts[id]["/workspace/"+name] = volID
	return nil
}

// DetachVolume unmounts the volume from the sandbox; the backing data is kept.
func (d *Driver) DetachVolume(_ context.Context, id runtime.SandboxID, volID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for prefix, v := range d.mounts[id] {
		if v == volID {
			delete(d.mounts[id], prefix)
		}
	}
	return nil
}

// DeleteVolumeData removes the volume's durable backing store entirely.
func (d *Driver) DeleteVolumeData(_ context.Context, volID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.volData, volID)
	return nil
}

// Pause marks the sandbox paused. The fake preserves all files (nothing is touched), so
// state trivially survives a pause/resume — the durable behavior the real driver proves.
func (d *Driver) Pause(_ context.Context, id runtime.SandboxID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sandboxes[id]; !ok {
		return runtime.ErrSandboxNotFound
	}
	d.paused[id] = true
	return nil
}

// Resume clears the paused mark.
func (d *Driver) Resume(_ context.Context, id runtime.SandboxID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sandboxes[id]; !ok {
		return runtime.ErrSandboxNotFound
	}
	d.paused[id] = false
	return nil
}

var (
	_ runtime.VolumeDriver = (*Driver)(nil)
	_ runtime.PauseDriver  = (*Driver)(nil)
)

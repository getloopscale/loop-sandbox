package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// VolumeDriver support. A durable volume is an ext4 image kept on the host under
// StateDir/volumes/, keyed by volume id; its data outlives every sandbox.
//
// Firecracker cannot add a drive to a running microVM, and it can replace the
// backing file of a drive that exists. So every VM boots with one spare drive, the
// volume slot, backed by an empty file of its own. Attaching a volume points the
// slot at the volume image and asks the guest agent to mount it at
// /workspace/<name>; detaching unmounts it and points the slot back at the empty
// file. One slot means one volume per sandbox.
//
// A snapshot must not capture a path that another VM could write, so a VM is
// pointed at the shared snapshot slot while it is paused for a snapshot, and every
// restored VM gets its own backing before it runs (see restore).
var _ runtime.VolumeDriver = (*Driver)(nil)

const (
	// slotDriveID is the Firecracker drive id of the volume slot. It is the second
	// drive, so the guest sees it as agent.VolumeDevice.
	slotDriveID = "vol"
	// slotFileName is the empty backing file of the slot, inside the VM's directory.
	slotFileName = "volume.slot"
	// slotFileSize is the size of an empty slot. Firecracker rejects an empty file.
	slotFileSize = 1 << 20
	// agentVolumeTimeout bounds one mount or unmount in the guest.
	agentVolumeTimeout = 15 * time.Second
)

// attachedVolume is the volume occupying a VM's slot.
type attachedVolume struct {
	id    string
	name  string
	image string
}

// snapshotSlotPath is the backing file every VM points its slot at while it is
// paused for a snapshot. No running VM ever has it as its slot.
func (d *Driver) snapshotSlotPath() string {
	return filepath.Join(d.cfg.StateDir, "snapshot-"+slotFileName)
}

// createSlotFile creates an empty slot backing file, or keeps an existing one.
func createSlotFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create volume slot %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(slotFileSize); err != nil {
		return fmt.Errorf("size volume slot %s: %w", path, err)
	}
	return nil
}

// slotBacking is the file the VM's slot must point at while the VM runs.
func (v *vm) slotBacking() string {
	if v.vol != nil {
		return v.vol.image
	}
	return v.slotPath
}

// setSlotBacking points the slot drive of a VM at path.
func setSlotBacking(api *apiClient, path string) error {
	return api.do("PATCH", "/drives/"+slotDriveID, map[string]any{"drive_id": slotDriveID, "path_on_host": path})
}

func (v *vm) api() *apiClient { return newAPIClient(filepath.Join(v.dir, "api.sock")) }

// volumeImagePath is the durable ext4 backing file for a volume.
func (d *Driver) volumeImagePath(volID string) string {
	return filepath.Join(d.cfg.StateDir, "volumes", sanitize(volID)+".ext4")
}

// AttachVolume provisions the volume image on first use, then mounts the volume in
// the running sandbox at /workspace/<name>. Attaching the volume a sandbox already
// has is a no-op; a sandbox whose slot holds another volume is ErrVolumeConflict.
func (d *Driver) AttachVolume(ctx context.Context, id runtime.SandboxID, volID, name string, sizeGB int64) error {
	if err := d.ensureVolumeImage(volID, sizeGB); err != nil {
		return err
	}
	return d.mountExistingVolume(ctx, id, volID, name)
}

// mountExistingVolume attaches a volume whose image already exists.
func (d *Driver) mountExistingVolume(_ context.Context, id runtime.SandboxID, volID, name string) error {
	v, err := d.lookup(id)
	if err != nil {
		return err
	}
	image := d.volumeImagePath(volID)
	if _, err := os.Stat(image); err != nil {
		return fmt.Errorf("volume %s has no image: %w", volID, err)
	}
	v.forkMu.Lock()
	defer v.forkMu.Unlock()
	if v.vol != nil {
		if v.vol.id == volID {
			return nil
		}
		return fmt.Errorf("%w: sandbox %s already has volume %s attached and a microVM has one volume slot",
			runtime.ErrVolumeConflict, id, v.vol.id)
	}
	api := v.api()
	if err := setSlotBacking(api, image); err != nil {
		return fmt.Errorf("point the volume slot at volume %s: %w", volID, err)
	}
	if err := d.agentVolumeOp(v, agentproto.OpVolumeMount, name); err != nil {
		// Leave the VM as it was: an empty slot.
		if rerr := setSlotBacking(api, v.slotPath); rerr != nil {
			return fmt.Errorf("mount volume %s: %w (and restoring the empty slot failed: %v)", volID, err, rerr)
		}
		return fmt.Errorf("mount volume %s: %w", volID, err)
	}
	v.vol = &attachedVolume{id: volID, name: name, image: image}
	return nil
}

// DetachVolume unmounts the volume from the sandbox and empties its slot. The
// image is kept. Detaching from a sandbox that is gone, or that does not have the
// volume, is a no-op.
func (d *Driver) DetachVolume(_ context.Context, id runtime.SandboxID, volID string) error {
	d.mu.Lock()
	if pv, ok := d.pausedVol[id]; ok && pv.id == volID {
		// Paused: there is no VM to unmount in. Resume will find the slot empty; the
		// guest mount point then errors on use, which is what a detached volume is.
		delete(d.pausedVol, id)
	}
	d.mu.Unlock()
	v, err := d.lookup(id)
	if errors.Is(err, runtime.ErrSandboxNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	v.forkMu.Lock()
	defer v.forkMu.Unlock()
	if v.vol == nil || v.vol.id != volID {
		return nil
	}
	if err := d.agentVolumeOp(v, agentproto.OpVolumeUnmount, v.vol.name); err != nil {
		return fmt.Errorf("unmount volume %s: %w", volID, err)
	}
	if err := setSlotBacking(v.api(), v.slotPath); err != nil {
		return fmt.Errorf("empty the volume slot after volume %s: %w", volID, err)
	}
	v.vol = nil
	return nil
}

func (d *Driver) agentVolumeOp(v *vm, op agentproto.Op, name string) error {
	resp, err := d.roundTrip(v, &agentproto.Request{Op: op, Volume: name}, agentVolumeTimeout)
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return errors.New(resp.Err)
	}
	return nil
}

// ensureVolumeImage creates the volume's ext4 image if it does not exist yet. An
// existing image is reused with its data.
func (d *Driver) ensureVolumeImage(volID string, sizeGB int64) error {
	img := d.volumeImagePath(volID)
	if _, err := os.Stat(img); err == nil {
		return nil
	}
	if sizeGB <= 0 {
		return fmt.Errorf("%w: volume %s size must be positive, got %d GiB", runtime.ErrInvalidRequest, volID, sizeGB)
	}
	if err := os.MkdirAll(filepath.Dir(img), 0o700); err != nil {
		return err
	}
	// Format a temporary file and rename it into place, so a crash mid-format never
	// leaves a half-formatted image that a later attach would take as ready.
	// mkfs.ext4 on a plain file needs no root.
	tmp := img + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(sizeGB << 30); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	_ = f.Close()
	// -F: force on a regular file; -q: quiet; -m 0: no reserved blocks, it is user data.
	out, err := exec.Command("mkfs.ext4", "-F", "-q", "-m", "0", tmp).CombinedOutput() //nolint:gosec // fixed binary, driver-built path
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mkfs.ext4 volume %s: %w: %s", volID, err, out)
	}
	if err := os.Rename(tmp, img); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalize volume %s: %w", volID, err)
	}
	return nil
}

// DeleteVolumeData removes the volume's durable backing image entirely.
func (d *Driver) DeleteVolumeData(_ context.Context, volID string) error {
	if err := os.Remove(d.volumeImagePath(volID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete volume image %s: %w", volID, err)
	}
	return nil
}

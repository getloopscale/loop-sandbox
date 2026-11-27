package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// apiClient is a minimal HTTP client over a firecracker API unix socket.
type apiClient struct {
	hc *http.Client
}

func newAPIClient(sock string) *apiClient {
	return &apiClient{hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

func (a *apiClient) do(method, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://unix"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fc api %s %s: %d %s", method, path, resp.StatusCode, b)
	}
	return nil
}

// startAPIProcess launches firecracker in API mode with cwd=dir and waits for the
// API socket to appear.
func (d *Driver) startAPIProcess(dir string) (*exec.Cmd, *apiClient, error) {
	apiSock := filepath.Join(dir, "api.sock")
	_ = os.Remove(apiSock)
	logf, err := os.Create(filepath.Join(dir, "fc.log"))
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(d.cfg.FirecrackerBin, "--api-sock", "api.sock") //nolint:gosec // fixed bin
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logf, logf
	err = cmd.Start()
	_ = logf.Close() // firecracker holds its own dup; don't keep/leak the parent fd
	if err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(apiSock); statErr == nil {
			return cmd, newAPIClient(apiSock), nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopProc(cmd)
	return nil, nil, fmt.Errorf("firecracker api socket did not appear")
}

// stopProc kills a process and reaps it (Kill without Wait leaks a zombie).
func stopProc(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

// snapshot holds the paths of a base snapshot (VM state + guest memory).
type snapshot struct {
	statePath string
	memPath   string
}

// createSnapshot boots a base microVM via the API, waits for the agent, pauses it,
// and writes a full snapshot. The base VM is then killed; restores load the files.
func (d *Driver) createSnapshot(ctx context.Context, dir string) (snap *snapshot, err error) {
	if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
		return nil, mkErr
	}
	// On any failure, don't leave a half-written snapshot dir behind (H3).
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	cmd, api, err := d.startAPIProcess(dir)
	if err != nil {
		return nil, err
	}
	// The base VM is only needed until the snapshot is written; reap it either way.
	defer stopProc(cmd)

	if err := api.do("PUT", "/boot-source", map[string]any{"kernel_image_path": d.cfg.KernelPath, "boot_args": bootArgs}); err != nil {
		return nil, err
	}
	if err := api.do("PUT", "/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": d.cfg.RootfsPath, "is_root_device": true, "is_read_only": true}); err != nil {
		return nil, err
	}
	// The base VM never runs tenant code, so its volume slot is the shared snapshot
	// slot; every VM restored from it is pointed at a slot of its own (see restore).
	if err := api.do("PUT", "/drives/"+slotDriveID, map[string]any{"drive_id": slotDriveID, "path_on_host": d.snapshotSlotPath(), "is_root_device": false, "is_read_only": false}); err != nil {
		return nil, err
	}
	if err := api.do("PUT", "/vsock", map[string]any{"guest_cid": guestCID, "uds_path": udsName}); err != nil {
		return nil, err
	}
	if err := api.do("PUT", "/machine-config", map[string]any{"vcpu_count": d.cfg.VCPUs, "mem_size_mib": d.cfg.MemMiB}); err != nil {
		return nil, err
	}
	if err := api.do("PUT", "/actions", map[string]any{"action_type": "InstanceStart"}); err != nil {
		return nil, err
	}
	// Wait for the agent to be ready before snapshotting so the restored VM is
	// immediately usable (the whole point of a warm snapshot).
	baseVM := &vm{dir: dir, udsPath: filepath.Join(dir, udsName)}
	if err := d.waitReady(ctx, baseVM); err != nil {
		return nil, fmt.Errorf("base VM not ready: %w", err)
	}
	if err := api.do("PATCH", "/vm", map[string]any{"state": "Paused"}); err != nil {
		return nil, err
	}
	if cErr := api.do("PUT", "/snapshot/create", map[string]any{
		"snapshot_path": "snapshot_file", "mem_file_path": "mem_file", "snapshot_type": "Full",
	}); cErr != nil {
		return nil, cErr
	}
	return &snapshot{statePath: filepath.Join(dir, "snapshot_file"), memPath: filepath.Join(dir, "mem_file")}, nil
}

// restore loads a microVM from a snapshot into a fresh per-VM dir and resumes it.
// Returns a ready-to-exec vm, with no kernel boot.
//
// A snapshot records its volume slot as the shared snapshot slot. The VM is loaded
// paused and its slot is pointed at a backing file before any guest code runs:
// vol's image when the snapshot was taken with a volume attached, otherwise a new
// empty file of the VM's own. Two VMs therefore never share a writable slot.
func (d *Driver) restore(ctx context.Context, snap *snapshot, vol *attachedVolume) (*vm, error) {
	dir, err := os.MkdirTemp(d.cfg.StateDir, "warm-")
	if err != nil {
		return nil, err
	}
	cmd, api, err := d.startAPIProcess(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := api.do("PUT", "/snapshot/load", map[string]any{
		"snapshot_path": snap.statePath,
		"mem_backend":   map[string]any{"backend_type": "File", "backend_path": snap.memPath},
		"resume_vm":     false,
	}); err != nil {
		stopProc(cmd)
		_ = os.RemoveAll(dir)
		return nil, err
	}
	v := &vm{dir: dir, udsPath: filepath.Join(dir, udsName), cmd: cmd, slotPath: filepath.Join(dir, slotFileName), vol: vol}
	if err := createSlotFile(v.slotPath); err != nil {
		_ = killVM(v)
		return nil, err
	}
	if err := setSlotBacking(api, v.slotBacking()); err != nil {
		_ = killVM(v)
		return nil, fmt.Errorf("point the volume slot of the restored VM: %w", err)
	}
	if err := api.do("PATCH", "/vm", map[string]any{"state": "Resumed"}); err != nil {
		_ = killVM(v)
		return nil, fmt.Errorf("resume the restored VM: %w", err)
	}
	if err := d.waitReady(ctx, v); err != nil {
		_ = killVM(v)
		return nil, err
	}
	return v, nil
}

// warmPool keeps a set of pre-restored, ready-to-exec microVMs so Create is a pop
// (near-instant) instead of a cold boot.
type warmPool struct {
	d    *Driver
	snap *snapshot
	ch   chan *vm
	quit chan struct{}
	done chan struct{} // closed when the filler goroutine has exited
}

// get pops a warm VM if one is available.
func (p *warmPool) get() (*vm, bool) {
	select {
	case v := <-p.ch:
		return v, true
	default:
		return nil, false
	}
}

// fill runs in the background, keeping the pool topped up.
func (p *warmPool) fill() {
	defer close(p.done)
	for {
		select {
		case <-p.quit:
			return
		default:
		}
		v, err := p.d.restore(context.Background(), p.snap, nil)
		if err != nil {
			select {
			case <-p.quit:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		select {
		case p.ch <- v:
		case <-p.quit:
			_ = killVM(v)
			return
		}
	}
}

// drain stops the filler, WAITS for it to exit (so no VM can be pushed after the
// final sweep — closes the L1 race), then kills every pooled VM.
func (p *warmPool) drain() {
	close(p.quit)
	<-p.done
	for {
		select {
		case v := <-p.ch:
			_ = killVM(v)
		default:
			return
		}
	}
}

// EnableWarmPool creates the base snapshot and starts a background filler keeping
// up to size warm VMs ready. Call once after New. Returns an error if the snapshot
// cannot be created (e.g. firecracker lacks snapshot support).
func (d *Driver) EnableWarmPool(ctx context.Context, size int) error {
	if size <= 0 {
		size = 1
	}
	snap, err := d.createSnapshot(ctx, filepath.Join(d.cfg.StateDir, "base-snapshot"))
	if err != nil {
		return err
	}
	p := &warmPool{d: d, snap: snap, ch: make(chan *vm, size), quit: make(chan struct{}), done: make(chan struct{})}
	d.pool = p
	go p.fill()
	return nil
}

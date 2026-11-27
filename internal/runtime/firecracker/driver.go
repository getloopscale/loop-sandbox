// Package firecracker is a runtime.Driver that runs each sandbox as a Firecracker
// microVM (hardware-virtualised, separate guest kernel — the strongest isolation
// floor). Commands and file operations are served by an in-guest agent
// (cmd/lsb-agent) over vsock; fast provisioning uses snapshot/restore + warm pools.
package firecracker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// maxExecOutput caps captured output host-side (the agent also caps in-guest).
const maxExecOutput = 8 << 20

// Config parameterises the driver. Paths must exist; New validates them.
type Config struct {
	FirecrackerBin   string        // path to the firecracker binary
	KernelPath       string        // uncompressed guest kernel (vmlinux)
	RootfsPath       string        // ext4 rootfs with lsb-agent injected (read-only)
	StateDir         string        // per-VM working dirs live here
	VCPUs            int           // guest vCPUs (default 1)
	MemMiB           int           // guest memory MiB (default 256)
	BootReadyTimeout time.Duration // max wait for the agent to answer (default 10s)
	MaxExecTimeout   time.Duration // hard ceiling for an exec round-trip (default 5m)
	FileOpTimeout    time.Duration // deadline for a file op round-trip (default 60s)
}

func (c *Config) applyDefaults() {
	if c.VCPUs <= 0 {
		c.VCPUs = 1
	}
	if c.MemMiB <= 0 {
		c.MemMiB = 256
	}
	if c.BootReadyTimeout <= 0 {
		c.BootReadyTimeout = 10 * time.Second
	}
	if c.MaxExecTimeout <= 0 {
		c.MaxExecTimeout = 5 * time.Minute
	}
	if c.FileOpTimeout <= 0 {
		c.FileOpTimeout = 60 * time.Second
	}
	if c.StateDir == "" {
		c.StateDir = filepath.Join(os.TempDir(), "loopsandbox-fc")
	}
}

// compile-time proof the driver satisfies the port.
var _ runtime.Driver = (*Driver)(nil)

// Driver implements runtime.Driver over Firecracker microVMs.
type Driver struct {
	cfg  Config
	mu   sync.Mutex
	vms  map[runtime.SandboxID]*vm
	pool *warmPool
	// paused maps a paused sandbox id to the DURABLE dir holding its snapshot (M16). While
	// paused the sandbox has NO live VM (compute released); Resume restores from this dir.
	paused map[runtime.SandboxID]string
	// pausedVol holds the volume a sandbox had attached when it was paused, so Resume
	// puts it back.
	pausedVol map[runtime.SandboxID]*attachedVolume
}

// maxUnixSocketPath is the longest path a unix socket address can hold on Linux
// (sizeof(sockaddr_un.sun_path) minus the terminating NUL).
const maxUnixSocketPath = 107

// maxVMDirName bounds the per-VM directory name under StateDir: sandbox ids are
// "sbx_" plus a UUID (40 bytes); 48 leaves room for the fixed prefixes of the
// temporary directories.
const maxVMDirName = 48

// New validates the config and returns a driver. It does NOT require root; the
// microVM itself is the isolation boundary (jailer process-sandboxing is an
// optional hardening layer, out of this baseline).
func New(cfg Config) (*Driver, error) {
	cfg.applyDefaults()
	for _, p := range []struct{ name, path string }{
		{"firecracker binary", cfg.FirecrackerBin},
		{"kernel", cfg.KernelPath},
		{"rootfs", cfg.RootfsPath},
	} {
		if p.path == "" {
			return nil, fmt.Errorf("firecracker: %s path is empty", p.name)
		}
		if _, err := os.Stat(p.path); err != nil {
			return nil, fmt.Errorf("firecracker: %s not found at %q: %w", p.name, p.path, err)
		}
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	// Every VM has a vsock socket at StateDir/<vm dir>/vsock.sock, and a unix socket
	// path is limited to 107 bytes. Past that limit no VM would ever become ready,
	// so refuse the configuration here.
	if longest := filepath.Join(cfg.StateDir, strings.Repeat("x", maxVMDirName), udsName); len(longest) > maxUnixSocketPath {
		return nil, fmt.Errorf("firecracker: state dir %q is too long: a VM socket path would be %d bytes, over the %d byte limit of a unix socket",
			cfg.StateDir, len(longest), maxUnixSocketPath)
	}
	d := &Driver{
		cfg: cfg, vms: make(map[runtime.SandboxID]*vm),
		paused: make(map[runtime.SandboxID]string), pausedVol: make(map[runtime.SandboxID]*attachedVolume),
	}
	if err := createSlotFile(d.snapshotSlotPath()); err != nil {
		return nil, err
	}
	return d, nil
}

// NewFromEnv builds a driver from LOOPSANDBOX_FC_* environment variables.
func NewFromEnv() (*Driver, error) {
	return New(Config{
		FirecrackerBin: os.Getenv("LOOPSANDBOX_FC_BIN"),
		KernelPath:     os.Getenv("LOOPSANDBOX_FC_KERNEL"),
		RootfsPath:     os.Getenv("LOOPSANDBOX_FC_ROOTFS"),
		StateDir:       os.Getenv("LOOPSANDBOX_FC_STATEDIR"),
	})
}

// Name implements runtime.Driver.
func (d *Driver) Name() string { return "firecracker" }

// Create provisions a microVM. If a warm pool is enabled and non-empty it pops a
// pre-restored VM (fast path); otherwise it cold-boots one.
func (d *Driver) Create(ctx context.Context, req runtime.CreateRequest) (*runtime.Sandbox, error) {
	// A boot-time volume OR a template rootfs forces a cold boot (a warm-pool VM has neither).
	if len(req.Volumes) > 1 {
		return nil, fmt.Errorf("%w: a microVM has one volume slot, %d volumes requested", runtime.ErrInvalidRequest, len(req.Volumes))
	}
	v, err := d.provision(ctx, req)
	if err != nil {
		return nil, err
	}
	sb := d.adopt(req.SandboxID, v)
	for _, vol := range req.Volumes {
		if err := d.mountExistingVolume(ctx, req.SandboxID, vol.ID, vol.Name); err != nil {
			_ = d.Destroy(ctx, req.SandboxID)
			return nil, fmt.Errorf("attach volume %s at create: %w", vol.ID, err)
		}
	}
	return sb, nil
}

// provision returns a ready VM: a warm one when the pool has it and the request
// does not need its own rootfs, otherwise a cold boot.
func (d *Driver) provision(ctx context.Context, req runtime.CreateRequest) (*vm, error) {
	if req.RootfsImage == "" && d.pool != nil {
		if v, ok := d.pool.get(); ok {
			return v, nil
		}
	}
	return d.coldBoot(ctx, req.SandboxID, req.RootfsImage)
}

// coldBoot boots a fresh microVM and waits for the agent to become ready.
func (d *Driver) coldBoot(ctx context.Context, id runtime.SandboxID, rootfsImage string) (*vm, error) {
	dir := filepath.Join(d.cfg.StateDir, sanitize(string(id)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	v := &vm{id: id, dir: dir, udsPath: filepath.Join(dir, "vsock.sock")}
	// M17: boot from a template's built rootfs (read-only, like the default) when provided.
	if rootfsImage != "" {
		v.rootfsOverride = rootfsImage
	}
	_ = os.Remove(v.udsPath)
	if err := d.boot(ctx, v); err != nil {
		return nil, err
	}
	if err := d.waitReady(ctx, v); err != nil {
		_ = killVM(v)
		return nil, err
	}
	return v, nil
}

// adopt registers a VM under the sandbox id and returns the runtime view.
func (d *Driver) adopt(id runtime.SandboxID, v *vm) *runtime.Sandbox {
	v.id = id
	d.mu.Lock()
	d.vms[id] = v
	d.mu.Unlock()
	return &runtime.Sandbox{ID: id, State: runtime.StateRunning}
}

// Inspect returns the runtime view of a sandbox.
func (d *Driver) Inspect(_ context.Context, id runtime.SandboxID) (*runtime.Sandbox, error) {
	d.mu.Lock()
	_, ok := d.vms[id]
	d.mu.Unlock()
	if !ok {
		return &runtime.Sandbox{ID: id, State: runtime.StateDestroyed}, nil
	}
	return &runtime.Sandbox{ID: id, State: runtime.StateRunning}, nil
}

// Destroy tears the microVM down. Idempotent.
func (d *Driver) Destroy(_ context.Context, id runtime.SandboxID) error {
	d.mu.Lock()
	v, ok := d.vms[id]
	delete(d.vms, id)
	pausedDir, paused := d.paused[id]
	delete(d.paused, id)
	delete(d.pausedVol, id)
	d.mu.Unlock()
	if paused { // a paused sandbox has no live VM — just reap its durable snapshot (M16)
		_ = os.RemoveAll(pausedDir)
	}
	if !ok {
		return nil
	}
	return killVM(v)
}

// lookup returns the VM for id or ErrSandboxNotFound.
func (d *Driver) lookup(id runtime.SandboxID) (*vm, error) {
	d.mu.Lock()
	v, ok := d.vms[id]
	d.mu.Unlock()
	if !ok {
		return nil, runtime.ErrSandboxNotFound
	}
	return v, nil
}

// Exec runs a command in the microVM via the in-guest agent.
func (d *Driver) Exec(_ context.Context, id runtime.SandboxID, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	v, err := d.lookup(id)
	if err != nil {
		return nil, err
	}
	areq := &agentproto.Request{
		Op:      agentproto.OpExec,
		Cmd:     req.Cmd,
		Env:     req.Env,
		WorkDir: req.WorkingDir,
	}
	if req.Stdin != nil {
		b, rerr := io.ReadAll(io.LimitReader(req.Stdin, maxExecOutput))
		if rerr != nil {
			return nil, rerr
		}
		areq.StdinB64 = b
	}
	// A hostile/wedged guest must never hang the control-plane thread forever: cap
	// the round-trip at MaxExecTimeout even when the caller passes no timeout (B1).
	timeout := req.Timeout
	if timeout <= 0 || timeout > d.cfg.MaxExecTimeout {
		timeout = d.cfg.MaxExecTimeout
	}
	resp, err := d.roundTrip(v, areq, timeout)
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("%w: %s", runtime.ErrInvalidRequest, resp.Err)
	}
	return &runtime.ExecResult{ExitCode: resp.ExitCode, Stdout: resp.Stdout, Stderr: resp.Stderr, Truncated: resp.Truncated}, nil
}

// WriteFile writes r to path inside the microVM.
func (d *Driver) WriteFile(_ context.Context, id runtime.SandboxID, path string, r io.Reader) error {
	v, err := d.lookup(id)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return err
	}
	resp, err := d.roundTrip(v, &agentproto.Request{Op: agentproto.OpWrite, Path: path, Data: data}, d.cfg.FileOpTimeout)
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return fmt.Errorf("%w: %s", runtime.ErrInvalidRequest, resp.Err)
	}
	return nil
}

// ReadFile returns the content of path inside the microVM.
func (d *Driver) ReadFile(_ context.Context, id runtime.SandboxID, path string) (io.ReadCloser, error) {
	v, err := d.lookup(id)
	if err != nil {
		return nil, err
	}
	resp, err := d.roundTrip(v, &agentproto.Request{Op: agentproto.OpRead, Path: path}, d.cfg.FileOpTimeout)
	if err != nil {
		return nil, err
	}
	if resp.Err == "not_found" {
		return nil, runtime.ErrFileNotFound
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("%w: %s", runtime.ErrInvalidRequest, resp.Err)
	}
	return io.NopCloser(bytes.NewReader(resp.Data)), nil
}

// DeleteFile removes path inside the microVM.
func (d *Driver) DeleteFile(_ context.Context, id runtime.SandboxID, path string) error {
	v, err := d.lookup(id)
	if err != nil {
		return err
	}
	resp, err := d.roundTrip(v, &agentproto.Request{Op: agentproto.OpDelete, Path: path}, d.cfg.FileOpTimeout)
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return fmt.Errorf("%w: %s", runtime.ErrInvalidRequest, resp.Err)
	}
	return nil
}

// ListFiles lists a directory inside the microVM.
func (d *Driver) ListFiles(_ context.Context, id runtime.SandboxID, path string) ([]runtime.FileEntry, error) {
	v, err := d.lookup(id)
	if err != nil {
		return nil, err
	}
	resp, err := d.roundTrip(v, &agentproto.Request{Op: agentproto.OpList, Path: path}, d.cfg.FileOpTimeout)
	if err != nil {
		return nil, err
	}
	if resp.Err == "not_found" {
		return nil, runtime.ErrFileNotFound
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("%w: %s", runtime.ErrInvalidRequest, resp.Err)
	}
	out := make([]runtime.FileEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		out = append(out, runtime.FileEntry{Name: e.Name, Size: e.Size, IsDir: e.IsDir})
	}
	return out, nil
}

// Close tears down all VMs (including the warm pool). For graceful shutdown.
func (d *Driver) Close() error {
	if d.pool != nil {
		d.pool.drain()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, v := range d.vms {
		_ = killVM(v)
		delete(d.vms, id)
	}
	return nil
}

// killVM stops the firecracker process and removes the VM's working dir.
func killVM(v *vm) error {
	if v.cmd != nil && v.cmd.Process != nil {
		_ = v.cmd.Process.Kill()
		_, _ = v.cmd.Process.Wait()
	}
	if v.dir != "" {
		_ = os.RemoveAll(v.dir)
	}
	if v.snapDir != "" { // fork mem-backend storage — remove with the VM (M13)
		_ = os.RemoveAll(v.snapDir)
	}
	return nil
}

// sanitize keeps a sandbox id safe as a directory name.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

// ProcessLocal reports that the microVM handles live in this process: another
// process holding a second Driver cannot see or destroy these VMs.
func (d *Driver) ProcessLocal() bool { return true }

var _ runtime.ProcessLocal = (*Driver)(nil)

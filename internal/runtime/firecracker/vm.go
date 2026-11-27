package firecracker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// agentPort is the in-guest vsock port the lsb-agent listens on (see cmd/lsb-agent).
const agentPort = 1024

// guestCID is the vsock context id assigned to every guest. Because each VM has its
// OWN firecracker process + uds multiplexer, the CID need not be host-unique.
const guestCID = 3

// vm is a running (or restored) microVM and its host-side handles.
type vm struct {
	id      runtime.SandboxID
	dir     string
	udsPath string
	cmd     *exec.Cmd
	// forkMu serializes fork operations against THIS vm (a fork pauses/snapshots/resumes
	// the live VM — concurrent forks of the same parent must not interleave). M13.
	forkMu sync.Mutex
	// snapDir, when set, is driver-owned storage holding this VM's restored mem-backend
	// file (for a fork); removed on Destroy so it does not outlive the VM. M13.
	snapDir string
	// slotPath is this VM's own empty backing file for the volume slot drive (see
	// volume.go). vol is the volume attached to the slot, nil when it is empty.
	// Both are guarded by forkMu.
	slotPath string
	vol      *attachedVolume
	// rootfsOverride/rootfsWritable (M17 template builder): boot from a specific rootfs image
	// instead of d.cfg.RootfsPath, optionally writable so a build's RUN steps persist into it.
	rootfsOverride string
	rootfsWritable bool
}

// udsName is the vsock socket filename, relative to the per-VM working dir (the
// firecracker process runs with Dir=v.dir). A RELATIVE uds path lets every VM —
// including warm-pool restores from one shared snapshot — own a distinct socket
// without jailer/root, because each process resolves it against its own cwd.
const udsName = "vsock.sock"

// bootConfig renders the firecracker config file for a cold boot. Every VM gets a
// second writable virtio-blk drive, the volume slot, backed by an empty file of its
// own; attaching a volume swaps the backing file of that drive (see volume.go).
func (d *Driver) bootConfig(v *vm) string {
	rootfs := d.cfg.RootfsPath
	rootRO := true
	args := bootArgs
	if v.rootfsOverride != "" { // M17 template builder: boot from a specific rootfs
		rootfs = v.rootfsOverride
	}
	if v.rootfsWritable { // build steps must persist into the rootfs image
		rootRO = false
		args += " lsb.rw"
	}
	drives := fmt.Sprintf(`{"drive_id":"rootfs","path_on_host":%q,"is_root_device":true,"is_read_only":%t}`, rootfs, rootRO)
	drives += fmt.Sprintf(`,{"drive_id":%q,"path_on_host":%q,"is_root_device":false,"is_read_only":false}`, slotDriveID, v.slotPath)
	return fmt.Sprintf(`{
"boot-source":{"kernel_image_path":%q,"boot_args":%q},
"drives":[%s],
"vsock":{"guest_cid":%d,"uds_path":%q},
"machine-config":{"vcpu_count":%d,"mem_size_mib":%d}
}`, d.cfg.KernelPath, args, drives, guestCID, udsName, d.cfg.VCPUs, d.cfg.MemMiB)
}

// bootArgs is the kernel command line. The lsb-agent is PID 1 (init); the console
// is kept for debuggability. pci=off + i8042 quirks speed the boot.
const bootArgs = "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/usr/bin/lsb-agent"

// boot starts the firecracker process for a cold-boot VM.
func (d *Driver) boot(ctx context.Context, v *vm) error {
	v.slotPath = filepath.Join(v.dir, slotFileName)
	if err := createSlotFile(v.slotPath); err != nil {
		return err
	}
	cfgPath := filepath.Join(v.dir, "vm.json")
	if err := os.WriteFile(cfgPath, []byte(d.bootConfig(v)), 0o600); err != nil {
		return err
	}
	logf, err := os.Create(filepath.Join(v.dir, "boot.log"))
	if err != nil {
		return err
	}
	// The firecracker process outlives this call; it is killed in Destroy. Dir is
	// the per-VM workdir so the relative uds resolves to this VM's own socket. We keep the
	// API socket (not --no-api) so the running VM can be snapshotted LIVE — required to
	// FORK it (M13). Boot still comes from the config file.
	cmd := exec.Command(d.cfg.FirecrackerBin, "--api-sock", "api.sock", "--config-file", "vm.json") //nolint:gosec // fixed bin + generated config
	cmd.Dir = v.dir
	cmd.Stdout, cmd.Stderr = logf, logf
	err = cmd.Start()
	// firecracker inherited its own dup of the fd; close the parent copy either way
	// so we never hold a log fd for the VM's whole lifetime (or leak it on error).
	_ = logf.Close()
	if err != nil {
		return fmt.Errorf("start firecracker: %w", err)
	}
	v.cmd = cmd
	return nil
}

// waitReady blocks until the in-guest agent answers a vsock ping or the deadline
// elapses. This is the "ready-to-exec" gate the provisioning benchmark measures.
func (d *Driver) waitReady(ctx context.Context, v *vm) error {
	deadline := time.Now().Add(d.cfg.BootReadyTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := dialAgent(ctx, v.udsPath)
		if err == nil {
			perr := pingAgent(conn)
			_ = conn.Close()
			if perr == nil {
				return nil
			}
		}
		// Cancellable backoff — honour ctx instead of a blind sleep (M2).
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
	}
	return fmt.Errorf("agent not ready within %s", d.cfg.BootReadyTimeout)
}

// dialAgent opens a stream to the guest agent via the firecracker uds multiplexer:
// connect to the uds, request the guest port, and confirm the "OK" handshake.
func dialAgent(ctx context.Context, udsPath string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agentPort); err != nil {
		_ = conn.Close()
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "OK") {
		_ = conn.Close()
		return nil, fmt.Errorf("vsock handshake: %q", strings.TrimSpace(line))
	}
	return conn, nil
}

func pingAgent(conn net.Conn) error {
	if err := agentproto.WriteFrame(conn, &agentproto.Request{Op: agentproto.OpPing}); err != nil {
		return err
	}
	_, err := agentproto.ReadResponse(conn)
	return err
}

// roundTrip sends one request to the guest agent and returns its response. If
// timeout > 0 it bounds the whole exchange; an i/o deadline maps to ErrTimeout.
func (d *Driver) roundTrip(v *vm, req *agentproto.Request, timeout time.Duration) (*agentproto.Response, error) {
	// Bound the whole exchange, including the dial, so a wedged guest can never hang
	// the caller indefinitely (B1). timeout is always > 0 by the callers' contract.
	dialCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := dialAgent(dialCtx, v.udsPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", runtime.ErrSandboxNotFound, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := agentproto.WriteFrame(conn, req); err != nil {
		if isTimeout(err) {
			return nil, runtime.ErrTimeout
		}
		return nil, err
	}
	resp, err := agentproto.ReadResponse(conn)
	if err != nil {
		if isTimeout(err) {
			return nil, runtime.ErrTimeout
		}
		return nil, err
	}
	return resp, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

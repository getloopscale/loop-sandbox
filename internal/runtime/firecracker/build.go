package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// TemplateBuilder support (M17): build a custom template rootfs by running the spec's steps in
// an ISOLATED builder microVM (never on the host — closes the untrusted-build-steps risk), then
// persisting the modified rootfs as a new image.
var _ runtime.TemplateBuilder = (*Driver)(nil)

var buildSeq atomic.Uint64

// BuildTemplate copies baseRootfs to outRootfs, boots a throwaway builder microVM from it
// (writable so RUN steps persist), runs each step in order, syncs, and tears the VM down. The
// resulting outRootfs is a bootable template image.
func (d *Driver) BuildTemplate(ctx context.Context, baseRootfs, outRootfs string, steps []string) (rerr error) {
	if err := copyFile(baseRootfs, outRootfs); err != nil {
		return fmt.Errorf("copy base rootfs: %w", err)
	}
	// On ANY failure, do not leave a partial/unfinalized template image behind (a later
	// attach would boot a half-built or fsck-dirty rootfs). Removed only on the failure path.
	defer func() {
		if rerr != nil {
			_ = os.Remove(outRootfs)
		}
	}()
	buildID := runtime.SandboxID(fmt.Sprintf("build-%d", buildSeq.Add(1)))
	dir := filepath.Join(d.cfg.StateDir, sanitize(string(buildID)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	v := &vm{
		id: buildID, dir: dir, udsPath: filepath.Join(dir, udsName),
		rootfsOverride: outRootfs, rootfsWritable: true,
	}
	_ = os.Remove(v.udsPath)
	if err := d.boot(ctx, v); err != nil {
		return fmt.Errorf("boot builder: %w", err)
	}
	if err := d.waitReady(ctx, v); err != nil {
		_ = killVM(v)
		return fmt.Errorf("builder not ready: %w", err)
	}
	d.adopt(buildID, v) // register so Exec can reach it
	defer func() { _ = d.Destroy(ctx, buildID) }()

	for _, step := range steps {
		res, err := d.Exec(ctx, buildID, runtime.ExecRequest{Cmd: []string{"sh", "-lc", step}})
		if err != nil {
			return fmt.Errorf("build step %q: %w", step, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("build step %q failed (exit %d): %s", step, res.ExitCode, res.Stderr)
		}
	}
	// Flush the writable rootfs and remount it read-only IN THE GUEST so the ext4 journal is
	// closed cleanly before the VM is SIGKILL'd on Destroy — otherwise the image boots later
	// with a dirty journal that a read-only mount cannot recover.
	_, _ = d.Exec(ctx, buildID, runtime.ExecRequest{Cmd: []string{"sync"}})
	_, _ = d.Exec(ctx, buildID, runtime.ExecRequest{Cmd: []string{"sh", "-lc", "mount -o remount,ro / 2>/dev/null; sync"}})
	_ = d.Destroy(ctx, buildID) // stop the VM before fscking the image on the host
	// Authoritative cleanup: fsck the built image on the host so any residual journal state
	// from the abrupt teardown is resolved and the image is guaranteed cleanly mountable.
	// e2fsck -p exits 0 (clean) or 1 (fixed) — both are success; ≥2 is a real error.
	if out, code := runE2fsck(outRootfs); code >= 2 {
		return fmt.Errorf("fsck built template: exit %d: %s", code, out)
	}
	return nil
}

// runE2fsck runs e2fsck -p -f on img and returns its combined output + exit code.
func runE2fsck(img string) (string, int) {
	cmd := exec.Command("e2fsck", "-p", "-f", img)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	return string(out), 2
}

// copyFile copies src to dst (whole-file; template rootfs images are a few hundred MiB).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

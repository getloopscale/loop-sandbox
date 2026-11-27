//go:build firecracker

package firecracker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// TestFirecracker_build_template: build a custom template by running steps in an isolated
// builder microVM (write a file into the rootfs), then boot a NEW sandbox from the built
// template rootfs and read the file back — proving the M17 build pipeline produces a real,
// bootable, customized template image on real microVMs.
func TestFirecracker_build_template(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	out := filepath.Join(d.cfg.StateDir, "tmpl-built.ext4")

	// Build: the RUN step bakes a marker file into the template's rootfs at /opt/marker.
	if err := d.BuildTemplate(ctx, d.cfg.RootfsPath, out, []string{
		"mkdir -p /opt && echo baked-by-builder > /opt/marker",
	}); err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("built rootfs missing: %v", err)
	}

	// Provision a sandbox FROM the built template rootfs via RootfsImage (the production path
	// — CreateSandboxFromTemplate threads the template's rootfs here) and read the baked file.
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "fromtmpl", TenantID: "t", RootfsImage: out}); err != nil {
		t.Fatalf("create from template: %v", err)
	}
	// Read the baked file via exec (file ops are confined to /workspace; the marker is baked
	// into the template rootfs at /opt, which a command can read).
	res, err := d.Exec(ctx, "fromtmpl", runtime.ExecRequest{Cmd: []string{"cat", "/opt/marker"}})
	if err != nil {
		t.Fatalf("exec cat marker: %v", err)
	}
	if got := string(bytes.TrimSpace(res.Stdout)); got != "baked-by-builder" {
		t.Errorf("baked marker = %q (exit %d, stderr %q), want 'baked-by-builder' (build step did NOT persist)", got, res.ExitCode, res.Stderr)
	}
	_ = d.Destroy(ctx, "fromtmpl")
}

//go:build firecracker

package firecracker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// TestFirecracker_pty_interactive: open a PTY into a real microVM guest, type a command,
// and read its output echoed back through the guest shell (M15).
func TestFirecracker_pty_interactive(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "pty1", TenantID: "t"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	p, err := d.OpenPTY(ctx, "pty1", nil)
	if err != nil {
		t.Fatalf("OpenPTY: %v", err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.Write([]byte("echo guest_pty_marker\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var acc strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		n, rerr := p.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			if strings.Contains(acc.String(), "guest_pty_marker") {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(acc.String(), "guest_pty_marker") {
		t.Errorf("guest PTY output missing marker; got %q", acc.String())
	}
}

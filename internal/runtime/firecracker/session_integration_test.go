//go:build firecracker

// Real code-interpreter session inside a Firecracker microVM guest (M12). Boots a real
// microVM, opens a Python session, and proves state is SHARED across cells in the actual
// guest — not just the hermetic host-python path.
package firecracker

import (
	"context"
	"strings"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

func TestFirecracker_session_shares_state_in_guest(t *testing.T) {
	d := testDriver(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, runtime.CreateRequest{SandboxID: "sess-1", TenantID: "t"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	const sid = "s1"
	if err := d.StartSession(ctx, "sess-1", sid); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if _, err := d.ExecSession(ctx, "sess-1", sid, "x = 41"); err != nil {
		t.Fatalf("cell1: %v", err)
	}
	res, err := d.ExecSession(ctx, "sess-1", sid, "print(x + 1)")
	if err != nil {
		t.Fatalf("cell2: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "42" {
		t.Errorf("guest cell2 stdout = %q, want 42 (state not shared in guest); stderr=%q", got, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if err := d.CloseSession(ctx, "sess-1", sid); err != nil {
		t.Errorf("CloseSession: %v", err)
	}
}

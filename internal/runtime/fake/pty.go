package fake

import (
	"context"
	"os"
	"os/exec"

	"github.com/creack/pty"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.PTYDriver = (*Driver)(nil)

// fakePTY wraps a real host PTY (creack/pty). TEST-ONLY (runs a shell on the host) — gated
// like sessions, so it is never a production path.
type fakePTY struct {
	f   *os.File
	cmd *exec.Cmd
}

func (p *fakePTY) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *fakePTY) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *fakePTY) Resize(rows, cols uint16) error {
	return pty.Setsize(p.f, &pty.Winsize{Rows: rows, Cols: cols})
}
func (p *fakePTY) Close() error {
	_ = p.f.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
	return nil
}

// OpenPTY starts an interactive shell/command under a real PTY on the host (test-only).
func (d *Driver) OpenPTY(_ context.Context, _ runtime.SandboxID, cmd []string) (runtime.PTY, error) {
	if !d.sessionsAllowed() {
		return nil, ErrFakeSessionsDisabled
	}
	if len(cmd) == 0 {
		cmd = []string{"sh"}
	}
	c := exec.Command(cmd[0], cmd[1:]...) //nolint:gosec // test-only, gated
	f, err := pty.Start(c)
	if err != nil {
		return nil, err
	}
	return &fakePTY{f: f, cmd: c}, nil
}

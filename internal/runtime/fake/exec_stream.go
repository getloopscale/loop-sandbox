package fake

import (
	"bufio"
	"context"
	"os/exec"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.StreamingExecDriver = (*Driver)(nil)

// ExecStream runs a real command and streams its stdout line-by-line as it runs (M15,
// test-only host execution — gated like sessions). Hermetic real evidence for the SSE path.
func (d *Driver) ExecStream(ctx context.Context, id runtime.SandboxID, req runtime.ExecRequest, onChunk func(runtime.StreamChunk)) (*runtime.ExecResult, error) {
	if !d.sessionsAllowed() {
		return nil, ErrFakeSessionsDisabled
	}
	if len(req.Cmd) == 0 {
		return &runtime.ExecResult{ExitCode: 127}, nil
	}
	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...) //nolint:gosec // test-only, gated
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		onChunk(runtime.StreamChunk{Stream: "stdout", Data: append(sc.Bytes(), '\n')})
	}
	exit := 0
	if werr := cmd.Wait(); werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			return nil, werr
		}
	}
	return &runtime.ExecResult{ExitCode: exit}, nil
}

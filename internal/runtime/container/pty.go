package container

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	dcontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.PTYDriver = (*Driver)(nil)

// containerPTY is a terminal in the sandbox. Output arrives raw; input and
// resizes go out as agentproto PTY frames. writeMu keeps a resize frame from
// landing in the middle of a data frame.
type containerPTY struct {
	att     types.HijackedResponse
	output  *io.PipeReader
	writeMu sync.Mutex
	once    sync.Once
}

func (p *containerPTY) Read(b []byte) (int, error) { return p.output.Read(b) }

func (p *containerPTY) Write(b []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := agentproto.WritePTYData(p.att.Conn, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resize sets the window size of the sandbox terminal.
func (p *containerPTY) Resize(rows, cols uint16) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return agentproto.WritePTYResize(p.att.Conn, rows, cols)
}

// Close ends the terminal. Closing the exec's streams makes the connect command
// exit, the agent sees its socket close and ends the shell.
func (p *containerPTY) Close() error {
	p.once.Do(func() {
		p.att.Close()
		_ = p.output.Close()
	})
	return nil
}

// OpenPTY opens a shell under a pseudo-terminal in the sandbox, through the
// agent's terminal socket.
func (d *Driver) OpenPTY(ctx context.Context, id runtime.SandboxID, _ []string) (runtime.PTY, error) {
	if d.agentVolume == "" {
		return nil, ErrAgentDisabled
	}
	execID, err := d.cli.ContainerExecCreate(ctx, containerName(id), dcontainer.ExecOptions{
		Cmd:         []string{agentPath, "connect", "--socket", agentproto.ContainerPTYSocket},
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if errdefs.IsNotFound(err) {
		return nil, runtime.ErrSandboxNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pty connect: %w", err)
	}
	// The terminal outlives this call, so the attach must not be bound to ctx.
	att, err := d.cli.ContainerExecAttach(context.WithoutCancel(ctx), execID.ID, dcontainer.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("pty attach: %w", err)
	}
	// Docker multiplexes stdout and stderr on one stream; the terminal is on stdout.
	output, demuxed := io.Pipe()
	go func() {
		_, cerr := stdcopy.StdCopy(demuxed, io.Discard, att.Reader)
		_ = demuxed.CloseWithError(cerr)
	}()
	return &containerPTY{att: att, output: output}, nil
}

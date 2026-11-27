package container

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containerd/errdefs"
	dcontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// Where the agent lives inside a sandbox container.
const (
	agentDir     = "/opt/lsb"
	agentBinName = "lsb-agent"
	agentPath    = agentDir + "/" + agentBinName
	agentRunDir  = "/run/lsb"
)

// AgentBinEnv names the environment variable that holds the path of a static
// lsb-agent binary. When it is set, every sandbox runs the agent as its first
// process, which is what code sessions need.
const AgentBinEnv = "LOOPSANDBOX_AGENT_BIN"

// agentOpTimeout bounds a short exchange with the agent, such as opening or
// closing a session.
const agentOpTimeout = 30 * time.Second

// sessionCellTimeout bounds one cell of a code session.
const sessionCellTimeout = 5 * time.Minute

// ErrAgentDisabled is returned by an operation that needs the in-sandbox agent
// when the driver was started without one.
var ErrAgentDisabled = fmt.Errorf("%w: the container driver has no agent; set %s", runtime.ErrInvalidRequest, AgentBinEnv)

// EnableAgent publishes the lsb-agent binary at binPath in a Docker volume and
// makes every sandbox created from now on run it. The volume is named after the
// binary's hash, so a new agent version never changes what a running sandbox
// executes.
func (d *Driver) EnableAgent(ctx context.Context, binPath string) error {
	bin, err := os.ReadFile(binPath) //nolint:gosec // an operator-configured path
	if err != nil {
		return fmt.Errorf("read agent binary: %w", err)
	}
	sum := sha256.Sum256(bin)
	name := "loopsandbox-agent-" + hex.EncodeToString(sum[:8])
	if _, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name}); err != nil {
		return fmt.Errorf("create agent volume %s: %w", name, err)
	}
	if err := d.ensureImage(ctx, d.defaultImage); err != nil {
		return err
	}
	// A container that is never started: it exists so the Docker copy API can
	// reach the volume.
	helper, err := d.cli.ContainerCreate(ctx,
		&dcontainer.Config{Image: d.defaultImage, Cmd: []string{"true"}, NetworkDisabled: true},
		&dcontainer.HostConfig{
			NetworkMode: "none",
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: agentDir}},
		}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("create agent volume helper: %w", err)
	}
	defer func() {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), helper.ID, dcontainer.RemoveOptions{Force: true})
	}()

	stat, err := d.cli.ContainerStatPath(ctx, helper.ID, agentPath)
	switch {
	case err == nil && stat.Size == int64(len(bin)):
		// Published by an earlier start, or by another process of this deployment.
	case err == nil || errdefs.IsNotFound(err):
		archive, aerr := agentArchive(bin)
		if aerr != nil {
			return aerr
		}
		if err := d.cli.CopyToContainer(ctx, helper.ID, agentDir, archive, dcontainer.CopyToContainerOptions{}); err != nil {
			return fmt.Errorf("copy agent into volume %s: %w", name, err)
		}
	default:
		return fmt.Errorf("inspect agent volume %s: %w", name, err)
	}
	d.agentVolume = name
	return nil
}

// agentArchive is a tar with the agent binary as its only entry, executable by
// every user: a sandbox runs as nobody.
func agentArchive(bin []byte) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: agentBinName, Mode: 0o755, Size: int64(len(bin))}); err != nil {
		return nil, fmt.Errorf("agent archive: %w", err)
	}
	if _, err := tw.Write(bin); err != nil {
		return nil, fmt.Errorf("agent archive: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("agent archive: %w", err)
	}
	return &buf, nil
}

// CapabilityEnabled implements runtime.CapabilityGate: the driver has the
// session, terminal, process, log and watch methods, and they work only when
// sandboxes run the agent.
func (d *Driver) CapabilityEnabled(c runtime.Capability) bool {
	if c == runtime.CapSessions || c == runtime.CapPTY || c == runtime.CapProcesses || c == runtime.CapFileWatch || c == runtime.CapLogs {
		return d.agentVolume != ""
	}
	if c == runtime.CapNetwork {
		return d.networkEnabled()
	}
	return true
}

// agentRoundTrip sends one request to the sandbox's agent and returns its
// response. The transport is an exec of the agent's "connect" subcommand, which
// joins the exec's standard streams to the agent's unix socket.
func (d *Driver) agentRoundTrip(ctx context.Context, id runtime.SandboxID, req *agentproto.Request, timeout time.Duration) (*agentproto.Response, error) {
	if d.agentVolume == "" {
		return nil, ErrAgentDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	execID, err := d.cli.ContainerExecCreate(ctx, containerName(id), dcontainer.ExecOptions{
		Cmd:         []string{agentPath, "connect"},
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if errdefs.IsNotFound(err) {
		return nil, runtime.ErrSandboxNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("agent connect: %w", err)
	}
	att, err := d.cli.ContainerExecAttach(ctx, execID.ID, dcontainer.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("agent attach: %w", err)
	}
	defer att.Close()

	// Docker multiplexes stdout and stderr on one stream; the protocol is on stdout.
	frames, demuxed := io.Pipe()
	defer func() { _ = frames.Close() }()
	var stderr bytes.Buffer
	go func() {
		_, cerr := stdcopy.StdCopy(demuxed, &limitedBuffer{buf: &stderr, max: 4 << 10}, att.Reader)
		_ = demuxed.CloseWithError(cerr)
	}()

	type result struct {
		resp *agentproto.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if werr := agentproto.WriteFrame(att.Conn, req); werr != nil {
			done <- result{err: werr}
			return
		}
		resp, rerr := agentproto.ReadResponse(frames)
		done <- result{resp: resp, err: rerr}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			// The connect command explains on stderr why it could not reach the agent.
			return nil, fmt.Errorf("agent %s: %w (%s)", req.Op, r.err, clipErr(stderr.Bytes()))
		}
		return r.resp, nil
	case <-ctx.Done():
		att.Close()
		_ = frames.Close()
		<-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: agent %s exceeded %s", runtime.ErrTimeout, req.Op, timeout)
		}
		return nil, ctx.Err()
	}
}

// limitedBuffer keeps the first max bytes written to it and drops the rest.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

var _ runtime.SessionDriver = (*Driver)(nil)

// StartSession asks the sandbox's agent to start an interpreter for sessionID.
func (d *Driver) StartSession(ctx context.Context, id runtime.SandboxID, sessionID string) error {
	return d.sessionOp(ctx, id, &agentproto.Request{Op: agentproto.OpSessionStart, SessionID: sessionID})
}

// CloseSession ends the interpreter of sessionID.
func (d *Driver) CloseSession(ctx context.Context, id runtime.SandboxID, sessionID string) error {
	return d.sessionOp(ctx, id, &agentproto.Request{Op: agentproto.OpSessionClose, SessionID: sessionID})
}

func (d *Driver) sessionOp(ctx context.Context, id runtime.SandboxID, req *agentproto.Request) error {
	resp, err := d.agentRoundTrip(ctx, id, req, agentOpTimeout)
	if err != nil {
		return err
	}
	if resp.Err == agentproto.ErrNoInterpreter {
		return runtime.ErrNoInterpreter
	}
	if resp.Err != "" {
		return fmt.Errorf("container %s: %s", req.Op, resp.Err)
	}
	return nil
}

// ExecSession runs one cell in the session's interpreter, which keeps its state
// between cells.
func (d *Driver) ExecSession(ctx context.Context, id runtime.SandboxID, sessionID, code string) (*runtime.ExecResult, error) {
	resp, err := d.agentRoundTrip(ctx, id, &agentproto.Request{Op: agentproto.OpSessionExec, SessionID: sessionID, Code: code}, sessionCellTimeout)
	if err != nil {
		// The cell may still be running. End the interpreter, so a runaway cell
		// does not keep the CPU; the client opens a new session.
		if cerr := d.CloseSession(context.WithoutCancel(ctx), id, sessionID); cerr != nil {
			return nil, errors.Join(err, cerr)
		}
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("container session exec: %s", resp.Err)
	}
	return &runtime.ExecResult{ExitCode: resp.ExitCode, Stdout: resp.Stdout, Stderr: resp.Stderr}, nil
}

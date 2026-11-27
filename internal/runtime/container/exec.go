package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/containerd/errdefs"
	dcontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// defaultWorkDir is where user code and files live (exec-permitted tmpfs).
const defaultWorkDir = "/workspace"

// defaultExecTimeout bounds an exec when the caller sets none.
const defaultExecTimeout = 30 * time.Second

// maxExecOutput caps how much combined stdout+stderr is buffered in the control
// plane per exec, so untrusted code (`yes`, `cat /dev/zero`) cannot OOM the API —
// a cross-tenant availability protection. Output past this is truncated.
const maxExecOutput = 8 << 20 // 8 MiB

// maxStderrInErr bounds how much stderr is embedded in a wrapped error (avoid
// leaking large/verbatim output into logs).
const maxStderrInErr = 256

// Exec runs a command inside the sandbox container and returns stdout/stderr/exit.
// A non-zero exit is returned in ExecResult, not as a Go error (US-02).
func (d *Driver) Exec(ctx context.Context, id runtime.SandboxID, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	// Cap buffered output to protect the control plane from a hostile flood.
	var stdout, stderr bytes.Buffer
	exit, err := d.runExec(ctx, id, req, &stdout, &stderr, maxExecOutput+1)
	if err != nil {
		return nil, err
	}
	return &runtime.ExecResult{
		ExitCode: exit, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		Truncated: stdout.Len()+stderr.Len() > maxExecOutput,
	}, nil
}

// ExecStream runs a command and hands each piece of its output to onChunk as the
// command produces it. The result carries the exit code only: the output was
// already delivered, so nothing is buffered here.
func (d *Driver) ExecStream(ctx context.Context, id runtime.SandboxID, req runtime.ExecRequest, onChunk func(runtime.StreamChunk)) (*runtime.ExecResult, error) {
	exit, err := d.runExec(ctx, id, req, chunkWriter{"stdout", onChunk}, chunkWriter{"stderr", onChunk}, 0)
	if err != nil {
		return nil, err
	}
	return &runtime.ExecResult{ExitCode: exit}, nil
}

// chunkWriter turns each write of one output stream into a StreamChunk.
type chunkWriter struct {
	stream string
	emit   func(runtime.StreamChunk)
}

func (w chunkWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		// The caller may keep the chunk; p is reused by the copier.
		w.emit(runtime.StreamChunk{Stream: w.stream, Data: bytes.Clone(p)})
	}
	return len(p), nil
}

// runExec runs req in the container, copying its output to stdout and stderr, and
// returns the exit code. limit, when positive, stops reading the output after
// that many bytes of the multiplexed stream.
func (d *Driver) runExec(ctx context.Context, id runtime.SandboxID, req runtime.ExecRequest, stdout, stderr io.Writer, limit int64) (int, error) {
	if len(req.Cmd) == 0 {
		return 0, fmt.Errorf("%w: empty command", runtime.ErrInvalidRequest)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	wd := req.WorkingDir
	if wd == "" {
		wd = defaultWorkDir
	}
	env := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}

	execID, err := d.cli.ContainerExecCreate(ctx, containerName(id), dcontainer.ExecOptions{
		Cmd: req.Cmd, Env: env, WorkingDir: wd,
		AttachStdin: req.Stdin != nil, AttachStdout: true, AttachStderr: true,
	})
	if errdefs.IsNotFound(err) {
		return 0, runtime.ErrSandboxNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("exec create: %w", err)
	}
	att, err := d.cli.ContainerExecAttach(ctx, execID.ID, dcontainer.ExecAttachOptions{})
	if err != nil {
		return 0, fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()

	// Pipe stdin (if any) CONCURRENTLY with reading output, so a command that
	// interleaves reads/writes cannot deadlock (stdout pipe filling while we block
	// writing stdin). A stdin pipe-closed-early is non-fatal: the process result
	// still stands.
	if req.Stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, req.Stdin)
			_ = att.CloseWrite()
		}()
	}

	// Read in a goroutine and select on ctx so a hung command is bounded by the
	// timeout: the hijacked stream read does NOT honor ctx on its own, so on
	// deadline we close the attach to unblock and return a timeout error.
	var output io.Reader = att.Reader
	if limit > 0 {
		output = io.LimitReader(att.Reader, limit)
	}
	copied := make(chan error, 1)
	go func() { _, e := stdcopy.StdCopy(stdout, stderr, output); copied <- e }()
	select {
	case e := <-copied:
		if e != nil {
			return 0, fmt.Errorf("exec stream: %w", e)
		}
	case <-ctx.Done():
		att.Close() // unblock the blocked StdCopy read
		// Wait for the copier, so the writers are not touched after this returns.
		// Its error is the read we just interrupted.
		interrupted := <-copied
		_ = interrupted
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, fmt.Errorf("%w: exec exceeded %s", runtime.ErrTimeout, timeout)
		}
		return 0, ctx.Err()
	}
	insp, err := d.cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return 0, fmt.Errorf("exec inspect: %w", err)
	}
	return insp.ExitCode, nil
}

// clipErr bounds stderr embedded in an error to avoid leaking large output to logs.
func clipErr(b []byte) string {
	if len(b) > maxStderrInErr {
		return string(b[:maxStderrInErr]) + "…"
	}
	return string(b)
}

// WriteFile writes r to path inside the container, creating the parent
// directories. It runs a script over exec-stdin rather than CopyToContainer,
// because Docker's copy mechanism refuses a read-only rootfs even for a writable
// tmpfs mount. The path is a positional argument of the script, so there is no
// shell interpolation.
func (d *Driver) WriteFile(ctx context.Context, id runtime.SandboxID, filePath string, r io.Reader) error {
	_, err := d.runFileScriptStdin(ctx, id, r, "write", writeScript, filePath)
	return err
}

// ReadFile returns the content of path inside the container. (docker cp is
// avoided: it refuses read-only rootfs even for tmpfs mounts.) A path that is
// missing or is not a regular file is ErrFileNotFound.
func (d *Driver) ReadFile(ctx context.Context, id runtime.SandboxID, filePath string) (io.ReadCloser, error) {
	res, err := d.runFileScript(ctx, id, "read", readScript, filePath)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(res.Stdout)), nil
}

// DeleteFile removes path inside the container. A missing path is not an error.
func (d *Driver) DeleteFile(ctx context.Context, id runtime.SandboxID, filePath string) error {
	_, err := d.runFileScript(ctx, id, "delete", deleteScript, filePath)
	return err
}

// ListFiles lists the immediate children of dirPath with their sizes, via an exec
// of listScript. A missing directory is ErrFileNotFound.
func (d *Driver) ListFiles(ctx context.Context, id runtime.SandboxID, dirPath string) ([]runtime.FileEntry, error) {
	res, err := d.runFileScript(ctx, id, "list", listScript, dirPath)
	if err != nil {
		return nil, err
	}
	return parseListing(string(res.Stdout)), nil
}

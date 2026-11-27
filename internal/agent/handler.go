// Package agent is the in-guest request handler for the Firecracker microVM
// sandbox. It serves the agentproto protocol over a vsock connection: exec (argv,
// no shell), and file ops confined to the workspace. The PID1 wrapper lives in
// cmd/lsb-agent; this package holds the testable logic (no VM required).
package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// workspaceLogical is the logical root every path is confined to (mapped to root).
const workspaceLogical = "/workspace"

// maxOutput caps captured stdout/stderr so untrusted code cannot OOM the agent.
const maxOutput = 8 << 20

// Handler serves agentproto requests. root is the on-disk directory the logical
// /workspace maps to (the real /workspace in-guest; a temp dir in tests).
type Handler struct {
	root     string
	sessions *sessions // M12 code-interpreter sessions (persistent kernels)
	procs    *processes
	watches  *watches
	// mount and unmount attach the volume device at a directory. They are fields so
	// the path rules of the volume ops are testable without root or a block device.
	mount   func(device, dir string) error
	unmount func(dir string) error
}

// New returns a handler rooted at dir.
func New(root string) *Handler {
	return &Handler{root: root, sessions: newSessions(), procs: newProcesses(), watches: newWatches(), mount: mountExt4, unmount: unmountDir}
}

// CloseSessions terminates all live interpreter kernels (agent shutdown).
func (h *Handler) CloseSessions() {
	h.sessions.closeAll()
	h.procs.killAll()
	h.watches.closeAll()
}

// Serve reads requests from conn and writes responses until EOF or a write error.
func (h *Handler) Serve(conn io.ReadWriter) error {
	for {
		req, err := agentproto.ReadRequest(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if req.Op == agentproto.OpExecStream {
			// A streamed exec owns the rest of the connection.
			return h.execStream(req, conn)
		}
		resp := h.handle(req)
		if err := agentproto.WriteFrame(conn, resp); err != nil {
			return err
		}
	}
}

func (h *Handler) handle(req *agentproto.Request) *agentproto.Response {
	switch req.Op {
	case agentproto.OpPing:
		return &agentproto.Response{}
	case agentproto.OpExec:
		return h.exec(req)
	case agentproto.OpWrite:
		return h.write(req)
	case agentproto.OpRead:
		return h.read(req)
	case agentproto.OpDelete:
		return h.delete(req)
	case agentproto.OpList:
		return h.list(req)
	case agentproto.OpStat:
		return h.stat(req)
	case agentproto.OpMkdir:
		return h.mkdir(req)
	case agentproto.OpMove:
		return h.move(req)
	case agentproto.OpSessionStart:
		return h.sessions.start(req.SessionID)
	case agentproto.OpSessionExec:
		return h.sessions.exec(req.SessionID, req.Code)
	case agentproto.OpSessionClose:
		return h.sessions.close(req.SessionID)
	case agentproto.OpProcStart, agentproto.OpProcList, agentproto.OpProcOutput,
		agentproto.OpProcStdin, agentproto.OpProcCloseStdin, agentproto.OpProcSignal:
		return h.processOp(req)
	case agentproto.OpStats:
		return h.stats()
	case agentproto.OpLogs:
		return h.procs.logsAfter(max(req.After, 0), min(time.Duration(req.WaitMS)*time.Millisecond, maxOutputWait))
	case agentproto.OpWatchStart, agentproto.OpWatchEvents, agentproto.OpWatchStop:
		return h.watchOp(req)
	case agentproto.OpVolumeMount:
		return h.volumeOp(req.Volume, h.mountVolume)
	case agentproto.OpVolumeUnmount:
		return h.volumeOp(req.Volume, h.unmountVolume)
	default:
		return &agentproto.Response{Err: "unknown op: " + string(req.Op)}
	}
}

// localPath validates a logical /workspace path and maps it to the on-disk root.
// It defends both string-level traversal (via ValidateSandboxPath + a prefix guard)
// AND symlink traversal: any existing component that resolves (via EvalSymlinks)
// outside root is rejected, so user code cannot plant /workspace/evil -> /etc and
// read/write through it. (The read-only rootfs + the VM boundary already contain
// the blast radius; this closes the guest-local disclosure gap too.)
func (h *Handler) localPath(logical string) (string, error) {
	clean, err := domain.ValidateSandboxPath(logical)
	if err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(clean, workspaceLogical)
	local := filepath.Join(h.root, rel)
	if !underRoot(local, h.root) {
		return "", domain.ErrInvalidArgument
	}
	// Resolve the deepest existing ancestor and re-assert it stays under root, so a
	// symlink anywhere along the path cannot escape the workspace.
	probe := local
	for probe != h.root && len(probe) > len(h.root) {
		if resolved, rerr := filepath.EvalSymlinks(probe); rerr == nil {
			if !underRoot(resolved, h.root) {
				return "", domain.ErrInvalidArgument
			}
			break
		}
		probe = filepath.Dir(probe)
	}
	return local, nil
}

// underRoot reports whether p is root or lives strictly beneath it.
func underRoot(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// command builds the process of an exec request: argv without a shell, in the
// requested directory under the workspace, with the request's environment on top
// of the agent's.
func (h *Handler) command(ctx context.Context, req *agentproto.Request) (*exec.Cmd, error) {
	workdir := h.root
	if req.WorkDir != "" {
		wd, err := h.localPath(req.WorkDir)
		if err != nil {
			return nil, err
		}
		workdir = wd
	}
	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...) //nolint:gosec // argv, no shell
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(req.StdinB64) > 0 {
		cmd.Stdin = bytes.NewReader(req.StdinB64)
	}
	return cmd, nil
}

func (h *Handler) exec(req *agentproto.Request) *agentproto.Response {
	if len(req.Cmd) == 0 {
		return &agentproto.Response{ExitCode: 127, Stderr: []byte("empty command")}
	}
	cmd, err := h.command(context.Background(), req)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	var out, errb cappedBuffer
	out.limit, errb.limit = maxOutput, maxOutput
	cmd.Stdout, cmd.Stderr = &out, &errb

	exit := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			return &agentproto.Response{Err: "exec: " + err.Error()}
		}
	}
	return &agentproto.Response{
		ExitCode:  exit,
		Stdout:    out.Bytes(),
		Stderr:    errb.Bytes(),
		Truncated: out.truncated || errb.truncated,
	}
}

func (h *Handler) write(req *agentproto.Request) *agentproto.Response {
	local, err := h.localPath(req.Path)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	if err := os.WriteFile(local, req.Data, 0o644); err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	return &agentproto.Response{}
}

func (h *Handler) read(req *agentproto.Request) *agentproto.Response {
	local, err := h.localPath(req.Path)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	b, err := os.ReadFile(local)
	if err != nil {
		if os.IsNotExist(err) {
			return &agentproto.Response{Err: "not_found"}
		}
		return &agentproto.Response{Err: err.Error()}
	}
	return &agentproto.Response{Data: b}
}

func (h *Handler) delete(req *agentproto.Request) *agentproto.Response {
	local, err := h.localPath(req.Path)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	if err := os.Remove(local); err != nil && !os.IsNotExist(err) {
		return &agentproto.Response{Err: err.Error()}
	}
	return &agentproto.Response{}
}

func (h *Handler) list(req *agentproto.Request) *agentproto.Response {
	local, err := h.localPath(req.Path)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	ents, err := os.ReadDir(local)
	if err != nil {
		if os.IsNotExist(err) {
			return &agentproto.Response{Err: "not_found"}
		}
		return &agentproto.Response{Err: err.Error()}
	}
	out := make([]agentproto.FileEntry, 0, len(ents))
	for _, e := range ents {
		info, ierr := e.Info()
		var size int64
		if ierr == nil {
			size = info.Size()
		}
		out = append(out, agentproto.FileEntry{Name: e.Name(), Size: size, IsDir: e.IsDir()})
	}
	return &agentproto.Response{Entries: out}
}

// cappedBuffer accumulates bytes up to limit and drops the rest. It deliberately
// does NOT embed bytes.Buffer: embedding would promote bytes.Buffer.ReadFrom, and
// os/exec copies process output via io.Copy which prefers ReaderFrom — that path
// would bypass the Write cap entirely (the OOM guard would be a no-op). Implementing
// only Write forces every byte through the cap. It reports a full write (so the
// child is not disrupted with a short-write/EPIPE) and records truncation so the
// caller can surface it (ExecResult.Truncated).
type cappedBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
			c.truncated = true
			return len(p), nil // report full write; excess dropped
		}
		c.buf = append(c.buf, p...)
		return len(p), nil
	}
	if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

// Bytes returns the accumulated (capped) content. Len returns its length.
func (c *cappedBuffer) Bytes() []byte { return c.buf }
func (c *cappedBuffer) Len() int      { return len(c.buf) }

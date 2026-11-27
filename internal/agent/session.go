package agent

import (
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// kernelSource is the in-guest Python interpreter kernel, embedded so it travels with the
// agent binary (no separate rootfs injection).
//
//go:embed kernel.py
var kernelSource string

// StartSession launches a persistent interpreter for id (idempotent). Public entrypoint
// so an in-process driver (fake) can reuse the exact tested session logic.
func (h *Handler) StartSession(id string) error {
	if r := h.sessions.start(id); r.Err != "" {
		return fmt.Errorf("%s", r.Err)
	}
	return nil
}

// ExecSession runs one cell; returns stdout, stderr, exit (1 if the cell raised), and a
// Go error ONLY for a session/transport failure.
func (h *Handler) ExecSession(id, code string) (stdout, stderr []byte, exit int, err error) {
	r := h.sessions.exec(id, code)
	if r.Err != "" {
		return nil, nil, 0, fmt.Errorf("%s", r.Err)
	}
	return r.Stdout, r.Stderr, r.ExitCode, nil
}

// CloseSession terminates id's interpreter (idempotent).
func (h *Handler) CloseSession(id string) error {
	if r := h.sessions.close(id); r.Err != "" {
		return fmt.Errorf("%s", r.Err)
	}
	return nil
}

// CellTimeout bounds a single cell so a runaway (e.g. `while True: pass`) cannot hang the
// serving goroutine forever — on timeout the kernel is KILLED and the session invalidated.
// Package var so tests can shorten it.
var CellTimeout = 60 * time.Second

// kernelSession is one persistent interpreter process. A per-session mutex serializes
// cells (concurrent cells on one session MUST NOT interleave — M12 DoD).
type kernelSession struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

// guardCell arms a timer that kills the kernel after CellTimeout. It returns a stop func
// and a *atomic.Bool that reports whether the timeout fired (so the caller can map a
// read error to a typed timeout).
func guardCell(sess *kernelSession) (stop func(), tripped *atomic.Bool) {
	tripped = &atomic.Bool{}
	t := time.AfterFunc(CellTimeout, func() {
		tripped.Store(true)
		_ = sess.cmd.Process.Kill()
	})
	return func() { t.Stop() }, tripped
}

func (s *sessions) remove(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

// sessions holds the live kernels for a Handler. Guarded by mu.
type sessions struct {
	mu sync.Mutex
	m  map[string]*kernelSession
}

func newSessions() *sessions { return &sessions{m: map[string]*kernelSession{}} }

// pythonBin is the interpreter the kernel runs under (overridable for tests).
var pythonBin = "python3"

// startSession spawns a fresh kernel for id. Re-starting an existing id is idempotent
// (returns ok without a second process).
func (s *sessions) start(id string) *agentproto.Response {
	if id == "" {
		return &agentproto.Response{Err: "session: empty session id"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; ok {
		return &agentproto.Response{} // already running
	}
	cmd := exec.Command(pythonBin, "-c", kernelSource) //nolint:gosec // fixed interpreter + embedded kernel
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return &agentproto.Response{Err: "session: stdin pipe: " + err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &agentproto.Response{Err: "session: stdout pipe: " + err.Error()}
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return &agentproto.Response{Err: agentproto.ErrNoInterpreter}
		}
		return &agentproto.Response{Err: "session: start kernel: " + err.Error()}
	}
	s.m[id] = &kernelSession{cmd: cmd, stdin: stdin, stdout: stdout}
	return &agentproto.Response{}
}

type cellResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error"`
}

// exec runs one cell in the session's kernel and returns its output. State from prior
// cells is visible (shared namespace). A dead kernel is a typed error, never a hang.
func (s *sessions) exec(id, code string) *agentproto.Response {
	s.mu.Lock()
	sess := s.m[id]
	s.mu.Unlock()
	if sess == nil {
		return &agentproto.Response{Err: "session: unknown session " + id}
	}
	sess.mu.Lock() // serialize cells within one session
	defer sess.mu.Unlock()
	stop, tripped := guardCell(sess)
	defer stop()

	if err := writeKernelFrame(sess.stdin, map[string]string{"code": code}); err != nil {
		return &agentproto.Response{Err: "session: kernel write (died?): " + err.Error()}
	}
	var res cellResult
	if err := readKernelFrame(sess.stdout, &res); err != nil {
		if tripped.Load() {
			s.remove(id)
			return &agentproto.Response{Err: "session: cell exceeded timeout — kernel killed"}
		}
		return &agentproto.Response{Err: "session: kernel read (died?): " + err.Error()}
	}
	// A Python exception inside the cell is a NORMAL result carried in error/stderr — not
	// an agent-level failure. Exit code 1 signals the cell raised.
	exit := 0
	if res.Error != "" {
		exit = 1
	}
	return &agentproto.Response{
		ExitCode: exit,
		Stdout:   []byte(res.Stdout),
		Stderr:   []byte(res.Stderr + res.Error),
	}
}

type streamFrame struct {
	Type   string `json:"type"`
	Stream string `json:"stream"`
	Data   string `json:"data"`
	Stderr string `json:"stderr"`
	Error  string `json:"error"`
}

// execStream runs a cell in streaming mode: onChunk is called for each partial output as
// the cell runs; the returned response carries the final exit + trailing stderr/error.
func (s *sessions) execStream(id, code string, onChunk func(stream, data string)) *agentproto.Response {
	s.mu.Lock()
	sess := s.m[id]
	s.mu.Unlock()
	if sess == nil {
		return &agentproto.Response{Err: "session: unknown session " + id}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	stop, tripped := guardCell(sess)
	defer stop()

	if err := writeKernelFrame(sess.stdin, map[string]any{"code": code, "stream": true}); err != nil {
		return &agentproto.Response{Err: "session: kernel write (died?): " + err.Error()}
	}
	for {
		var f streamFrame
		if err := readKernelFrame(sess.stdout, &f); err != nil {
			if tripped.Load() {
				s.remove(id)
				return &agentproto.Response{Err: "session: cell exceeded timeout — kernel killed"}
			}
			return &agentproto.Response{Err: "session: kernel read (died?): " + err.Error()}
		}
		switch f.Type {
		case "out":
			onChunk(f.Stream, f.Data)
		case "done":
			exit := 0
			if f.Error != "" {
				exit = 1
			}
			return &agentproto.Response{ExitCode: exit, Stderr: []byte(f.Stderr + f.Error)}
		default:
			return &agentproto.Response{Err: "session: unexpected kernel frame type " + f.Type}
		}
	}
}

// ExecSessionStream is the public streaming entrypoint (in-process drivers reuse it).
func (h *Handler) ExecSessionStream(id, code string, onChunk func(stream, data string)) (stderr []byte, exit int, err error) {
	r := h.sessions.execStream(id, code, onChunk)
	if r.Err != "" {
		return nil, 0, fmt.Errorf("%s", r.Err)
	}
	return r.Stderr, r.ExitCode, nil
}

// close terminates the session's kernel. Idempotent.
func (s *sessions) close(id string) *agentproto.Response {
	s.mu.Lock()
	sess := s.m[id]
	delete(s.m, id)
	s.mu.Unlock()
	if sess == nil {
		return &agentproto.Response{} // already gone
	}
	_ = sess.stdin.Close()
	_ = sess.cmd.Process.Kill()
	_, _ = sess.cmd.Process.Wait()
	return &agentproto.Response{}
}

// CloseSessionsWithPrefix kills every kernel whose id starts with prefix (used to reap a
// sandbox's sessions on destroy).
func (h *Handler) CloseSessionsWithPrefix(prefix string) {
	s := h.sessions
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.m {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			_ = sess.stdin.Close()
			_ = sess.cmd.Process.Kill()
			_, _ = sess.cmd.Process.Wait()
			delete(s.m, id)
		}
	}
}

// closeAll kills every live kernel (agent shutdown).
func (s *sessions) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.m {
		_ = sess.stdin.Close()
		_ = sess.cmd.Process.Kill()
		_, _ = sess.cmd.Process.Wait()
		delete(s.m, id)
	}
}

func writeKernelFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readKernelFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > (16 << 20) { // sanity bound
		return fmt.Errorf("session: kernel frame too large (%d)", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

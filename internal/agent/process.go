//go:build unix

package agent

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// Limits of the background processes of one sandbox.
const (
	// maxRunningProcesses bounds how many started processes may be alive at once.
	maxRunningProcesses = 32
	// maxExitedProcesses is how many ended processes stay listed, newest first.
	maxExitedProcesses = 64
	// maxProcessOutput is how much output of one process is kept for readers. Older
	// output is dropped, and a reader whose cursor points into it is told so.
	maxProcessOutput = 1 << 20
	// maxOutputWait bounds how long one output request waits for new output.
	maxOutputWait = 30 * time.Second
	// processWaitDelay bounds the wait for output after the process itself ended,
	// in case a child it left behind still holds the pipes.
	processWaitDelay = 2 * time.Second
)

// signals are the signal names OpProcSignal accepts.
var signals = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2, "STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT,
}

// outputRecord is one write of a process to one of its output streams.
type outputRecord struct {
	stderr bool
	data   []byte
}

// process is a command started in the background. Its output is kept as a log of
// records addressed by a byte cursor that counts both streams, so a reader that
// reconnects asks for everything after the cursor it last saw.
type process struct {
	mu          sync.Mutex
	changed     *sync.Cond // output arrived or the process ended
	pid         int
	log         *sandboxLog // the sandbox-wide log this process also writes to
	cmd         []string
	startedAt   time.Time
	stdin       io.WriteCloser
	stdinClosed bool
	records     []outputRecord
	base        int64 // cursor of the first byte still kept
	total       int64 // cursor after the last byte written
	exited      bool
	exitCode    int
}

// streamWriter appends what the process writes to one stream to its log.
type streamWriter struct {
	p      *process
	stderr bool
}

func (w streamWriter) Write(b []byte) (int, error) {
	p := w.p
	p.mu.Lock()
	defer p.mu.Unlock()
	data := append([]byte(nil), b...)
	p.records = append(p.records, outputRecord{stderr: w.stderr, data: data})
	if p.log != nil {
		p.log.append(p.pid, w.stderr, data)
	}
	p.total += int64(len(b))
	for p.total-p.base > maxProcessOutput && len(p.records) > 1 {
		p.base += int64(len(p.records[0].data))
		p.records = p.records[1:]
	}
	p.changed.Broadcast()
	return len(b), nil
}

func (p *process) info() *agentproto.ProcessInfo {
	return &agentproto.ProcessInfo{
		PID: p.pid, Cmd: p.cmd, StartedAt: p.startedAt.UnixMilli(),
		Running: !p.exited, ExitCode: p.exitCode,
	}
}

// outputAfter returns what the process wrote after the cursor, waiting up to wait
// for output or for the process to end when there is none yet.
func (p *process) outputAfter(after int64, wait time.Duration) *agentproto.Response {
	p.mu.Lock()
	defer p.mu.Unlock()
	if wait > 0 && p.total <= after && !p.exited {
		deadline := time.Now().Add(wait)
		timer := time.AfterFunc(wait, func() {
			p.mu.Lock()
			p.changed.Broadcast()
			p.mu.Unlock()
		})
		defer timer.Stop()
		for p.total <= after && !p.exited && time.Now().Before(deadline) {
			p.changed.Wait()
		}
	}
	resp := &agentproto.Response{Next: max(after, p.total), Process: p.info()}
	if after < p.base {
		resp.Truncated = true
		after = p.base
	}
	offset := p.base
	for _, rec := range p.records {
		end := offset + int64(len(rec.data))
		if end > after {
			data := rec.data[max(after-offset, 0):]
			if rec.stderr {
				resp.Stderr = append(resp.Stderr, data...)
			} else {
				resp.Stdout = append(resp.Stdout, data...)
			}
		}
		offset = end
	}
	return resp
}

// processes is the table of background processes of the sandbox.
type processes struct {
	mu  sync.Mutex
	m   map[int]*process
	log *sandboxLog
}

func newProcesses() *processes {
	return &processes{m: map[int]*process{}, log: newSandboxLog()}
}

// sandboxLog is the output of every background process in the order it was
// written, addressed by a byte cursor like the output of one process. It outlives
// the processes, so the output of one that was forgotten is still here.
type sandboxLog struct {
	mu      sync.Mutex
	changed *sync.Cond
	records []agentproto.LogRecord
	base    int64
	total   int64
}

func newSandboxLog() *sandboxLog {
	l := &sandboxLog{}
	l.changed = sync.NewCond(&l.mu)
	return l
}

func (l *sandboxLog) append(pid int, stderr bool, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, agentproto.LogRecord{PID: pid, Stderr: stderr, Data: data, AtMS: time.Now().UnixMilli()})
	l.total += int64(len(data))
	for l.total-l.base > maxProcessOutput && len(l.records) > 1 {
		l.base += int64(len(l.records[0].Data))
		l.records = l.records[1:]
	}
	l.changed.Broadcast()
}

// logsAfter returns the records written after the cursor, waiting up to wait for
// one when there is none yet.
func (t *processes) logsAfter(after int64, wait time.Duration) *agentproto.Response {
	l := t.log
	l.mu.Lock()
	defer l.mu.Unlock()
	if wait > 0 && l.total <= after {
		deadline := time.Now().Add(wait)
		timer := time.AfterFunc(wait, func() {
			l.mu.Lock()
			l.changed.Broadcast()
			l.mu.Unlock()
		})
		defer timer.Stop()
		for l.total <= after && time.Now().Before(deadline) {
			l.changed.Wait()
		}
	}
	resp := &agentproto.Response{Next: max(after, l.total)}
	if after < l.base {
		resp.Truncated = true
		after = l.base
	}
	offset := l.base
	for _, rec := range l.records {
		end := offset + int64(len(rec.Data))
		if end > after {
			rec.Data = rec.Data[max(after-offset, 0):]
			resp.Logs = append(resp.Logs, rec)
		}
		offset = end
	}
	return resp
}

func (t *processes) get(pid int) *process {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[pid]
}

// admit registers p and forgets the oldest ended processes beyond the limit.
func (t *processes) admit(p *process) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[p.pid] = p // a reused PID replaces the ended process that had it
	var ended []*process
	for _, q := range t.m {
		q.mu.Lock()
		if q.exited {
			ended = append(ended, q)
		}
		q.mu.Unlock()
	}
	if len(ended) <= maxExitedProcesses {
		return
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].startedAt.Before(ended[j].startedAt) })
	for _, q := range ended[:len(ended)-maxExitedProcesses] {
		delete(t.m, q.pid)
	}
}

func (t *processes) running() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, q := range t.m {
		q.mu.Lock()
		if !q.exited {
			n++
		}
		q.mu.Unlock()
	}
	return n
}

func (t *processes) list() []agentproto.ProcessInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]agentproto.ProcessInfo, 0, len(t.m))
	for _, q := range t.m {
		q.mu.Lock()
		out = append(out, *q.info())
		q.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func (h *Handler) processOp(req *agentproto.Request) *agentproto.Response {
	switch req.Op {
	case agentproto.OpProcStart:
		return h.startProcess(req)
	case agentproto.OpProcList:
		return &agentproto.Response{Processes: h.procs.list()}
	}
	p := h.procs.get(req.PID)
	if p == nil {
		return &agentproto.Response{Err: agentproto.ErrNotFound}
	}
	switch req.Op {
	case agentproto.OpProcOutput:
		return p.outputAfter(max(req.After, 0), min(time.Duration(req.WaitMS)*time.Millisecond, maxOutputWait))
	case agentproto.OpProcStdin:
		return p.writeStdin(req.Data)
	case agentproto.OpProcCloseStdin:
		return p.closeStdin()
	case agentproto.OpProcSignal:
		return p.signal(req.Signal)
	default:
		return &agentproto.Response{Err: "unknown op: " + string(req.Op)}
	}
}

func (h *Handler) startProcess(req *agentproto.Request) *agentproto.Response {
	if len(req.Cmd) == 0 {
		return &agentproto.Response{Err: "empty command"}
	}
	if h.procs.running() >= maxRunningProcesses {
		return &agentproto.Response{Err: agentproto.ErrTooManyProcesses}
	}
	// The process outlives this request, so it is not bound to a request context.
	cmd, err := h.command(context.Background(), req)
	if err != nil {
		return &agentproto.Response{Err: err.Error()}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = processWaitDelay
	p := &process{cmd: req.Cmd, log: h.procs.log}
	p.changed = sync.NewCond(&p.mu)
	cmd.Stdout = streamWriter{p: p}
	cmd.Stderr = streamWriter{p: p, stderr: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return &agentproto.Response{Err: "process: stdin pipe: " + err.Error()}
	}
	p.stdin = stdin
	p.startedAt = time.Now()
	// Output is copied from the moment the process starts, and a record carries
	// the PID. Holding the lock until the PID is known makes the first write wait
	// for it.
	p.mu.Lock()
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return &agentproto.Response{Err: agentproto.ErrStartFailed, Stderr: []byte(err.Error())}
	}
	p.pid = cmd.Process.Pid
	p.mu.Unlock()
	started := p.info() // read before the waiter below may change it
	h.procs.admit(p)
	go func() {
		werr := cmd.Wait() // returns after the output was copied
		p.mu.Lock()
		p.exited = true
		p.exitCode = exitCodeOf(cmd, werr)
		p.stdinClosed = true
		p.changed.Broadcast()
		p.mu.Unlock()
	}()
	return &agentproto.Response{Process: started}
}

// exitCodeOf is the exit code of an ended command, with the shell's convention of
// 128 plus the signal number for one a signal ended.
func exitCodeOf(cmd *exec.Cmd, waitErr error) int {
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		return code
	}
	if waitErr != nil {
		return 1
	}
	return 0
}

func (p *process) writeStdin(data []byte) *agentproto.Response {
	p.mu.Lock()
	closed, exited := p.stdinClosed, p.exited
	p.mu.Unlock()
	switch {
	case exited:
		return &agentproto.Response{Err: agentproto.ErrProcessExited}
	case closed:
		return &agentproto.Response{Err: agentproto.ErrStdinClosed}
	}
	// Written outside the lock: a process that does not read would otherwise block
	// every other operation on it.
	if _, err := p.stdin.Write(data); err != nil {
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe) {
			return &agentproto.Response{Err: agentproto.ErrStdinClosed}
		}
		return &agentproto.Response{Err: "process: write stdin: " + err.Error()}
	}
	return &agentproto.Response{}
}

func (p *process) closeStdin() *agentproto.Response {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stdinClosed {
		p.stdinClosed = true
		_ = p.stdin.Close()
	}
	return &agentproto.Response{}
}

func (p *process) signal(name string) *agentproto.Response {
	sig, ok := signals[name]
	if !ok {
		return &agentproto.Response{Err: "unknown signal: " + name}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return &agentproto.Response{Err: agentproto.ErrProcessExited}
	}
	// The whole process group: a shell's children get the signal too.
	if err := syscall.Kill(-p.pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return &agentproto.Response{Err: "process: signal: " + err.Error()}
	}
	return &agentproto.Response{}
}

// killAll ends every background process. It is for the agent's shutdown.
func (t *processes) killAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, q := range t.m {
		_ = syscall.Kill(-q.pid, syscall.SIGKILL)
	}
}

// Package agentproto is the wire protocol shared by the in-guest agent
// (cmd/lsb-agent) and the Firecracker host driver (internal/runtime/firecracker).
// Frames are length-prefixed JSON: a 4-byte big-endian length followed by the
// JSON body. Binary file content rides in []byte fields (JSON base64).
package agentproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Op is a request opcode.
type Op string

const (
	OpPing Op = "ping"
	OpExec Op = "exec"
	// OpExecStream runs a command like OpExec and answers with several frames: one
	// Response with Stream set per piece of output, as the command produces it, then
	// a final Response with Stream empty that carries the exit code. It is the last
	// request of its connection: the agent kills the command if the host hangs up.
	OpExecStream Op = "exec_stream"
	OpWrite      Op = "write"
	OpRead       Op = "read"
	OpDelete     Op = "delete"
	OpList       Op = "list"
	// File operations beyond read and write: OpStat describes Path, OpMkdir creates
	// the directory Path with its parents, OpMove renames Path to To.
	OpStat  Op = "stat"
	OpMkdir Op = "mkdir"
	OpMove  Op = "move"
	// Stateful code-interpreter session ops (M12): a persistent in-guest kernel whose
	// namespace survives across cells.
	OpSessionStart Op = "session_start"
	OpSessionExec  Op = "session_exec"
	OpSessionClose Op = "session_close"
	// Durable volume ops: mount the volume block device at /workspace/<Volume>, or
	// unmount it. The host swaps the backing file of the device before and after.
	OpVolumeMount   Op = "volume_mount"
	OpVolumeUnmount Op = "volume_unmount"
	// Background process ops. OpProcStart starts Cmd and answers at once with
	// Process. The others name the process by PID: OpProcOutput returns the output
	// produced after the cursor After, waiting up to WaitMS for some; OpProcStdin
	// writes Data to its standard input and OpProcCloseStdin closes it; OpProcSignal
	// sends Signal to its process group. OpProcList answers with Processes.
	OpProcStart      Op = "proc_start"
	OpProcList       Op = "proc_list"
	OpProcOutput     Op = "proc_output"
	OpProcStdin      Op = "proc_stdin"
	OpProcCloseStdin Op = "proc_close_stdin"
	OpProcSignal     Op = "proc_signal"
	// Directory watch ops. OpWatchStart watches the directory Path, and the ones
	// under it when Recursive is set, and answers with WatchID. OpWatchEvents
	// returns the events after the cursor After, waiting up to WaitMS for one.
	// OpWatchStop ends the watch.
	OpWatchStart  Op = "watch_start"
	OpWatchEvents Op = "watch_events"
	OpWatchStop   Op = "watch_stop"
	// OpStats answers with Stats, the resource use of the whole sandbox. OpLogs
	// returns, in Logs, what the background processes wrote after the cursor
	// After, waiting up to WaitMS for some.
	OpStats Op = "stats"
	OpLogs  Op = "logs"
)

// Request is a host→agent command.
type Request struct {
	Op       Op                `json:"op"`
	Cmd      []string          `json:"cmd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	WorkDir  string            `json:"workdir,omitempty"`
	Path     string            `json:"path,omitempty"`
	Data     []byte            `json:"data,omitempty"`
	StdinB64 []byte            `json:"stdin,omitempty"`
	// Session ops (M12): SessionID identifies the persistent kernel; Code is the cell.
	SessionID string `json:"session_id,omitempty"`
	// Volume is the volume name for the volume ops; it names the mount directory.
	Volume string `json:"volume,omitempty"`
	Code   string `json:"code,omitempty"`
	// To is the destination of OpMove; Overwrite lets it replace an existing To.
	To        string `json:"to,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
	// Process ops: PID names the process, After and WaitMS are the output cursor
	// and how long to wait for new output, Signal is a name such as "TERM".
	PID    int    `json:"pid,omitempty"`
	After  int64  `json:"after,omitempty"`
	WaitMS int    `json:"wait_ms,omitempty"`
	Signal string `json:"signal,omitempty"`
	// Watch ops: Recursive extends OpWatchStart to subdirectories; WatchID names
	// the watch for the other two.
	Recursive bool   `json:"recursive,omitempty"`
	WatchID   string `json:"watch_id,omitempty"`
}

// Response.Err values of the watch ops.
const (
	ErrTooManyWatches = "too_many_watches"
	ErrNotDirectory   = "not_a_directory"
	// ErrInvalidPath means the path is not inside the workspace, directly or
	// through a symbolic link.
	ErrInvalidPath = "invalid_path"
)

// File event types. EventOverflow means events were lost and the client should
// list the directory again.
const (
	EventCreate   = "create"
	EventWrite    = "write"
	EventRemove   = "remove"
	EventRename   = "rename"
	EventChmod    = "chmod"
	EventOverflow = "overflow"
)

// Stats is the resource use of a sandbox as its agent sees it.
type Stats struct {
	CPUNanos    uint64 `json:"cpu_nanos"`    // CPU time used since the sandbox started
	MemoryUsed  uint64 `json:"memory_used"`  // bytes
	MemoryTotal uint64 `json:"memory_total"` // bytes
	Processes   int    `json:"processes"`
}

// LogRecord is one write of a background process to one of its output streams.
type LogRecord struct {
	PID    int    `json:"pid"`
	Stderr bool   `json:"stderr,omitempty"`
	Data   []byte `json:"data"`
	AtMS   int64  `json:"at_ms"` // milliseconds since the Unix epoch
}

// FileEvent is one change seen by a watch. Path is a sandbox path. A rename is
// reported as EventRename on the old path and EventCreate on the new one.
type FileEvent struct {
	Type  string `json:"type"`
	Path  string `json:"path,omitempty"`
	IsDir bool   `json:"is_dir,omitempty"`
}

// Response.Err values of the process ops.
const (
	ErrProcessExited    = "process_exited"
	ErrStdinClosed      = "stdin_closed"
	ErrTooManyProcesses = "too_many_processes"
	// ErrStartFailed means the command could not be started; Stderr says why.
	ErrStartFailed = "start_failed"
)

// ProcessInfo describes a background process on the wire.
type ProcessInfo struct {
	PID       int      `json:"pid"`
	Cmd       []string `json:"cmd"`
	StartedAt int64    `json:"started_at"` // milliseconds since the Unix epoch
	Running   bool     `json:"running"`
	ExitCode  int      `json:"exit_code"`
}

// ErrNotFound and ErrExists are the Response.Err values of the file operations
// for a missing path and for a path that already exists.
const (
	ErrNotFound = "not_found"
	ErrExists   = "exists"
)

// ErrNoInterpreter is the Response.Err of OpSessionStart when the sandbox has no
// python3 to run the session with.
const ErrNoInterpreter = "no_interpreter"

// FileInfo is the answer to OpStat.
type FileInfo struct {
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	Mode    uint32 `json:"mode"`     // permission bits
	ModTime int64  `json:"mod_time"` // seconds since the Unix epoch
}

// FileEntry mirrors runtime.FileEntry on the wire.
type FileEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

// Response is an agent→host reply. Err (non-empty) signals an agent-side failure.
type Response struct {
	ExitCode  int         `json:"exit_code"`
	Stdout    []byte      `json:"stdout,omitempty"`
	Stderr    []byte      `json:"stderr,omitempty"`
	Data      []byte      `json:"data,omitempty"`
	Entries   []FileEntry `json:"entries,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	Err       string      `json:"err,omitempty"`
	// Info is the answer to OpStat.
	Info *FileInfo `json:"info,omitempty"`
	// Stream, in an answer to OpExecStream, marks a partial output frame: it names
	// the stream ("stdout" or "stderr") whose bytes are in Data.
	Stream string `json:"stream,omitempty"`
	// Process ops: Process answers OpProcStart and accompanies OpProcOutput,
	// Processes answers OpProcList, Next is the cursor after the returned output.
	Process   *ProcessInfo  `json:"process,omitempty"`
	Processes []ProcessInfo `json:"processes,omitempty"`
	Next      int64         `json:"next,omitempty"`
	// Watch ops: WatchID answers OpWatchStart; Events answers OpWatchEvents, with
	// Next and Truncated as for process output. Closed means the watch ended, as
	// when the watched directory was removed.
	WatchID string      `json:"watch_id,omitempty"`
	Events  []FileEvent `json:"events,omitempty"`
	Closed  bool        `json:"closed,omitempty"`
	// Stats answers OpStats. Logs answers OpLogs, with Next and Truncated as for
	// process output.
	Stats *Stats      `json:"stats,omitempty"`
	Logs  []LogRecord `json:"logs,omitempty"`
}

// maxFrame bounds a single frame so a corrupt/hostile length cannot allocate
// unbounded memory (32 MiB — matches the API file cap).
const maxFrame = 32 << 20

// ErrFrameTooLarge is returned when a declared frame length exceeds maxFrame.
var ErrFrameTooLarge = errors.New("agentproto: frame too large")

// WriteFrame writes v as a length-prefixed JSON frame.
func WriteFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("agentproto: marshal: %w", err)
	}
	if len(body) > maxFrame {
		return ErrFrameTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// readFrame reads one length-prefixed frame body.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, ErrFrameTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// ReadRequest reads and decodes one Request frame.
func ReadRequest(r io.Reader) (*Request, error) {
	body, err := readFrame(r)
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("agentproto: decode request: %w", err)
	}
	return &req, nil
}

// ReadResponse reads and decodes one Response frame.
func ReadResponse(r io.Reader) (*Response, error) {
	body, err := readFrame(r)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("agentproto: decode response: %w", err)
	}
	return &resp, nil
}

// ContainerSocket is the unix socket the agent of a container sandbox listens on.
// The host reaches it by running the agent's "connect" subcommand in an exec.
const ContainerSocket = "/run/lsb/agent.sock"

// ContainerPTYSocket is the unix socket of the framed terminal channel in a
// container sandbox, the counterpart of PTYGuestPort in a microVM.
const ContainerPTYSocket = "/run/lsb/pty.sock"

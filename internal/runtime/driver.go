// Package runtime defines the Driver port — the boundary between the domain and
// any concrete sandbox runtime (fake, container/gVisor, Firecracker microVM).
//
// The domain depends ONLY on this interface (DIP, TRD §4.4 / ADR-003). Concrete
// runtimes live in subpackages (fake/, and later container/, firecracker/) and are
// wired at the composition root. The interface intentionally avoids leaking any
// Docker/Kubernetes/Firecracker detail.
package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrInvalidRequest is a driver-level sentinel for a request a driver refuses on
// policy/validation grounds (e.g. a disallowed image). The application layer maps
// it to a client error (4xx), not an internal error — without importing any
// concrete driver (DIP).
var ErrInvalidRequest = errors.New("invalid runtime request")

// ErrFileNotFound is returned by ReadFile when the path does not exist. The
// application layer maps it to 404.
var ErrFileNotFound = errors.New("file not found")

// Errors of the background process operations.
var (
	// ErrProcessNotFound means the sandbox has no such process.
	ErrProcessNotFound = errors.New("process not found")
	// ErrProcessExited means the operation needs a running process.
	ErrProcessExited = errors.New("the process has ended")
	// ErrStdinClosed means the standard input of the process was closed.
	ErrStdinClosed = errors.New("the standard input of the process is closed")
	// ErrTooManyProcesses means the sandbox is at its limit of running processes.
	ErrTooManyProcesses = errors.New("too many running processes in the sandbox")
)

// ProcessSpec is a command to start in the background.
type ProcessSpec struct {
	Cmd        []string
	Env        map[string]string
	WorkingDir string
}

// ProcessInfo describes a background process. ExitCode is meaningful once
// Running is false; a process ended by a signal has 128 plus the signal number.
type ProcessInfo struct {
	PID       int
	Cmd       []string
	StartedAt time.Time
	Running   bool
	ExitCode  int
}

// ProcessOutput is a piece of a process's output. Next is the cursor to ask
// with for what follows. Truncated means output between the requested cursor
// and the returned bytes was no longer kept.
type ProcessOutput struct {
	Stdout    []byte
	Stderr    []byte
	Next      int64
	Truncated bool
	Process   ProcessInfo
}

// ProcessDriver is an optional Driver interface for commands that keep running
// after the request that started them. Only processes started through it are
// listed. The output cursor counts the bytes of both streams, so a client that
// lost its connection resumes from the last cursor it saw.
type ProcessDriver interface {
	StartProcess(ctx context.Context, id SandboxID, spec ProcessSpec) (*ProcessInfo, error)
	ListProcesses(ctx context.Context, id SandboxID) ([]ProcessInfo, error)
	// ProcessOutput returns the output after the cursor. With none yet, it waits up
	// to wait for some, or for the process to end.
	ProcessOutput(ctx context.Context, id SandboxID, pid int, after int64, wait time.Duration) (*ProcessOutput, error)
	WriteProcessStdin(ctx context.Context, id SandboxID, pid int, data []byte) error
	CloseProcessStdin(ctx context.Context, id SandboxID, pid int) error
	// SignalProcess sends the named signal ("TERM", "KILL", ...) to the process
	// and the processes it started.
	SignalProcess(ctx context.Context, id SandboxID, pid int, signal string) error
}

// Errors of the directory watch operations.
var (
	// ErrWatchNotFound means the sandbox has no such watch.
	ErrWatchNotFound = errors.New("watch not found")
	// ErrTooManyWatches means the sandbox is at its limit of watches.
	ErrTooManyWatches = errors.New("too many watches in the sandbox")
	// ErrNotDirectory means the path exists and is not a directory.
	ErrNotDirectory = errors.New("not a directory")
)

// FileEvent is one change seen by a directory watch. Type is "create", "write",
// "remove", "rename" or "chmod", or "overflow" when events were lost and the
// directory should be listed again. A rename is a "rename" of the old path and
// a "create" of the new one.
type FileEvent struct {
	Type  string
	Path  string
	IsDir bool
}

// FileEvents is a piece of a watch's events. Next is the cursor to ask with for
// what follows; Truncated means events before the returned ones were no longer
// kept; Closed means the watch ended and will report nothing more.
type FileEvents struct {
	Events    []FileEvent
	Next      int64
	Truncated bool
	Closed    bool
}

// WatchDriver is an optional Driver interface for watching a directory of the
// sandbox. Events are kept in the sandbox and read from a cursor, so a client
// that lost its connection misses nothing.
type WatchDriver interface {
	// StartWatch watches the directory path, and the ones under it when recursive
	// is set. It returns the id of the watch.
	StartWatch(ctx context.Context, id SandboxID, path string, recursive bool) (string, error)
	// WatchEvents returns the events after the cursor. With none yet, it waits up
	// to wait for one.
	WatchEvents(ctx context.Context, id SandboxID, watchID string, after int64, wait time.Duration) (*FileEvents, error)
	StopWatch(ctx context.Context, id SandboxID, watchID string) error
}

// SandboxMetrics is the resource use of one sandbox at a moment. CPUUsageNanos
// is cumulative since the sandbox started, so a rate is the difference between
// two samples divided by the time between them.
type SandboxMetrics struct {
	CPUUsageNanos    uint64
	MemoryUsedBytes  uint64
	MemoryLimitBytes uint64
	// DiskUsedBytes and DiskLimitBytes are the use and the size of the workspace.
	// Both are zero when the driver could not read them.
	DiskUsedBytes  uint64
	DiskLimitBytes uint64
	At             time.Time
}

// MetricsDriver is an optional Driver interface for the resource use of a sandbox.
type MetricsDriver interface {
	SandboxMetrics(ctx context.Context, id SandboxID) (*SandboxMetrics, error)
}

// LogEntry is one write of a background process to one of its output streams.
type LogEntry struct {
	PID    int
	Stream string // "stdout" or "stderr"
	Data   []byte
	At     time.Time
}

// SandboxLogs is a piece of the output of a sandbox's background processes, in
// the order it was written. Next and Truncated work as in ProcessOutput.
type SandboxLogs struct {
	Entries   []LogEntry
	Next      int64
	Truncated bool
}

// LogsDriver is an optional Driver interface for the combined output of the
// background processes of a sandbox.
type LogsDriver interface {
	// SandboxLogs returns the output after the cursor. With none yet, it waits up
	// to wait for some.
	SandboxLogs(ctx context.Context, id SandboxID, after int64, wait time.Duration) (*SandboxLogs, error)
}

// ErrNoInterpreter is returned when a code session is started in a sandbox whose
// image has no interpreter for it.
var ErrNoInterpreter = errors.New("the sandbox has no python3 interpreter")

// ErrFileExists is returned when a file operation would replace a path that
// already exists and was not allowed to. The application layer maps it to 409.
var ErrFileExists = errors.New("file exists")

// ErrSandboxNotFound is returned by exec/file ops when the sandbox does not exist
// at the runtime. The application layer maps it to 404.
var ErrSandboxNotFound = errors.New("sandbox not found at runtime")

// ErrTimeout is returned by Exec when the command exceeds its timeout. The
// application layer maps it to 504.
var ErrTimeout = errors.New("exec timed out")

// ErrVolumeConflict is returned when a volume operation conflicts with the state of
// the sandbox: it already has a volume in its single slot, or it is asked to fork
// while a volume is attached (two sandboxes cannot write one volume).
var ErrVolumeConflict = errors.New("volume conflict")

// SandboxID is the opaque runtime identity of a sandbox.
type SandboxID string

// State is the lifecycle state of a sandbox as seen by the runtime.
// It mirrors the domain state machine (TRD §5.4) at the runtime boundary.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateDestroyed State = "destroyed"
)

// CreateRequest carries the parameters to create a sandbox. M2 adds resource
// bounds and an optional OCI runtime selector (e.g. "runsc" for gVisor); the
// fake driver ignores the resource fields.
type CreateRequest struct {
	SandboxID   SandboxID
	TenantID    string // for per-tenant container labelling / reconciliation
	Image       string
	OCIRuntime  string // "" or "runc" → default; "runsc" → gVisor
	CPUNanos    int64  // NanoCPUs (1e9 = 1 core); 0 = unset
	MemoryBytes int64  // 0 = unset
	PidsLimit   int64  // 0 = unset
	// Volumes are durable volumes to attach at boot (M16). Firecracker attaches block
	// devices pre-boot (no hotplug), so a volume is mounted when a sandbox boots with it.
	Volumes []VolumeMount
	// RootfsImage, when set (M17 template provisioning), is a local rootfs image path the
	// microVM boots from instead of the driver's default rootfs — so a sandbox provisioned
	// from a built template runs that template's customized rootfs. Ignored by non-VM drivers.
	RootfsImage string
	// GPU, when non-empty (M19), requests GPU device passthrough (e.g. "1" or a GPU class).
	// The container driver maps it to `--gpus`; the sandbox is scheduled onto a GPU cell.
	GPU string
	// AllowOut and DenyOut are the sandbox's outbound rules, in the syntax
	// egress.ParseRules accepts. A driver without a sandbox network ignores them.
	AllowOut []string
	DenyOut  []string
}

// VolumeMount attaches a durable volume to a sandbox at boot (M16). The driver resolves ID
// to the volume's backing store and mounts it in the guest at /workspace/<Name>.
type VolumeMount struct {
	ID   string
	Name string
}

// Sandbox is the runtime's view of a created sandbox.
type Sandbox struct {
	ID       SandboxID
	TenantID string // populated by drivers that label per tenant (orphan reaping)
	State    State
}

// OrphanLister is an optional Driver capability: list managed runtime resources
// still present on the node, so the reconciler can reap ones with no DB record.
type OrphanLister interface {
	ListOrphans(ctx context.Context) ([]Sandbox, error)
}

// Driver is the port every runtime implements. M0 needs only the lifecycle triad.
type Driver interface {
	// Name identifies the driver (e.g. "fake", "container", "firecracker").
	Name() string
	// Create provisions a sandbox. It is the caller's job to ensure SandboxID is unique.
	Create(ctx context.Context, req CreateRequest) (*Sandbox, error)
	// Inspect returns the current runtime view of a sandbox.
	Inspect(ctx context.Context, id SandboxID) (*Sandbox, error)
	// Destroy tears a sandbox down. Destroy MUST be idempotent: destroying an
	// already-destroyed or unknown sandbox is not an error (the desired end state
	// is reached either way).
	Destroy(ctx context.Context, id SandboxID) error

	// Exec runs a non-interactive command inside the sandbox and returns its
	// stdout/stderr/exit code. A non-zero exit is NOT a Go error (process failure
	// is distinct from transport failure — US-02); Go errors are reserved for the
	// exec itself failing to run.
	Exec(ctx context.Context, id SandboxID, req ExecRequest) (*ExecResult, error)

	// WriteFile writes r to path inside the sandbox (path already validated by the
	// caller). ReadFile returns the content; DeleteFile removes it; ListFiles lists
	// a directory. The M3 file operations (TRD §8.3).
	WriteFile(ctx context.Context, id SandboxID, path string, r io.Reader) error
	ReadFile(ctx context.Context, id SandboxID, path string) (io.ReadCloser, error)
	DeleteFile(ctx context.Context, id SandboxID, path string) error
	ListFiles(ctx context.Context, id SandboxID, path string) ([]FileEntry, error)
}

// SessionDriver is an OPTIONAL capability (ISP): a driver that supports stateful
// code-interpreter sessions (M12) — a persistent in-guest interpreter whose namespace
// survives across cells. The application type-asserts the driver to this interface;
// drivers that don't support sessions simply don't implement it.
type SessionDriver interface {
	// StartSession launches a persistent interpreter for (sandbox, session). Idempotent.
	StartSession(ctx context.Context, id SandboxID, sessionID string) error
	// ExecSession runs one cell in the session; prior cells' state is visible. A cell that
	// raises is a normal result (ExitCode 1 + traceback in Stderr), not a Go error; Go
	// errors are reserved for the session/transport failing.
	ExecSession(ctx context.Context, id SandboxID, sessionID, code string) (*ExecResult, error)
	// CloseSession terminates the session's interpreter. Idempotent.
	CloseSession(ctx context.Context, id SandboxID, sessionID string) error
}

// ForkDriver is an OPTIONAL capability (ISP): a driver that can FORK a running sandbox —
// clone its current in-memory + disk state into a new, isolated sandbox (M13). Writes in
// the fork do not affect the parent and vice-versa.
type ForkDriver interface {
	Fork(ctx context.Context, parentID, forkID SandboxID) (*Sandbox, error)
}

// PauseDriver is an OPTIONAL capability (ISP): a driver that can pause a running sandbox —
// snapshot its memory + disk to DURABLE storage the driver owns and release the compute —
// then resume it later from that snapshot (M16). The driver owns the snapshot storage
// (keyed by sandbox id) so it survives the VM process being torn down.
type PauseDriver interface {
	// Pause snapshots the running sandbox to durable storage and stops its compute.
	Pause(ctx context.Context, id SandboxID) error
	// Resume restores a paused sandbox from its snapshot and resumes execution.
	Resume(ctx context.Context, id SandboxID) error
}

// InstanceScoped is an optional Driver interface for a driver whose sandboxes
// live on a host that several deployments may share. The composition root gives
// it the id of its deployment before the driver is used.
type InstanceScoped interface {
	SetInstance(id string)
}

// SnapshotDriver is an optional capability: a driver that can save the memory
// and disk state of a running sandbox under a name of the caller's choice and
// start any number of new sandboxes from it. The driver owns the storage of a
// snapshot, which outlives the sandbox it was taken from.
type SnapshotDriver interface {
	// CreateSnapshot saves the state of sandbox id as snapshotID and returns the
	// bytes it takes on disk. The sandbox keeps running.
	CreateSnapshot(ctx context.Context, id SandboxID, snapshotID string) (sizeBytes int64, err error)
	// RestoreSnapshot starts a new sandbox, newID, from snapshotID. It returns
	// ErrSnapshotNotFound when the driver has no such snapshot.
	RestoreSnapshot(ctx context.Context, snapshotID string, newID SandboxID) (*Sandbox, error)
	// DeleteSnapshot removes the storage of snapshotID. Sandboxes already started
	// from it keep running. Deleting a snapshot that does not exist is not an error.
	DeleteSnapshot(ctx context.Context, snapshotID string) error
}

// ErrSnapshotNotFound is returned when the driver has no snapshot with the id.
var ErrSnapshotNotFound = errors.New("snapshot not found at runtime")

// TemplateBuilder is an OPTIONAL capability (ISP): a driver that can build a custom template
// rootfs by running a spec's steps in an ISOLATED builder sandbox (never on the host), then
// persisting the result as a new rootfs image (M17). baseRootfs is the starting image; the
// built image is written to outRootfs; steps are shell commands run in order.
type TemplateBuilder interface {
	BuildTemplate(ctx context.Context, baseRootfs, outRootfs string, steps []string) error
}

// VolumeDriver is an OPTIONAL capability (ISP): a driver that manages durable named
// volumes — block storage whose data survives sandbox destroy and can be re-attached to a
// new sandbox (M16). The driver owns the volume's backing store (keyed by volume id).
type VolumeDriver interface {
	// AttachVolume mounts volume volID (label name, sizeGB) into the sandbox, creating its
	// backing store on first attach and reusing the existing data on re-attach.
	AttachVolume(ctx context.Context, id SandboxID, volID, name string, sizeGB int64) error
	// DetachVolume unmounts the volume from the sandbox; the backing store (data) is kept.
	DetachVolume(ctx context.Context, id SandboxID, volID string) error
	// DeleteVolumeData removes the volume's durable backing store entirely (on volume delete).
	DeleteVolumeData(ctx context.Context, volID string) error
}

// StreamChunk is one partial output emitted while a streaming cell runs.
type StreamChunk struct {
	Stream string // "stdout" | "stderr"
	Data   []byte
}

// StreamingSessionDriver is an OPTIONAL capability: a SessionDriver that can stream a
// cell's output incrementally (partial stdout AS THE CELL RUNS — M12).
type StreamingSessionDriver interface {
	SessionDriver
	// ExecSessionStream runs a cell, invoking onChunk for each partial output as it is
	// produced, and returns the final result (exit + trailing stderr) when the cell ends.
	ExecSessionStream(ctx context.Context, id SandboxID, sessionID, code string, onChunk func(StreamChunk)) (*ExecResult, error)
}

// ExecRequest describes a command to run inside a sandbox.
type ExecRequest struct {
	Cmd        []string          // argv (Cmd[0] is the binary)
	Env        map[string]string // additional environment
	WorkingDir string            // defaults to /workspace when empty
	Timeout    time.Duration     // 0 → caller/driver default
	Stdin      io.Reader         // optional stdin piped to the process
}

// ExecResult is the outcome of an Exec.
type ExecResult struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool // output exceeded the driver's buffer cap and was truncated
}

// FileEntry is one entry returned by ListFiles.
type FileEntry struct {
	Name  string
	Size  int64
	IsDir bool
}

// FileInfo describes one path inside a sandbox.
type FileInfo struct {
	Name  string // last element of the path
	Size  int64  // bytes; 0 for a directory
	IsDir bool
	// Mode holds the permission bits, as in chmod (for example 0o644).
	Mode    uint32
	ModTime time.Time
}

// FileOpsDriver is an OPTIONAL capability: a driver that can inspect, create
// directories and move paths, beyond the read, write, list and delete of Driver.
// Paths are already validated by the caller.
type FileOpsDriver interface {
	// StatFile describes path, or returns ErrFileNotFound.
	StatFile(ctx context.Context, id SandboxID, path string) (*FileInfo, error)
	// MakeDir creates the directory path and any missing parent. A directory that
	// already exists is not an error; a path that exists and is not a directory is
	// ErrFileExists.
	MakeDir(ctx context.Context, id SandboxID, path string) error
	// MoveFile renames from to to, creating the parents of to. A missing from is
	// ErrFileNotFound. When to exists, the move replaces it only if overwrite is
	// set, and is ErrFileExists otherwise.
	MoveFile(ctx context.Context, id SandboxID, from, to string, overwrite bool) error
}

// StreamingExecDriver is an OPTIONAL capability: a driver that can stream a command's
// output incrementally as it runs (M15 streamed exec), for SSE delivery.
type StreamingExecDriver interface {
	ExecStream(ctx context.Context, id SandboxID, req ExecRequest, onChunk func(StreamChunk)) (*ExecResult, error)
}

// PTY is an interactive pseudo-terminal channel into a sandbox: write bytes = keystrokes,
// read bytes = terminal output. Resize adjusts the window; Close ends the session.
type PTY interface {
	io.ReadWriteCloser
	Resize(rows, cols uint16) error
}

// BrowserSession is a computer-use handle to a sandbox's headless browser (M18): the primitives
// a computer-use agent needs. It is satisfied by the CDP client (internal/browser.Session).
type BrowserSession interface {
	Navigate(ctx context.Context, url string) error
	Screenshot(ctx context.Context) ([]byte, error)
	DOM(ctx context.Context) (string, error)
	Click(ctx context.Context, x, y float64) error
	Type(ctx context.Context, text string) error
	Scroll(ctx context.Context, x, y, dx, dy float64) error
	Close() error
}

// BrowserDriver is an OPTIONAL capability (ISP): a driver that runs a headless browser in the
// sandbox and opens a computer-use session against it (M18).
type BrowserDriver interface {
	OpenBrowser(ctx context.Context, id SandboxID) (BrowserSession, error)
}

// PTYDriver is an OPTIONAL capability: a driver that can open an interactive PTY into a
// running sandbox (M15). onExit (if the impl supports it) is out of band — callers just
// read until EOF.
type PTYDriver interface {
	OpenPTY(ctx context.Context, id SandboxID, cmd []string) (PTY, error)
}

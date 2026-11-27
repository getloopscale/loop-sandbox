// Package fake is an in-memory Driver implementation for tests and the M0 walking
// skeleton. It proves the domain → Driver boundary without any real runtime. M3
// adds a minimal in-memory filesystem and a tiny exec (echo/cat) so the API layer
// can be exercised without Docker.
package fake

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// Driver is a concurrency-safe in-memory runtime.
type Driver struct {
	mu            sync.Mutex
	sandboxes     map[runtime.SandboxID]runtime.State
	files         map[runtime.SandboxID]map[string][]byte
	dirs          map[runtime.SandboxID]map[string]bool // directories made with MakeDir
	createErr     error                                 // when set, Create fails (fault injection for tests)
	allowSessions bool                                  // opt-in for the host-executing session capability (test-only)
	// M16 durable volumes: volData is the backing store keyed by volume id — it is NOT
	// wiped when a sandbox is destroyed, so a volume's data survives destroy + re-attach.
	// mounts binds a sandbox's mount prefix ("/workspace/<name>") to a volume id; file ops under
	// that prefix are routed to volData instead of the ephemeral per-sandbox files.
	volData map[string]map[string][]byte
	mounts  map[runtime.SandboxID]map[string]string
	paused  map[runtime.SandboxID]bool // M16 pause/resume
	// snapshots holds the files of each named snapshot, keyed by snapshot id.
	snapshots map[string]map[string][]byte
	net       network
}

// New returns an empty fake driver. Sessions (which run code on the HOST) are DISABLED —
// use NewWithSessions() in tests that exercise the session path.
func New() *Driver {
	return &Driver{
		sandboxes: make(map[runtime.SandboxID]runtime.State),
		files:     make(map[runtime.SandboxID]map[string][]byte),
		dirs:      make(map[runtime.SandboxID]map[string]bool),
		volData:   make(map[string]map[string][]byte),
		mounts:    make(map[runtime.SandboxID]map[string]string),
		paused:    make(map[runtime.SandboxID]bool),
	}
}

// NewWithSessions returns a fake driver with the (host-executing, TEST-ONLY) session
// capability enabled. Never use in production — it runs untrusted code on the host.
func NewWithSessions() *Driver {
	d := New()
	d.allowSessions = true
	return d
}

// NewFailing returns a fake driver whose Create always fails with err.
func NewFailing(err error) *Driver {
	d := New()
	d.createErr = err
	return d
}

// Name implements runtime.Driver.
func (d *Driver) Name() string { return "fake" }

// Create records a sandbox in the running state, or fails if fault-injected.
func (d *Driver) Create(_ context.Context, req runtime.CreateRequest) (*runtime.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createErr != nil {
		return nil, d.createErr
	}
	d.sandboxes[req.SandboxID] = runtime.StateRunning
	d.files[req.SandboxID] = make(map[string][]byte)
	if d.net.enabled {
		d.setPolicy(req.SandboxID, runtime.NetworkPolicy{AllowOut: req.AllowOut, DenyOut: req.DenyOut})
	}
	return &runtime.Sandbox{ID: req.SandboxID, State: runtime.StateRunning}, nil
}

// Inspect returns the recorded state, or destroyed if unknown.
func (d *Driver) Inspect(_ context.Context, id runtime.SandboxID) (*runtime.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.sandboxes[id]
	if !ok {
		return &runtime.Sandbox{ID: id, State: runtime.StateDestroyed}, nil
	}
	return &runtime.Sandbox{ID: id, State: st}, nil
}

// Destroy is idempotent.
func (d *Driver) Destroy(_ context.Context, id runtime.SandboxID) error {
	d.closeSandboxSessions(id) // reap any interpreter kernels so they don't orphan
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sandboxes[id] = runtime.StateDestroyed
	delete(d.files, id)
	delete(d.dirs, id)
	return nil
}

// Exec runs a tiny built-in interpreter: `echo ARGS...` and `cat PATH`. Anything
// else exits 0 with empty output. Enough to exercise the API without Docker.
func (d *Driver) Exec(_ context.Context, id runtime.SandboxID, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(req.Cmd) == 0 {
		return &runtime.ExecResult{ExitCode: 127, Stderr: []byte("empty command")}, nil
	}
	switch req.Cmd[0] {
	case "echo":
		return &runtime.ExecResult{ExitCode: 0, Stdout: []byte(strings.Join(req.Cmd[1:], " ") + "\n")}, nil
	case "printenv": // printenv NAME: the value the command received, as the real one does
		if len(req.Cmd) == 2 {
			if v, ok := req.Env[req.Cmd[1]]; ok {
				return &runtime.ExecResult{ExitCode: 0, Stdout: []byte(v + "\n")}, nil
			}
			return &runtime.ExecResult{ExitCode: 1}, nil
		}
	case "cat":
		if len(req.Cmd) == 2 {
			if b, ok := d.files[id][req.Cmd[1]]; ok {
				return &runtime.ExecResult{ExitCode: 0, Stdout: append([]byte(nil), b...)}, nil
			}
			return &runtime.ExecResult{ExitCode: 1, Stderr: []byte("cat: no such file")}, nil
		}
	}
	return &runtime.ExecResult{ExitCode: 0}, nil
}

// WriteFile stores the file in memory.
func (d *Driver) WriteFile(_ context.Context, id runtime.SandboxID, path string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.backing(id, path)[path] = b
	return nil
}

// ReadFile returns the stored file or a not-found via the runtime sentinel.
func (d *Driver) ReadFile(_ context.Context, id runtime.SandboxID, path string) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.backing(id, path)[path]
	if !ok {
		return nil, runtime.ErrFileNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

// DeleteFile removes the file (idempotent).
func (d *Driver) DeleteFile(_ context.Context, id runtime.SandboxID, path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.backing(id, path), path)
	return nil
}

// ListFiles lists files under a prefix path.
func (d *Driver) ListFiles(_ context.Context, id runtime.SandboxID, path string) ([]runtime.FileEntry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []runtime.FileEntry
	for p, b := range d.backing(id, path) {
		if strings.HasPrefix(p, path) {
			out = append(out, runtime.FileEntry{Name: p, Size: int64(len(b))})
		}
	}
	return out, nil
}

// ProcessLocal reports that the fake's sandboxes live in this process's memory.
func (d *Driver) ProcessLocal() bool { return true }

var _ runtime.ProcessLocal = (*Driver)(nil)

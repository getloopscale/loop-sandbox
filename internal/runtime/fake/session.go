package fake

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/getloopscale/loop-sandbox/internal/agent"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// SessionDriver support (M12): the fake driver delegates to a real in-process agent
// handler, so sessions get GENUINE Python state-sharing (via python3) without a microVM —
// hermetic yet real. Sessions are namespaced by (sandbox, session) so distinct sandboxes
// never share a kernel.
//
// SECURITY: this runs untrusted code as `python3` ON THE HOST (no VM/seccomp/namespace).
// It is a TEST-ONLY affordance and is DISABLED by default so it can never be a production
// RCE path: sessions are refused unless the driver was built via NewWithSessions() (tests)
// or the explicit env opt-in is set. The default fake driver (New) refuses sessions.
var _ runtime.SessionDriver = (*Driver)(nil)

// ErrFakeSessionsDisabled is returned when session ops are attempted on a fake driver that
// did not opt into the (host-executing, test-only) session capability.
var ErrFakeSessionsDisabled = errors.New("fake driver runs code on the host — sessions are test-only and disabled")

func (d *Driver) sessionsAllowed() bool {
	return d.allowSessions || os.Getenv("LOOPSANDBOX_ALLOW_UNSAFE_FAKE_SESSIONS") == "1"
}

var (
	fakeAgentOnce sync.Once
	fakeAgent     *agent.Handler
)

func (d *Driver) handler() *agent.Handler {
	fakeAgentOnce.Do(func() { fakeAgent = agent.New("") })
	return fakeAgent
}

func sessKey(id runtime.SandboxID, sessionID string) string {
	return string(id) + "\x00" + sessionID
}

// StartSession launches a real interpreter for (sandbox, session).
func (d *Driver) StartSession(_ context.Context, id runtime.SandboxID, sessionID string) error {
	if !d.sessionsAllowed() {
		return ErrFakeSessionsDisabled
	}
	return d.handler().StartSession(sessKey(id, sessionID))
}

// ExecSession runs a cell with shared state.
func (d *Driver) ExecSession(_ context.Context, id runtime.SandboxID, sessionID, code string) (*runtime.ExecResult, error) {
	if !d.sessionsAllowed() {
		return nil, ErrFakeSessionsDisabled
	}
	stdout, stderr, exit, err := d.handler().ExecSession(sessKey(id, sessionID), code)
	if err != nil {
		return nil, err
	}
	return &runtime.ExecResult{ExitCode: exit, Stdout: stdout, Stderr: stderr}, nil
}

// CloseSession terminates the session's interpreter.
func (d *Driver) CloseSession(_ context.Context, id runtime.SandboxID, sessionID string) error {
	if !d.sessionsAllowed() {
		return nil // nothing was ever started
	}
	return d.handler().CloseSession(sessKey(id, sessionID))
}

// closeSandboxSessions terminates every kernel belonging to a sandbox (called on Destroy
// so a create→start-session→destroy loop cannot orphan host python processes).
func (d *Driver) closeSandboxSessions(id runtime.SandboxID) {
	if d.sessionsAllowed() {
		d.handler().CloseSessionsWithPrefix(string(id) + "\x00")
	}
}

var _ runtime.StreamingSessionDriver = (*Driver)(nil)

// ExecSessionStream streams a cell's output incrementally (real python chunks).
func (d *Driver) ExecSessionStream(_ context.Context, id runtime.SandboxID, sessionID, code string, onChunk func(runtime.StreamChunk)) (*runtime.ExecResult, error) {
	if !d.sessionsAllowed() {
		return nil, ErrFakeSessionsDisabled
	}
	stderr, exit, err := d.handler().ExecSessionStream(sessKey(id, sessionID), code, func(stream, data string) {
		onChunk(runtime.StreamChunk{Stream: stream, Data: []byte(data)})
	})
	if err != nil {
		return nil, err
	}
	return &runtime.ExecResult{ExitCode: exit, Stderr: stderr}, nil
}

// CapabilityEnabled reports which capabilities this fake instance actually serves.
// The session, streaming, PTY and browser paths run on the host, so they are off
// unless a test opted in; the browser additionally needs a CDP endpoint.
func (d *Driver) CapabilityEnabled(c runtime.Capability) bool {
	switch c {
	case runtime.CapSessions, runtime.CapSessionStream, runtime.CapExecStream, runtime.CapPTY:
		return d.sessionsAllowed()
	case runtime.CapBrowser:
		return d.sessionsAllowed() && os.Getenv("LOOPSANDBOX_FAKE_CDP") != ""
	default:
		return true
	}
}

var _ runtime.CapabilityGate = (*Driver)(nil)

package fake

import (
	"context"
	"errors"
	"os"

	"github.com/getloopscale/loop-sandbox/internal/browser"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// ErrFakeBrowserDisabled is returned when no CDP endpoint is configured for the fake browser.
var ErrFakeBrowserDisabled = errors.New("fake browser disabled: set LOOPSANDBOX_FAKE_CDP to a Chrome DevTools endpoint")

// OpenBrowser (M18) connects a computer-use session to a Chrome DevTools endpoint given by
// LOOPSANDBOX_FAKE_CDP (e.g. http://127.0.0.1:9222). This lets tests drive the full browser
// API surface against a REAL headless Chrome without a firecracker guest. Absent the env var it
// is disabled — the fake never launches a browser itself.
func (d *Driver) OpenBrowser(ctx context.Context, _ runtime.SandboxID) (runtime.BrowserSession, error) {
	// GATED like the other host-touching fake capabilities: connecting to a HOST Chrome whose
	// egress is not sandbox-controlled must never be reachable in production (M18 review HIGH).
	// Enabled only via NewWithSessions() (tests) or the explicit unsafe env opt-in.
	if !d.sessionsAllowed() {
		return nil, ErrFakeSessionsDisabled
	}
	base := os.Getenv("LOOPSANDBOX_FAKE_CDP")
	if base == "" {
		return nil, ErrFakeBrowserDisabled
	}
	return browser.Connect(ctx, base)
}

var _ runtime.BrowserDriver = (*Driver)(nil)

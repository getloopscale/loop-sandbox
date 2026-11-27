package app

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/egress"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// ErrBrowserUnsupported is returned when the runtime cannot run a headless browser (M18).
var ErrBrowserUnsupported = errors.New("browser/computer-use not supported by this runtime")

// browserSessions caches one computer-use session per sandbox (a browser is stateful: navigate
// then screenshot the SAME page). Guarded by its own mutex.
type browserSessions struct {
	mu sync.Mutex
	m  map[string]runtime.BrowserSession
}

// defaultEgress is the anti-SSRF policy applied to a navigate target host at the API boundary
// (defense-in-depth; the sandbox's network policy (M10) is the primary enforcement on the
// browser's actual traffic).
var defaultEgress, _ = egress.New()

// browser returns (opening if needed) the actor's browser session, after an ownership check.
func (s *Service) browser(ctx context.Context, actor Actor, sandboxID string) (runtime.BrowserSession, error) {
	if _, err := s.useSandbox(ctx, actor, sandboxID); err != nil {
		// The sandbox is gone (e.g. TTL-expired + reconciled) — reap any cached session so a
		// dead websocket doesn't leak (M18 review HIGH: reconciler destroy doesn't reach here).
		s.closeBrowser(sandboxID)
		return nil, err
	}
	bd, ok := s.driver.(runtime.BrowserDriver)
	ok = ok && s.supports(runtime.CapBrowser)
	if !ok {
		return nil, ErrBrowserUnsupported
	}
	s.browsers.mu.Lock()
	defer s.browsers.mu.Unlock()
	if s.browsers.m == nil {
		s.browsers.m = make(map[string]runtime.BrowserSession)
	}
	if sess, ok := s.browsers.m[sandboxID]; ok {
		return sess, nil
	}
	sess, err := bd.OpenBrowser(ctx, runtime.SandboxID(sandboxID))
	if err != nil {
		return nil, err
	}
	s.browsers.m[sandboxID] = sess
	return sess, nil
}

// closeBrowser tears down a sandbox's browser session (called on sandbox destroy).
func (s *Service) closeBrowser(sandboxID string) {
	s.browsers.mu.Lock()
	defer s.browsers.mu.Unlock()
	if sess, ok := s.browsers.m[sandboxID]; ok {
		_ = sess.Close()
		delete(s.browsers.m, sandboxID)
	}
}

// browserErr evicts the sandbox's session when an op fails (a broken CDP websocket must not
// stick in the cache — the next call re-opens a fresh one), then returns err unchanged.
func (s *Service) browserErr(sandboxID string, err error) error {
	if err != nil {
		s.closeBrowser(sandboxID)
	}
	return err
}

// BrowserNavigate loads url in the sandbox's browser (M18). The URL is anti-SSRF validated at
// the boundary; the sandbox network policy guards the actual fetch.
func (s *Service) BrowserNavigate(ctx context.Context, actor Actor, sandboxID, rawURL string) error {
	ctx, end := tracing.Start(ctx, "browser.navigate", actor.TenantID, sandboxID)
	defer end()
	if err := validateNavigateURL(rawURL); err != nil {
		return err
	}
	sess, err := s.browser(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	if err := s.browserErr(sandboxID, sess.Navigate(ctx, rawURL)); err != nil {
		return err
	}
	s.audit.Record(actor.TenantID, actor.KeyID, "browser.navigate", "sandbox", sandboxID, actor.RequestID, s.clock.Now())
	return nil
}

// BrowserScreenshot captures the current page as PNG (M18).
func (s *Service) BrowserScreenshot(ctx context.Context, actor Actor, sandboxID string) ([]byte, error) {
	sess, err := s.browser(ctx, actor, sandboxID)
	if err != nil {
		return nil, err
	}
	png, err := sess.Screenshot(ctx)
	return png, s.browserErr(sandboxID, err)
}

// BrowserDOM returns the current page's DOM snapshot (M18).
func (s *Service) BrowserDOM(ctx context.Context, actor Actor, sandboxID string) (string, error) {
	sess, err := s.browser(ctx, actor, sandboxID)
	if err != nil {
		return "", err
	}
	dom, err := sess.DOM(ctx)
	return dom, s.browserErr(sandboxID, err)
}

// BrowserClick dispatches a click at (x, y) (M18).
func (s *Service) BrowserClick(ctx context.Context, actor Actor, sandboxID string, x, y float64) error {
	sess, err := s.browser(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	return s.browserErr(sandboxID, sess.Click(ctx, x, y))
}

// BrowserType inserts text into the focused element (M18).
func (s *Service) BrowserType(ctx context.Context, actor Actor, sandboxID, text string) error {
	sess, err := s.browser(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	return s.browserErr(sandboxID, sess.Type(ctx, text))
}

// BrowserScroll scrolls the page (M18).
func (s *Service) BrowserScroll(ctx context.Context, actor Actor, sandboxID string, x, y, dx, dy float64) error {
	sess, err := s.browser(ctx, actor, sandboxID)
	if err != nil {
		return err
	}
	return s.browserErr(sandboxID, sess.Scroll(ctx, x, y, dx, dy))
}

// validateNavigateURL rejects non-http(s) schemes and literal-IP hosts in a blocked range
// (loopback / private / IMDS) — defense-in-depth anti-SSRF for the computer-use surface.
func validateNavigateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return domain.ErrInvalidArgument
	}
	// data: URLs are inert (no network) and useful for agents rendering local HTML — allow.
	if u.Scheme == "data" {
		return nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return domain.ErrInvalidArgument
	}
	host := strings.ToLower(u.Hostname())
	// Reject cloud metadata / obvious SSRF hostnames by NAME (the IP check below only catches
	// literals; a hostname resolving to IMDS/private is the classic bypass). This is
	// defense-in-depth at the boundary; enforced network isolation on the sandbox's browser
	// traffic is the durable control (M18 review HIGH).
	for _, blocked := range blockedNavigateHosts {
		if host == blocked || strings.HasSuffix(host, "."+blocked) {
			return domain.ErrInvalidArgument
		}
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if ok, _ := defaultEgress.AllowedIP(ip); !ok {
			return domain.ErrInvalidArgument
		}
	}
	return nil
}

// blockedNavigateHosts are metadata/link-local hostnames a computer-use navigate must never
// reach (SSRF to cloud IMDS / internal services).
var blockedNavigateHosts = []string{
	"metadata.google.internal", "metadata.goog", "metadata",
	"localhost", "instance-data",
}

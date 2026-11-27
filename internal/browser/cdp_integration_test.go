//go:build browser

package browser

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// launchChrome starts a real headless Chrome with remote debugging and returns its CDP base
// URL + a cleanup. It skips the test when no Chrome binary is available.
func launchChrome(t *testing.T) (string, func()) {
	t.Helper()
	var bin string
	for _, c := range []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(c); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		t.Skip("no chrome/chromium binary available")
	}
	port := freePort(t)
	dir, err := os.MkdirTemp("", "lsb-chrome-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	cmd := exec.Command(bin,
		"--headless=new", "--no-sandbox", "--disable-gpu",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+dir, "about:blank",
	)
	cmd.Stdout, cmd.Stderr = os.NewFile(0, os.DevNull), os.NewFile(0, os.DevNull)
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start chrome: %v", err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// Wait for the DevTools endpoint.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return base, func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(dir) // after chrome is dead so nothing holds the dir
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// TestBrowser_computer_use_against_real_chrome drives a REAL headless Chrome via CDP: navigate
// to a data URL, screenshot (real PNG), DOM snapshot (contains our marker), and type — proving
// the M18 computer-use primitives work against real Chrome DevTools Protocol.
func TestBrowser_computer_use_against_real_chrome(t *testing.T) {
	base, stop := launchChrome(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := Connect(ctx, base)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = sess.Close() }()

	const marker = "loopsandbox-computer-use"
	if err := sess.Navigate(ctx, "data:text/html,<html><body><h1>"+marker+"</h1><input id=x></body></html>"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// DOM snapshot must contain our marker.
	dom, err := sess.DOM(ctx)
	if err != nil {
		t.Fatalf("DOM: %v", err)
	}
	if !strings.Contains(dom, marker) {
		t.Errorf("DOM missing marker; got %.200s", dom)
	}

	// Screenshot must be a real PNG (magic bytes \x89PNG).
	png, err := sess.Screenshot(ctx)
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if len(png) < 8 || png[0] != 0x89 || png[1] != 'P' || png[2] != 'N' || png[3] != 'G' {
		t.Errorf("screenshot is not a PNG (len=%d, prefix=%x)", len(png), png[:min(8, len(png))])
	}

	// Input primitives must not error against a real page.
	if err := sess.Click(ctx, 10, 10); err != nil {
		t.Errorf("Click: %v", err)
	}
	if err := sess.Type(ctx, "hello"); err != nil {
		t.Errorf("Type: %v", err)
	}
	if err := sess.Scroll(ctx, 10, 10, 0, 100); err != nil {
		t.Errorf("Scroll: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestBrowser_concurrent_ops_no_mixup: fire many concurrent DOM+screenshot calls on ONE
// session; each must get its own correct response and there must be no data race (M18 review
// BLOCKER regression). Run with -race.
func TestBrowser_concurrent_ops_no_mixup(t *testing.T) {
	base, stop := launchChrome(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := Connect(ctx, base)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = sess.Close() }()
	const marker = "concurrent-marker-xyz"
	if err := sess.Navigate(ctx, "data:text/html,<html><body>"+marker+"</body></html>"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 12; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, e := sess.Screenshot(ctx); errs <- e }()
		go func() {
			defer wg.Done()
			dom, e := sess.DOM(ctx)
			if e == nil && !strings.Contains(dom, marker) {
				e = fmt.Errorf("DOM response mixup: missing marker in %.80s", dom)
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("concurrent op: %v", e)
		}
	}
}

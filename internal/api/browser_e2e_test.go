//go:build browser

package api_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// launchChromeAPI starts a real headless Chrome and points the fake driver at it via
// LOOPSANDBOX_FAKE_CDP, so the FULL browser API surface is exercised against real Chrome.
func launchChromeAPI(t *testing.T) func() {
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
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	dir, _ := os.MkdirTemp("", "lsb-chrome-api-")
	cmd := exec.Command(bin, "--headless=new", "--no-sandbox", "--disable-gpu",
		fmt.Sprintf("--remote-debugging-port=%d", port), "--remote-debugging-address=127.0.0.1",
		"--user-data-dir="+dir, "about:blank")
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start chrome: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Setenv("LOOPSANDBOX_FAKE_CDP", fmt.Sprintf("http://127.0.0.1:%d", port))
	return func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait(); _ = os.RemoveAll(dir) }
}

// TestE2E_browser_computer_use_against_real_chrome drives the browser API end-to-end against a
// REAL headless Chrome: navigate → DOM (marker) → screenshot (PNG over HTTP) → input (M18).
func TestE2E_browser_computer_use_against_real_chrome(t *testing.T) {
	stop := launchChromeAPI(t)
	defer stop()
	srv := newServer()
	id := createSandbox(t, srv, "key-a")

	const marker = "lsb-browser-api"
	if rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/browser/navigate", "key-a",
		[]byte(`{"url":"data:text/html,<html><body><h1>`+marker+`</h1></body></html>"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("navigate = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	// DOM snapshot over HTTP contains the marker.
	rec := req(srv, http.MethodGet, "/v1/sandboxes/"+id+"/browser/dom", "key-a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dom = %d, want 200", rec.Code)
	}
	var dom struct {
		DOM string `json:"dom"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &dom)
	if !strings.Contains(dom.DOM, marker) {
		t.Errorf("DOM missing marker; got %.200s", dom.DOM)
	}
	// Screenshot over HTTP is a real PNG.
	rec = req(srv, http.MethodGet, "/v1/sandboxes/"+id+"/browser/screenshot", "key-a", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("screenshot = %d ct=%s, want 200 image/png", rec.Code, rec.Header().Get("Content-Type"))
	}
	if b := rec.Body.Bytes(); len(b) < 8 || b[0] != 0x89 || b[1] != 'P' {
		t.Errorf("screenshot body is not a PNG (len=%d)", len(b))
	}
	// Input primitive (click) succeeds.
	if rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/browser/input", "key-a",
		[]byte(`{"action":"click","x":10,"y":10}`)); rec.Code != http.StatusNoContent {
		t.Errorf("click = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	// Cross-tenant navigate → not-found.
	if rec := req(srv, http.MethodPost, "/v1/sandboxes/"+id+"/browser/navigate", "key-b",
		[]byte(`{"url":"data:text/html,x"}`)); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant navigate = %d, want 404", rec.Code)
	}
}

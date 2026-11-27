package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestE2E_pty_interactive: open an interactive PTY over WebSocket, type a command, and see
// its output echoed back through the real host PTY (fake driver, gated test-only). M15.
func TestE2E_pty_interactive(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ts := httptest.NewServer(newServer())
	defer ts.Close()
	id := createSandboxHTTP(t, ts, "key-a")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/sandboxes/" + id + "/pty"
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer key-a"}},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	if err := conn.Write(ctx, websocket.MessageBinary, []byte("echo hello_pty_marker\n")); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	var acc strings.Builder
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
		_, data, rerr := conn.Read(rctx)
		rcancel()
		if rerr != nil {
			break
		}
		acc.Write(data)
		if strings.Contains(acc.String(), "hello_pty_marker") {
			break
		}
	}
	if !strings.Contains(acc.String(), "hello_pty_marker") {
		t.Errorf("PTY output missing echoed marker; got %q", acc.String())
	}
}

func createSandboxHTTP(t *testing.T, ts *httptest.Server, key string) string {
	t.Helper()
	r, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sandboxes", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+key)
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&b)
	if b.ID == "" {
		t.Fatal("no sandbox id")
	}
	return b.ID
}

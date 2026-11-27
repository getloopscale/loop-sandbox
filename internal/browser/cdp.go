// Package browser is a minimal Chrome DevTools Protocol (CDP) client that surfaces the
// computer-use primitives (navigate, screenshot, DOM snapshot, click, type, scroll) an agent
// needs to drive a headless Chromium inside a sandbox (M18). It speaks CDP over a WebSocket to
// a page target; it never launches or embeds a browser itself — the sandbox owns the browser.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Session is a CDP connection to a single page target.
type Session struct {
	conn *websocket.Conn
	seq  atomic.Int64
	// mu serializes CDP calls: coder/websocket forbids concurrent Reader, and a single reader
	// is required so a response frame is never stolen by a concurrent call waiting on a
	// different id (M18 review BLOCKER). One in-flight CDP call per session.
	mu sync.Mutex
}

// compile-time proof the CDP client satisfies the runtime computer-use port (M18). Declared
// via an interface literal to avoid importing runtime here (keeps browser dependency-free).
var _ interface {
	Navigate(context.Context, string) error
	Screenshot(context.Context) ([]byte, error)
	DOM(context.Context) (string, error)
	Click(context.Context, float64, float64) error
	Type(context.Context, string) error
	Scroll(context.Context, float64, float64, float64, float64) error
	Close() error
} = (*Session)(nil)

// cdpRequest / cdpResponse are the CDP JSON-RPC envelopes.
type cdpRequest struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

type cdpResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Connect opens a CDP session to a page target. cdpBase is the HTTP DevTools endpoint
// (e.g. http://127.0.0.1:9222); it creates a fresh page and connects to it.
func Connect(ctx context.Context, cdpBase string) (*Session, error) {
	wsURL, err := newPageWS(ctx, cdpBase)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp dial: %w", err)
	}
	// Cap a single CDP frame: a full-page screenshot/DOM is at most a few MiB; a low ceiling
	// bounds control-plane memory against a malicious page inflating the response (M18 review).
	conn.SetReadLimit(16 << 20)
	s := &Session{conn: conn}
	// Enable the domains we drive.
	for _, d := range []string{"Page.enable", "DOM.enable", "Runtime.enable"} {
		if _, err := s.call(ctx, d, nil); err != nil {
			_ = conn.Close(websocket.StatusInternalError, "enable failed")
			return nil, err
		}
	}
	return s, nil
}

// newPageWS asks the DevTools HTTP endpoint to open a new page and returns its WS debugger URL.
func newPageWS(ctx context.Context, cdpBase string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, cdpBase+"/json/new?about:blank", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Some Chrome builds only accept GET/PUT on /json/new; retry with GET.
		greq, _ := http.NewRequestWithContext(ctx, http.MethodGet, cdpBase+"/json/new?about:blank", nil)
		resp, err = http.DefaultClient.Do(greq)
		if err != nil {
			return "", fmt.Errorf("cdp new page: %w", err)
		}
	}
	defer func() { _ = resp.Body.Close() }()
	var target struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&target); err != nil {
		return "", fmt.Errorf("cdp target decode: %w", err)
	}
	if target.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("cdp: no webSocketDebuggerUrl in target")
	}
	return target.WebSocketDebuggerURL, nil
}

// call sends one CDP command and waits for the matching response, skipping event frames.
func (s *Session) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.seq.Add(1)
	if err := wsjson.Write(ctx, s.conn, cdpRequest{ID: id, Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("cdp write %s: %w", method, err)
	}
	for {
		var resp cdpResponse
		if err := wsjson.Read(ctx, s.conn, &resp); err != nil {
			return nil, fmt.Errorf("cdp read %s: %w", method, err)
		}
		if resp.ID != id {
			continue // an event or another command's reply — skip
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("cdp %s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// Close ends the session.
func (s *Session) Close() error { return s.conn.Close(websocket.StatusNormalClosure, "") }

// Navigate loads url in the page and waits for the page's load event (bounded), so a
// subsequent screenshot/DOM reflects the navigated page rather than stale/partial content
// (M18 review). Falls back to a short settle if the load event does not arrive in time.
func (s *Session) Navigate(ctx context.Context, url string) error {
	if _, err := s.call(ctx, "Page.navigate", map[string]any{"url": url}); err != nil {
		return err
	}
	return s.waitLoad(ctx)
}

// waitLoad blocks until Page.loadEventFired (or a bounded timeout). It holds the session lock
// for the duration so it is the single websocket reader (consistent with call()).
func (s *Session) waitLoad(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		var frame struct {
			Method string `json:"method"`
		}
		if err := wsjson.Read(lctx, s.conn, &frame); err != nil {
			// data: URLs and very fast loads may fire before we read; a timeout here is not
			// fatal — the navigation was already issued. Give a tiny settle and proceed.
			return nil
		}
		if frame.Method == "Page.loadEventFired" || frame.Method == "Page.frameStoppedLoading" {
			return nil
		}
	}
}

// Screenshot captures the page as PNG bytes.
func (s *Session) Screenshot(ctx context.Context) ([]byte, error) {
	res, err := s.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Data)
}

// DOM returns the page's outer HTML (a DOM snapshot). It uses a SINGLE Runtime.evaluate
// (document.documentElement.outerHTML) rather than the DOM.getDocument→getOuterHTML pair, so
// it is atomic — a concurrent DOM snapshot cannot invalidate a node id mid-sequence.
func (s *Session) DOM(ctx context.Context) (string, error) {
	res, err := s.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": "document.documentElement.outerHTML", "returnByValue": true,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", err
	}
	if out.ExceptionDetails != nil {
		return "", fmt.Errorf("cdp DOM eval: %s", out.ExceptionDetails.Text)
	}
	return out.Result.Value, nil
}

// Click dispatches a left mouse press+release at (x, y).
func (s *Session) Click(ctx context.Context, x, y float64) error {
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		if _, err := s.call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Type inserts text into the focused element.
func (s *Session) Type(ctx context.Context, text string) error {
	_, err := s.call(ctx, "Input.insertText", map[string]any{"text": text})
	return err
}

// Scroll scrolls the page by (dx, dy) at (x, y).
func (s *Session) Scroll(ctx context.Context, x, y, dx, dy float64) error {
	_, err := s.call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy,
	})
	return err
}

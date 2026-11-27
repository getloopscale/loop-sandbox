package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/getloopscale/loop-sandbox/internal/app"
)

const (
	// ptyReadLimit caps a single inbound WebSocket frame (keystrokes/resize control). A
	// terminal never legitimately sends a megabyte in one frame; the cap is defense-in-depth
	// against a memory-heavy frame from untrusted client code.
	ptyReadLimit = 1 << 20
	// ptyPingInterval / ptyPingTimeout evict half-open clients: a client that opened the PTY
	// then vanished (never sends a close, TCP silently dead) would otherwise pin a guest shell
	// + goroutines forever. A failed keepalive ping tears the whole session down.
	ptyPingInterval = 30 * time.Second
	ptyPingTimeout  = 10 * time.Second
)

// PTY handles GET (WebSocket upgrade) /v1/sandboxes/{id}/pty — an interactive terminal.
// Binary frames are raw terminal I/O (client→pty = keystrokes, pty→client = output); a
// text frame {"type":"resize","rows":R,"cols":C} resizes the window (M15).
func (h *Handler) PTY(w http.ResponseWriter, r *http.Request) {
	actor, ok := actorFrom(w, r)
	if !ok {
		return
	}
	pty, err := h.svc.OpenPTY(r.Context(), actor, r.PathValue("id"), nil)
	if err != nil {
		if errors.Is(err, app.ErrPTYUnsupported) {
			writeError(w, http.StatusNotImplemented, "not_implemented", "interactive PTY is not supported by this runtime")
			return
		}
		writeDomainError(w, err)
		return
	}
	defer func() { _ = pty.Close() }()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(ptyReadLimit)

	// One cancellable context shared by both pumps + the heartbeat. When ANY of them exits
	// (pty EOF, client close, dead keepalive) it cancels, collapsing the others — so a shell
	// that exits tears the socket down, and a dead client tears the shell down. No half of the
	// session can linger waiting on the other.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// pty → client (terminal output).
	go func() {
		defer cancel()
		buf := make([]byte, 4096)
		for {
			n, rerr := pty.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "eof")
				return
			}
		}
	}()

	// Heartbeat: evict a half-open client whose TCP is silently dead.
	go func() {
		defer cancel()
		t := time.NewTicker(ptyPingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, ptyPingTimeout)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					return
				}
			}
		}
	}()

	// client → pty (keystrokes + resize control).
	defer cancel()
	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			return
		}
		if typ == websocket.MessageText {
			var ctl struct {
				Type string `json:"type"`
				Rows uint16 `json:"rows"`
				Cols uint16 `json:"cols"`
			}
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" {
				_ = pty.Resize(ctl.Rows, ctl.Cols)
				continue
			}
		}
		if _, werr := pty.Write(data); werr != nil {
			return
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/coder/websocket"
	"golang.org/x/term"
)

// connect attaches the terminal of the caller to a shell in the sandbox. With a
// terminal on stdin it switches to raw mode and follows window resizes; with a
// pipe it sends the input and prints what the shell answers.
func (c *cli) connect(ctx context.Context, api *client, args []string) error {
	if len(args) != 1 {
		return usagef("sandbox connect needs exactly one sandbox")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, api.ptyURL(args[0]), &websocket.DialOptions{
		HTTPClient: api.http,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + api.key}},
	})
	if err != nil {
		return dialError(resp, err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(1 << 20)

	if f, ok := c.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fd := int(f.Fd())
		state, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("switch the terminal to raw mode: %w", err)
		}
		defer func() { _ = term.Restore(fd, state) }()
		resized := make(chan os.Signal, 1)
		signal.Notify(resized, syscall.SIGWINCH)
		defer signal.Stop(resized)
		resized <- syscall.SIGWINCH // send the current size once
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-resized:
					cols, rows, err := term.GetSize(fd)
					if err != nil {
						continue
					}
					msg, _ := json.Marshal(map[string]any{"type": "resize", "rows": rows, "cols": cols})
					if conn.Write(ctx, websocket.MessageText, msg) != nil {
						return
					}
				}
			}
		}()
	}

	// stdin to the shell. When stdin ends the shell keeps running until it exits
	// by itself, so piped input such as "exit" is what ends a scripted session.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := c.stdin.Read(buf)
			if n > 0 {
				if conn.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// The shell to stdout, until the server closes the connection.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure || errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("terminal connection lost: %w", err)
		}
		if _, err := c.stdout.Write(data); err != nil {
			return err
		}
	}
}

// dialError turns a refused WebSocket upgrade into the error the API answered.
func dialError(resp *http.Response, err error) error {
	if resp == nil {
		return fmt.Errorf("open the terminal: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	ae := &apiError{Status: resp.StatusCode, Code: "unknown", Message: string(raw)}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
		ae.Code, ae.Message = envelope.Error.Code, envelope.Error.Message
	}
	return ae
}

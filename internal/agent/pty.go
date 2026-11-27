//go:build linux

package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// ServePTY spawns an interactive shell under a PTY and pumps it over conn. It runs
// inside the guest and blocks until either side closes.
//
// Output goes to the client as raw bytes. Input arrives framed (agentproto PTY
// frames): data frames are written to the terminal and resize frames change its
// window size.
func ServePTY(conn io.ReadWriteCloser) error {
	cmd := exec.Command("/bin/sh") //nolint:gosec // interactive shell inside the isolated guest
	f, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	inputDone := make(chan error, 1)
	outputDone := make(chan struct{}, 1)
	go func() { inputDone <- pumpPTYInput(f, conn) }()
	go func() { _, _ = io.Copy(conn, f); outputDone <- struct{}{} }()
	select {
	case err := <-inputDone:
		return err
	case <-outputDone:
		return nil
	}
}

// pumpPTYInput applies the client's frames to the terminal until the client closes.
func pumpPTYInput(terminal *os.File, client io.Reader) error {
	for {
		kind, payload, err := agentproto.ReadPTYFrame(client)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read pty frame: %w", err)
		}
		switch kind {
		case agentproto.PTYFrameData:
			if _, err := terminal.Write(payload); err != nil {
				return nil // the shell exited
			}
		case agentproto.PTYFrameResize:
			rows, cols, derr := agentproto.DecodePTYResize(payload)
			if derr != nil {
				return derr
			}
			if err := pty.Setsize(terminal, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
				return fmt.Errorf("resize pty to %dx%d: %w", rows, cols, err)
			}
		default:
			return fmt.Errorf("unknown pty frame kind %d", kind)
		}
	}
}

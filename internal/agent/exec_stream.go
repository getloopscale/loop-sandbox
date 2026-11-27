package agent

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// killGrace is how long a killed command's descendants may keep its output pipes
// open before the agent stops waiting for them.
const killGrace = 2 * time.Second

// frameWriter sends each write of one output stream as a partial Response frame.
// The two streams of a command share the connection, so they share the mutex. A
// failed send cancels the command: nobody is left to read its output.
type frameWriter struct {
	mu     *sync.Mutex
	conn   io.Writer
	stream string
	cancel context.CancelFunc
}

func (w frameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := agentproto.WriteFrame(w.conn, &agentproto.Response{Stream: w.stream, Data: p}); err != nil {
		w.cancel()
		return 0, err
	}
	return len(p), nil
}

// execStream runs the command of req, sending its output to conn as it is
// produced and then a final frame with the exit code. When the host closes the
// connection first, the command and its process group are killed.
func (h *Handler) execStream(req *agentproto.Request, conn io.ReadWriter) error {
	var mu sync.Mutex
	final := func(resp *agentproto.Response) error {
		mu.Lock()
		defer mu.Unlock()
		return agentproto.WriteFrame(conn, resp)
	}
	if len(req.Cmd) == 0 {
		return final(&agentproto.Response{ExitCode: 127, Err: "empty command"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, err := h.command(ctx, req)
	if err != nil {
		return final(&agentproto.Response{Err: err.Error()})
	}
	cmd.Stdout = frameWriter{&mu, conn, "stdout", cancel}
	cmd.Stderr = frameWriter{&mu, conn, "stderr", cancel}
	ownProcessGroup(cmd)
	cmd.WaitDelay = killGrace

	// The host sends nothing more on this connection, so a read that returns means
	// it hung up (or broke the protocol): either way the command has no reader.
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancel()
	}()

	exit := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		switch {
		case ctx.Err() != nil:
			return ctx.Err() // the host is gone; there is nobody to answer
		case errors.As(err, &ee):
			exit = ee.ExitCode()
		default:
			return final(&agentproto.Response{Err: "exec: " + err.Error()})
		}
	}
	return final(&agentproto.Response{ExitCode: exit})
}

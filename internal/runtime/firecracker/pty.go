package firecracker

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

var _ runtime.PTYDriver = (*Driver)(nil)

// fcPTY is a terminal in the guest. Output arrives raw; input and resizes go out
// as agentproto PTY frames. writeMu keeps a resize frame from landing in the
// middle of a data frame.
type fcPTY struct {
	conn    net.Conn
	writeMu sync.Mutex
}

func (p *fcPTY) Read(b []byte) (int, error) { return p.conn.Read(b) }

func (p *fcPTY) Write(b []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := agentproto.WritePTYData(p.conn, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *fcPTY) Close() error { return p.conn.Close() }

// Resize sets the window size of the guest terminal.
func (p *fcPTY) Resize(rows, cols uint16) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return agentproto.WritePTYResize(p.conn, rows, cols)
}

// OpenPTY dials the guest agent's PTY channel and returns a bidirectional terminal.
func (d *Driver) OpenPTY(ctx context.Context, id runtime.SandboxID, _ []string) (runtime.PTY, error) {
	v, err := d.lookup(id)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", v.udsPath)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agentproto.PTYGuestPort); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Read the handshake line byte-by-byte so we never buffer (and lose) PTY bytes that
	// arrive right after the "OK\n".
	line, err := readLine(conn)
	if err != nil || len(line) < 2 || line[:2] != "OK" {
		_ = conn.Close()
		return nil, fmt.Errorf("pty vsock handshake failed: %q", line)
	}
	return &fcPTY{conn: conn}, nil
}

func readLine(conn net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 256 {
		if _, err := conn.Read(one); err != nil {
			return string(b), err
		}
		if one[0] == '\n' {
			break
		}
		b = append(b, one[0])
	}
	return string(b), nil
}

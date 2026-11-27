package egress

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// maxSniffBytes bounds what the proxy reads while looking for a host name. A
// TLS ClientHello and the headers of an HTTP request both fit with room to spare.
const maxSniffBytes = 16 << 10

// sniffHost reads the start of a client stream and returns the host name it asks
// for: the Host header on port 80, the TLS server name on port 443. It returns ""
// for any other port and whenever the stream is not what the port suggests.
// Whatever it consumes from r has to be replayed to the upstream by the caller.
func sniffHost(r io.Reader, port uint16) string {
	r = io.LimitReader(r, maxSniffBytes)
	switch port {
	case 80:
		req, err := http.ReadRequest(bufio.NewReader(r))
		if err != nil {
			return ""
		}
		if host, _, err := net.SplitHostPort(req.Host); err == nil {
			return host
		}
		return req.Host
	case 443:
		return tlsServerName(r)
	}
	return ""
}

var errHelloSeen = errors.New("egress: client hello read")

// tlsServerName lets crypto/tls parse the ClientHello and stops the handshake
// as soon as the server name is known. Nothing is written back to the client.
func tlsServerName(r io.Reader) string {
	var name string
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS10, // parse whatever the client sends; nothing is negotiated here
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			name = hello.ServerName
			return nil, errHelloSeen
		},
	}
	_ = tls.Server(readOnlyConn{r}, cfg).Handshake()
	return name
}

// readOnlyConn is the client stream as crypto/tls wants it, with writes discarded.
type readOnlyConn struct{ r io.Reader }

func (c readOnlyConn) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (readOnlyConn) Write(p []byte) (int, error)      { return len(p), nil }
func (readOnlyConn) Close() error                     { return nil }
func (readOnlyConn) LocalAddr() net.Addr              { return nil }
func (readOnlyConn) RemoteAddr() net.Addr             { return nil }
func (readOnlyConn) SetDeadline(time.Time) error      { return nil }
func (readOnlyConn) SetReadDeadline(time.Time) error  { return nil }
func (readOnlyConn) SetWriteDeadline(time.Time) error { return nil }

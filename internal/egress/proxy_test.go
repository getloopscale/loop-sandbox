package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxyRig is a Proxy in front of one upstream that records what it receives.
// Every dial lands on that upstream, and the address the proxy asked for is kept,
// so a test can use public addresses without leaving the machine.
type proxyRig struct {
	t        *testing.T
	proxy    *Proxy
	addr     string
	original netip.AddrPort

	mu       sync.Mutex
	dialed   []netip.AddrPort
	received []byte
	denials  []Denial
}

func newProxyRig(t *testing.T, rules *Rules, original string, lookup map[string][]string, configure ...func(*Proxy)) *proxyRig {
	t.Helper()
	rig := &proxyRig{t: t, original: netip.MustParseAddrPort(original)}

	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	go func() {
		for {
			c, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = c.Write([]byte("upstream-hello\n"))
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					rig.mu.Lock()
					rig.received = append(rig.received, buf[:n]...)
					rig.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()

	platform, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rig.proxy = &Proxy{
		Platform:     platform,
		SniffTimeout: 300 * time.Millisecond,
		OriginalDst:  func(net.Conn) (netip.AddrPort, error) { return rig.original, nil },
		Dial: func(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
			rig.mu.Lock()
			rig.dialed = append(rig.dialed, to)
			rig.mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, "tcp", upstream.Addr().String())
		},
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, a := range lookup[host] {
				out = append(out, netip.MustParseAddr(a))
			}
			if len(out) == 0 {
				return nil, errors.New("no such host")
			}
			return out, nil
		},
		OnDeny: func(d Denial) {
			rig.mu.Lock()
			rig.denials = append(rig.denials, d)
			rig.mu.Unlock()
		},
	}
	rig.proxy.SetRules(rules)
	for _, f := range configure {
		f(rig.proxy)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { _ = rig.proxy.Serve(l) }()
	rig.addr = l.Addr().String()
	return rig
}

func (r *proxyRig) connect() net.Conn {
	r.t.Helper()
	c, err := net.DialTimeout("tcp", r.addr, 2*time.Second)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return c
}

// reply returns the first line the client reads, or "" when the proxy closed
// the connection without relaying anything.
func reply(c net.Conn) string {
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	return string(buf[:n])
}

func (r *proxyRig) waitReceived(want string) string {
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.mu.Lock()
		got := string(r.received)
		r.mu.Unlock()
		if strings.Contains(got, want) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *proxyRig) snapshot() (dialed []netip.AddrPort, denials []Denial) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]netip.AddrPort(nil), r.dialed...), append([]Denial(nil), r.denials...)
}

const (
	publicA = "93.184.216.34"
	publicB = "151.101.0.223"
)

func TestProxyRelaysBothWaysToTheOriginalDestination(t *testing.T) {
	rig := newProxyRig(t, mustRules(t, nil, nil), publicA+":5432", nil)
	c := rig.connect()

	if got := reply(c); got != "upstream-hello\n" {
		t.Fatalf("client read %q, want the upstream greeting", got)
	}
	_, _ = c.Write([]byte("client-data"))
	if got := rig.waitReceived("client-data"); got != "client-data" {
		t.Errorf("upstream received %q", got)
	}
	if dialed, _ := rig.snapshot(); len(dialed) != 1 || dialed[0] != rig.original {
		t.Errorf("dialed %v, want the original destination %v", dialed, rig.original)
	}
}

func TestProxyClosesADeniedConnectionWithoutDialing(t *testing.T) {
	rig := newProxyRig(t, mustRules(t, nil, []string{publicA}), publicA+":5432", nil)
	c := rig.connect()

	if got := reply(c); got != "" {
		t.Errorf("a denied connection relayed %q", got)
	}
	dialed, denials := rig.snapshot()
	if len(dialed) != 0 {
		t.Errorf("a denied connection was dialed: %v", dialed)
	}
	if len(denials) != 1 || denials[0].Destination != rig.original || !strings.Contains(denials[0].Reason, "deny_out") {
		t.Errorf("denials = %+v, want one that names the destination and deny_out", denials)
	}
}

func TestProxyRefusesThePlatformRangesWithNoRules(t *testing.T) {
	rig := newProxyRig(t, mustRules(t, nil, nil), "169.254.169.254:80", nil)
	c := rig.connect()
	_, _ = c.Write([]byte("GET /latest/meta-data/ HTTP/1.1\r\nHost: 169.254.169.254\r\n\r\n"))

	if got := reply(c); got != "" {
		t.Errorf("the metadata endpoint relayed %q", got)
	}
	if dialed, _ := rig.snapshot(); len(dialed) != 0 {
		t.Errorf("the metadata endpoint was dialed: %v", dialed)
	}
}

func TestProxyAllowsAnHTTPHostAndDialsTheAddressItResolved(t *testing.T) {
	rules := mustRules(t, []string{"pypi.org"}, []string{denyAll})
	// The sandbox connected to publicA, but pypi.org resolves to publicB: the name
	// in the request is what was allowed, so the address the proxy resolves is used.
	rig := newProxyRig(t, rules, publicA+":80", map[string][]string{"pypi.org": {publicB}})
	c := rig.connect()
	request := "GET /simple/ HTTP/1.1\r\nHost: pypi.org\r\nUser-Agent: pip\r\n\r\n"
	_, _ = c.Write([]byte(request))

	if got := reply(c); got != "upstream-hello\n" {
		t.Fatalf("client read %q, want the upstream greeting", got)
	}
	if got := rig.waitReceived("pip\r\n\r\n"); got != request {
		t.Errorf("upstream received %q, want the whole request", got)
	}
	want := netip.MustParseAddrPort(publicB + ":80")
	if dialed, _ := rig.snapshot(); len(dialed) != 1 || dialed[0] != want {
		t.Errorf("dialed %v, want %v", dialed, want)
	}
}

func TestProxyDeniesAnHTTPHostOutsideAllowOut(t *testing.T) {
	rules := mustRules(t, []string{"pypi.org"}, []string{denyAll})
	rig := newProxyRig(t, rules, publicA+":80", map[string][]string{"example.com": {publicA}})
	c := rig.connect()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))

	if got := reply(c); got != "" {
		t.Errorf("a host outside allow_out relayed %q", got)
	}
	if _, denials := rig.snapshot(); len(denials) != 1 || denials[0].Host != "example.com" {
		t.Errorf("denials = %+v, want one that names example.com", denials)
	}
}

func TestProxyAllowsATLSServerNameAndRelaysTheHandshake(t *testing.T) {
	rules := mustRules(t, []string{"*.pythonhosted.org"}, []string{denyAll})
	rig := newProxyRig(t, rules, publicA+":443", map[string][]string{"files.pythonhosted.org": {publicB}})
	c := rig.connect()
	go func() {
		// The upstream is not a TLS server, so the handshake fails after the
		// ClientHello was relayed. Only the relay is under test.
		_ = tls.Client(c, &tls.Config{ServerName: "files.pythonhosted.org", MinVersion: tls.VersionTLS12}).Handshake()
	}()

	deadline := time.Now().Add(3 * time.Second)
	var received []byte
	for len(received) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		rig.mu.Lock()
		received = append([]byte(nil), rig.received...)
		rig.mu.Unlock()
	}
	if len(received) == 0 || received[0] != 0x16 {
		t.Fatalf("upstream did not receive the TLS handshake record; got %d bytes", len(received))
	}
	if !strings.Contains(string(received), "files.pythonhosted.org") {
		t.Error("the relayed ClientHello does not carry the server name")
	}
	want := netip.MustParseAddrPort(publicB + ":443")
	if dialed, _ := rig.snapshot(); len(dialed) != 1 || dialed[0] != want {
		t.Errorf("dialed %v, want %v", dialed, want)
	}
}

func TestProxyDeniesATLSServerNameOutsideAllowOut(t *testing.T) {
	rules := mustRules(t, []string{"pypi.org"}, []string{denyAll})
	rig := newProxyRig(t, rules, publicA+":443", map[string][]string{"example.com": {publicA}})
	c := rig.connect()

	err := tls.Client(c, &tls.Config{ServerName: "example.com", MinVersion: tls.VersionTLS12}).Handshake()
	if err == nil {
		t.Fatal("the handshake to a denied name succeeded")
	}
	dialed, denials := rig.snapshot()
	if len(dialed) != 0 || len(denials) != 1 || denials[0].Host != "example.com" {
		t.Errorf("dialed=%v denials=%+v, want no dial and one denial for example.com", dialed, denials)
	}
}

func TestProxyDeniesAnAllowedNameThatResolvesToAPlatformRange(t *testing.T) {
	rules := mustRules(t, []string{"pypi.org"}, []string{denyAll})
	rig := newProxyRig(t, rules, publicA+":80", map[string][]string{"pypi.org": {"10.0.0.7", "169.254.169.254"}})
	c := rig.connect()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: pypi.org\r\n\r\n"))

	if got := reply(c); got != "" {
		t.Errorf("a name that resolves to a private address relayed %q", got)
	}
	if dialed, _ := rig.snapshot(); len(dialed) != 0 {
		t.Errorf("dialed %v", dialed)
	}
}

func TestProxyDoesNotWaitForeverOnAServerFirstProtocol(t *testing.T) {
	// Port 443 with a client that sends nothing: the proxy gives up reading the
	// name and judges the connection by its address.
	rig := newProxyRig(t, mustRules(t, nil, nil), publicA+":443", nil)
	c := rig.connect()

	if got := reply(c); got != "upstream-hello\n" {
		t.Errorf("client read %q, want the upstream greeting", got)
	}
}

func TestProxyAppliesNewRulesToTheNextConnection(t *testing.T) {
	rig := newProxyRig(t, mustRules(t, nil, nil), publicA+":5432", nil)
	if got := reply(rig.connect()); got != "upstream-hello\n" {
		t.Fatalf("first connection read %q", got)
	}

	rig.proxy.SetRules(mustRules(t, nil, []string{denyAll}))

	if got := reply(rig.connect()); got != "" {
		t.Errorf("a connection made after the rules changed relayed %q", got)
	}
}

func TestProxyClosesTheClientWhenTheUpstreamCloses(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("bye"))
		_ = c.Close()
	}()
	rig := newProxyRig(t, mustRules(t, nil, nil), publicA+":5432", nil, func(p *Proxy) {
		p.Dial = func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", upstream.Addr().String())
		}
	})
	c := rig.connect()

	got, err := io.ReadAll(c)
	if err != nil || string(got) != "bye" {
		t.Errorf("read %q, %v; want bye and a clean end", got, err)
	}
}

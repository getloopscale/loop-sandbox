package egress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy is the enforcement point of a sandbox's outbound policy. The firewall of
// the sandbox's network namespace redirects every TCP connection to it; the proxy
// learns where the connection was going, reads the host name when the protocol
// carries one, asks Rules, and relays the connection or closes it.
//
// The fields are set before Serve and not changed after. SetRules may be called
// at any time and applies to the next connection.
type Proxy struct {
	// Platform holds the ranges no rule can open.
	Platform *Policy
	// OriginalDst returns where a redirected connection was going.
	OriginalDst func(net.Conn) (netip.AddrPort, error)
	// Dial opens the upstream connection. Nil dials TCP directly and refuses, at
	// connect time, an address in the platform ranges.
	Dial func(ctx context.Context, to netip.AddrPort) (net.Conn, error)
	// Lookup resolves a host name. Nil uses the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// OnDeny is told about each refused connection. Nil ignores them.
	OnDeny func(Denial)
	// SniffTimeout is how long the proxy waits for the client to say which host
	// it wants on ports 80 and 443. Zero means 3 seconds.
	SniffTimeout time.Duration

	rules atomic.Pointer[Rules]
}

// Denial describes one refused connection.
type Denial struct {
	Destination netip.AddrPort // where the sandbox tried to connect
	Host        string         // the name it asked for, when one was read
	Reason      string
}

const (
	defaultSniffTimeout = 3 * time.Second
	dialTimeout         = 10 * time.Second
)

// SetRules replaces the policy. Connections already relayed are not affected.
func (p *Proxy) SetRules(r *Rules) { p.rules.Store(r) }

// Serve accepts redirected connections until l is closed.
func (p *Proxy) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("egress proxy accept: %w", err)
		}
		go p.handle(conn)
	}
}

func (p *Proxy) handle(client net.Conn) {
	defer client.Close()

	dst, err := p.OriginalDst(client)
	if err != nil {
		p.deny(Denial{Reason: "original destination unknown: " + err.Error()})
		return
	}
	rules := p.rules.Load()
	if rules == nil {
		p.deny(Denial{Destination: dst, Reason: "no policy loaded"})
		return
	}

	// Whatever is read while looking for the host name is kept and sent upstream.
	var consumed bytes.Buffer
	host := ""
	if dst.Port() == 80 || dst.Port() == 443 {
		timeout := p.SniffTimeout
		if timeout == 0 {
			timeout = defaultSniffTimeout
		}
		_ = client.SetReadDeadline(time.Now().Add(timeout))
		host = sniffHost(io.TeeReader(client, &consumed), dst.Port())
		_ = client.SetReadDeadline(time.Time{})
	}

	decision := rules.Decide(p.Platform, dst.Addr(), host)
	if !decision.Allowed {
		p.deny(Denial{Destination: dst, Host: host, Reason: decision.Reason})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	target := dst
	if decision.ByDomain {
		addr, err := p.resolveAllowed(ctx, host)
		if err != nil {
			p.deny(Denial{Destination: dst, Host: host, Reason: err.Error()})
			return
		}
		target = netip.AddrPortFrom(addr, dst.Port())
	}

	upstream, err := p.dial(ctx, target)
	if err != nil {
		p.deny(Denial{Destination: dst, Host: host, Reason: "connect failed: " + err.Error()})
		return
	}
	defer upstream.Close()

	if consumed.Len() > 0 {
		if _, err := upstream.Write(consumed.Bytes()); err != nil {
			return
		}
	}
	relay(client, upstream)
}

// resolveAllowed resolves host and returns the first address outside the
// platform ranges. A name was allowed, so the proxy decides which address that
// name means; otherwise a sandbox could name an allowed host and connect anywhere.
func (p *Proxy) resolveAllowed(ctx context.Context, host string) (netip.Addr, error) {
	lookup := p.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		}
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, a := range addrs {
		if ok, _ := p.Platform.AllowedIP(a); ok {
			return a.Unmap(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%s resolves only to addresses in the platform ranges", host)
}

func (p *Proxy) dial(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, to)
	}
	d := net.Dialer{Control: p.Platform.ControlFunc()}
	return d.DialContext(ctx, "tcp", to.String())
}

func (p *Proxy) deny(d Denial) {
	if p.OnDeny != nil {
		p.OnDeny(d)
	}
}

// relay copies both directions until both ends are done. When one side stops
// sending, the other is told with a half-close, so a request followed by a
// response of unknown length still works.
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyAndCloseWrite(b, a) }()
	go func() { defer wg.Done(); copyAndCloseWrite(a, b) }()
	wg.Wait()
}

func copyAndCloseWrite(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if hc, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
		return
	}
	_ = dst.Close()
}

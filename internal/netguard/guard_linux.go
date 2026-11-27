package netguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/getloopscale/loop-sandbox/internal/egress"
)

// Run installs the firewall and serves the proxy until it fails. Refused
// connections are written to log as one JSON object per line.
func Run(cfg Config, log io.Writer) error {
	rules, err := egress.ParseRules(cfg.AllowOut, cfg.DenyOut)
	if err != nil {
		return fmt.Errorf("netguard: %w", err)
	}
	platform, err := egress.New()
	if err != nil {
		return fmt.Errorf("netguard: %w", err)
	}
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", ProxyPort))
	if err != nil {
		return fmt.Errorf("netguard: listen: %w", err)
	}
	if err := InstallFirewall(ProxyPort, cfg.DNS); err != nil {
		return err
	}

	dialer := &net.Dialer{Control: markedControl(platform)}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, netip.AddrPortFrom(cfg.DNS[0], 53).String())
		},
	}
	denials := json.NewEncoder(log)
	proxy := &egress.Proxy{
		Platform:    platform,
		OriginalDst: egress.OriginalDst,
		Dial: func(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", to.String())
		},
		Lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return resolver.LookupNetIP(ctx, "ip4", host)
		},
		OnDeny: func(d egress.Denial) {
			_ = denials.Encode(map[string]string{
				"event": "egress_denied", "destination": d.Destination.String(), "host": d.Host, "reason": d.Reason,
			})
		},
	}
	proxy.SetRules(rules)
	if _, err := fmt.Fprintln(log, ReadyLine); err != nil {
		return fmt.Errorf("netguard: write log: %w", err)
	}
	return proxy.Serve(l)
}

// markedControl marks the proxy's own sockets so the firewall leaves them alone,
// and still refuses, at connect time, an address in the platform ranges. The
// resolver is the one exception: it has to reach the configured DNS server.
func markedControl(platform *egress.Policy) func(network, address string, c syscall.RawConn) error {
	check := platform.ControlFunc()
	return func(network, address string, c syscall.RawConn) error {
		if !strings.HasPrefix(network, "udp") {
			if err := check(network, address, c); err != nil {
				return err
			}
		}
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(Mark))
		}); err != nil {
			return fmt.Errorf("netguard: socket control: %w", err)
		}
		if sockErr != nil {
			return errors.Join(errors.New("netguard: mark socket"), sockErr)
		}
		return nil
	}
}

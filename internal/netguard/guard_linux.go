package netguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

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
	var logMu sync.Mutex
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
			logMu.Lock()
			defer logMu.Unlock()
			_ = denials.Encode(DenialRecord{
				Event: DenialEvent, Time: time.Now().UTC().Format(time.RFC3339Nano),
				Destination: d.Destination.String(), Host: d.Host, Reason: d.Reason,
			})
		},
	}
	proxy.SetRules(rules)
	control, err := listenControl()
	if err != nil {
		return err
	}
	go serveControl(control, proxy)
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

func listenControl() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(ControlSocket), 0o700); err != nil {
		return nil, fmt.Errorf("netguard: control socket directory: %w", err)
	}
	_ = os.Remove(ControlSocket)
	l, err := net.Listen("unix", ControlSocket)
	if err != nil {
		return nil, fmt.Errorf("netguard: control socket: %w", err)
	}
	return l, nil
}

// serveControl applies each RulesRequest to the proxy. Invalid rules are refused
// and the rules in force stay as they were.
func serveControl(l net.Listener, proxy *egress.Proxy) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(controlTimeout))
			var req RulesRequest
			var resp RulesResponse
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				resp.Error = "malformed request: " + err.Error()
			} else if rules, err := egress.ParseRules(req.AllowOut, req.DenyOut); err != nil {
				resp.Error = err.Error()
			} else {
				proxy.SetRules(rules)
			}
			_ = json.NewEncoder(conn).Encode(resp)
		}()
	}
}

const controlTimeout = 5 * time.Second

// SetRules asks the guard running in this container to replace its rules.
func SetRules(req RulesRequest) error {
	conn, err := net.DialTimeout("unix", ControlSocket, controlTimeout)
	if err != nil {
		return fmt.Errorf("netguard: reach the guard: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(controlTimeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("netguard: send rules: %w", err)
	}
	var resp RulesResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("netguard: read answer: %w", err)
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	return nil
}

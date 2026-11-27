package egress_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/egress"
)

func mustPolicy(t *testing.T, cidrs ...string) *egress.Policy {
	t.Helper()
	p, err := egress.New(cidrs...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// The M10 acceptance criterion: the cloud metadata endpoint (IMDS) is blocked.
func TestPolicy_blocks_imds(t *testing.T) {
	p := mustPolicy(t)
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254"} {
		if ok, reason := p.AllowedAddr(ip); ok {
			t.Errorf("IMDS %s allowed, want blocked", ip)
		} else if !strings.Contains(reason, "IMDS") {
			t.Errorf("IMDS %s reason = %q, want IMDS", ip, reason)
		}
	}
}

func TestPolicy_blocks_private_loopback_linklocal(t *testing.T) {
	p := mustPolicy(t)
	for _, ip := range []string{
		"127.0.0.1", "::1", // loopback
		"10.1.2.3", "192.168.0.5", "172.16.0.1", // private v4
		"fd12:3456::1", // ULA private v6
		"169.254.1.1",  // link-local v4
		"fe80::1",      // link-local v6
		"224.0.0.1",    // multicast
		"0.0.0.0",      // unspecified
	} {
		if ok, _ := p.AllowedAddr(ip); ok {
			t.Errorf("%s allowed, want blocked", ip)
		}
	}
}

func TestPolicy_allows_public(t *testing.T) {
	p := mustPolicy(t)
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if ok, reason := p.AllowedAddr(ip); !ok {
			t.Errorf("public %s blocked (%s), want allowed", ip, reason)
		}
	}
}

func TestPolicy_extra_blocked_cidr(t *testing.T) {
	// A public control-plane/worker subnet blocked in addition to the defaults.
	p := mustPolicy(t, "198.51.100.0/24") // TEST-NET-2 (public, not a default-blocked range)
	if ok, reason := p.AllowedAddr("198.51.100.7"); ok {
		t.Errorf("control-plane range allowed, want blocked")
	} else if !strings.Contains(reason, "control-plane") {
		t.Errorf("reason = %q, want control-plane", reason)
	}
	// a public IP outside the extra range is still allowed.
	if ok, _ := p.AllowedAddr("8.8.8.8"); !ok {
		t.Error("public IP wrongly blocked by extra CIDR")
	}
}

// CGNAT (RFC-6598) is used inside AWS/EKS — must be blocked BY DEFAULT (HIGH finding).
func TestPolicy_blocks_cgnat_broadcast_thisnet(t *testing.T) {
	p := mustPolicy(t)
	for _, ip := range []string{
		"100.64.0.1", "100.127.255.254", // CGNAT
		"0.1.2.3",         // 0.0.0.0/8 this-network
		"255.255.255.255", // limited broadcast
	} {
		if ok, _ := p.AllowedAddr(ip); ok {
			t.Errorf("%s allowed, want blocked", ip)
		}
	}
}

// NAT64/6to4 must not smuggle a blocked IPv4 (e.g. IMDS) over IPv6.
func TestPolicy_blocks_embedded_ipv4_imds(t *testing.T) {
	p := mustPolicy(t)
	for _, ip := range []string{
		"64:ff9b::a9fe:a9fe", // NAT64 well-known prefix embedding 169.254.169.254 (IMDS)
		"64:ff9b::a00:1",     // NAT64 embedding 10.0.0.1 (private)
	} {
		if ok, reason := p.AllowedAddr(ip); ok {
			t.Errorf("NAT64 %s allowed, want blocked", ip)
		} else if !strings.Contains(reason, "embedded IPv4") {
			t.Errorf("NAT64 %s reason = %q, want embedded IPv4", ip, reason)
		}
	}
}

// RFC-1918 boundary edges prove the masks are exactly /8, /12, /16.
func TestPolicy_rfc1918_boundaries(t *testing.T) {
	p := mustPolicy(t)
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"9.255.255.255", false}, {"10.0.0.0", true}, {"10.255.255.255", true}, {"11.0.0.0", false},
		{"172.15.255.255", false}, {"172.16.0.0", true}, {"172.31.255.255", true}, {"172.32.0.0", false},
		{"192.167.255.255", false}, {"192.168.0.0", true}, {"192.168.255.255", true}, {"192.169.0.0", false},
	}
	for _, c := range cases {
		ok, _ := p.AllowedAddr(c.ip)
		if c.blocked && ok {
			t.Errorf("%s allowed, want blocked (boundary)", c.ip)
		}
		if !c.blocked && !ok {
			t.Errorf("%s blocked, want allowed (boundary)", c.ip)
		}
	}
}

// The ControlFunc must PERMIT a public destination (not over-block) and error on a
// malformed address — proving it does not deny legitimate egress.
func TestPolicy_controlfunc_allows_public_and_errors_malformed(t *testing.T) {
	p := mustPolicy(t)
	cf := p.ControlFunc()
	if err := cf("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("public dest control = %v, want nil (permit)", err)
	}
	if err := cf("tcp", "not-an-address", nil); err == nil {
		t.Error("malformed address control = nil, want error")
	}
}

// DNS-rebinding defense proven with a HOSTNAME: dialing "localhost" (which resolves
// to a loopback IP) through the policy dialer is refused at connect — the RESOLVED IP
// is checked, not the hostname. This is the raison d'être of ControlFunc.
func TestPolicy_dialer_refuses_hostname_resolving_to_blocked_ip(t *testing.T) {
	p := mustPolicy(t)
	d := &net.Dialer{Timeout: 2 * time.Second, Control: p.ControlFunc()}
	_, err := d.DialContext(context.Background(), "tcp", "localhost:1")
	if err == nil {
		t.Fatal("dial to localhost (→127.0.0.1) succeeded, want egress-denied")
	}
	if !strings.Contains(err.Error(), "egress denied") {
		t.Errorf("hostname-rebind dial err = %v, want egress denied", err)
	}
}

func TestPolicy_rejects_ipv4_mapped_imds(t *testing.T) {
	// an IPv4-mapped IPv6 form of IMDS must still be blocked (normalisation).
	p := mustPolicy(t)
	if ok, _ := p.AllowedAddr("::ffff:169.254.169.254"); ok {
		t.Error("IPv4-mapped IMDS allowed, want blocked")
	}
}

// DNS-rebinding defense proven end-to-end: a Dialer wired with the policy Control
// hook refuses to CONNECT to a private/IMDS address even when reached via dial —
// the resolved IP is checked at connect time, not the hostname.
func TestPolicy_dialer_refuses_blocked_ip(t *testing.T) {
	p := mustPolicy(t)
	d := &net.Dialer{Timeout: 2 * time.Second, Control: p.ControlFunc()}

	// dialing an IMDS/private IP must be refused by the Control hook before connect.
	for _, addr := range []string{"169.254.169.254:80", "127.0.0.1:1", "10.0.0.1:80"} {
		_, err := d.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			t.Errorf("dial %s succeeded, want egress-denied", addr)
			continue
		}
		if !strings.Contains(err.Error(), "egress denied") {
			t.Errorf("dial %s err = %v, want egress denied", addr, err)
		}
	}
}

// A real loopback listener is unreachable through the policy dialer (proves the hook
// actually gates a live connection, not just returns an error string).
func TestPolicy_dialer_blocks_live_loopback_listener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, e := ln.Accept(); e == nil {
			_ = c.Close()
		}
	}()

	p := mustPolicy(t)
	d := &net.Dialer{Timeout: 2 * time.Second, Control: p.ControlFunc()}
	if _, err := d.DialContext(context.Background(), "tcp", ln.Addr().String()); err == nil {
		t.Error("policy dialer connected to a loopback listener, want blocked")
	}
}

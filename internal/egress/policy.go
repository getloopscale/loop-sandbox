// Package egress enforces the sandbox's outbound-network policy (TRD §10.6): egress
// is deny-by-default; the policy blocks loopback, private/ULA ranges, link-local,
// multicast, and — critically — cloud metadata (AWS IMDS 169.254.169.254 /
// fd00:ec2::254) and the control-plane/worker addresses. DNS-rebinding is mitigated
// by re-checking the ACTUAL resolved IP at dial time (a hostname that resolves to a
// blocked IP is rejected), not merely the hostname.
package egress

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// imds are the cloud metadata endpoints an SSRF would target. Blocking these is the
// M10 acceptance criterion.
var imds = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"), // AWS/GCP/Azure IPv4 IMDS
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IPv6 IMDS
}

// defaultBlocked are ranges Go's netip helpers do NOT classify but which are still
// dangerous egress targets: CGNAT (RFC-6598, used inside AWS/EKS pod networking),
// the whole 0.0.0.0/8 "this network", and the limited broadcast address.
var defaultBlocked = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),      // CGNAT
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network"
	netip.MustParsePrefix("255.255.255.255/32"), // limited broadcast
}

// nat64 is the well-known NAT64 prefix; 6to4 embeds an IPv4 after 2002::/16. Both can
// smuggle a blocked IPv4 (e.g. IMDS) over IPv6 on dual-stack/IPv6-only clusters, so we
// extract the embedded v4 and re-check it.
var (
	nat64  = netip.MustParsePrefix("64:ff9b::/96")
	sixTo4 = netip.MustParsePrefix("2002::/16")
)

// embeddedV4 returns the IPv4 embedded in a NAT64 / 6to4 address, if any.
func embeddedV4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch {
	case nat64.Contains(ip):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixTo4.Contains(ip):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

// Policy decides whether an egress destination IP is permitted. It is deny-by-default
// for the dangerous ranges; a caller MAY add an explicit allow-list of extra blocked
// CIDRs (e.g. the control-plane/worker subnets).
type Policy struct {
	// extraBlocked are additional CIDRs to block (control-plane/workers, per env).
	extraBlocked []netip.Prefix
}

// New builds a policy. extraBlockedCIDRs are additional networks to deny (e.g.
// "10.0.0.0/8" worker subnet); malformed entries are rejected.
func New(extraBlockedCIDRs ...string) (*Policy, error) {
	p := &Policy{}
	for _, c := range extraBlockedCIDRs {
		pre, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("egress: bad CIDR %q: %w", c, err)
		}
		p.extraBlocked = append(p.extraBlocked, pre.Masked())
	}
	return p, nil
}

// BlockedCIDRs returns the denylist as CIDR strings, so the SAME policy that gates
// app-level dials also drives the generated k8s NetworkPolicy egress rules (single
// source of truth). Order: IMDS, the RFC-defined dangerous ranges, then extras.
func (p *Policy) BlockedCIDRs() []string {
	out := []string{
		"169.254.169.254/32", // AWS/GCP/Azure IPv4 IMDS
		"fd00:ec2::254/128",  // AWS IPv6 IMDS
		"127.0.0.0/8",        // loopback v4
		"::1/128",            // loopback v6
		"10.0.0.0/8",         // private v4
		"172.16.0.0/12",      // private v4
		"192.168.0.0/16",     // private v4
		"fc00::/7",           // ULA private v6
		"169.254.0.0/16",     // link-local v4
		"fe80::/10",          // link-local v6
		"224.0.0.0/4",        // multicast v4
		"100.64.0.0/10",      // CGNAT (RFC-6598 — AWS/EKS internal)
		"0.0.0.0/8",          // "this network"
		"255.255.255.255/32", // limited broadcast
	}
	for _, pre := range p.extraBlocked {
		out = append(out, pre.String())
	}
	return out
}

// AllowedIP reports whether traffic to ip is permitted, and if not, why. This is the
// authoritative check — DNS-rebinding-safe because it operates on a resolved IP.
func (p *Policy) AllowedIP(ip netip.Addr) (bool, string) {
	ip = ip.Unmap() // normalise IPv4-mapped IPv6 to IPv4 so ranges match
	// NAT64/6to4 can smuggle a blocked IPv4 over IPv6 — re-check the embedded v4.
	if v4, ok := embeddedV4(ip); ok {
		if allowed, reason := p.AllowedIP(v4); !allowed {
			return false, "embedded IPv4 " + reason
		}
	}
	for _, m := range imds {
		if ip == m {
			return false, "cloud metadata endpoint (IMDS) blocked"
		}
	}
	switch {
	case ip.IsLoopback():
		return false, "loopback blocked"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return false, "link-local blocked"
	case ip.IsPrivate():
		return false, "private range blocked"
	case ip.IsMulticast():
		return false, "multicast blocked"
	case ip.IsUnspecified():
		return false, "unspecified address blocked"
	}
	for _, pre := range defaultBlocked {
		if pre.Contains(ip) {
			return false, "reserved/CGNAT range blocked"
		}
	}
	for _, pre := range p.extraBlocked {
		if pre.Contains(ip) {
			return false, "control-plane/worker range blocked"
		}
	}
	return true, ""
}

// AllowedAddr parses an IP string and checks it. A non-IP (hostname) is rejected —
// callers MUST resolve first and pass the resolved IP (DNS-rebinding defense).
func (p *Policy) AllowedAddr(ipStr string) (bool, string) {
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false, "not an IP literal (resolve first)"
	}
	return p.AllowedIP(ip)
}

// ControlFunc returns a net.Dialer Control hook that rejects a connection whose
// resolved address violates the policy. Wiring this into a Dialer means EVERY dialed
// IP is checked at connect time — after DNS resolution — closing the rebinding hole.
// Callers MUST dial through this hook (a Dialer with Control set); do NOT pre-resolve
// and call AllowedAddr yourself — that reintroduces a TOCTOU rebind window.
func (p *Policy) ControlFunc() func(network, address string, c syscall.RawConn) error {
	return func(_ string, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("egress: bad address %q: %w", address, err)
		}
		ok, reason := p.AllowedAddr(host)
		if !ok {
			return fmt.Errorf("egress denied: %s (%s)", host, reason)
		}
		return nil
	}
}

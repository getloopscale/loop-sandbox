package egress

import (
	"fmt"
	"net/netip"
	"strings"
)

// MaxRulesPerList bounds allow_out and deny_out. Every entry becomes a firewall
// set element or a comparison on each new connection.
const MaxRulesPerList = 100

// Rules is the outbound policy a caller gives one sandbox: destinations it may
// reach (allow_out) and destinations it may not (deny_out).
//
// The semantics are the ones E2B documents for allowOut and denyOut, so a policy
// written for one works on the other. With no rules every public destination is
// reachable. An entry of allow_out wins over deny_out, which is how "only these
// hosts" is written: allow them and deny 0.0.0.0/0. Only allow_out takes domain
// names, because a deny by name is trivially bypassed by connecting to the address.
//
// The platform ranges of Policy are outside the caller's reach: no rule allows them.
type Rules struct {
	allowNets    []netip.Prefix
	allowDomains []string // lower case; "*", "example.com" or "*.example.com"
	denyNets     []netip.Prefix
}

// Decision is the outcome for one connection.
type Decision struct {
	Allowed bool
	// ByDomain is set when the host name, and not the address, is what allow_out
	// permitted. The enforcement point then has to connect to an address it
	// resolved for that name itself: the address the client chose proves nothing.
	ByDomain bool
	Reason   string // why it was denied; empty when allowed
}

// ParseRules validates allow_out and deny_out. It refuses whatever the firewall
// could not enforce, and each error names the list and the entry.
func ParseRules(allowOut, denyOut []string) (*Rules, error) {
	if len(allowOut) > MaxRulesPerList {
		return nil, fmt.Errorf("allow_out has %d entries, the limit is %d", len(allowOut), MaxRulesPerList)
	}
	if len(denyOut) > MaxRulesPerList {
		return nil, fmt.Errorf("deny_out has %d entries, the limit is %d", len(denyOut), MaxRulesPerList)
	}
	r := &Rules{}
	for _, entry := range allowOut {
		if pre, ok := parseNet(entry); ok {
			r.allowNets = append(r.allowNets, pre)
			continue
		}
		domain, err := parseDomainPattern(entry)
		if err != nil {
			return nil, fmt.Errorf("allow_out entry %q: %w", entry, err)
		}
		r.allowDomains = append(r.allowDomains, domain)
	}
	for _, entry := range denyOut {
		pre, ok := parseNet(entry)
		if !ok {
			return nil, fmt.Errorf("deny_out entry %q: want an IP address or a CIDR block; domain names are only accepted in allow_out", entry)
		}
		r.denyNets = append(r.denyNets, pre)
	}
	return r, nil
}

// parseNet reads an IP address or a CIDR block as a prefix.
func parseNet(entry string) (netip.Prefix, bool) {
	if pre, err := netip.ParsePrefix(entry); err == nil {
		return netip.PrefixFrom(pre.Addr().Unmap(), pre.Bits()).Masked(), true
	}
	if addr, err := netip.ParseAddr(entry); err == nil {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), true
	}
	return netip.Prefix{}, false
}

// parseDomainPattern accepts "*", a host name, or "*." followed by a host name,
// and returns it in lower case.
func parseDomainPattern(entry string) (string, error) {
	pattern := strings.ToLower(entry)
	if pattern == "*" {
		return pattern, nil
	}
	name := strings.TrimPrefix(pattern, "*.")
	if name == "" || len(name) > 253 {
		return "", fmt.Errorf("want an IP address, a CIDR block or a domain name")
	}
	for _, label := range strings.Split(name, ".") {
		if !validLabel(label) {
			return "", fmt.Errorf("want an IP address, a CIDR block or a domain name such as example.com or *.example.com")
		}
	}
	return pattern, nil
}

// validLabel reports whether s is one DNS label: letters, digits and inner hyphens.
func validLabel(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// HasDomains reports whether a rule depends on the host name of a connection.
// When it does, the enforcement point has to read the name from the HTTP Host
// header or the TLS server name, and the sandbox needs a DNS server.
func (r *Rules) HasDomains() bool { return len(r.allowDomains) > 0 }

// Decide judges one outbound connection to ip. host is the name the client asked
// for when the enforcement point could read one, and empty otherwise.
//
// The address is always the one actually dialed, so a name that resolves to a
// platform range is refused whatever the rules say.
func (r *Rules) Decide(platform *Policy, ip netip.Addr, host string) Decision {
	ip = ip.Unmap()
	if ok, reason := platform.AllowedIP(ip); !ok {
		return Decision{Reason: reason}
	}
	for _, pre := range r.allowNets {
		if pre.Contains(ip) {
			return Decision{Allowed: true}
		}
	}
	if host != "" {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		for _, pattern := range r.allowDomains {
			if matchDomain(host, pattern) {
				return Decision{Allowed: true, ByDomain: true}
			}
		}
	}
	for _, pre := range r.denyNets {
		if pre.Contains(ip) {
			return Decision{Reason: "denied by deny_out " + pre.String()}
		}
	}
	return Decision{Allowed: true}
}

// matchDomain reports whether host, in lower case, matches pattern. "*.example.com"
// covers every name under example.com and not example.com itself.
func matchDomain(host, pattern string) bool {
	switch {
	case pattern == "*", pattern == host:
		return true
	case strings.HasPrefix(pattern, "*."):
		return strings.HasSuffix(host, pattern[1:])
	}
	return false
}

package egress

import (
	"net/netip"
	"strings"
	"testing"
)

func mustRules(t *testing.T, allow, deny []string) *Rules {
	t.Helper()
	r, err := ParseRules(allow, deny)
	if err != nil {
		t.Fatalf("ParseRules(%v, %v): %v", allow, deny, err)
	}
	return r
}

func decide(t *testing.T, r *Rules, ip, host string) Decision {
	t.Helper()
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return r.Decide(p, netip.MustParseAddr(ip), host)
}

const denyAll = "0.0.0.0/0"

func TestNoRulesAllowsPublicDestinations(t *testing.T) {
	d := decide(t, mustRules(t, nil, nil), "93.184.216.34", "example.com")
	if !d.Allowed {
		t.Errorf("no rules denied a public address: %s", d.Reason)
	}
}

func TestPlatformRangesAreDeniedEvenWhenAllowed(t *testing.T) {
	r := mustRules(t, []string{"0.0.0.0/0", "169.254.169.254", "*"}, nil)
	for _, ip := range []string{"169.254.169.254", "10.0.0.5", "127.0.0.1", "192.168.1.1", "100.64.0.1"} {
		if d := decide(t, r, ip, "metadata.internal"); d.Allowed {
			t.Errorf("%s was allowed; the platform ranges must win over allow_out", ip)
		}
	}
}

func TestDenyOutBlocksAnAddressInItsRange(t *testing.T) {
	r := mustRules(t, nil, []string{"8.8.8.0/24", "1.1.1.1"})
	for ip, want := range map[string]bool{"8.8.8.8": false, "1.1.1.1": false, "9.9.9.9": true} {
		if d := decide(t, r, ip, ""); d.Allowed != want {
			t.Errorf("%s allowed=%v, want %v (%s)", ip, d.Allowed, want, d.Reason)
		}
	}
}

func TestAllowOutWinsOverDenyOut(t *testing.T) {
	r := mustRules(t, []string{"8.8.8.8"}, []string{denyAll})
	if d := decide(t, r, "8.8.8.8", ""); !d.Allowed {
		t.Errorf("allowed address was denied: %s", d.Reason)
	}
	if d := decide(t, r, "9.9.9.9", ""); d.Allowed {
		t.Error("an address outside allow_out passed a deny-all rule")
	}
}

func TestAllowedDomainPassesADenyAllRule(t *testing.T) {
	r := mustRules(t, []string{"pypi.org", "*.pythonhosted.org"}, []string{denyAll})
	cases := map[string]bool{
		"pypi.org":                  true,
		"PyPI.org":                  true,
		"files.pythonhosted.org":    true,
		"a.b.pythonhosted.org":      true,
		"pythonhosted.org":          false, // "*.x" does not cover the apex
		"evilpythonhosted.org":      false,
		"pypi.org.attacker.example": false,
		"":                          false, // no hostname known for this connection
	}
	for host, want := range cases {
		if d := decide(t, r, "151.101.0.223", host); d.Allowed != want {
			t.Errorf("host %q allowed=%v, want %v (%s)", host, d.Allowed, want, d.Reason)
		}
	}
}

func TestAnAllowedDomainDoesNotReachAPlatformRange(t *testing.T) {
	// DNS rebinding: an allowed name that resolves to a private address.
	r := mustRules(t, []string{"pypi.org"}, []string{denyAll})
	if d := decide(t, r, "10.1.2.3", "pypi.org"); d.Allowed {
		t.Error("an allowed domain reached a private address")
	}
}

func TestIPv6RulesMatchIPv6Addresses(t *testing.T) {
	r := mustRules(t, []string{"2606:4700::/32"}, []string{"::/0"})
	if d := decide(t, r, "2606:4700::1111", ""); !d.Allowed {
		t.Errorf("allowed IPv6 range was denied: %s", d.Reason)
	}
	if d := decide(t, r, "2001:4860:4860::8888", ""); d.Allowed {
		t.Error("an IPv6 address outside allow_out passed ::/0")
	}
}

func TestDenyAllV4AlsoCoversIPv4MappedAddresses(t *testing.T) {
	r := mustRules(t, nil, []string{denyAll})
	if d := decide(t, r, "::ffff:9.9.9.9", ""); d.Allowed {
		t.Error("an IPv4-mapped IPv6 address escaped 0.0.0.0/0")
	}
}

func TestParseRulesRefusesWhatItCannotEnforce(t *testing.T) {
	tooMany := make([]string, MaxRulesPerList+1)
	for i := range tooMany {
		tooMany[i] = "8.8.8.8"
	}
	cases := []struct {
		name        string
		allow, deny []string
		wantInError string
	}{
		{"domain in deny_out", nil, []string{"example.com"}, "deny_out"},
		{"wildcard in the middle", []string{"a.*.example.com"}, nil, "allow_out"},
		{"bare wildcard suffix", []string{"*."}, nil, "allow_out"},
		{"bad CIDR", []string{"10.0.0.0/33"}, nil, "allow_out"},
		{"empty entry", []string{""}, nil, "allow_out"},
		{"URL instead of a host", []string{"https://example.com"}, nil, "allow_out"},
		{"host with a port", []string{"example.com:443"}, nil, "allow_out"},
		{"too many entries", tooMany, nil, "allow_out"},
	}
	for _, c := range cases {
		_, err := ParseRules(c.allow, c.deny)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantInError) {
			t.Errorf("%s: error %q does not name %s", c.name, err, c.wantInError)
		}
	}
}

func TestRulesReportWhetherAHostnameIsNeeded(t *testing.T) {
	if mustRules(t, []string{"8.8.8.8"}, nil).HasDomains() {
		t.Error("rules with no domain claim to need hostnames")
	}
	if !mustRules(t, []string{"pypi.org"}, nil).HasDomains() {
		t.Error("rules with a domain do not report it")
	}
}

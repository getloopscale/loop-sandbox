package netguard

import (
	"net/netip"
	"slices"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigFromEnvReadsTheLists(t *testing.T) {
	cfg, err := ConfigFromEnv(env(map[string]string{
		EnvAllowOut: "pypi.org, *.pythonhosted.org", EnvDenyOut: "0.0.0.0/0", EnvDNS: "8.8.8.8,1.1.1.1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.AllowOut, []string{"pypi.org", "*.pythonhosted.org"}) || !slices.Equal(cfg.DenyOut, []string{"0.0.0.0/0"}) {
		t.Errorf("AllowOut = %v, DenyOut = %v", cfg.AllowOut, cfg.DenyOut)
	}
	if !slices.Equal(cfg.DNS, []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}) {
		t.Errorf("DNS = %v", cfg.DNS)
	}
}

func TestConfigFromEnvWithNoRulesIsAnOpenPolicy(t *testing.T) {
	cfg, err := ConfigFromEnv(env(map[string]string{EnvDNS: "8.8.8.8"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowOut) != 0 || len(cfg.DenyOut) != 0 {
		t.Errorf("AllowOut = %v, DenyOut = %v; want none", cfg.AllowOut, cfg.DenyOut)
	}
}

func TestConfigFromEnvRefusesAMissingOrInvalidResolver(t *testing.T) {
	for _, dns := range []string{"", "dns.google"} {
		if _, err := ConfigFromEnv(env(map[string]string{EnvDNS: dns})); err == nil {
			t.Errorf("resolver %q was accepted", dns)
		}
	}
}

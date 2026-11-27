// Package netguard runs in the network namespace a sandbox shares and makes the
// egress proxy unavoidable: the firewall it installs sends every TCP connection
// of the sandbox to the proxy and drops what the proxy cannot judge.
package netguard

import (
	"fmt"
	"net/netip"
	"strings"
)

// ProxyPort is where the proxy listens inside the shared namespace.
const ProxyPort uint16 = 15001

// ReadyLine is written to the log once the firewall is in place and the proxy
// accepts connections. The driver starts the sandbox only after reading it.
const ReadyLine = "netguard: ready"

// Environment variables the driver uses to configure the guard.
const (
	EnvAllowOut = "LSB_NET_ALLOW_OUT" // comma-separated allow_out entries
	EnvDenyOut  = "LSB_NET_DENY_OUT"  // comma-separated deny_out entries
	EnvDNS      = "LSB_NET_DNS"       // comma-separated resolver addresses
)

// Config is what Run needs.
type Config struct {
	AllowOut []string
	DenyOut  []string
	DNS      []netip.Addr
}

// ConfigFromEnv reads a Config from the guard's environment variables.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{AllowOut: splitList(getenv(EnvAllowOut)), DenyOut: splitList(getenv(EnvDenyOut))}
	for _, s := range splitList(getenv(EnvDNS)) {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %q is not an address", EnvDNS, s)
		}
		cfg.DNS = append(cfg.DNS, addr)
	}
	if len(cfg.DNS) == 0 {
		return Config{}, fmt.Errorf("%s names no resolver", EnvDNS)
	}
	return cfg, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

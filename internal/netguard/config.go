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

// ControlSocket is the unix socket where the guard takes new rules. It lives in
// the guard container, which the sandbox cannot enter.
const ControlSocket = "/run/lsb/netguard.sock"

// RulesRequest replaces the rules of a running guard.
type RulesRequest struct {
	AllowOut []string `json:"allow_out"`
	DenyOut  []string `json:"deny_out"`
}

// RulesResponse answers a RulesRequest. Error is empty when the rules were applied.
type RulesResponse struct {
	Error string `json:"error,omitempty"`
}

// DenialEvent is the "event" value of the log line the guard writes for each
// refused connection.
const DenialEvent = "egress_denied"

// DenialRecord is one refused connection as the guard logs it.
type DenialRecord struct {
	Event       string `json:"event"`
	Time        string `json:"time"` // RFC 3339, UTC
	Destination string `json:"destination"`
	Host        string `json:"host,omitempty"`
	Reason      string `json:"reason"`
}

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

//go:build linux

package main

import (
	"os"

	"github.com/getloopscale/loop-sandbox/internal/netguard"
)

// modeNetguard runs the agent as the network guard of a sandbox: a separate
// container that shares the sandbox's network namespace, holds CAP_NET_ADMIN and
// enforces the sandbox's outbound policy. The sandbox itself never runs this mode.
const modeNetguard = "netguard"

func runNetguard() {
	cfg, err := netguard.ConfigFromEnv(os.Getenv)
	if err != nil {
		fatal("netguard: %v", err)
	}
	if err := netguard.Run(cfg, os.Stdout); err != nil {
		fatal("%v", err)
	}
	os.Exit(0)
}

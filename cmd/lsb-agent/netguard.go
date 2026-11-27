//go:build linux

package main

import (
	"encoding/json"
	"fmt"
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

// modeNetguardRules runs inside the guard container, started by the driver, and
// hands new rules to the guard. Its one argument is a netguard.RulesRequest in JSON.
const modeNetguardRules = "netguard-rules"

func runNetguardRules(args []string) {
	if len(args) != 1 {
		fatal("netguard-rules: want one argument, the rules in JSON")
	}
	var req netguard.RulesRequest
	if err := json.Unmarshal([]byte(args[0]), &req); err != nil {
		fatal("netguard-rules: %v", err)
	}
	if err := netguard.SetRules(req); err != nil {
		// The driver reads this line; exit status 3 tells a refused rule apart
		// from a guard that could not be reached.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
}

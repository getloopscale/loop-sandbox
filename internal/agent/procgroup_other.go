//go:build !unix

package agent

import "os/exec"

// ownProcessGroup is a no-op where process groups are not available: cancelling
// kills only the command itself.
func ownProcessGroup(*exec.Cmd) {}

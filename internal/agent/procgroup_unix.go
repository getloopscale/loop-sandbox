//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own and makes cancelling
// it kill the whole group, so a shell's children do not outlive the request.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

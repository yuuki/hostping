//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// killProcessGroup starts cmd in a process group of its own and makes a
// cancellation kill the whole group: a gate script that is killed alone would
// leave its scheduler query running and holding stderr open.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

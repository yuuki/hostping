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

// reapProcessGroup kills what is left of cmd's process group once cmd has
// been waited for. The group outlives its leader while any member runs, so
// its ID is not reused while there is anything to kill. With no member left
// the call fails with ESRCH, unless the kernel handed the leader's PID to a
// new group leader within the microseconds since the wait, which sequential
// PID allocation makes impractical.
func reapProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

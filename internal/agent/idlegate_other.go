//go:build !unix

package agent

import "os/exec"

// killProcessGroup leaves exec's default: only the gate process is killed.
func killProcessGroup(*exec.Cmd) {}

// reapProcessGroup has no process group to clean up.
func reapProcessGroup(*exec.Cmd) {}

//go:build unix

package agent

import (
	"errors"
	"syscall"
	"time"
)

// processGone reports whether pid has exited (or become a zombie of another
// parent that has been reaped) within wait.
func processGone(pid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

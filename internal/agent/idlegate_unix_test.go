//go:build unix

package agent

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/telemetry"
)

// A gate killed by a signal answered nothing: it is an error, not a denial,
// even though its wait status is non-zero.
func TestRunIdleGateSignaled(t *testing.T) {
	out := runIdleGate(context.Background(), writeGate(t, "kill -TERM $$\nsleep 30"), 5*time.Second)
	if out.reason != telemetry.GateError {
		t.Fatalf("signaled gate: %+v, want error", out)
	}
}

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

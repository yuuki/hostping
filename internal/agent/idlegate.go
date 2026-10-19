package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/telemetry"
)

// idleGateStderrMax bounds how much of the gate's stderr the log line quotes.
const idleGateStderrMax = 256

// idleGateDenyExit is the exit status with which a gate denies the run. Any
// other non-zero status means the gate could not decide.
const idleGateDenyExit = 1

// idleGateWaitDelay bounds how long the gate may hold its stderr open after
// it was killed or exited (a grandchild that left the process group).
const idleGateWaitDelay = time.Second

// schedGate is --idle-gate-command: a site-supplied check consulted right
// before each idle-trigger run, so the full mesh stays out of a scheduler
// job's allocation. The utilization-based idle verdict cannot see an
// allocation: a job that is loading data or writing a checkpoint leaves the
// GPUs and RNICs as quiet as a free host.
//
// Exit 1 is the site's "not now" (reason denied). Any other non-zero exit is
// the gate saying it could not decide, for example because the scheduler
// could not be queried (reason error), so that a broken backend alerts
// instead of reading as a host that is always allocated.
//
// It fails closed. A non-zero exit, a timeout and an exec failure all skip
// the attempt, and the next one follows --idle-interval. The outcome is
// logged once per change, not per attempt: a gate that denies every attempt
// on a busy host would otherwise log one line per --idle-interval forever.
type schedGate struct {
	path    string
	timeout time.Duration
	// run executes the gate; nil runs path (runIdleGate). Tests inject it.
	run  func(ctx context.Context, path string, timeout time.Duration) idleGateOutcome
	logf func(format string, args ...any)
	// last is the previous attempt's reason ("" while allowed), for logging
	// state changes only.
	last string
}

// idleGateOutcome is one gate attempt: reason is "" when the idle run may
// start, else a telemetry.Gate* value; detail says why, for the log.
type idleGateOutcome struct {
	reason string
	detail string
}

// newSchedGate returns the gate cfg asks for, or nil when there is none (a nil
// gate allows every run).
func newSchedGate(cfg config.Config, logf func(format string, args ...any)) *schedGate {
	if cfg.IdleGateCommand == "" {
		return nil
	}
	g := &schedGate{path: cfg.IdleGateCommand, timeout: cfg.IdleGateTimeout, logf: logf}
	logf("hostping: idle runs gated by %s (timeout %v); exit 1 (denied), another non-zero exit, a timeout or an exec failure skips the run until the next --idle-interval", g.path, g.timeout)
	if _, err := exec.LookPath(g.path); err != nil {
		// Not fatal: the site may install the script after the daemon. Until
		// then every idle run is skipped, and the first attempt logs why.
		logf("hostping: WARNING: --idle-gate-command %s is not executable yet (%v); idle runs are skipped until it is", g.path, err)
	}
	return g
}

// check runs the gate and returns "" when the idle run may start, else the
// skip reason (telemetry.GateDenied, GateTimeout or GateError).
func (g *schedGate) check(ctx context.Context) string {
	if g == nil {
		return ""
	}
	run := g.run
	if run == nil {
		run = runIdleGate
	}
	out := run(ctx, g.path, g.timeout)
	if ctx.Err() != nil {
		// Shutdown killed the gate: not an outcome worth logging.
		return out.reason
	}
	if out.reason != g.last {
		if out.reason == "" {
			g.logf("hostping: idle gate %s allows idle runs again", g.path)
		} else {
			g.logf("hostping: idle gate %s skips idle runs (reason=%s): %s; logged again only when this changes", g.path, out.reason, out.detail)
		}
		g.last = out.reason
	}
	return out.reason
}

// runIdleGate runs path with no arguments and no shell, stdin and stdout on
// /dev/null, and kills it (with its process group, so a script's children go
// too) once timeout elapses. Whatever the gate left running in its group when
// it exited is killed too: a gate is a check, and one attempt per
// --idle-interval must not pile up background children.
func runIdleGate(ctx context.Context, path string, timeout time.Duration) idleGateOutcome {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	var stderr headBuffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = idleGateWaitDelay
	killProcessGroup(cmd)
	err := cmd.Run()
	reapProcessGroup(cmd)
	if errors.Is(err, exec.ErrWaitDelay) {
		// The gate exited 0, but a child it left behind kept stderr open
		// past WaitDelay; the child is gone now. The answer stands.
		err = nil
	}
	if err == nil {
		return idleGateOutcome{}
	}
	var out idleGateOutcome
	var exit *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		out = idleGateOutcome{reason: telemetry.GateTimeout, detail: fmt.Sprintf("no exit within %v", timeout)}
	case errors.As(err, &exit) && exit.ExitCode() == idleGateDenyExit:
		out = idleGateOutcome{reason: telemetry.GateDenied, detail: err.Error()}
	case exit != nil && exit.Exited():
		// The gate ran but reports that it could not decide (exit 2 and up,
		// or a shell's 126/127 for a missing interpreter or command).
		out = idleGateOutcome{reason: telemetry.GateError, detail: err.Error() + " (could not decide)"}
	default:
		// Could not start (missing, not executable), killed by a signal, or
		// canceled by shutdown.
		out = idleGateOutcome{reason: telemetry.GateError, detail: err.Error()}
	}
	if msg := stderr.String(); msg != "" {
		out.detail += ": " + msg
	}
	return out
}

// headBuffer keeps the first idleGateStderrMax bytes written to it and
// discards the rest, so a chatty gate cannot grow the daemon's memory.
type headBuffer struct {
	b []byte
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := idleGateStderrMax - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// String is the kept stderr on one line, trimmed.
func (h *headBuffer) String() string {
	return strings.Join(strings.Fields(string(h.b)), " ")
}

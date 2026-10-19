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

// idleGateWaitDelay bounds how long the gate may hold its stderr open after
// it was killed or exited (a grandchild that left the process group).
const idleGateWaitDelay = time.Second

// schedGate is --idle-gate-command: a site-supplied check consulted right
// before each idle-trigger run, so the full mesh stays out of a scheduler
// job's allocation. The utilization-based idle verdict cannot see an
// allocation: a job that is loading data or writing a checkpoint leaves the
// GPUs and RNICs as quiet as a free host.
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
	logf("hostping: idle runs gated by %s (timeout %v); a non-zero exit, timeout or exec failure skips the run until the next --idle-interval", g.path, g.timeout)
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
// too) once timeout elapses.
func runIdleGate(ctx context.Context, path string, timeout time.Duration) idleGateOutcome {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	var stderr headBuffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = idleGateWaitDelay
	killProcessGroup(cmd)
	err := cmd.Run()
	if err == nil {
		return idleGateOutcome{}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return idleGateOutcome{reason: telemetry.GateTimeout, detail: fmt.Sprintf("no exit within %v", timeout)}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.Exited() {
		detail := err.Error()
		if msg := stderr.String(); msg != "" {
			detail += ": " + msg
		}
		return idleGateOutcome{reason: telemetry.GateDenied, detail: detail}
	}
	// Could not start (missing, not executable), killed by a signal, or
	// canceled by shutdown.
	return idleGateOutcome{reason: telemetry.GateError, detail: err.Error()}
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

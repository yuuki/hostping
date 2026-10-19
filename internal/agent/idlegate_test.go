package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/telemetry"
)

// fakeGate answers each attempt with the next outcome (the last one repeats)
// and counts the attempts.
type fakeGate struct {
	outcomes []idleGateOutcome
	calls    int
	// cancel, when set, is called during the attempt: shutdown mid-gate.
	cancel context.CancelFunc
}

func (f *fakeGate) run(context.Context, string, time.Duration) idleGateOutcome {
	f.calls++
	if f.cancel != nil {
		f.cancel()
	}
	i := min(f.calls, len(f.outcomes)) - 1
	return f.outcomes[i]
}

func gateWith(f *fakeGate, logs *[]string) *schedGate {
	return &schedGate{path: "/gate", timeout: time.Second, run: f.run, logf: func(format string, args ...any) {
		if logs != nil {
			*logs = append(*logs, fmt.Sprintf(format, args...))
		}
	}}
}

var (
	gateAllow = idleGateOutcome{}
	gateDeny  = idleGateOutcome{reason: telemetry.GateDenied, detail: "exit status 1"}
)

func (ms metrics) gateSkips(t *testing.T, reason string) int64 {
	t.Helper()
	if _, ok := ms["hostping.engine.idle_gate.skipped"]; !ok {
		return 0
	}
	return ms.count(t, "hostping.engine.idle_gate.skipped", "reason", reason)
}

// A gate that allows lets the idle run start as without one.
func TestDaemonIdleGateAllows(t *testing.T) {
	td := newTestDaemon(t, nil)
	f := &fakeGate{outcomes: []idleGateOutcome{gateAllow}}
	td.gate = gateWith(f, nil)
	td.idleRun(t)
	if f.calls != 1 {
		t.Fatalf("gate ran %d times, want once: on the poll that starts the idle run, not on the warmup", f.calls)
	}
	ms := td.collect(t)
	if _, ok := ms.stamp("hostping.engine.last_run_timestamp"); !ok {
		t.Fatal("an allowed idle run must run")
	}
	if n := ms.gateSkips(t, telemetry.GateDenied); n != 0 {
		t.Fatalf("idle_gate.skipped denied = %d, want 0", n)
	}
}

// A gate that denies skips the run, counts it, and is not asked again until
// the next --idle-interval.
func TestDaemonIdleGateDenies(t *testing.T) {
	td := newTestDaemon(t, nil)
	f := &fakeGate{outcomes: []idleGateOutcome{gateDeny}}
	td.gate = gateWith(f, nil)
	td.idleRun(t)
	ms := td.collect(t)
	if _, ok := ms.stamp("hostping.engine.last_run_timestamp"); ok {
		t.Fatal("a denied idle run must not run")
	}
	if n := ms.gateSkips(t, telemetry.GateDenied); n != 1 {
		t.Fatalf("idle_gate.skipped denied = %d, want 1", n)
	}
	for range 5 {
		td.tick(t)
	}
	if f.calls != 1 {
		t.Fatalf("gate ran %d times within one --idle-interval, want once", f.calls)
	}
	// The next interval: asked again, and this time allowed.
	f.outcomes = append(f.outcomes, gateAllow)
	td.mon.ResumeIdle(time.Now().Add(-2 * time.Hour))
	td.tick(t)
	if f.calls != 2 {
		t.Fatalf("gate ran %d times, want a second attempt at the next --idle-interval", f.calls)
	}
	if _, ok := td.collect(t).stamp("hostping.engine.last_run_timestamp"); !ok {
		t.Fatal("the allowed second attempt must run")
	}
}

// Only the scheduled idle run is gated: a mutual-probe alarm on an idle host
// runs without asking.
func TestDaemonIdleGateLeavesAlarmRuns(t *testing.T) {
	td := newTestDaemon(t, nil)
	f := &fakeGate{outcomes: []idleGateOutcome{gateDeny}}
	td.gate = gateWith(f, nil)
	td.mon.MutualAlarm = func() bool { return true }
	td.idleRun(t)
	if f.calls != 0 {
		t.Fatalf("gate ran %d times for a mutual-alarm run, want 0", f.calls)
	}
	if _, ok := td.collect(t).stamp("hostping.engine.last_run_timestamp"); !ok {
		t.Fatal("the alarm run must run")
	}
}

// Shutdown while the gate runs stops the loop and counts nothing.
func TestDaemonIdleGateShutdown(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.tick(t) // warmup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeGate{outcomes: []idleGateOutcome{{reason: telemetry.GateError, detail: "signal: killed"}}, cancel: cancel}
	var logs []string
	td.gate = gateWith(f, &logs)
	if td.daemon.tick(ctx) {
		t.Fatal("shutdown during the gate must stop the loop")
	}
	if n := td.collect(t).gateSkips(t, telemetry.GateError); n != 0 {
		t.Fatalf("shutdown counted %d gate errors", n)
	}
	if len(logs) != 0 {
		t.Fatalf("shutdown logged %q", logs)
	}
}

// The outcome is logged when it changes, not on every attempt.
func TestIdleGateLogsStateChanges(t *testing.T) {
	timeout := idleGateOutcome{reason: telemetry.GateTimeout, detail: "no exit within 1s"}
	f := &fakeGate{outcomes: []idleGateOutcome{gateAllow, gateDeny, gateDeny, timeout, timeout, gateAllow, gateAllow}}
	var logs []string
	g := gateWith(f, &logs)
	var reasons []string
	for range f.outcomes {
		reasons = append(reasons, g.check(context.Background()))
	}
	if got, want := strings.Join(reasons, ","), ",denied,denied,timeout,timeout,,"; got != want {
		t.Fatalf("reasons = %q, want %q", got, want)
	}
	if len(logs) != 3 {
		t.Fatalf("logged %d lines, want 3 (denied, timeout, allowed again): %q", len(logs), logs)
	}
	for i, want := range []string{"reason=denied): exit status 1", "reason=timeout): no exit within 1s", "allows idle runs again"} {
		if !strings.Contains(logs[i], want) {
			t.Errorf("log %d = %q, want it to contain %q", i, logs[i], want)
		}
	}
	var none *schedGate
	if r := none.check(context.Background()); r != "" {
		t.Fatalf("no gate: %q, want every run allowed", r)
	}
}

func TestNewIdleGate(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	if g := newSchedGate(config.Defaults(), logf); g != nil || len(logs) != 0 {
		t.Fatalf("no --idle-gate-command: gate %v, logs %q", g, logs)
	}
	cfg := config.Defaults()
	cfg.IdleGateCommand = filepath.Join(t.TempDir(), "missing")
	g := newSchedGate(cfg, logf)
	if g == nil || g.timeout != 5*time.Second {
		t.Fatalf("gate = %+v", g)
	}
	if len(logs) != 2 || !strings.Contains(logs[1], "not executable yet") {
		t.Fatalf("a missing gate must start with a warning, got %q", logs)
	}
}

// writeGate writes an executable shell script and returns its path.
func writeGate(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("gate scripts need /bin/sh")
	}
	p := filepath.Join(t.TempDir(), "gate")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunIdleGate(t *testing.T) {
	ctx := context.Background()
	if out := runIdleGate(ctx, writeGate(t, "exit 0"), 5*time.Second); out.reason != "" {
		t.Errorf("exit 0: %+v, want allowed", out)
	}
	out := runIdleGate(ctx, writeGate(t, "echo 'job 4242 is running' >&2\nexit 1"), 5*time.Second)
	if out.reason != telemetry.GateDenied || !strings.Contains(out.detail, "exit status 1: job 4242 is running") {
		t.Errorf("exit 1: %+v, want denied quoting stderr", out)
	}
	// The Slurm example of docs/operations.md fails closed when the query
	// fails, even with empty output.
	out = runIdleGate(ctx, writeGate(t, "jobs=$(false) || exit 1\n[ -z \"$jobs\" ]"), 5*time.Second)
	if out.reason != telemetry.GateDenied {
		t.Errorf("failed query: %+v, want denied", out)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if out := runIdleGate(ctx, missing, 5*time.Second); out.reason != telemetry.GateError {
		t.Errorf("missing command: %+v, want error", out)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runIdleGate(ctx, plain, 5*time.Second); out.reason != telemetry.GateError {
		t.Errorf("not executable: %+v, want error", out)
	}
}

// A hung gate is killed with its children at the timeout, so it cannot hold
// the poll loop past it even when a child keeps stderr open.
func TestRunIdleGateTimeout(t *testing.T) {
	gate := writeGate(t, "sleep 30 &\nsleep 30")
	start := time.Now()
	out := runIdleGate(context.Background(), gate, 200*time.Millisecond)
	if out.reason != telemetry.GateTimeout {
		t.Fatalf("hung gate: %+v, want timeout", out)
	}
	if d := time.Since(start); d > 200*time.Millisecond+idleGateWaitDelay+time.Second {
		t.Fatalf("hung gate held the loop for %v", d)
	}
}

// A gate that exits but leaves a child behind does not leak it: the rest of
// its process group is killed once it has been waited for, and its answer
// stands.
func TestRunIdleGateReapsChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	for code, want := range map[int]string{0: "", 1: telemetry.GateDenied} {
		gate := writeGate(t, fmt.Sprintf("sleep 30 &\necho $! > %s\nexit %d", pidFile, code))
		if out := runIdleGate(context.Background(), gate, 5*time.Second); out.reason != want {
			t.Fatalf("exit %d with a child: %+v, want reason %q", code, out, want)
		}
		b, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		if _, err := fmt.Sscan(string(b), &pid); err != nil {
			t.Fatal(err)
		}
		if !processGone(pid, 2*time.Second) {
			t.Fatalf("exit %d: child %d still running after the gate returned", code, pid)
		}
	}
}

// The timeout and error lines quote stderr too.
func TestRunIdleGateTimeoutQuotesStderr(t *testing.T) {
	gate := writeGate(t, "echo 'slurmctld unreachable' >&2\nsleep 30")
	out := runIdleGate(context.Background(), gate, 200*time.Millisecond)
	if out.reason != telemetry.GateTimeout || !strings.Contains(out.detail, "slurmctld unreachable") {
		t.Fatalf("hung gate: %+v, want timeout quoting stderr", out)
	}
}

func TestHeadBuffer(t *testing.T) {
	var h headBuffer
	n, err := h.Write([]byte(strings.Repeat("x", idleGateStderrMax-1) + "yz\n"))
	if n != idleGateStderrMax+2 || err != nil {
		t.Fatalf("Write = %d, %v: must report the whole write", n, err)
	}
	if _, err := h.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	if s := h.String(); len(s) != idleGateStderrMax || !strings.HasSuffix(s, "y") {
		t.Fatalf("kept %d bytes ending %q, want the first %d", len(s), s[len(s)-1:], idleGateStderrMax)
	}
}

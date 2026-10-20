package agent

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/state"
	"github.com/yuuki/hostping/internal/topology"
)

func bootID(id string) func() (string, error) {
	return func() (string, error) { return id, nil }
}

// logLines collects what a persister logs.
type logLines []string

func (l *logLines) logf(format string, args ...any) {
	*l = append(*l, fmt.Sprintf(format, args...))
}

func (l logLines) has(sub string) bool {
	for _, s := range l {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// daemonProcess is one daemon's engine, monitor and persister over a
// simulated host whose rnic0->mem0 path reads a third of its bandwidth on
// every run (pollutedProber), which grays mem.0 run after run: a slowly
// flapping memory channel as the streak sees it. The simulator's own faults
// do not serve here: mem_flap is a steady slow channel, which reads abnormal
// rather than gray, and flap is the mutual-probe RNIC flap, not a link streak.
type daemonProcess struct {
	eng  *engine.Engine
	mon  *monitor.Monitor
	ps   *persister
	logs *logLines
}

func startDaemon(t *testing.T, cfg config.Config, boot string) daemonProcess {
	t.Helper()
	g := topology.Fig2()
	polluted := true
	eng := engine.New(cfg, g, pollutedProber{Prober: sim.Prober{World: sim.New(g)}, polluted: &polluted})
	eng.Logf = func(string, ...any) {}
	mon := monitor.New(cfg, g, monitor.StaticSampler{GPU: idleGPUs(g, nil)})
	logs := &logLines{}
	ps, err := newPersister(cfg, g, bootID(boot), time.Now, logs.logf)
	if err != nil {
		t.Fatal(err)
	}
	ps.restore(eng, mon)
	return daemonProcess{eng: eng, mon: mon, ps: ps, logs: logs}
}

// confirmedIdleRun is one idle run whose held report a clean poll confirms,
// recorded as the daemon loop records it.
func (d daemonProcess) confirmedIdleRun(t *testing.T) model.Report {
	t.Helper()
	var p idleHold
	idleEngineRun(t, d.eng, &p)
	rep, out, _ := p.resolve(d.eng, idleAfterRun)
	if out != holdConfirmed {
		t.Fatalf("idle clean poll must confirm, got %v", out)
	}
	d.ps.idleResolved(d.eng, rep, true)
	return rep
}

func stateCfg(t *testing.T) config.Config {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	return cfg
}

// A slowly flapping link keeps its streak across a daemon restart: the run
// after the restart completes the --flap-window that the runs before it
// started, where a daemon without --state-dir would need the whole window
// again.
func TestStreakSurvivesRestart(t *testing.T) {
	cfg := stateCfg(t)
	first := startDaemon(t, cfg, "boot-a")
	for i := 0; i < cfg.FlapWindow-1; i++ {
		if rep := first.confirmedIdleRun(t); hasFlapping(rep) {
			t.Fatalf("run %d of %d already flaps: %v", i+1, cfg.FlapWindow, rep.Causes)
		}
	}
	if len(first.eng.Gray) == 0 {
		t.Fatal("the slow path must gray a link, or this test proves nothing")
	}
	first.ps.shutdown(first.eng, &idleHold{})

	second := startDaemon(t, cfg, "boot-a")
	if !second.logs.has("restored") {
		t.Fatalf("restart did not restore: %v", *second.logs)
	}
	if rep := second.confirmedIdleRun(t); !hasFlapping(rep) {
		t.Fatalf("run %d across a restart must flap: causes=%v gray=%v", cfg.FlapWindow, rep.Causes, second.eng.Gray)
	}

	// Without --state-dir the restart starts the window over.
	plain := cfg
	plain.StateDir = ""
	third := startDaemon(t, plain, "boot-a")
	if rep := third.confirmedIdleRun(t); hasFlapping(rep) {
		t.Fatalf("a fresh daemon flapped on its first run: %v", rep.Causes)
	}
}

// After a reboot the streaks are not evidence about the retrained links.
func TestStreakDoesNotSurviveReboot(t *testing.T) {
	cfg := stateCfg(t)
	first := startDaemon(t, cfg, "boot-a")
	for i := 0; i < cfg.FlapWindow-1; i++ {
		first.confirmedIdleRun(t)
	}
	second := startDaemon(t, cfg, "boot-b")
	if len(second.eng.Gray) != 0 || !second.logs.has("rebooted") {
		t.Fatalf("a different boot restored gray=%v logs=%v", second.eng.Gray, *second.logs)
	}
	if rep := second.confirmedIdleRun(t); hasFlapping(rep) {
		t.Fatalf("streaks from before a reboot made run 1 flap: %v", rep.Causes)
	}
}

// The slow-registration streak survives a restart too: one confirmed idle run
// before the restart and one after it are two consecutive idle runs.
func TestSlowMRStreakSurvivesRestart(t *testing.T) {
	cfg := stateCfg(t)
	start := func() daemonProcess {
		g := topology.Fig2()
		eng := slowMREngine(t, cfg)
		mon := monitor.New(cfg, g, monitor.StaticSampler{GPU: idleGPUs(g, nil)})
		logs := &logLines{}
		ps, err := newPersister(cfg, g, bootID("boot-a"), time.Now, logs.logf)
		if err != nil {
			t.Fatal(err)
		}
		ps.restore(eng, mon)
		return daemonProcess{eng: eng, mon: mon, ps: ps, logs: logs}
	}
	first := start()
	if rep := first.confirmedIdleRun(t); hasSlowMR(rep) {
		t.Fatalf("reported after one idle run: %v", rep.Causes)
	}
	first.ps.shutdown(first.eng, &idleHold{})
	if f := readState(t, cfg.StateDir); f.SlowMR["gpu3"] != 1 {
		t.Fatalf("saved slow_mr = %v, want gpu3:1", f.SlowMR)
	}

	second := start()
	if !second.logs.has("slow-registration streaks on 1 GPUs") {
		t.Fatalf("restart did not restore the streak: %v", *second.logs)
	}
	if rep := second.confirmedIdleRun(t); !hasSlowMR(rep) {
		t.Fatalf("second idle run across a restart must report slow MR: %v", rep.Causes)
	}
}

// The path latency streaks survive a restart too.
func TestLatencyStreakSurvivesRestart(t *testing.T) {
	cfg := stateCfg(t)
	start := func() daemonProcess {
		g := topology.Fig2()
		eng := latencyEngine(t, cfg)
		mon := monitor.New(cfg, g, monitor.StaticSampler{GPU: idleGPUs(g, nil)})
		logs := &logLines{}
		ps, err := newPersister(cfg, g, bootID("boot-a"), time.Now, logs.logf)
		if err != nil {
			t.Fatal(err)
		}
		ps.restore(eng, mon)
		return daemonProcess{eng: eng, mon: mon, ps: ps, logs: logs}
	}
	first := start()
	if rep := first.confirmedIdleRun(t); hasLatency(rep) {
		t.Fatalf("reported after one idle run: %v", rep.Causes)
	}
	first.ps.shutdown(first.eng, &idleHold{})
	saved := readState(t, cfg.StateDir).Latency
	if len(saved) == 0 || !maps.Equal(saved, first.eng.Latency) {
		t.Fatalf("saved latency = %v, want %v", saved, first.eng.Latency)
	}

	second := start()
	if !second.logs.has(fmt.Sprintf("latency streaks on %d paths", len(saved))) {
		t.Fatalf("restart did not restore the streaks: %v", *second.logs)
	}
	for n := 2; n <= analyzer.LatencyRuns; n++ {
		if rep := second.confirmedIdleRun(t); hasLatency(rep) != (n == analyzer.LatencyRuns) {
			t.Fatalf("idle run %d of %d across a restart: causes %v", n, analyzer.LatencyRuns, rep.Causes)
		}
	}
}

// A discarded idle run is written with its streaks rolled back, but still
// counts for the idle cadence.
func TestDiscardedRunIsSavedRolledBack(t *testing.T) {
	cfg := stateCfg(t)
	d := startDaemon(t, cfg, "boot-a")
	var p idleHold
	idleEngineRun(t, d.eng, &p)
	rep, out, _ := p.resolve(d.eng, busyAfterRun)
	if out != holdDiscarded {
		t.Fatalf("busy clean poll must discard, got %v", out)
	}
	d.ps.idleResolved(d.eng, rep, false)
	f := readState(t, cfg.StateDir)
	if len(f.Gray) != 0 {
		t.Fatalf("discarded run saved gray %v", f.Gray)
	}
	if !f.LastIdleEnd.Equal(runEnd(rep)) || !f.GrayUpdatedAt.IsZero() {
		t.Fatalf("last_idle_end=%v gray_updated_at=%v, want %v and zero", f.LastIdleEnd, f.GrayUpdatedAt, runEnd(rep))
	}
}

// A busy run is saved only when it changed the streaks.
func TestBusyRunSavedOnlyWhenGrayChanges(t *testing.T) {
	cfg := stateCfg(t)
	d := startDaemon(t, cfg, "boot-a")
	same := d.eng.SnapshotStreaks()
	d.ps.otherRun(d.eng, same, model.Report{Started: time.Now()})
	if _, err := os.Stat(filepath.Join(cfg.StateDir, state.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unchanged busy run wrote the state file: %v", err)
	}
	d.eng.Gray["mem.0"] = 1
	end := time.Now().Truncate(time.Second)
	d.ps.otherRun(d.eng, same, model.Report{Started: end})
	f := readState(t, cfg.StateDir)
	if f.Gray["mem.0"] != 1 || !f.GrayUpdatedAt.Equal(end) || !f.LastIdleEnd.IsZero() {
		t.Fatalf("busy run saved %+v", f)
	}
}

// Shutdown with an idle report still held keeps the streaks from before it.
func TestShutdownLeavesHeldRunOut(t *testing.T) {
	cfg := stateCfg(t)
	d := startDaemon(t, cfg, "boot-a")
	var p idleHold
	idleEngineRun(t, d.eng, &p)
	if len(d.eng.Gray) == 0 {
		t.Fatal("the slow path must gray a link, or this test proves nothing")
	}
	d.ps.shutdown(d.eng, &p)
	f := readState(t, cfg.StateDir)
	if len(f.Gray) != 0 {
		t.Fatalf("an unconfirmed run's streaks were saved: %v", f.Gray)
	}
	if !f.LastIdleEnd.Equal(runEnd(p.rep)) {
		t.Fatalf("last_idle_end=%v, want the held run's end %v", f.LastIdleEnd, runEnd(p.rep))
	}
}

// Shutdown before any run, with nothing restored, writes nothing.
func TestShutdownWithNothingToSave(t *testing.T) {
	cfg := stateCfg(t)
	d := startDaemon(t, cfg, "boot-a")
	d.ps.shutdown(d.eng, &idleHold{})
	if ents, _ := os.ReadDir(cfg.StateDir); len(ents) != 0 {
		t.Fatalf("shutdown wrote %d entries", len(ents))
	}
}

// The idle timer resumes from the saved run: the next idle run is due
// --idle-interval after the last one ended, not right after the restart.
func TestIdleCadenceResumes(t *testing.T) {
	for _, tc := range []struct {
		name string
		ago  time.Duration
		run  bool
	}{
		{"not yet due", 10 * time.Minute, false},
		{"already due", 2 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := stateCfg(t)
			cfg.IdleInterval = time.Hour // maxAge = 3h
			g := topology.Fig2()
			st, err := state.Open(cfg.StateDir, "boot-a", state.Fingerprint(g))
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Save(state.File{LastIdleEnd: time.Now().Add(-tc.ago)}, time.Now()); err != nil {
				t.Fatal(err)
			}
			h := newIdleHost(t)
			h.m.Cfg = cfg
			ps, err := newPersister(cfg, g, bootID("boot-a"), time.Now, func(string, ...any) {})
			if err != nil {
				t.Fatal(err)
			}
			ps.restore(engine.New(cfg, g, nil), h.m)
			if d := h.poll(); d.Run {
				t.Fatalf("warmup must not run: %+v", d)
			}
			if d := h.poll(); d.Run != tc.run {
				t.Fatalf("idle poll after a restart %v after the last idle run: run=%v, want %v", tc.ago, d.Run, tc.run)
			}
		})
	}
}

// One-shot runs never read or write the state directory, and the persister
// they would build is off.
func TestOneShotLeavesStateDirAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.Once = true
	cfg.StateDir = dir
	cfg.ReportFormat = config.ReportFormatNone
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	defer quiet(t)()
	if err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a one-shot run touched --state-dir: %v", err)
	}
	ps, err := newPersister(cfg, topology.Fig2(), bootID("boot-a"), time.Now, func(string, ...any) {})
	if err != nil || ps.store != nil {
		t.Fatalf("one-shot persister: store=%v err=%v", ps.store, err)
	}
}

func TestPersisterOff(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	ps, err := newPersister(cfg, g, bootID("boot-a"), time.Now, func(string, ...any) {})
	if err != nil || ps.store != nil {
		t.Fatalf("no --state-dir: store=%v err=%v", ps.store, err)
	}

	// An unreadable boot ID switches persistence off with one log line.
	cfg.StateDir = t.TempDir()
	var logs logLines
	ps, err = newPersister(cfg, g, func() (string, error) { return "", errors.New("no /proc") }, time.Now, logs.logf)
	if err != nil || ps.store != nil {
		t.Fatalf("unreadable boot ID: store=%v err=%v", ps.store, err)
	}
	if len(logs) != 1 || !logs.has("boot ID unreadable") {
		t.Fatalf("logs = %v", logs)
	}
	eng := engine.New(cfg, g, nil)
	eng.Gray["mem.0"] = 2
	ps.idleResolved(eng, model.Report{Trigger: model.TriggerIdle, Started: time.Now()}, true)
	ps.shutdown(eng, &idleHold{})
	if ents, _ := os.ReadDir(cfg.StateDir); len(ents) != 0 {
		t.Fatalf("disabled persistence wrote %d entries", len(ents))
	}
}

// An unwritable --state-dir fails the start, like --report-file.
func TestPersisterRejectsUnwritableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.StateDir = filepath.Join(file, "state") // under a regular file: never creatable
	_, err := newPersister(cfg, topology.Fig2(), bootID("boot-a"), time.Now, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "--state-dir") {
		t.Fatalf("err = %v, want a --state-dir error", err)
	}
}

// A corrupt file is ignored with a log line, never fatal.
func TestCorruptStateIsIgnored(t *testing.T) {
	cfg := stateCfg(t)
	if err := os.WriteFile(filepath.Join(cfg.StateDir, state.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemon(t, cfg, "boot-a")
	if len(d.eng.Gray) != 0 || !d.logs.has("corrupt") {
		t.Fatalf("gray=%v logs=%v", d.eng.Gray, *d.logs)
	}
	// The next save replaces it.
	d.confirmedIdleRun(t)
	if f := readState(t, cfg.StateDir); f.Version != state.Version {
		t.Fatalf("state not rewritten: %+v", f)
	}
}

func readState(t *testing.T, dir string) state.File {
	t.Helper()
	st, err := state.Open(dir, "boot-a", state.Fingerprint(topology.Fig2()))
	if err != nil {
		t.Fatal(err)
	}
	f, ok, why := st.Load(time.Now(), 24*time.Hour)
	if !ok {
		t.Fatalf("state file not loadable: %s", why)
	}
	return f
}

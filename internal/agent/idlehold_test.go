package agent

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// idleHost is a Fig.2 monitor over a sampler the test steps by hand: counter
// deltas of zero are idle, any positive delta over the few microseconds
// between polls is far above Thp_low.
type idleHost struct {
	t   *testing.T
	m   *monitor.Monitor
	s   monitor.StaticSampler
	tx  uint64
	gpu []model.GPUStatus
}

func newIdleHost(t *testing.T) *idleHost {
	t.Helper()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	gpu := []model.GPUStatus{{ID: "gpu0", Idle: true}}
	s := monitor.StaticSampler{Counters: map[string]model.RNICCounters{"rnic0": {}}, GPU: gpu}
	return &idleHost{t: t, m: monitor.New(cfg, topology.Fig2(), s), s: s, gpu: gpu}
}

func (h *idleHost) poll() monitor.Decision {
	h.t.Helper()
	h.s.Counters["rnic0"] = model.RNICCounters{TxBytes: h.tx}
	d, err := h.m.Poll(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

// idleRun warms the monitor up, takes the idle trigger, and holds its report
// as the daemon loop does after the engine returns.
func (h *idleHost) idleRun(p *idleHold) {
	h.t.Helper()
	if d := h.poll(); d.Run {
		h.t.Fatalf("warmup must not run: %+v", d)
	}
	d := h.poll()
	if !d.Run || d.Trigger != model.TriggerIdle || d.Mode != model.ModeIdle {
		h.t.Fatalf("idle host must take the idle trigger: %+v", d)
	}
	if _, out, _ := p.resolve(nil, d); out != holdNone {
		h.t.Fatalf("nothing held yet, got outcome %v", out)
	}
	h.m.MarkRun(time.Now())
	if !needsIdleConfirm(d.Mode) {
		h.t.Fatal("an idle-mode run must be confirmed")
	}
	p.hold(model.Report{Trigger: d.Trigger, Mode: d.Mode, Paths: make([]model.PathMeasurement, 3)}, engine.Streaks{})
	// The run's own probe traffic lands in the next delta.
	h.tx += 1 << 30
	d = h.poll()
	if d.Ready {
		h.t.Fatalf("first poll after the run must re-baseline: %+v", d)
	}
	if _, out, _ := p.resolve(nil, d); out != holdWaiting {
		h.t.Fatalf("re-baseline poll must keep the report held, got %v", out)
	}
}

// A collective workload that comes back mid-run keeps an RNIC above Thp_low
// on the first clean poll: the report is discarded, and the next idle attempt
// follows the usual --idle-interval rather than the next idle poll, so a
// workload that pauses between iterations does not get a full mesh in every
// pause.
func TestIdleHoldDiscardsWhenRNICBusyAfterRun(t *testing.T) {
	h := newIdleHost(t)
	var p idleHold
	h.idleRun(&p)

	h.tx += 1 << 30 // the workload
	d := h.poll()
	if !d.Ready || d.HostIdle {
		t.Fatalf("clean poll must see the load: %+v", d)
	}
	rep, out, why := p.resolve(nil, d)
	if out != holdDiscarded || rep.Trigger != model.TriggerIdle {
		t.Fatalf("busy clean poll must discard the idle report, got %v %+v", out, rep)
	}
	if !strings.Contains(why, "rnic0") {
		t.Fatalf("reason %q must name the busy RNIC", why)
	}
	if _, out, _ := p.resolve(nil, d); out != holdNone {
		t.Fatalf("a resolved report must not be resolved twice, got %v", out)
	}

	// The workload pauses again: no idle run inside the one-hour interval.
	if d := h.poll(); d.Run {
		t.Fatalf("a discarded idle run must keep the idle interval: %+v", d)
	}
}

func TestIdleHoldDiscardsWhenGPUBusyAfterRun(t *testing.T) {
	h := newIdleHost(t)
	var p idleHold
	h.idleRun(&p)

	h.gpu[0].Idle = false
	_, out, why := p.resolve(nil, h.poll())
	if out != holdDiscarded || !strings.Contains(why, "gpu0") {
		t.Fatalf("busy GPU must discard, got %v %q", out, why)
	}
}

func TestIdleHoldDiscardsWhenIdleUnknownAfterRun(t *testing.T) {
	h := newIdleHost(t)
	var p idleHold
	h.idleRun(&p)

	h.gpu[0].Unknown = true
	_, out, why := p.resolve(nil, h.poll())
	if out != holdDiscarded || !strings.Contains(why, "could not be read") {
		t.Fatalf("an unclassified host cannot confirm the run, got %v %q", out, why)
	}
}

// The host stays idle: the report is emitted on the first clean poll and the
// idle trigger keeps its --idle-interval.
func TestIdleHoldConfirmsWhenHostStaysIdle(t *testing.T) {
	h := newIdleHost(t)
	var p idleHold
	h.idleRun(&p)

	d := h.poll()
	rep, out, _ := p.resolve(nil, d)
	if out != holdConfirmed || rep.Trigger != model.TriggerIdle || len(rep.Paths) != 3 {
		t.Fatalf("idle clean poll must release the report, got %v %+v", out, rep)
	}
	if d.Run {
		t.Fatalf("confirmed idle run must keep the idle interval: %+v", d)
	}
	if d := h.poll(); d.Run {
		t.Fatalf("confirmed idle run must keep the idle interval: %+v", d)
	}
}

// A mutual-alarm run on an idle host is an idle-mode judgement and is held the
// same way.
func TestIdleHoldDiscardsAlarmRunOnIdleHost(t *testing.T) {
	h := newIdleHost(t)
	var p idleHold
	h.idleRun(&p)
	if _, out, _ := p.resolve(nil, h.poll()); out != holdConfirmed {
		t.Fatalf("idle run must be confirmed, got %v", out)
	}

	h.m.MarkRun(time.Now())
	p.hold(model.Report{Trigger: model.TriggerMutualTimeout, Mode: model.ModeIdle}, engine.Streaks{})
	h.poll() // re-baseline
	h.tx += 1 << 30
	rep, out, _ := p.resolve(nil, h.poll())
	if out != holdDiscarded || rep.Trigger != model.TriggerMutualTimeout {
		t.Fatalf("busy clean poll must discard the alarm run, got %v %+v", out, rep)
	}
}

func TestBusyModeRunsAreNotHeld(t *testing.T) {
	if needsIdleConfirm(model.ModeBusy) {
		t.Fatal("busy-mode runs already expect load")
	}
}

// pollutedProber is the simulated host with a workload that comes back during
// the run: while polluted, rnic0's memory path measures a third of its idle
// bandwidth, which grays the memory link it crosses.
type pollutedProber struct {
	sim.Prober
	polluted *bool
}

func (p pollutedProber) Loopback(ctx context.Context, n model.RNIC, ep model.Endpoint, small, large uint32) (model.PathMeasurement, error) {
	m, err := p.Prober.Loopback(ctx, n, ep, small, large)
	if err == nil && *p.polluted && n.ID == "rnic0" && ep.ID == "mem0" {
		m.BandwidthBPS /= 3
	}
	return m, err
}

func hasFlapping(rep model.Report) bool {
	for _, c := range rep.Causes {
		if strings.HasSuffix(string(c.Code), "_flapping") {
			return true
		}
	}
	return false
}

// idleEngineRun runs one idle full mesh and holds its report as the daemon
// loop does.
func idleEngineRun(t *testing.T, eng *engine.Engine, p *idleHold) {
	t.Helper()
	before := eng.SnapshotStreaks()
	rep, err := eng.Run(context.Background(), engine.RunRequest{Trigger: model.TriggerIdle, Mode: model.ModeIdle, HostIdle: true})
	if err != nil {
		t.Fatal(err)
	}
	p.hold(rep, before)
}

var (
	busyAfterRun = monitor.Decision{Ready: true, IdleKnown: true, BusyRNICs: map[string]bool{"rnic0": true}}
	idleAfterRun = monitor.Decision{Ready: true, IdleKnown: true, HostIdle: true}
)

// Discarded runs leave no trace in the engine: however many polluted idle
// runs are thrown away, their gray streaks never add up to a flapping cause.
func TestDiscardedIdleRunsDoNotAccumulateGrayHits(t *testing.T) {
	cfg := config.Defaults()
	g := topology.Fig2()
	polluted := true
	eng := engine.New(cfg, g, pollutedProber{Prober: sim.Prober{World: sim.New(g)}, polluted: &polluted})
	eng.Logf = func(string, ...any) {}
	var p idleHold

	for i := 0; i < cfg.FlapWindow+3; i++ {
		idleEngineRun(t, eng, &p)
		if len(eng.Gray) == 0 {
			t.Fatal("a polluted run must gray a link, or this test proves nothing")
		}
		if _, out, _ := p.resolve(eng, busyAfterRun); out != holdDiscarded {
			t.Fatalf("run %d: busy clean poll must discard, got %v", i, out)
		}
		if len(eng.Gray) != 0 {
			t.Fatalf("run %d: discarded run left gray hits %v", i, eng.Gray)
		}
	}

	polluted = false
	idleEngineRun(t, eng, &p)
	rep, out, _ := p.resolve(eng, idleAfterRun)
	if out != holdConfirmed {
		t.Fatalf("clean idle run must be confirmed, got %v", out)
	}
	if hasFlapping(rep) || len(eng.Gray) != 0 {
		t.Fatalf("clean run after discarded ones: causes=%v gray=%v", rep.Causes, eng.Gray)
	}
}

// A confirmed run keeps its gray streak: a link gray in FlapWindow confirmed
// idle runs is still diagnosed as flapping.
func TestConfirmedIdleRunsKeepGrayHits(t *testing.T) {
	cfg := config.Defaults()
	g := topology.Fig2()
	polluted := true
	eng := engine.New(cfg, g, pollutedProber{Prober: sim.Prober{World: sim.New(g)}, polluted: &polluted})
	eng.Logf = func(string, ...any) {}
	var p idleHold

	var rep model.Report
	for i := 0; i < cfg.FlapWindow; i++ {
		idleEngineRun(t, eng, &p)
		var out holdOutcome
		rep, out, _ = p.resolve(eng, idleAfterRun)
		if out != holdConfirmed {
			t.Fatalf("run %d: idle clean poll must confirm, got %v", i, out)
		}
	}
	if !hasFlapping(rep) {
		t.Fatalf("gray in %d confirmed runs must flap: causes=%v gray=%v", cfg.FlapWindow, rep.Causes, eng.Gray)
	}
}

// The snapshot is a deep copy both ways: neither later runs nor a restored
// engine reach into it.
func TestSnapshotStreaksIsACopy(t *testing.T) {
	eng := engine.New(config.Defaults(), topology.Fig2(), nil)
	eng.Gray["mem.0"] = 2
	eng.SlowMR["gpu3"] = 1
	eng.Latency["rnic0->gpu3"] = 1
	s := eng.SnapshotStreaks()
	eng.Gray["mem.0"] = 5
	eng.SlowMR["gpu3"] = 7
	eng.Latency["rnic0->gpu3"] = 7
	eng.RestoreStreaks(s)
	eng.Gray["mem.0"]++
	eng.SlowMR["gpu3"]++
	eng.Latency["rnic0->gpu3"]++
	if s.Gray["mem.0"] != 2 || eng.Gray["mem.0"] != 3 || s.SlowMR["gpu3"] != 1 || eng.SlowMR["gpu3"] != 2 ||
		s.Latency["rnic0->gpu3"] != 1 || eng.Latency["rnic0->gpu3"] != 2 {
		t.Fatalf("snapshot=%+v gray=%v slow_mr=%v latency=%v", s, eng.Gray, eng.SlowMR, eng.Latency)
	}
}

func hasSlowMR(rep model.Report) bool {
	for _, c := range rep.Causes {
		if c.Code == model.CauseGPUMRRegistrationSlow {
			return true
		}
	}
	return false
}

// slowMREngine is the simulated Fig.2 host with gpu3's registration slow from
// every RNIC.
func slowMREngine(t *testing.T, cfg config.Config) *engine.Engine {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault("slow_mr:gpu3"); err != nil {
		t.Fatal(err)
	}
	eng := engine.New(cfg, g, sim.Prober{World: w})
	eng.Logf = func(string, ...any) {}
	return eng
}

// A discarded idle run rolls the slow-registration streak back with the gray
// ones: however many polluted runs are thrown away, only confirmed ones count
// towards the two consecutive idle runs gpu_mr_registration_slow needs.
func TestDiscardedIdleRunRollsSlowMRStreakBack(t *testing.T) {
	eng := slowMREngine(t, config.Defaults())
	var p idleHold

	idleEngineRun(t, eng, &p)
	rep, out, _ := p.resolve(eng, idleAfterRun)
	if out != holdConfirmed || hasSlowMR(rep) || eng.SlowMR["gpu3"] != 1 {
		t.Fatalf("first confirmed run: out=%v causes=%v slow_mr=%v", out, rep.Causes, eng.SlowMR)
	}
	for i := 0; i < 3; i++ {
		idleEngineRun(t, eng, &p)
		if eng.SlowMR["gpu3"] != 2 {
			t.Fatalf("run %d: the run must extend the streak before it is discarded, or this test proves nothing: %v", i, eng.SlowMR)
		}
		if _, out, _ := p.resolve(eng, busyAfterRun); out != holdDiscarded {
			t.Fatalf("run %d: busy clean poll must discard, got %v", i, out)
		}
		if eng.SlowMR["gpu3"] != 1 {
			t.Fatalf("run %d: discarded run left slow_mr %v", i, eng.SlowMR)
		}
	}
	idleEngineRun(t, eng, &p)
	rep, out, _ = p.resolve(eng, idleAfterRun)
	if out != holdConfirmed || !hasSlowMR(rep) {
		t.Fatalf("second confirmed run must report slow MR: out=%v causes=%v", out, rep.Causes)
	}
}

func hasLatency(rep model.Report) bool {
	for _, c := range rep.Causes {
		if c.Code == model.CausePathLatencyAbnormal {
			return true
		}
	}
	return false
}

// latencyEngine is the simulated Fig.2 host with every path to gpu3 slower
// than its siblings, a rise the paired recheck confirms on every run.
func latencyEngine(t *testing.T, cfg config.Config) *engine.Engine {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault("lat:pcie.gpu3"); err != nil {
		t.Fatal(err)
	}
	eng := engine.New(cfg, g, sim.Prober{World: w})
	eng.Logf = func(string, ...any) {}
	return eng
}

// A discarded idle run rolls the path latency streaks back with the others:
// only confirmed idle runs count towards the analyzer.LatencyRuns
// consecutive runs a sibling latency verdict needs.
func TestDiscardedIdleRunRollsLatencyStreakBack(t *testing.T) {
	eng := latencyEngine(t, config.Defaults())
	var p idleHold

	idleEngineRun(t, eng, &p)
	rep, out, _ := p.resolve(eng, idleAfterRun)
	if out != holdConfirmed || hasLatency(rep) || len(eng.Latency) == 0 {
		t.Fatalf("first confirmed run: out=%v causes=%v latency=%v", out, rep.Causes, eng.Latency)
	}
	after1 := maps.Clone(eng.Latency)
	for k, v := range after1 {
		if v != 1 || !strings.HasSuffix(k, "->gpu3") {
			t.Fatalf("first run streaks = %v, want 1 on paths to gpu3", after1)
		}
	}
	for i := 0; i < 3; i++ {
		idleEngineRun(t, eng, &p)
		for k := range after1 {
			if eng.Latency[k] != 2 {
				t.Fatalf("run %d: the run must extend the streak before it is discarded, or this test proves nothing: %v", i, eng.Latency)
			}
		}
		if _, out, _ := p.resolve(eng, busyAfterRun); out != holdDiscarded {
			t.Fatalf("run %d: busy clean poll must discard, got %v", i, out)
		}
		if !maps.Equal(eng.Latency, after1) {
			t.Fatalf("run %d: discarded run left latency streaks %v, want %v", i, eng.Latency, after1)
		}
	}
	for n := 2; n <= analyzer.LatencyRuns; n++ {
		idleEngineRun(t, eng, &p)
		rep, out, _ = p.resolve(eng, idleAfterRun)
		if out != holdConfirmed || hasLatency(rep) != (n == analyzer.LatencyRuns) {
			t.Fatalf("confirmed run %d of %d: out=%v causes=%v", n, analyzer.LatencyRuns, out, rep.Causes)
		}
	}
}

// gpuFlipSampler reports every GPU busy from the given GPUs call on.
type gpuFlipSampler struct {
	monitor.StaticSampler
	calls  *int
	busyAt int
}

func (s gpuFlipSampler) GPUs(context.Context) ([]model.GPUStatus, error) {
	*s.calls++
	return []model.GPUStatus{{ID: "gpu0", Idle: *s.calls < s.busyAt}}, nil
}

// One-shot idle diagnose: one clean interval after the run decides whether the
// idle verdicts stand.
func TestConfirmIdleAfterRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		busyAt int
		ok     bool
	}{
		{"stays idle", 1 << 30, true},
		{"gpu busy after run", 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.MetricInterval = time.Millisecond
			calls := 0
			s := gpuFlipSampler{calls: &calls, busyAt: tc.busyAt}
			m := monitor.New(cfg, topology.Fig2(), s)
			// liveRequest's warmup + decision polls.
			for i := 0; i < 2; i++ {
				if _, err := m.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			ok, why, err := confirmIdleAfterRun(context.Background(), m, cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.ok {
				t.Fatalf("ok=%v why=%q, want ok=%v", ok, why, tc.ok)
			}
			if !ok && !strings.Contains(why, "gpu0") {
				t.Fatalf("reason %q must name the busy GPU", why)
			}
			if calls != 4 {
				t.Fatalf("confirmation must take the re-baseline and one clean poll, sampler polled %d times", calls)
			}
		})
	}
}

// CPU load that appears on a package during an idle-mode run
// reached paths that were judged against idle baselines, so the run is
// discarded. Load on a package that was already loaded when the run started
// changes nothing: the run judged its paths cpu_busy.
func TestIdleHoldCPULoad(t *testing.T) {
	load := func(high ...int) []model.CPULoad {
		out := []model.CPULoad{{NUMA: 0, Socket: 0, Busy: 0.1}, {NUMA: 1, Socket: 1, Busy: 0.1}}
		for _, s := range high {
			out[s].Busy, out[s].High = 0.9, true
		}
		return out
	}
	idle := func(l []model.CPULoad) monitor.Decision {
		return monitor.Decision{Ready: true, IdleKnown: true, HostIdle: true, CPULoad: l}
	}
	for _, tc := range []struct {
		name         string
		before, poll []model.CPULoad
		want         holdOutcome
	}{
		{"no load", load(), load(), holdConfirmed},
		{"load persists", load(1), load(1), holdConfirmed},
		{"load ends", load(1), load(), holdConfirmed},
		{"load rises", load(), load(1), holdDiscarded},
		{"load moves", load(0), load(1), holdDiscarded},
		{"no CPU signal", nil, nil, holdConfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p idleHold
			p.hold(model.Report{Trigger: model.TriggerIdle, CPULoad: tc.before}, engine.Streaks{})
			_, out, why := p.resolve(nil, idle(tc.poll))
			if out != tc.want {
				t.Fatalf("outcome %v (%q), want %v", out, why, tc.want)
			}
			if out == holdDiscarded && !strings.Contains(why, "socket 1") {
				t.Errorf("reason %q must name the loaded socket", why)
			}
		})
	}

	var p idleHold
	p.hold(model.Report{Trigger: model.TriggerIdle}, engine.Streaks{})
	_, out, why := p.resolve(nil, monitor.Decision{Ready: true, CPUUnknown: true})
	if out != holdDiscarded || !strings.Contains(why, "CPU load unknown") {
		t.Fatalf("unread CPU load cannot confirm the run, got %v %q", out, why)
	}
}

func TestRequestFromDecisionCPU(t *testing.T) {
	if _, err := requestFromDecision(monitor.Decision{Ready: true, CPUUnknown: true}); err == nil || !strings.Contains(err.Error(), "CPU load unread") {
		t.Fatalf("unread CPU load must refuse a live one-shot, got %v", err)
	}
	l := []model.CPULoad{{NUMA: 1, Socket: 1, Busy: 0.9, High: true}}
	req, err := requestFromDecision(monitor.Decision{Ready: true, IdleKnown: true, HostIdle: true, CPULoad: l})
	if err != nil {
		t.Fatal(err)
	}
	if req.Mode != model.ModeIdle || len(req.CPULoad) != 1 || !req.CPULoad[0].High {
		t.Fatalf("request %+v: CPU load is carried and does not make the host busy", req)
	}
}

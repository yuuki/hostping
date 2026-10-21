package monitor_test

import (
	"context"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/topology"
)

func TestPFCTriggersBusyEngine(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{
			"rnic0": {RxBytes: 0, TxPauseDurationUs: 0},
		},
		GPU: []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := monitor.New(cfg, g, s)
	d0, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d0.Run {
		t.Fatalf("warmup poll must not trigger: %+v", d0)
	}
	s.Counters["rnic0"] = model.RNICCounters{RxBytes: 1 << 20, TxPauseDurationUs: 40_000}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Run || d.Trigger != model.TriggerPFC || d.Mode != model.ModeBusy {
		t.Fatalf("decision %+v", d)
	}
	if !d.BusyRNICs["rnic0"] {
		t.Fatalf("PFC RNIC must be in BusyRNICs even when throughput is below Thplow: %+v", d.BusyRNICs)
	}
}

func TestIdleHostSchedulesFullMeshAfterWarmup(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	m := monitor.New(cfg, g, monitor.StaticSampler{GPU: []model.GPUStatus{{ID: "gpu0", Idle: true}}})
	d0, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d0.Run || d0.HostIdle {
		t.Fatalf("first sample is warmup, got %+v", d0)
	}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Run || d.Trigger != model.TriggerIdle || d.Mode != model.ModeIdle || !d.HostIdle {
		t.Fatalf("decision %+v", d)
	}
}

func TestUnknownGPUIsNotIdle(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	m := monitor.New(cfg, g, monitor.StaticSampler{GPU: []model.GPUStatus{{ID: "gpu0", Unknown: true}}})
	_, _ = m.Poll(context.Background())
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.HostIdle || d.Trigger == model.TriggerIdle {
		t.Fatalf("unknown GPU must not look idle: %+v", d)
	}
	if d.IdleKnown {
		t.Fatal("unknown GPU must leave IdleKnown false")
	}
}

func TestUnknownLineRateIsNotIdle(t *testing.T) {
	g := topology.Fig2()
	g.RNICs[0].LineRate = 0
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	m := monitor.New(cfg, g, monitor.StaticSampler{GPU: []model.GPUStatus{{ID: "gpu0", Idle: true}}})
	_, _ = m.Poll(context.Background())
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.HostIdle || d.Trigger == model.TriggerIdle {
		t.Fatalf("unknown line rate must not start idle full-mesh: %+v", d)
	}
	if d.IdleKnown {
		t.Fatal("unknown line rate must leave IdleKnown false")
	}
}

func TestCounterWrap32(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{
			"rnic0": {RxBytes: 0xfffffff0, TxBytes: 0},
		},
		GPU: []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := monitor.New(cfg, g, s)
	_, _ = m.Poll(context.Background())
	s.Counters["rnic0"] = model.RNICCounters{RxBytes: 0x20, TxBytes: 0}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Rates[0].ThroughputBPS == 0 {
		t.Fatalf("wrap should produce a positive delta: %+v", d.Rates[0])
	}
}

func TestDropWrap32DoesNotSumThenDelta(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{
			"rnic0": {Drops: map[string]uint64{"rx_discards_phy": 0xfffffff0, "tx_discards_phy": 0xfffffff0}},
		},
		GPU: []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := monitor.New(cfg, g, s)
	_, _ = m.Poll(context.Background())
	s.Counters["rnic0"] = model.RNICCounters{Drops: map[string]uint64{"rx_discards_phy": 0x10, "tx_discards_phy": 0x20}}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Independent wrap of each 32-bit counter: (0x10+0x10) + (0x20+0x10) = 0x50
	if got := d.DropDelta["rnic0"].FaultTotal(); got != 0x50 || d.Rates[0].DropDelta != 0x50 {
		t.Fatalf("drop delta=%d rate=%d want 0x50", got, d.Rates[0].DropDelta)
	}
}

func TestIdleKnownAfterWarmup(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	m := monitor.New(cfg, g, monitor.StaticSampler{GPU: []model.GPUStatus{{ID: "gpu0", Idle: true}}})
	d0, _ := m.Poll(context.Background())
	if d0.IdleKnown {
		t.Fatal("warmup is not IdleKnown")
	}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.IdleKnown || !d.HostIdle {
		t.Fatalf("known idle %+v", d)
	}
}

func TestBusyCooldownRateLimitsDropTriggers(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	cfg.BusyCooldown = time.Hour
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {Drops: drops(0)}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := monitor.New(cfg, g, s)
	if _, err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs := 0
	for i := 1; i <= 5; i++ {
		s.Counters["rnic0"] = model.RNICCounters{Drops: drops(uint64(i))}
		d, err := m.Poll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if d.Run {
			runs++
			if d.Trigger != model.TriggerDrops || d.Mode != model.ModeBusy {
				t.Fatalf("poll %d decision %+v", i, d)
			}
		} else if d.Trigger != "" || d.Mode != "" {
			t.Fatalf("suppressed poll %d must carry no trigger: %+v", i, d)
		}
		// Drops stay visible to telemetry even when the run is suppressed.
		if !d.BusyRNICs["rnic0"] || d.DropDelta["rnic0"].FaultTotal() == 0 {
			t.Fatalf("poll %d must still report the busy RNIC: %+v", i, d)
		}
	}
	if runs != 1 {
		t.Fatalf("busy cooldown must allow one run, got %d/5", runs)
	}
}

func TestBusyTriggerReArmsAfterCooldown(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	cfg.BusyCooldown = time.Millisecond
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {Drops: drops(0)}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := monitor.New(cfg, g, s)
	if _, err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Counters["rnic0"] = model.RNICCounters{Drops: drops(1)}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Run {
		t.Fatalf("first busy poll must run: %+v", d)
	}
	time.Sleep(2 * time.Millisecond)
	s.Counters["rnic0"] = model.RNICCounters{Drops: drops(2)}
	d, err = m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Run || d.Trigger != model.TriggerDrops {
		t.Fatalf("busy trigger must re-arm after the cooldown: %+v", d)
	}
}

func TestPFCKeepsPriorityOverDrops(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {Drops: drops(0)}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := monitor.New(cfg, g, s)
	if _, err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Counters["rnic0"] = model.RNICCounters{Drops: drops(3), TxPauseDurationUs: 40_000}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Run || d.Trigger != model.TriggerPFC {
		t.Fatalf("PFC must win over drops: %+v", d)
	}
}

// busPollSampler records the PollBus calls a Monitor makes.
type busPollSampler struct {
	monitor.StaticSampler
	resets *[]bool
}

func (s busPollSampler) PollBus(_ context.Context, reset bool) { *s.resets = append(*s.resets, reset) }

func TestPollAdvancesBusPollerAndResetsAfterRun(t *testing.T) {
	var resets []bool
	s := busPollSampler{StaticSampler: monitor.StaticSampler{GPU: []model.GPUStatus{{ID: "gpu0", Idle: true}}}, resets: &resets}
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	m := monitor.New(cfg, topology.Fig2(), s)
	poll := func() {
		if _, err := m.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	poll()
	m.MarkRun(time.Now())
	poll()
	poll()
	// Only the first poll after the run holds the run's own traffic.
	want := []bool{false, false, true, false}
	if len(resets) != len(want) {
		t.Fatalf("PollBus resets %v, want %v", resets, want)
	}
	for i := range want {
		if resets[i] != want[i] {
			t.Fatalf("PollBus resets %v, want %v", resets, want)
		}
	}
}

func TestMarkRunDiscardsTheNextSample(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	cfg.BusyCooldown = 0
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {Drops: drops(0)}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := monitor.New(cfg, g, s)
	if _, err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Counters["rnic0"] = model.RNICCounters{Drops: drops(1)}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Run {
		t.Fatalf("busy poll must run with the cooldown disabled: %+v", d)
	}
	m.MarkRun(time.Now())
	// Probe traffic: drops and bytes jump, but this delta spans the engine run.
	s.Counters["rnic0"] = model.RNICCounters{Drops: drops(99), RxBytes: 1 << 30}
	d, err = m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Run || d.Ready || d.Trigger != "" {
		t.Fatalf("post-run sample must be re-baselined, got %+v", d)
	}
	if d.DropDelta["rnic0"].FaultTotal() != 0 || d.Rates[0].ThroughputBPS != 0 {
		t.Fatalf("post-run sample must not derive rates from probe traffic: %+v", d)
	}
	// The sample after that is measured against the post-run baseline.
	s.Counters["rnic0"] = model.RNICCounters{Drops: drops(100), RxBytes: 1 << 30}
	d, err = m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Ready || d.DropDelta["rnic0"].FaultTotal() != 1 {
		t.Fatalf("baseline must resume from the discarded sample: %+v", d)
	}
}

func TestIdleTriggerIsNotAffectedByBusyCooldown(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.BusyCooldown = time.Hour
	m := monitor.New(cfg, g, monitor.StaticSampler{GPU: []model.GPUStatus{{ID: "gpu0", Idle: true}}})
	if _, err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		d, err := m.Poll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !d.Run || d.Trigger != model.TriggerIdle || d.Mode != model.ModeIdle || !d.HostIdle {
			t.Fatalf("idle poll %d decision %+v", i, d)
		}
	}
}

// loadedHost drives a Fig.2 monitor on a stepped clock. Each step advances one
// second and moves the listed RNICs' Tx counters by 90 % of line rate, with no
// PFC pause and no drops: the load §4.2's triggers cannot see.
type loadedHost struct {
	t      *testing.T
	m      *monitor.Monitor
	s      monitor.StaticSampler
	clock  time.Time
	loaded []string
	tx     uint64
	pause  uint64
}

func newLoadedHost(t *testing.T, cfg config.Config, loaded ...string) *loadedHost {
	t.Helper()
	g := topology.Fig2()
	h := &loadedHost{
		t: t,
		s: monitor.StaticSampler{
			Counters: map[string]model.RNICCounters{},
			GPU:      []model.GPUStatus{{ID: "gpu0", Idle: false}},
		},
		clock:  time.Unix(1_000_000, 0),
		loaded: loaded,
	}
	h.m = monitor.New(cfg, g, h.s)
	h.m.SetClock(func() time.Time { return h.clock })
	if d := h.poll(); d.Ready || d.Run {
		t.Fatalf("warmup poll must not run: %+v", d)
	}
	return h
}

func (h *loadedHost) poll() monitor.Decision {
	h.t.Helper()
	for _, id := range h.loaded {
		h.s.Counters[id] = model.RNICCounters{TxBytes: h.tx, TxPauseDurationUs: h.pause}
	}
	d, err := h.m.Poll(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

// step advances the clock by one second of 90 % line-rate load and polls.
func (h *loadedHost) step() monitor.Decision {
	h.t.Helper()
	h.clock = h.clock.Add(time.Second)
	h.tx += uint64(0.9 * 200e9 / 8)
	return h.poll()
}

func TestBusyIntervalMeasuresLoadedHostWithoutPFC(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.BusyInterval = time.Minute // cooldown stays at its 30 s default
	// Partly loaded: rnic0 and rnic1 carry traffic, rnic2 and rnic3 do not.
	h := newLoadedHost(t, cfg, "rnic0", "rnic1")
	var runs []int
	for i := 1; i <= 121; i++ {
		d := h.step()
		if !d.Ready || d.HostIdle {
			t.Fatalf("step %d: loaded host must be ready and not idle: %+v", i, d)
		}
		if !d.Run {
			if d.Trigger != "" || d.Mode != "" {
				t.Fatalf("step %d: no run must carry no trigger: %+v", i, d)
			}
			continue
		}
		runs = append(runs, i)
		if d.Trigger != model.TriggerPeriodic || d.Mode != model.ModeBusy {
			t.Fatalf("step %d decision %+v", i, d)
		}
		if len(d.BusyRNICs) != 2 || !d.BusyRNICs["rnic0"] || !d.BusyRNICs["rnic1"] {
			t.Fatalf("step %d: busy set must be the RNICs above Thp_low, got %+v", i, d.BusyRNICs)
		}
	}
	// The interval starts at the first ready poll (step 1): runs at +60 s and
	// +120 s, none before.
	if len(runs) != 2 || runs[0] != 61 || runs[1] != 121 {
		t.Fatalf("periodic runs at steps %v, want [61 121]", runs)
	}
}

func TestBusyIntervalOffByDefault(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	if cfg.BusyInterval != 0 {
		t.Fatalf("--busy-interval must default to off, got %v", cfg.BusyInterval)
	}
	h := newLoadedHost(t, cfg, "rnic0", "rnic1", "rnic2", "rnic3")
	for i := 1; i <= 3600; i++ {
		if d := h.step(); d.Run {
			t.Fatalf("step %d: default config must not measure a loaded host without PFC/drops: %+v", i, d)
		}
	}
}

func TestBusyIntervalIdleHostKeepsIdleTrigger(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.BusyInterval = time.Nanosecond
	cfg.BusyCooldown = 0
	h := newLoadedHost(t, cfg)
	h.s.GPU[0].Idle = true
	for i := 1; i <= 5; i++ {
		d := h.step()
		if !d.Run || d.Trigger != model.TriggerIdle || d.Mode != model.ModeIdle {
			t.Fatalf("step %d: idle host must take the idle run: %+v", i, d)
		}
	}
}

func TestBusyIntervalYieldsToPFCAndItsCooldown(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.BusyInterval = 10 * time.Second
	cfg.BusyCooldown = 30 * time.Second
	h := newLoadedHost(t, cfg, "rnic0")
	for i := 1; i <= 5; i++ {
		if d := h.step(); d.Run {
			t.Fatalf("step %d: no run before the interval: %+v", i, d)
		}
	}
	// PFC at step 6: the paper trigger wins and restarts both clocks.
	h.pause += 100_000
	d := h.step()
	if !d.Run || d.Trigger != model.TriggerPFC || d.Mode != model.ModeBusy {
		t.Fatalf("PFC must take precedence over the periodic run: %+v", d)
	}
	// The periodic interval (10 s) passes long before the cooldown (30 s); the
	// cooldown still holds the next run until step 36.
	for i := 7; i <= 36; i++ {
		d := h.step()
		if i < 36 && d.Run {
			t.Fatalf("step %d: periodic run inside the busy cooldown: %+v", i, d)
		}
		if i == 36 && (!d.Run || d.Trigger != model.TriggerPeriodic) {
			t.Fatalf("step %d: periodic run must follow the cooldown: %+v", i, d)
		}
	}
}

func TestPFCRunRestartsBusyInterval(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.BusyInterval = time.Minute // cooldown stays at its 30 s default
	h := newLoadedHost(t, cfg, "rnic0")
	// The interval starts at step 1, so the periodic run is due at step 61.
	// A PFC run at step 51 (+50 s) measures the host and restarts it: the
	// next periodic run is at step 111 (+110 s), not 61.
	var runs []int
	var triggers []model.Trigger
	for i := 1; i <= 111; i++ {
		if i == 51 {
			h.pause += 100_000
		}
		if d := h.step(); d.Run {
			runs = append(runs, i)
			triggers = append(triggers, d.Trigger)
		}
	}
	if len(runs) != 2 || runs[0] != 51 || triggers[0] != model.TriggerPFC ||
		runs[1] != 111 || triggers[1] != model.TriggerPeriodic {
		t.Fatalf("runs at steps %v triggers %v, want PFC at 51 then periodic at 111", runs, triggers)
	}
}

// A run the daemon skipped (--idle-gate-command) measured nothing, so it
// leaves the busy interval where the run before put it.
func TestSkipRunKeepsBusyInterval(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.BusyInterval = time.Minute
	// The skipped idle run never touches the busy cooldown; a PFC run does,
	// so turn it off to see the interval alone.
	cfg.BusyCooldown = 0
	h := newLoadedHost(t, cfg, "rnic0")
	var runs []int
	for i := 1; i <= 61; i++ {
		if i == 51 {
			h.pause += 100_000
		}
		d := h.step()
		if !d.Run {
			continue
		}
		runs = append(runs, i)
		if i == 51 {
			h.m.SkipRun()
		}
	}
	if len(runs) != 2 || runs[0] != 51 || runs[1] != 61 {
		t.Fatalf("runs at steps %v, want the skipped one at 51 and periodic still at 61", runs)
	}
}

func TestMarkRunRestartsBusyInterval(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.BusyInterval = time.Minute
	cfg.BusyCooldown = 0
	h := newLoadedHost(t, cfg, "rnic0")
	for i := 1; i <= 40; i++ {
		if d := h.step(); d.Run {
			t.Fatalf("step %d: no run before the interval: %+v", i, d)
		}
	}
	// An engine run ending at step 40 (e.g. an operator's diagnose through
	// the same monitor) restarts the interval from its end.
	h.m.MarkRun(h.clock)
	var runs []int
	for i := 41; i <= 100; i++ {
		if d := h.step(); d.Run {
			runs = append(runs, i)
		}
	}
	if len(runs) != 1 || runs[0] != 100 {
		t.Fatalf("periodic runs at steps %v, want [100]", runs)
	}
}

func TestBusyIntervalNeedsLineRate(t *testing.T) {
	g := topology.Fig2()
	g.RNICs[0].LineRate = 0
	cfg := config.Defaults()
	cfg.BusyInterval = time.Nanosecond
	cfg.BusyCooldown = 0
	clock := time.Unix(1_000_000, 0)
	m := monitor.New(cfg, g, monitor.StaticSampler{GPU: []model.GPUStatus{{ID: "gpu0", Idle: false}}})
	m.SetClock(func() time.Time { return clock })
	for i := 0; i < 5; i++ {
		clock = clock.Add(time.Second)
		d, err := m.Poll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if d.Run {
			t.Fatalf("poll %d: an unclassifiable busy set must not start a periodic run: %+v", i, d)
		}
	}
}

// RNIC PCIe utilization comes from the same counter delta as the busy
// classification. The poll right after an engine run is re-baselined, so it
// carries no utilization at all: the run's own probe traffic must never read
// as host load on the hop. The next poll measures the load again.
func TestPollRNICPCIeUtilExcludesProbeTraffic(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := monitor.New(cfg, g, s)
	m.SetClock(func() time.Time { return clock })
	poll := func() monitor.Decision {
		t.Helper()
		d, err := m.Poll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	capBPS := func(id string) float64 {
		l, _ := g.Link(id)
		return float64(l.CapacityBPS)
	}

	if d := poll(); d.Util != nil {
		t.Fatalf("warmup poll has no delta, util must be unknown: %v", d.Util)
	}
	// 12.6 GB tx and 3 GB rx in one second: max(rx, tx) counts.
	clock = clock.Add(time.Second)
	s.Counters["rnic0"] = model.RNICCounters{TxBytes: 12_600_000_000, RxBytes: 3_000_000_000}
	d := poll()
	want := 12_600_000_000 * 8 / capBPS("pcie.rnic0")
	if got := d.Util["pcie.rnic0"]; got != want {
		t.Fatalf("pcie.rnic0 util = %v, want %v", got, want)
	}
	if v, ok := d.Util["pcie.rnic1"]; !ok || v != 0 {
		t.Fatalf("a quiet RNIC is a measured 0, got %v ok=%v", v, ok)
	}
	for _, id := range []string{"pcie.gpu0", "mem.0", "upi.0"} {
		if _, ok := d.Util[id]; ok {
			t.Fatalf("%s is not the monitor's to measure", id)
		}
	}

	// An engine run moves a lot of probe traffic through rnic0.
	m.MarkRun(clock)
	clock = clock.Add(time.Second)
	s.Counters["rnic0"] = model.RNICCounters{TxBytes: 40_000_000_000, RxBytes: 40_000_000_000}
	if d := poll(); d.Util != nil {
		t.Fatalf("re-baselined poll must not report the run's traffic as util: %v", d.Util)
	}
	clock = clock.Add(time.Second)
	s.Counters["rnic0"] = model.RNICCounters{TxBytes: 40_000_000_000, RxBytes: 40_000_000_000}
	if got := poll().Util["pcie.rnic0"]; got != 0 {
		t.Fatalf("after the run, a quiet RNIC is 0 again, got %v", got)
	}
}

// Discovery marks a PCIe capacity it could not read as assumed; utilization
// against it would be a guess, so it is unknown.
func TestPollRNICPCIeUtilUnknownOnAssumedCapacity(t *testing.T) {
	g := topology.Fig2()
	for i := range g.Links {
		if g.Links[i].ID == "pcie.rnic0" {
			g.Links[i].CapacityAssumed = true
		}
	}
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	s := monitor.StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := monitor.New(cfg, g, s)
	_, _ = m.Poll(context.Background())
	s.Counters["rnic0"] = model.RNICCounters{TxBytes: 1 << 30}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Util["pcie.rnic0"]; ok {
		t.Fatalf("util computed against an assumed capacity: %v", d.Util)
	}
	if _, ok := d.Util["pcie.rnic1"]; !ok {
		t.Fatalf("other RNICs keep theirs: %v", d.Util)
	}
}

// drops is a drop counter set holding one port discard counter at n.
func drops(n uint64) map[string]uint64 { return map[string]uint64{"rx_discards_phy": n} }

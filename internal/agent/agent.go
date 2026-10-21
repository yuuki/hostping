package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/buildinfo"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/mutual"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/state"
	"github.com/yuuki/hostping/internal/telemetry"
	"github.com/yuuki/hostping/internal/topology"
	"github.com/yuuki/hostping/internal/zigprobe"
)

// BuildInfo is this binary's build identity, including the version of the
// Zig probe library it loaded.
func BuildInfo() buildinfo.Info {
	b := buildinfo.Get()
	b.Zig = zigprobe.Version()
	return b
}

func LoadGraph(cfg config.Config) (topology.Graph, error) {
	g, err := loadGraph(cfg)
	if err != nil {
		return g, err
	}
	if err := applyCapacities(&g, cfg); err != nil {
		return topology.Graph{}, err
	}
	return g, nil
}

func loadGraph(cfg config.Config) (topology.Graph, error) {
	if len(cfg.ExcludeRNICs) > 0 && (cfg.TopologyFile != "" || cfg.Sim) {
		// The graph is not discovered, so there is nothing to exclude from:
		// leave the RNIC out of the topology file instead.
		return topology.Graph{}, fmt.Errorf("--exclude-rnic applies to sysfs discovery only, not to --sim or --topology-file")
	}
	if cfg.TopologyFile != "" {
		return topology.LoadFile(cfg.TopologyFile)
	}
	if cfg.Sim {
		return topology.Named(cfg.TopologyName)
	}
	g, err := topology.Discover(topology.DiscoverOptions{Exclude: cfg.ExcludeRNICs})
	if err != nil {
		return topology.Graph{}, fmt.Errorf("sysfs discovery: %w (use --sim for the Fig.2 world)", err)
	}
	if len(g.RNICs) == 0 {
		return topology.Graph{}, fmt.Errorf("no RNICs discovered; use --sim")
	}
	return g, nil
}

func NewProber(cfg config.Config, g topology.Graph) (engine.Prober, *sim.World, error) {
	p := zigprobe.Prober{
		Graph:           g,
		Iters:           cfg.Iters,
		MutualIters:     cfg.MutualIters,
		MemWarmup:       cfg.MemWarmup,
		TimeoutMS:       uint32(cfg.Timeout.Milliseconds()),
		MutualMsg:       cfg.MutualMsgBytes,
		MutualTimeoutMS: uint32(cfg.MutualTimeout.Milliseconds()),
		MutualTransport: cfg.MutualTransport,
	}
	if cfg.Sim {
		w := sim.New(g)
		if err := w.ApplyFault(cfg.SimFault); err != nil {
			return nil, nil, err
		}
		p.Sim = w
		return p, w, nil
	}
	return p, nil, nil
}

// rig is what an engine run needs, built the same way for the daemon, a
// one-shot diagnose and calibrate.
type rig struct {
	g       topology.Graph
	eng     *engine.Engine
	world   *sim.World
	sampler monitor.Sampler
	mon     *monitor.Monitor
	close   func()
	// logf receives the operator log lines (log.Printf in the binaries).
	logf func(format string, args ...any)
}

func newRig(cfg config.Config, logf func(string, ...any)) (*rig, error) {
	if err := cfg.ValidateMutual(); err != nil {
		return nil, err
	}
	if err := cfg.ValidateIdleGate(); err != nil {
		return nil, err
	}
	// A baseline file or flag that does not parse would otherwise fail every
	// engine run, which the daemon only logs: fail at start-up instead, and
	// say which distances the operator's values replace.
	ov, err := cfg.Overrides()
	if err != nil {
		return nil, fmt.Errorf("baseline: %w", err)
	}
	if lines := baseline.DescribeSources(ov); lines != "" {
		for _, line := range strings.Split(lines, "\n") {
			logf("hostping: %s", line)
		}
	}
	g, err := LoadGraph(cfg)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateDropIgnore(g.RNICs); err != nil {
		return nil, err
	}
	for _, n := range g.RNICs {
		if cfg.DropIgnored(n) {
			logf("hostping: %s (%s) drops ignored by --drop-ignore-rnic: still probed and exported, never a drop trigger or rnic_packet_drops", n.ID, n.Device)
		}
	}
	if line := baseline.DescribeTables(ov, g.RNICs); line != "" {
		logf("hostping: %s", line)
	}
	if line := baseline.DescribeUnmatched(ov, g.RootPortPairs()); line != "" {
		logf("hostping: %s", line)
	}
	prober, world, err := NewProber(cfg, g)
	if err != nil {
		return nil, err
	}
	rt := &rig{g: g, eng: engine.New(cfg, g, prober), world: world, close: func() {}, logf: logf}
	// A one-shot run keeps no streaks, so its latency verdicts cannot persist
	// across runs; the paired recheck stands in (analyzer.Input.OneShot).
	rt.eng.OneShot = cfg.Once
	if world != nil {
		rt.sampler = monitor.StaticSampler{Util: world.Util(), GPU: idleGPUs(g, world.ComputeModes())}
	} else {
		s, closeUncore := liveSampler(g, cfg)
		rt.sampler, rt.close = s, closeUncore
	}
	rt.mon = monitor.New(cfg, g, rt.sampler)
	return rt, nil
}

func Run(cfg config.Config) error {
	build := BuildInfo()
	rp, err := newReporter(cfg, os.Stdout, build)
	if err != nil {
		return err
	}
	rt, err := newRig(cfg, log.Printf)
	if err != nil {
		return err
	}
	defer rt.close()
	g := rt.g
	// Before any exporter binds: an unusable --state-dir fails the start.
	ps, err := newPersister(cfg, g, state.KernelBootID, time.Now, log.Printf)
	if err != nil {
		return err
	}
	// The daemon's poll loop reports its liveness on /healthz; a one-shot run
	// has no loop, and its /healthz (when --metrics is given) stays 200.
	var live *telemetry.Liveness
	if !cfg.Once {
		live = telemetry.NewLiveness(cfg.MetricInterval, nil)
	}
	tel, err := startTelemetry(cfg, telemetry.WithLiveness(live), telemetry.WithTopology(g.RNICs, g.Endpoints), telemetry.WithTopologyDiagram(g.Diagram()))
	if err != nil {
		return err
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()

	// SIGINT/SIGTERM must reach the engine: a probe loop can only be
	// interrupted between probes, so a run that is not watching ctx delays
	// shutdown by one probe timeout per remaining path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Once {
		return runOnce(ctx, rt, tel, cfg, rp)
	}

	log.Printf("hostping agent version=%s commit=%s topology=%s rnics=%d endpoints=%d sim=%v metrics=%s", build.Version, build.Commit, g.Name, len(g.RNICs), len(g.Endpoints), cfg.Sim, cfg.PrometheusAddr)
	loop, loopDone := startMutualLoop(ctx, cfg, g, rt.world, log.Printf)
	if loop != nil {
		defer func() { <-loopDone }() // runs after stop(): the loop sees ctx end and closes its endpoints
		rt.eng.Mutual = loop
		rt.mon.MutualAlarm = loop.Alarm
	}
	ps.restore(rt.eng, rt.mon)
	d := &daemon{rig: rt, loop: loop, tel: tel, live: live, rp: rp, ps: ps, gate: newSchedGate(cfg, log.Printf)}
	defer ps.shutdown(rt.eng, &d.pending)
	t := time.NewTicker(cfg.MetricInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if !d.tick(ctx) {
				return nil
			}
		}
	}
}

// daemon is the state of the daemon's poll loop over the rig it runs on.
type daemon struct {
	*rig
	loop    *mutual.Loop
	tel     *telemetry.Telemetry
	live    *telemetry.Liveness
	rp      *reporter
	ps      *persister
	pending idleHold
	// gate is --idle-gate-command; nil allows every idle run.
	gate *schedGate
}

// tick is one --metric-interval of the loop: a monitor poll, the held
// idle-mode report it settles, and the engine run it starts, if any. It
// returns false when shutdown cut it short.
func (d *daemon) tick(ctx context.Context) bool {
	dec, err := d.mon.Poll(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown cut the poll short; nothing to report.
			return false
		}
		d.live.Polled()
		d.tel.RecordPoll(ctx, time.Now(), err)
		d.logf("monitor: %v", err)
		return true
	}
	d.live.Polled()
	d.tel.RecordPoll(ctx, time.Now(), nil)
	if dec.Ready {
		d.tel.RecordMonitorIdle(ctx, dec.IdleKnown && dec.HostIdle)
		d.tel.RecordCPULoad(ctx, dec.CPULoad)
	}
	d.tel.RecordRates(ctx, dec.Rates)
	if d.loop != nil {
		if ms, ready := d.loop.Snapshot(); ready {
			d.tel.RecordMutual(ctx, ms)
		}
	}
	// Settle a held idle-mode report before this poll starts a run of its own.
	switch rep, out, why := d.pending.resolve(d.eng, dec); out {
	case holdConfirmed:
		d.ps.idleResolved(d.eng, rep, true)
		d.report(ctx, rep)
	case holdDiscarded:
		d.ps.idleResolved(d.eng, rep, false)
		d.tel.RecordDiscarded(ctx, discardReason(dec))
		d.logf("hostping: discarded idle-mode run (trigger=%s paths=%d): the host became busy during it, so its idle verdicts are unreliable: %s", rep.Trigger, len(rep.Paths), why)
	}
	if !dec.Run {
		return true
	}
	if dec.Trigger == model.TriggerIdle {
		// The monitor already counted this attempt, so a skipped run is
		// retried at the next --idle-interval, not on the next poll. Only the
		// scheduled idle run is gated: PFC/drop, periodic and mutual-alarm
		// runs answer a symptom and are rate-limited on their own.
		if reason := d.gate.check(ctx); reason != "" {
			if ctx.Err() != nil {
				return false
			}
			d.mon.SkipRun()
			d.tel.RecordIdleGateSkipped(ctx, reason)
			return true
		}
	}
	before := d.eng.SnapshotStreaks()
	d.live.RunStarted()
	rep, err := d.eng.Run(ctx, engine.RunRequest{
		Trigger:   dec.Trigger,
		Mode:      dec.Mode,
		BusyRNICs: dec.BusyRNICs,
		Util:      utilFrom(ctx, d.world, d.sampler, dec.Util),
		PCIe:      pcieFrom(d.world, d.g),
		Hints:     hintsFrom(d.world),
		HostIdle:  dec.HostIdle,
		DropDelta: dec.DropDelta,
		// Read on the poll that started the run, like the idle verdict.
		GPUComputeMode: dec.GPUComputeMode,
		CPULoad:        dec.CPULoad,
	})
	end := time.Now()
	d.live.RunEnded()
	// The engine ran synchronously; its probe traffic must not be read back
	// as host load on the next poll.
	d.mon.MarkRun(end)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown interrupted the run between probes: not a failure.
			return false
		}
		d.tel.RecordRunEnd(ctx, end)
		d.tel.RecordRunFailed(ctx, telemetry.FailedEngine)
		d.logf("engine: %v", err)
		return true
	}
	d.tel.RecordRunEnd(ctx, end)
	if needsIdleConfirm(dec.Mode) {
		// Judged against idle baselines on the strength of the poll before
		// the run: held until a clean poll after it confirms the host is
		// still idle.
		d.pending.hold(rep, before)
		return true
	}
	d.ps.otherRun(d.eng, before, rep)
	d.report(ctx, rep)
	return true
}

// report exports and writes a confirmed report. The run counts as a success
// only once the report reached every output.
func (d *daemon) report(ctx context.Context, rep model.Report) {
	d.tel.RecordReport(ctx, rep)
	if err := d.rp.emit(rep); err != nil {
		d.tel.RecordRunFailed(ctx, telemetry.FailedReport)
		return
	}
	d.tel.RecordSuccess(ctx, runEnd(rep))
}

// mutualMinProbes is the fewest probes a pair needs in the window before its
// timeout ratio may start an engine run on its own: at 100 ms per RNIC and a
// 20 s window, every pair of a host with up to a few dozen RNICs clears it.
const mutualMinProbes = 10

// startMutualLoop starts the ToN'24 §IV-A-2 probe schedule in the background:
// every RNIC probes one random peer among its --mutual-scope pairs every
// --mutual-interval (opt-in: off by default), and each pair's
// timeout ratio is kept over --mutual-window for the engine and the metrics.
// It returns nil (and the engine keeps probing pairs in a burst per run) when
// the schedule is off, there is nothing to pair, or no endpoint opens. The
// returned channel is closed once the loop has stopped and closed its
// endpoints.
func startMutualLoop(ctx context.Context, cfg config.Config, g topology.Graph, world *sim.World, logf func(string, ...any)) (*mutual.Loop, <-chan struct{}) {
	if cfg.MutualInterval <= 0 {
		return nil, nil
	}
	pairs := g.MutualPairsFor(cfg.MutualScope, cfg.MutualInfiniBand)
	if len(pairs) == 0 {
		// The engine says why, once, on its first run.
		return nil, nil
	}
	rnics := pairedRNICs(g, pairs)
	var tr mutual.Transport
	if world != nil {
		tr = &mutual.SimTransport{World: world, RNICs: rnics}
	} else {
		ud, errs, err := zigprobe.OpenUD(rnics, cfg.MutualMsgBytes)
		if err != nil {
			logf("hostping: mutual probe schedule unavailable, probing pairs per engine run instead: %v", err)
			return nil, nil
		}
		for i, e := range errs {
			if e != nil {
				logf("hostping: mutual probe endpoint %s unavailable (retried every 30s): %v", rnics[i].ID, e)
			}
		}
		tr = ud
	}
	l := mutual.New(mutual.Config{
		Interval:   cfg.MutualInterval,
		Timeout:    cfg.MutualTimeout,
		Window:     cfg.MutualWindow,
		Spin:       mutual.DefaultSpin,
		LossIsDrop: world != nil,
		TimeoutTh:  cfg.TimeoutTh,
		MinProbes:  mutualMinProbes,
	}, rnics, pairs, tr, mutual.WithLogf(logf))
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()
	logf("hostping: mutual probe every %v per RNIC (timeout %v, window %v, rnics=%d pairs=%d)",
		cfg.MutualInterval, cfg.MutualTimeout, cfg.MutualWindow, len(rnics), len(pairs))
	return l, done
}

// pairedRNICs is every RNIC that appears in a pair, in graph order.
func pairedRNICs(g topology.Graph, pairs [][2]model.RNIC) []model.RNIC {
	in := map[string]bool{}
	for _, p := range pairs {
		in[p[0].ID], in[p[1].ID] = true, true
	}
	var out []model.RNIC
	for _, n := range g.RNICs {
		if in[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// startTelemetry honours the one-shot exporter policy: a one-shot run that was
// not asked for metrics exports nothing, so it never binds the Prometheus port
// nor starts an OTLP periodic reader. A nil Telemetry records nothing.
func startTelemetry(cfg config.Config, opts ...telemetry.Option) (*telemetry.Telemetry, error) {
	cfg.PrometheusAddr, cfg.OTLPEndpoint = cfg.TelemetryExporters()
	if cfg.Once && cfg.PrometheusAddr == "" && cfg.OTLPEndpoint == "" {
		return nil, nil
	}
	return telemetry.Start(context.Background(), cfg, BuildInfo(), opts...)
}

func runOnce(ctx context.Context, rt *rig, tel *telemetry.Telemetry, cfg config.Config, rp *reporter) error {
	world := rt.world
	req := engine.RunRequest{
		Trigger:  model.TriggerManual,
		PCIe:     pcieFrom(world, rt.eng.Graph),
		Hints:    hintsFrom(world),
		HostIdle: true,
		Mode:     model.ModeIdle,
	}
	if world != nil {
		req.GPUComputeMode = world.ComputeModes()
		req.BusyRNICs = world.Busy
		if len(req.BusyRNICs) > 0 {
			req.Mode = model.ModeBusy
			req.HostIdle = false
		}
	} else {
		// liveRequest is a complete request (requestFromDecision fills every
		// field, Trigger included); only the --sim inputs are added here.
		live, err := liveRequest(ctx, rt.mon, cfg)
		if err != nil {
			return err
		}
		live.PCIe, live.Hints = req.PCIe, req.Hints
		req = live
	}
	// Sampled after the warmup polls and before the probes start, so the
	// utilization is the host's load, not hostping's own traffic.
	req.Util = utilFrom(ctx, world, rt.sampler, req.Util)
	rep, err := rt.eng.Run(ctx, req)
	if ctx.Err() == nil {
		tel.RecordRunEnd(ctx, time.Now())
	}
	if err != nil {
		if ctx.Err() == nil {
			tel.RecordRunFailed(ctx, telemetry.FailedEngine)
		}
		return err
	}
	if world == nil && needsIdleConfirm(req.Mode) {
		// Same race as the daemon's idle run: a workload that comes back
		// mid-run is judged against idle baselines. The operator asked for
		// this report, so it is still printed, but flagged and failed.
		ok, why, err := confirmIdleAfterRun(ctx, rt.mon, cfg, req.CPULoad)
		if err != nil {
			_ = rp.emit(rep)
			return fmt.Errorf("could not confirm the host stayed idle during the idle-mode run: %w", err)
		}
		if !ok {
			_ = rp.emit(rep)
			rt.logf("hostping: WARNING: the host became busy during the idle-mode run; its idle verdicts are unreliable: %s", why)
			return fmt.Errorf("host became busy during the idle-mode run (%s); rerun when the host is idle", why)
		}
	}
	tel.RecordReport(ctx, rep)
	if err := rp.emit(rep); err != nil {
		tel.RecordRunFailed(ctx, telemetry.FailedReport)
		return fmt.Errorf("write report: %w", err)
	}
	tel.RecordSuccess(ctx, runEnd(rep))
	return nil
}

// confirmIdleAfterRun polls one clean monitor interval after a one-shot
// idle-mode run: the first poll after MarkRun only re-baselines (it holds the
// run's own probe traffic), the next one, MetricInterval later, is the verdict.
// before is the CPU load the run started with: a CPU package loaded now that
// was not then fails the verdict too (cpuLoadRose). Load that returns and ends
// within the run is not caught.
func confirmIdleAfterRun(ctx context.Context, mon *monitor.Monitor, cfg config.Config, before []model.CPULoad) (bool, string, error) {
	mon.MarkRun(time.Now())
	if _, err := mon.Poll(ctx); err != nil {
		return false, "", fmt.Errorf("monitor re-baseline: %w", err)
	}
	dec, err := pollAfter(ctx, mon, cfg.MetricInterval)
	if err != nil {
		return false, "", err
	}
	if !dec.Ready {
		return false, "", fmt.Errorf("monitor re-baseline incomplete")
	}
	ok, why := idleVerdict(dec)
	if ok {
		if rose := cpuLoadRose(before, dec); rose != "" {
			return false, rose, nil
		}
	}
	return ok, why, nil
}

// pollAfter waits one metric interval (1 s when unset) and polls.
func pollAfter(ctx context.Context, mon *monitor.Monitor, wait time.Duration) (monitor.Decision, error) {
	if wait <= 0 {
		wait = time.Second
	}
	select {
	case <-ctx.Done():
		return monitor.Decision{}, ctx.Err()
	case <-time.After(wait):
	}
	return mon.Poll(ctx)
}

// liveRequest warms the hardware monitor and refuses if idle vs busy is unknown.
func liveRequest(ctx context.Context, mon *monitor.Monitor, cfg config.Config) (engine.RunRequest, error) {
	if mon == nil {
		return engine.RunRequest{}, fmt.Errorf("live diagnose requires a hardware monitor")
	}
	if _, err := mon.Poll(ctx); err != nil {
		return engine.RunRequest{}, fmt.Errorf("monitor warmup: %w", err)
	}
	dec, err := pollAfter(ctx, mon, cfg.MetricInterval)
	if err != nil {
		return engine.RunRequest{}, err
	}
	return requestFromDecision(dec)
}

func requestFromDecision(dec monitor.Decision) (engine.RunRequest, error) {
	if !dec.Ready {
		return engine.RunRequest{}, fmt.Errorf("monitor warmup incomplete; refusing to assume idle")
	}
	if !dec.IdleKnown {
		switch kind, rnics := unknownIdleCause(dec); kind {
		case countersUnread:
			return engine.RunRequest{}, fmt.Errorf("cannot classify idle vs busy (PFC pause or drop counters unread on %s); refusing to assume idle", rnics)
		case cpuUnread:
			return engine.RunRequest{}, fmt.Errorf("cannot classify idle vs busy (CPU load unread); refusing to assume idle")
		default:
			return engine.RunRequest{}, fmt.Errorf("cannot classify idle vs busy (GPU util or RNIC line rate unread); refusing to assume idle")
		}
	}
	mode := model.ModeIdle
	if !dec.HostIdle {
		mode = model.ModeBusy
	}
	if dec.Mode != "" {
		mode = dec.Mode
	}
	trig := dec.Trigger
	if trig == "" {
		trig = model.TriggerManual
	}
	return engine.RunRequest{
		Trigger:        trig,
		Mode:           mode,
		BusyRNICs:      dec.BusyRNICs,
		HostIdle:       dec.HostIdle,
		DropDelta:      dec.DropDelta,
		Util:           dec.Util,
		GPUComputeMode: dec.GPUComputeMode,
		CPULoad:        dec.CPULoad,
	}, nil
}

// skippedSuffix stops the reassuring "no intra-host bottleneck" line from
// claiming more than the run measured. The engine names the reason only when
// it changes, so this line carries the count on every run instead.
func skippedSuffix(rep model.Report) string {
	n := rep.SkippedEndpointCount("")
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" skipped_endpoints=%d", n)
}

// idleGPUs is the simulated monitor's GPU reading: every GPU idle, with the
// compute modes the --fault spec set.
func idleGPUs(g topology.Graph, modes map[string]string) []model.GPUStatus {
	var out []model.GPUStatus
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			out = append(out, model.GPUStatus{ID: e.ID, Idle: true, ComputeMode: modes[e.ID]})
		}
	}
	return out
}

// utilFrom assembles Input.Util, keyed by topology link ID, for the run about
// to start: the RNIC PCIe utilization the monitor derived from the counter
// delta that triggered it (rnic), overlaid with whatever the sampler reads
// now, before any probe is sent (GPU PCIe on a live host). A link neither
// source reads is left out, which the analyzer treats as unknown.
func utilFrom(ctx context.Context, w *sim.World, s monitor.Sampler, rnic map[string]float64) map[string]float64 {
	if w != nil {
		return w.Util()
	}
	out := map[string]float64{}
	for id, u := range rnic {
		out[id] = u
	}
	if s != nil {
		for id, u := range s.Bus(ctx) {
			out[id] = u
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func pcieFrom(w *sim.World, g topology.Graph) map[string]model.PCIeConfig {
	if w != nil {
		return w.PCIeConfigs()
	}
	out := map[string]model.PCIeConfig{}
	put := func(key, bdf string) {
		if key == "" || bdf == "" {
			return
		}
		if _, ok := out[key]; ok {
			return
		}
		c, err := pci.ReadLink("/sys", bdf)
		if err != nil {
			return
		}
		out[key] = c
	}
	for _, n := range g.RNICs {
		put(n.ID, n.PCIeBDF)
	}
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			put(e.ID, e.PCIeBDF)
		}
	}
	for _, l := range g.Links {
		if l.Kind != model.LinkKindRootPort && l.Kind != model.LinkKindPCIeSwitch {
			continue
		}
		put(l.ID, bdfFromTopoID(l.ID))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func bdfFromTopoID(id string) string {
	for _, p := range []string{"rp.", "sw."} {
		if len(id) > len(p) && id[:len(p)] == p {
			rest := id[len(p):]
			if pci.IsBDF(rest) {
				return rest
			}
		}
	}
	return ""
}

// hintsFrom is the simulated monitor's RNIC hints; a live run has none.
func hintsFrom(w *sim.World) map[string]string {
	if w == nil {
		return nil
	}
	return w.Hints()
}

func PrintTopology(cfg config.Config) error {
	g, err := LoadGraph(cfg)
	if err != nil {
		return err
	}
	// The documented pre-flight check for a new option set.
	if err := cfg.ValidateDropIgnore(g.RNICs); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(g)
}

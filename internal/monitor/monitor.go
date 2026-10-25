package monitor

import (
	"context"
	"math"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/logging"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// Sampler reads RNIC counters, GPU idle bits, and bus utilization.
type Sampler interface {
	RNIC(ctx context.Context, n model.RNIC) (model.RNICCounters, error)
	GPUs(ctx context.Context) ([]model.GPUStatus, error)
	// Bus samples every link utilization (0–1) it has a live source for,
	// keyed by topology link ID. A link it cannot read is absent: unknown,
	// never 0. It is called right before an engine run, never during one.
	Bus(ctx context.Context) map[string]float64
}

// BusPoller is an optional Sampler extension for bus sources that are rates
// over the poll interval, like the RNIC counters: PollBus is called once per
// Poll, before the RNIC counters are read, so the utilization Bus returns
// when a run starts covers the interval of the poll that triggered it. reset
// is set on the first poll after a run, whose interval holds the run's own
// probe traffic; that reading only becomes the new baseline.
type BusPoller interface {
	PollBus(ctx context.Context, reset bool)
}

// Decision is the hardware monitor output (NSDI'23 §4.2).
type Decision struct {
	Run       bool
	Trigger   model.Trigger
	Mode      model.RunMode
	BusyRNICs map[string]bool
	Rates     []model.RNICRates
	HostIdle  bool
	// DropDelta is each RNIC's drop-counter deltas on this poll, split into
	// fault and lossy-class drops, for every RNIC whose drop counter was
	// measured and that --drop-ignore-rnic does not name.
	DropDelta map[string]model.RNICDrops
	Ready     bool
	// IdleKnown is false when GPU util, an RNIC line rate, or a PFC pause or
	// drop counter that has a source could not be read. Live one-shot
	// diagnose must refuse rather than assume idle.
	IdleKnown bool
	// UnreadCounters lists, in graph order, the RNICs whose PFC pause or drop
	// counter has a source but was not measured on this poll: it could not
	// be read, or it is being re-baselined after reading again. Each clears
	// HostIdle and IdleKnown.
	UnreadCounters []string
	// Util is the RNIC PCIe hop utilization (pcie.<rnic>) over this poll's
	// counter delta, for every RNIC whose hop capacity was read. Because the
	// first poll after a run is re-baselined, a decision that starts a run
	// carries the load of the interval before it, never the previous run's
	// probe traffic.
	Util map[string]float64
	// BusyGPUs is every GPU whose utilization was read as busy, in sampler
	// order. A GPU whose state is unknown is not listed: it clears IdleKnown.
	BusyGPUs []string
	// GPUComputeMode is each GPU's compute mode by endpoint ID, for the GPUs
	// whose mode was read. The engine skips GPUs that are not Default.
	GPUComputeMode map[string]string
	// CPULoad is each NUMA node's CPU busy fraction over this poll's
	// interval, in NUMA order; nil when the sampler has no CPU signal or the
	// poll is not Ready. CPU load does not make the host busy: the run is
	// still idle or busy by its RNICs and GPUs, and only the paths through a
	// package with a High node are judged cpu_busy.
	CPULoad []model.CPULoad
	// CPUUnknown is set when the sampler has a CPU signal that could not be
	// read on this poll. It clears HostIdle and IdleKnown.
	CPUUnknown bool
}

type Monitor struct {
	Cfg     config.Config
	Graph   topology.Graph
	Sampler Sampler
	prev    map[string]model.RNICCounters
	// cad is the trigger cadence: when each trigger last ran.
	cad cadence
	// now is the poll clock; tests replace it to step through intervals.
	now func() time.Time
	// rebaseline drops the next counter delta because it also contains the
	// engine's own probe traffic.
	rebaseline bool
	// MutualAlarm, when set, reports that the scheduled mutual probe's window
	// shows a pair above Timeout_th (mutual.Loop.Alarm). The window only
	// moves on once per MutualWindow, so alarm runs are spaced by at least
	// that and by BusyCooldown.
	MutualAlarm func() bool
	// Logf receives the counter source reports (a PFC or drop counter with
	// no source, one that stops or resumes reading). nil logs through zerolog.
	Logf func(format string, args ...any)
	// pauseSrc and dropSrc follow each RNIC's counter sources across polls,
	// re-baselines included.
	pauseSrc map[string]*sourceTrack
	dropSrc  map[string]*sourceTrack
	// dropResidual is each RNIC's aggregate drop balance across polls
	// (settleResidual).
	dropResidual map[string]map[string]int64
	// cpuPrev is the previous CPU reading (pollCPU); cpuFailed is set while
	// the CPU signal cannot be read, so the failure is logged once.
	cpuPrev   map[int]CPUTicks
	cpuFailed bool
}

func New(cfg config.Config, g topology.Graph, s Sampler) *Monitor {
	return &Monitor{Cfg: cfg, Graph: g, Sampler: s, prev: map[string]model.RNICCounters{}, now: time.Now,
		pauseSrc: map[string]*sourceTrack{}, dropSrc: map[string]*sourceTrack{}, dropResidual: map[string]map[string]int64{}}
}

func (m *Monitor) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
		return
	}
	logging.Infof(format, args...)
}

// sourceState is what the monitor knows about one RNIC's PFC pause or drop
// counter.
type sourceState uint8

const (
	_ sourceState = iota // the zero value: not polled yet
	// sourceNotApplicable: no source has been found since the daemon
	// started (an InfiniBand link has no PFC; some RoCE NICs expose no pause
	// counter). The counter cannot trigger a run and does not block idle.
	sourceNotApplicable
	// sourceKnown: every source read before is readable now.
	sourceKnown
	// sourceUnknown: a source read before cannot be read now. Unknown is
	// not 0: the host is not idle until it reads again.
	sourceUnknown
)

// sourceTrack is one RNIC counter family's sources across polls.
type sourceTrack struct {
	seen  uint8 // every source bit ever read
	state sourceState
}

// counterFamily names a counter family in the log.
type counterFamily struct{ counter, trigger string }

var (
	pauseFamily = counterFamily{"PFC pause-duration", "PFC trigger"}
	dropFamily  = counterFamily{"packet-drop", "drop trigger"}
)

// trackSource classifies this poll's sources for one counter family and logs
// each change of state once, as skipped endpoints are logged.
func (m *Monitor) trackSource(tracks map[string]*sourceTrack, id string, fam counterFamily, src uint8, gap string) sourceState {
	t := tracks[id]
	if t == nil {
		t = &sourceTrack{}
		tracks[id] = t
	}
	var st sourceState
	switch {
	case src == 0 && t.seen == 0:
		st = sourceNotApplicable
	case t.seen&^src != 0:
		st = sourceUnknown
	default:
		st = sourceKnown
	}
	t.seen |= src
	if st == t.state {
		return st
	}
	prev := t.state
	t.state = st
	switch st {
	case sourceNotApplicable:
		m.logf("hostping: %s has no %s source (%s); the %s cannot fire for it", id, fam.counter, gap, fam.trigger)
	case sourceUnknown:
		if gap == "" {
			gap = "a source read before is missing"
		}
		m.logf("hostping: %s %s counter could not be read (%s); the host is not idle until it reads again", id, fam.counter, gap)
	case sourceKnown:
		switch prev {
		case sourceNotApplicable:
			m.logf("hostping: %s %s source found; the %s can fire for it", id, fam.counter, fam.trigger)
		case sourceUnknown:
			m.logf("hostping: %s %s counter reads again; re-baselined", id, fam.counter)
		}
	}
	return st
}

// MarkRun records that the engine ran at t. The run's loopback and mutual
// probes show up in the next counter delta, so that sample is re-baselined
// (treated like warmup) instead of being read as host load, and the busy
// cooldown and the --busy-interval are measured from the end of the run.
func (m *Monitor) MarkRun(t time.Time) {
	m.rebaseline = true
	m.cad.markRun(t)
}

// SkipRun records that the run the last Poll started did not happen (the
// daemon's --idle-gate-command denied it). Nothing measured the host, so the
// --busy-interval keeps counting from the run before. The attempt itself
// still counts: the idle cadence, like the busy cooldown, is not rewound.
func (m *Monitor) SkipRun() { m.cad.skipRun() }

// ResumeIdle sets the idle cadence as if the last idle-trigger run had
// started at t: the next idle run is due --idle-interval after it, and on the
// first idle poll when that is already past. The daemon calls it once, before
// the first poll, with the last idle run a previous process recorded
// (--state-dir), so a restart does not pull the next full mesh forward.
func (m *Monitor) ResumeIdle(t time.Time) { m.cad.lastIdle = t }

func (m *Monitor) Poll(ctx context.Context) (Decision, error) {
	// A canceled poll (shutdown) reads nothing: no counter source is
	// classified, so nothing is logged as having become unreadable.
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	reset := m.rebaseline
	if m.rebaseline {
		// Forget the pre-run counters: this sample becomes the new baseline and
		// yields no rates and no trigger, exactly like the first poll.
		m.rebaseline = false
		m.prev = map[string]model.RNICCounters{}
		m.cpuPrev = nil
	}
	if bp, ok := m.Sampler.(BusPoller); ok {
		bp.PollBus(ctx, reset)
	}
	now := m.now()
	rn, err := m.pollRNICs(ctx, now)
	if err != nil {
		return Decision{}, err
	}

	gpus, err := m.Sampler.GPUs(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return Decision{}, err
	}
	gpuIdle, gpuUnknown, busyGPUs, modes := gpuVerdict(gpus)

	cpu, cpuKnown := m.pollCPU(ctx)

	// Every RNIC of a ready poll went through rnicRates, so the flag covers
	// every line rate; a poll that is not ready knows nothing anyway.
	lineRateKnown := !rn.host.lineRateUnknown
	// Unknown CPU load, like an unknown GPU, is not idle: the paths through
	// a loaded package would be judged against idle baselines.
	cpuUnknown := rn.ready && !cpuKnown
	hostIdle := rn.ready && rn.host.idle() && gpuIdle && !gpuUnknown && !cpuUnknown
	idleKnown := rn.ready && !gpuUnknown && lineRateKnown && len(rn.unread) == 0 && !cpuUnknown
	d := Decision{BusyRNICs: rn.busy, Rates: rn.rates, HostIdle: hostIdle, DropDelta: rn.drops, Ready: rn.ready, IdleKnown: idleKnown, UnreadCounters: rn.unread, BusyGPUs: busyGPUs, GPUComputeMode: modes, CPUUnknown: cpuUnknown}
	if !rn.ready {
		return d, nil
	}
	d.CPULoad = cpu
	if len(rn.util) > 0 {
		d.Util = rn.util
	}
	d.Run, d.Trigger, d.Mode = m.cad.decide(now, &m.Cfg, triggerInputs{
		pfc: rn.host.pfc, drop: rn.host.drop, hostIdle: hostIdle, lineRateKnown: lineRateKnown, alarm: m.MutualAlarm,
	})
	return d, nil
}

// rnicPoll is what one Poll read from the RNICs, in graph order.
type rnicPoll struct {
	rates  []model.RNICRates
	busy   map[string]bool
	drops  map[string]model.RNICDrops
	util   map[string]float64
	unread []string
	// host is what every RNIC's delta says about the host, merged.
	host rnicFlags
	// ready is false when an RNIC had no previous sample to take a delta
	// from (the first poll, or the one after a run): no rates, no trigger.
	ready bool
}

// pollRNICs samples every RNIC at now, tracks each one's counter sources,
// and takes the delta against the previous sample it keeps. An RNIC that
// cannot be read, or a context done meanwhile, is the poll's error.
func (m *Monitor) pollRNICs(ctx context.Context, now time.Time) (rnicPoll, error) {
	rn := rnicPoll{busy: map[string]bool{}, drops: map[string]model.RNICDrops{}, util: map[string]float64{}, ready: true}
	for _, n := range m.Graph.RNICs {
		c, err := m.Sampler.RNIC(ctx, n)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			return rnicPoll{}, err
		}
		c.Timestamp = now
		pauseState := m.trackSource(m.pauseSrc, n.ID, pauseFamily, c.PauseSources, c.PauseGap)
		dropState := m.trackSource(m.dropSrc, n.ID, dropFamily, c.DropSources, c.DropGap)
		p, ok := m.prev[n.ID]
		m.prev[n.ID] = c
		if !ok {
			rn.ready = false
			rn.rates = append(rn.rates, model.RNICRates{Device: n.ID})
			continue
		}
		r, f, deltas := rnicRates(n, p, c, m.sampleInterval(p, c), pauseState, dropState, m.Cfg.ThpLowFraction, m.Cfg.PFCHigh)
		if u, ok := rnicUtil(m.Graph, n.ID, r.ThroughputBPS); ok {
			rn.util[model.PCIeLinkID(n.ID)] = u
		}
		if r.DropKnown && !m.Cfg.DropIgnored(n) {
			// Only fault drops trigger; lossy-class and ignored drops
			// still made the RNIC busy in rnicRates, since they are load.
			res := m.dropResidual[n.ID]
			if res == nil {
				res = map[string]int64{}
				m.dropResidual[n.ID] = res
			}
			d := splitDrops(c.Drops, deltas, m.Cfg.Lossless, res)
			rn.drops[n.ID] = d
			f.drop = d.FaultTotal() > 0
		}
		if f.unread {
			rn.unread = append(rn.unread, n.ID)
		}
		if f.busy {
			rn.busy[n.ID] = true
		}
		rn.host.merge(f)
		rn.rates = append(rn.rates, r)
	}
	return rn, nil
}

// sampleInterval is the seconds between two counter samples of one RNIC, or
// the metric interval (at least 1 s) when the clock did not move.
func (m *Monitor) sampleInterval(prev, cur model.RNICCounters) float64 {
	dt := cur.Timestamp.Sub(prev.Timestamp).Seconds()
	if dt <= 0 {
		dt = m.Cfg.MetricInterval.Seconds()
		if dt <= 0 {
			dt = 1
		}
	}
	return dt
}

// rnicFlags is what one RNIC's counter delta says about the host; merged over
// every RNIC, it is what all of them say.
type rnicFlags struct {
	// busy: at or above Thplow, paused above PFCHigh, or dropping (§4.2).
	busy bool
	// pfc and drop are the paper's two busy triggers. rnicRates leaves drop
	// to pollRNICs, which knows which drops count against the RNIC.
	pfc, drop bool
	// unread: a PFC pause or drop counter that has a source was not measured
	// on this poll. Unread ≠ no pauses and no drops.
	unread bool
	// lineRateUnknown: the RNIC cannot be classified against Thplow.
	// Unknown ≠ idle.
	lineRateUnknown bool
}

func (f *rnicFlags) merge(o rnicFlags) {
	f.busy = f.busy || o.busy
	f.pfc = f.pfc || o.pfc
	f.drop = f.drop || o.drop
	f.unread = f.unread || o.unread
	f.lineRateUnknown = f.lineRateUnknown || o.lineRateUnknown
}

// idle reports whether nothing the RNICs show keeps the host from an idle
// run: no load, every counter read, every line rate known.
func (f rnicFlags) idle() bool { return !f.busy && !f.unread && !f.lineRateUnknown }

// rnicRates is one RNIC's rates over the dt seconds between prev and cur,
// with the pause and drop counter states trackSource gave this poll, the
// verdicts those rates carry, and each drop counter's delta when the drops
// were measured. It is pure: it reads no monitor state.
func rnicRates(n model.RNIC, prev, cur model.RNICCounters, dt float64, pause, drop sourceState, thpLowFraction, pfcHigh float64) (model.RNICRates, rnicFlags, map[string]uint64) {
	r := model.RNICRates{Device: n.ID}
	var f rnicFlags
	dRx := counterDelta(cur.RxBytes, prev.RxBytes)
	dTx := counterDelta(cur.TxBytes, prev.TxBytes)
	// Unidirectional line-rate comparison (NSDI Thplow).
	peak := dRx
	if dTx > peak {
		peak = dTx
	}
	r.ThroughputBPS = uint64((float64(peak) * 8) / dt)
	// A counter is differenced only against a sample read from the same
	// sources: one that could not be read, or that reads again after a
	// gap, yields no ratio or delta on this poll, never one against 0.
	if pause == sourceKnown && prev.PauseSources == cur.PauseSources {
		r.PauseKnown = true
		dPause := counterDelta(cur.TxPauseDurationUs, prev.TxPauseDurationUs)
		r.PauseDurationRatio = (float64(dPause) / 1e6) / dt
		if r.PauseDurationRatio < 0 || math.IsNaN(r.PauseDurationRatio) {
			r.PauseDurationRatio = 0
		}
	}
	var drops map[string]uint64
	if drop == sourceKnown && prev.DropSources == cur.DropSources && sameCounters(prev.Drops, cur.Drops) {
		r.DropKnown = true
		drops = dropDeltas(prev.Drops, cur.Drops)
		for _, d := range drops {
			r.DropDelta += d
		}
	}
	if (!r.PauseKnown && pause != sourceNotApplicable) || (!r.DropKnown && drop != sourceNotApplicable) {
		// Unread ≠ no pauses and no drops; do not start a full-mesh run.
		f.unread = true
	}
	if n.LineRate == 0 {
		// Unknown line rate ≠ idle; do not start a full-mesh run.
		r.Idle = false
		f.lineRateUnknown = true
	} else {
		thLow := uint64(float64(n.LineRate) * thpLowFraction)
		r.Idle = r.ThroughputBPS < thLow
		if !r.Idle {
			f.busy = true
		}
	}
	if r.PauseDurationRatio > pfcHigh {
		f.pfc = true
		// A paused RNIC is often below Thplow; it is still loaded (§4.2).
		f.busy = true
	}
	if r.DropDelta > 0 {
		f.busy = true
	}
	return r, f, drops
}

// gpuVerdict reads the sampled GPUs: idle is every GPU idle and read; unknown
// is set when any could not be read; busy lists the busy ones and modes maps
// the compute mode of each GPU that reported one, both in sampler order.
func gpuVerdict(gpus []model.GPUStatus) (idle, unknown bool, busy []string, modes map[string]string) {
	idle = true
	for _, g := range gpus {
		if g.ComputeMode != "" {
			if modes == nil {
				modes = map[string]string{}
			}
			modes[g.ID] = g.ComputeMode
		}
		if g.Unknown {
			unknown = true
			idle = false
			continue
		}
		if !g.Idle {
			idle = false
			busy = append(busy, g.ID)
		}
	}
	return idle, unknown, busy, modes
}

// cadence is when each trigger last started a run; it decides which trigger,
// if any, a ready poll starts.
type cadence struct {
	lastIdle time.Time
	lastBusy time.Time
	// lastPeriodic is the reference for Config.BusyInterval: the last engine
	// run of any trigger (idle, PFC/drops, periodic, or a MarkRun), or the
	// first ready poll when none has run yet.
	lastPeriodic time.Time
	// periodicBefore is lastPeriodic before the last poll that started a
	// run, for SkipRun.
	periodicBefore time.Time
	lastAlarm      time.Time
}

// triggerInputs is what a ready poll measured, as the triggers see it.
type triggerInputs struct {
	// pfc and drop: a PFC pause ratio above PFCHigh, a drop delta above 0.
	pfc, drop bool
	// hostIdle is the Decision's HostIdle.
	hostIdle bool
	// lineRateKnown: every RNIC can be classified against Thplow.
	lineRateKnown bool
	// alarm is Monitor.MutualAlarm, nil without a mutual probe.
	alarm func() bool
}

// markRun is MarkRun: the busy cooldown and the --busy-interval are measured
// from the end of the run.
func (c *cadence) markRun(t time.Time) {
	c.lastBusy = t
	c.lastPeriodic = t
}

// skipRun is SkipRun: the --busy-interval keeps counting from the run before.
func (c *cadence) skipRun() { c.lastPeriodic = c.periodicBefore }

// decide picks the trigger a ready poll at now starts, if any, and records it.
// The triggers are tried in priority order: PFC/drops, mutual alarm, idle,
// periodic.
func (c *cadence) decide(now time.Time, cfg *config.Config, in triggerInputs) (run bool, trig model.Trigger, mode model.RunMode) {
	if c.lastPeriodic.IsZero() {
		// The periodic interval starts at the first ready poll, so a daemon
		// that starts on a loaded host waits one --busy-interval like any
		// other: the flag bounds probe overhead, it does not ask for a run.
		c.lastPeriodic = now
	}
	busyCooled := c.lastBusy.IsZero() || now.Sub(c.lastBusy) >= cfg.BusyCooldown
	switch {
	case in.pfc || in.drop:
		// Rate-limit: the previous run's probe traffic can keep PFC/drop
		// counters moving, which would otherwise re-trigger on every poll.
		if busyCooled {
			trig = model.TriggerDrops
			if in.pfc {
				trig = model.TriggerPFC
			}
			run, mode = true, model.ModeBusy
			c.lastBusy = now
		}
	case c.alarmDue(now, cfg, in):
		// ToN'24 §IV-A-2: the mutual probe runs continuously and a pair that
		// times out above Timeout_th is diagnosed now, not at the next idle
		// or PFC run. The mode follows the host as for any other trigger.
		run, trig, mode = true, model.TriggerMutualTimeout, model.ModeIdle
		if !in.hostIdle {
			mode = model.ModeBusy
		}
		c.lastAlarm = now
		c.lastBusy = now
	case in.hostIdle && (c.lastIdle.IsZero() || now.Sub(c.lastIdle) >= cfg.IdleInterval):
		run, trig, mode = true, model.TriggerIdle, model.ModeIdle
		c.lastIdle = now
	case c.periodicDue(now, cfg, in) && busyCooled:
		// Extension beyond NSDI'23 §4.2: a loaded host whose bottleneck does
		// not overflow the RNIC receive buffer never raises PFC or drops, and
		// is never idle, so neither paper trigger would measure it. Busy mode
		// keeps the probe set to the §4.3 reduced one on the RNICs classified
		// above (ThroughputBPS ≥ Thp_low), the same set a manual diagnose uses.
		run, trig, mode = true, model.TriggerPeriodic, model.ModeBusy
		c.lastBusy = now
	}
	if run {
		// Any run just measured the host, so the periodic interval restarts:
		// --busy-interval only fills the gaps the other triggers leave.
		c.periodicBefore = c.lastPeriodic
		c.lastPeriodic = now
	}
	return run, trig, mode
}

// alarmDue reports whether the mutual probe asks for a run now. Each alarm run
// is at least one MutualWindow after the last, so one bad window is diagnosed
// once rather than on every poll, and never inside BusyCooldown. A busy-mode
// alarm run needs every line rate, for the same reason as periodicDue.
func (c *cadence) alarmDue(now time.Time, cfg *config.Config, in triggerInputs) bool {
	if in.alarm == nil || (!in.hostIdle && !in.lineRateKnown) {
		return false
	}
	gap := cfg.MutualWindow
	if cfg.BusyCooldown > gap {
		gap = cfg.BusyCooldown
	}
	if !c.lastAlarm.IsZero() && now.Sub(c.lastAlarm) < gap {
		return false
	}
	if !c.lastBusy.IsZero() && now.Sub(c.lastBusy) < cfg.BusyCooldown {
		return false
	}
	return in.alarm()
}

// periodicDue reports whether --busy-interval asks for a busy run now. It is
// off by default, never fires on an idle host (the idle run covers it), and
// needs every line rate: without one the busy RNIC set cannot be classified,
// and an unclassified loaded RNIC would be judged against the idle baseline.
func (c *cadence) periodicDue(now time.Time, cfg *config.Config, in triggerInputs) bool {
	if cfg.BusyInterval <= 0 || in.hostIdle || !in.lineRateKnown {
		return false
	}
	return now.Sub(c.lastPeriodic) >= cfg.BusyInterval
}

// counterDelta is wrap-safe for 64-bit and 32-bit sysfs counters.
func counterDelta(cur, prev uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	if prev <= 0xffffffff && cur <= 0xffffffff {
		return (0xffffffff - prev) + cur + 1
	}
	return (0xffffffffffffffff - prev) + cur + 1
}

// StaticSampler is used in --sim and tests.
type StaticSampler struct {
	Counters map[string]model.RNICCounters
	GPU      []model.GPUStatus
	Util     map[string]float64
}

// RNIC returns the configured counters. A static counter is its own source,
// so a family with no source bits is read from one; set PauseGap or DropGap
// (with no source bits) to model a counter that has none.
func (s StaticSampler) RNIC(_ context.Context, n model.RNIC) (model.RNICCounters, error) {
	c, ok := s.Counters[n.ID]
	if !ok {
		c = model.RNICCounters{Device: n.Device, Netdev: n.Netdev}
	}
	if c.PauseSources == 0 && c.PauseGap == "" {
		c.PauseSources = 1
	}
	if c.DropSources == 0 && c.DropGap == "" {
		c.DropSources = 1
	}
	return c, nil
}

func (s StaticSampler) GPUs(context.Context) ([]model.GPUStatus, error) { return s.GPU, nil }

// Bus returns a copy of the configured utilization; a link it does not name
// is unknown.
func (s StaticSampler) Bus(context.Context) map[string]float64 {
	if len(s.Util) == 0 {
		return nil
	}
	out := make(map[string]float64, len(s.Util))
	for k, v := range s.Util {
		out[k] = v
	}
	return out
}

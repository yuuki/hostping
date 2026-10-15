package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// Prober measures one RNIC→endpoint loopback and RNIC↔RNIC mutual connectivity.
type Prober interface {
	Loopback(ctx context.Context, rnic model.RNIC, ep model.Endpoint, small, large uint32) (model.PathMeasurement, error)
	Mutual(ctx context.Context, src, dst model.RNIC) (model.MutualMeasurement, error)
}

// SimProber uses the paper Eq.1 world.
type SimProber struct{ World *sim.World }

func (p SimProber) Loopback(_ context.Context, rnic model.RNIC, ep model.Endpoint, small, large uint32) (model.PathMeasurement, error) {
	return p.World.Loopback(rnic.ID, ep.ID, small, large)
}

func (p SimProber) Mutual(_ context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	return p.World.Mutual(src.ID, dst.ID), nil
}

// MutualWindow is the scheduled mutual probe (internal/mutual): each pair's
// probes over a sliding window. ready is false until one window has filled.
type MutualWindow interface {
	Snapshot() (ms []model.MutualMeasurement, ready bool)
}

type Engine struct {
	Cfg    config.Config
	Graph  topology.Graph
	Prober Prober
	// Mutual, when set, supplies the mutual measurements from the daemon's
	// probe schedule (ToN'24 §IV-A-2) instead of a burst of probes per run.
	// Until its window has filled, the run falls back to the burst.
	Mutual MutualWindow
	Gray   map[string]int
	// SlowMR counts, per GPU endpoint, the consecutive idle runs whose
	// registration time crossed the gpu_mr_registration_slow threshold.
	SlowMR map[string]int
	// Latency counts, per path keyed "rnic->endpoint", the consecutive idle
	// runs that read the path latency-abnormal against its siblings.
	Latency map[string]int
	// OneShot marks an engine that runs once and keeps nothing (hostping
	// --once, hostpingctl diagnose): with no earlier run to persist across,
	// a sibling latency verdict is reported when its paired recheck
	// confirmed it (analyzer.Input.OneShot).
	OneShot bool
	// Logf receives the aggregated engine reports (skipped endpoints, an empty
	// mutual pair set). A nil Logf logs through the standard logger, which is
	// where the agent writes everything else.
	Logf func(format string, args ...any)
	// SkipLatencyRecheck turns off the paired latency re-measurement
	// (recheckLatency). Calibrate sets it: it reads only the sweep's
	// readings, never the latency verdict, so the recheck would only
	// lengthen each calibration run.
	SkipLatencyRecheck bool
	// SkipBWRecheck turns off the paired re-read of idle bandwidth verdicts
	// (recheckIdleBandwidth). Calibrate sets it too: it reads only the
	// sweep's bandwidths, never a verdict.
	SkipBWRecheck bool

	// skipKey is the last reported set of (endpoint kind, reason) pairs.
	skipKey string

	// mutualKey is the last reported fabric set behind an empty mutual pair
	// set; see reportMutualPairs.
	mutualKey string

	// ibExcludedLogged is set once the InfiniBand RNICs left out of the
	// mutual probe have been named; see reportInfiniBandExcluded.
	ibExcludedLogged bool

	// latencyRechecks counts the runs that planned a latency recheck. It is
	// the rotating offset of analyzer.PlanLatencyRecheck, so the per-run cap
	// moves over the candidates from run to run; a one-shot engine passes 0.
	latencyRechecks int

	// addrKey is, per RNIC ID, the last logged loopback port addressing;
	// see reportAddressing.
	addrKey map[string]model.PortAddressing
}

func New(cfg config.Config, g topology.Graph, p Prober) *Engine {
	return &Engine{Cfg: cfg, Graph: g, Prober: p, Gray: map[string]int{}, SlowMR: map[string]int{}, Latency: map[string]int{}}
}

// GrayState is a copy of the engine's per-link consecutive-gray counters
// (NSDI'23 §4.3 flapping).
type GrayState map[string]int

// Streaks is a copy of the evidence the engine carries from run to run, taken
// by SnapshotStreaks: the per-link consecutive-gray counters, the per-GPU
// count of consecutive idle runs with slow memory registration, and the
// per-path count of consecutive idle runs latency-abnormal against siblings.
type Streaks struct {
	Gray    GrayState
	SlowMR  map[string]int
	Latency map[string]int
}

// SnapshotStreaks returns a deep copy of the engine's streaks, so a run whose
// report is later thrown away can be undone with RestoreStreaks.
func (e *Engine) SnapshotStreaks() Streaks {
	return Streaks{Gray: GrayState(cloneCounts(e.Gray)), SlowMR: cloneCounts(e.SlowMR), Latency: cloneCounts(e.Latency)}
}

// RestoreStreaks puts back streaks taken by SnapshotStreaks, so a discarded
// run leaves no streak behind: a series of runs polluted by returning load
// must not add up to a flapping, slow-registration or path-latency diagnosis.
// The state is copied, so s stays usable. A latency streak of a path the
// graph no longer has is dropped.
func (e *Engine) RestoreStreaks(s Streaks) {
	e.Gray = cloneCounts(s.Gray)
	e.SlowMR = cloneCounts(s.SlowMR)
	e.Latency = cloneCounts(s.Latency)
	for k := range e.Latency {
		rnic, ep, ok := strings.Cut(k, "->")
		if _, found := e.Graph.Path(rnic, ep); !ok || !found {
			delete(e.Latency, k)
		}
	}
}

// cloneCounts copies a counter map; nil becomes an empty map.
func cloneCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type RunRequest struct {
	Trigger   model.Trigger
	Mode      model.RunMode
	BusyRNICs map[string]bool
	Util      map[string]float64
	PCIe      map[string]model.PCIeConfig
	Hints     map[string]string
	HostIdle  bool
	DropDelta map[string]uint64
	// GPUComputeMode is each GPU's compute mode by endpoint ID. A GPU whose
	// mode blocks probing (model.ComputeModeBlocksProbe) is skipped with
	// ErrComputeMode; a GPU absent here is unknown and probed as before.
	GPUComputeMode map[string]string
	// CPULoad is each NUMA node's CPU load on the poll that started the run
	// (monitor.Decision.CPULoad). An abnormal path through a package with a
	// High node is judged cpu_busy. It is copied into the report.
	CPULoad []model.CPULoad
}

// GPUReleaser is an optional Prober extension: ReleaseGPU hands back whatever
// GPU context the run's probes created, so an idle daemon holds none between
// runs. Run calls it once at the end of every run, however the run ends.
type GPUReleaser interface {
	ReleaseGPU()
}

func (e *Engine) Run(ctx context.Context, req RunRequest) (model.Report, error) {
	if req.Mode == "" {
		req.Mode = model.ModeIdle
	}
	started := time.Now()
	host, _ := os.Hostname()
	ov, err := e.Cfg.Overrides()
	if err != nil {
		return model.Report{}, fmt.Errorf("baseline: %w", err)
	}
	if r, ok := e.Prober.(GPUReleaser); ok {
		defer r.ReleaseGPU()
	}
	var ms []model.PathMeasurement
	var skips []probeSkip
	samples := busySampleCount(e.Cfg)
	for _, n := range e.Graph.RNICs {
		paths := e.Graph.ProbePaths(n.ID, req.Mode)
		repeated := e.busyRepeated(req, n.ID, paths)
		// portFail is the failure that showed this RNIC's port cannot loop
		// back (portFailFast); answered is whether any of its probes returned
		// a reading in this run.
		var portFail error
		answered := false
		for _, p := range paths {
			ep, ok := e.Graph.Endpoint(p.Endpoint)
			if !ok {
				continue
			}
			if err := computeModeSkip(ep, req.GPUComputeMode); err != nil {
				skips = append(skips, probeSkip{rnic: n.ID, ep: ep.ID, kind: ep.Kind, err: err})
				continue
			}
			if portFail != nil {
				skips = append(skips, probeSkip{rnic: n.ID, ep: ep.ID, kind: ep.Kind, err: notProbedError(portFail)})
				continue
			}
			want := 1
			if repeated[p.Endpoint] {
				want = samples
			}
			var got []model.PathMeasurement
			var skipErr error
			for i := 0; i < want; i++ {
				// A prober blocks until its own timeout, so the run can only be
				// interrupted between probes; abort before starting another one.
				if err := ctx.Err(); err != nil {
					return model.Report{}, err
				}
				m, err := e.Prober.Loopback(ctx, n, ep, e.Cfg.SmallMsgBytes, e.Cfg.LargeMsgBytes)
				if err != nil {
					if !skippableProbe(err) {
						return model.Report{}, fmt.Errorf("loopback %s->%s: %w", n.ID, ep.ID, err)
					}
					// The reasons a probe is skippable (no GDR, no device, no
					// GID, a failed setup) do not come and go between two
					// readings of the same path, so spend no further probes on
					// it. A path that already answered keeps the readings it
					// has: it is measured, not skipped, and counting it as both
					// would double it in the report.
					skipErr = err
					break
				}
				m.EndpointKind = ep.Kind
				e.reportAddressing(n, m.Addressing)
				got = append(got, m)
			}
			if len(got) == 0 {
				skips = append(skips, probeSkip{rnic: n.ID, ep: ep.ID, kind: ep.Kind, err: skipErr})
				if !answered && portFailFast(n, skipErr) {
					portFail = skipErr
				}
				continue
			}
			answered = true
			ms = append(ms, medianMeasurement(got))
		}
	}
	if len(ms) == 0 {
		return model.Report{}, noLoopbackError(skips)
	}
	// Some endpoints answered and some did not: the run continues on a partial
	// mesh, so say which endpoints are missing and why before analysing it.
	groups := groupSkips(skips)
	e.reportSkips(groups)

	recheck, capped, err := e.recheckLatency(ctx, req.Mode, ms, ov)
	if err != nil {
		return model.Report{}, err
	}
	busyRecheck, err := e.recheckBusy(ctx, req, ms, ov)
	if err != nil {
		return model.Report{}, err
	}
	bwRecheck, err := e.recheckIdleBandwidth(ctx, req, ms, ov)
	if err != nil {
		return model.Report{}, err
	}

	e.reportInfiniBandExcluded()
	pairs := e.Graph.MutualPairsFor(e.Cfg.MutualScope, e.Cfg.MutualInfiniBand)
	e.reportMutualPairs(pairs)
	mutual, err := e.mutual(ctx, pairs)
	if err != nil {
		return model.Report{}, err
	}
	for i := range mutual {
		mutual[i].CounterDrops = req.DropDelta[mutual[i].Src] + req.DropDelta[mutual[i].Dst]
	}
	markCrossRail(e.Graph, mutual)

	in := analyzer.Input{
		Graph:        e.Graph,
		Cfg:          e.Cfg,
		Mode:         req.Mode,
		BusyRNICs:    req.BusyRNICs,
		Measurements: ms,
		Mutual:       mutual,
		Util:         req.Util,
		PCIe:         req.PCIe,
		RNICHints:    req.Hints,
		GrayHits:     e.Gray,
		SlowMRHits:   e.SlowMR,
		LatencyHits:  e.Latency,
		OneShot:      e.OneShot,
		DropDelta:    req.DropDelta,
		Bases:        ov,
		// The recheck changes only latency verdicts: ms keeps the sweep's
		// readings, so bandwidth status and its diagnoses are as before.
		LatencyRecheck: recheck,
		// The candidates past the recheck's cap: unconfirmed, so they
		// neither extend a latency streak nor become a cause this run.
		LatencyRecheckSkipped: capped,
		// Likewise the busy recheck changes only bandwidth verdicts, never
		// the readings in ms.
		BusyRecheck: busyRecheck,
		// And the idle bandwidth recheck, likewise.
		BWRecheck:      bwRecheck,
		CPUBusySockets: model.CPUBusySockets(req.CPULoad),
	}
	res := analyzer.Analyze(in)
	e.Gray = res.GrayHits
	e.SlowMR = res.SlowMRHits
	e.Latency = res.LatencyHits

	var abn []string
	seen := map[string]struct{}{}
	for _, p := range res.Paths {
		if p.Status == model.PathAbnormal {
			if _, ok := seen[p.RNIC]; !ok {
				abn = append(abn, p.RNIC)
				seen[p.RNIC] = struct{}{}
			}
		}
	}

	return model.Report{
		Trigger:          req.Trigger,
		Mode:             req.Mode,
		Host:             host,
		Started:          started,
		Elapsed:          time.Since(started),
		ElapsedNs:        time.Since(started).Nanoseconds(),
		Paths:            res.Paths,
		SkippedEndpoints: skippedEndpoints(groups),
		Links:            res.Links,
		Causes:           res.Causes,
		Mutual:           mutual,
		HostIdle:         req.HostIdle,
		AbnormalRNIC:     abn,
		Baselines:        baseline.Tables(e.Graph.RNICs, ov),
		CPULoad:          req.CPULoad,
	}, nil
}

// recheckLatency re-measures the paths the sweep reads latency-abnormal
// against their siblings (analyzer.PlanLatencyRecheck): each path is read in
// turn with siblings of its class — path, sibling, path, sibling, ... — so both
// sides are read in the same time window, and analyzer.JudgeLatencyRecheck
// decides from the re-reads whether the verdict holds. The result is keyed by
// "rnic->endpoint" for analyzer.Input.LatencyRecheck. Only the re-reads'
// small-message p50 is used; the sweep's measurements are left as they are.
//
// Busy runs judge no latency and recheck nothing, and neither does an engine
// with SkipLatencyRecheck. A probe that fails, or returns no small-message
// latency, abandons that path's recheck and the path keeps its sweep verdict:
// a recheck that could not be completed is no evidence the verdict was wrong.
// Like the sweep, the context is checked before every probe.
//
// The second result names the candidates left out by the per-run cap, for
// analyzer.Input.LatencyRecheckSkipped. Each run that plans a recheck passes
// the engine's count of such runs as the plan's offset, so the cap rotates
// over the candidates and no RNIC is starved run after run.
func (e *Engine) recheckLatency(ctx context.Context, mode model.RunMode, ms []model.PathMeasurement, ov baseline.Overlay) (map[string]model.LatencyRecheck, map[string]model.LatencyRecheckSkip, error) {
	if mode == model.ModeBusy || e.SkipLatencyRecheck {
		return nil, nil, nil
	}
	sel := analyzer.PlanLatencyRecheck(e.Graph, ms, mode, e.Cfg, ov, e.latencyRechecks)
	plans := sel.Plans
	if len(plans) == 0 {
		return nil, nil, nil
	}
	e.latencyRechecks++
	var capped map[string]model.LatencyRecheckSkip
	if len(sel.Capped) > 0 {
		capped = map[string]model.LatencyRecheckSkip{}
		names := make([]string, 0, len(sel.Capped))
		for _, p := range sel.Capped {
			capped[p.String()] = model.LatencyRecheckSkipCap
			names = append(names, p.String())
		}
		e.logf("hostping: latency recheck capped at %d paths; %d left for a later run, their streaks held: %s", analyzer.LatencyRecheckMaxPaths, len(names), strings.Join(names, " "))
	}
	read := func(p analyzer.PathRef) (uint64, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, ok := e.Graph.RNIC(p.RNIC)
		if !ok {
			return 0, fmt.Errorf("no RNIC %s", p.RNIC)
		}
		ep, ok := e.Graph.Endpoint(p.Endpoint)
		if !ok {
			return 0, fmt.Errorf("no endpoint %s", p.Endpoint)
		}
		m, err := e.Prober.Loopback(ctx, n, ep, e.Cfg.SmallMsgBytes, e.Cfg.LargeMsgBytes)
		if err != nil {
			return 0, err
		}
		if m.SmallLatNsP50 == 0 {
			return 0, fmt.Errorf("no small-message latency")
		}
		return m.SmallLatNsP50, nil
	}
	out := map[string]model.LatencyRecheck{}
	for _, plan := range plans {
		var path, sibs []uint64
		var names []string
		var failed error
		for _, sib := range plan.Siblings {
			for _, r := range []analyzer.PathRef{plan.Path, sib} {
				v, err := read(r)
				if err != nil {
					if cerr := ctx.Err(); cerr != nil {
						return nil, nil, cerr
					}
					failed = fmt.Errorf("%s: %w", r, err)
					break
				}
				if r == plan.Path {
					path = append(path, v)
				} else {
					sibs = append(sibs, v)
				}
			}
			if failed != nil {
				break
			}
			names = append(names, sib.String())
		}
		if failed != nil {
			e.logf("hostping: latency recheck of %s abandoned, keeping the sweep verdict: %v", plan.Path, failed)
			continue
		}
		rc := analyzer.JudgeLatencyRecheck(path, sibs, e.Cfg)
		rc.Siblings = names
		out[plan.Path.String()] = rc
	}
	return out, capped, nil
}

// recheckBusy re-reads the paths a busy sweep reads abnormal against their
// siblings on a loaded RNIC (analyzer.PlanBusyRecheck): for
// analyzer.BusyRecheckRounds rounds, each round reads the path and then every
// planned sibling once, all on the same RNIC, so both sides share a time
// window. analyzer.JudgeBusyRecheck decides from the re-reads alone whether
// the verdict holds. The result is keyed by "rnic->endpoint" for
// analyzer.Input.BusyRecheck; the sweep's measurements are left as they are.
//
// Only busy runs recheck. A probe that fails, or returns no bandwidth,
// abandons that path's recheck and the path keeps its sweep verdict: a
// recheck that could not be completed is no evidence the verdict was wrong.
// Like the sweep, the context is checked before every probe.
func (e *Engine) recheckBusy(ctx context.Context, req RunRequest, ms []model.PathMeasurement, ov baseline.Overlay) (map[string]model.BusyRecheck, error) {
	if req.Mode != model.ModeBusy {
		return nil, nil
	}
	plans := analyzer.PlanBusyRecheck(e.Graph, ms, req.Mode, req.BusyRNICs, e.Cfg, ov)
	if len(plans) == 0 {
		return nil, nil
	}
	read := func(p analyzer.PathRef) (uint64, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, ok := e.Graph.RNIC(p.RNIC)
		if !ok {
			return 0, fmt.Errorf("no RNIC %s", p.RNIC)
		}
		ep, ok := e.Graph.Endpoint(p.Endpoint)
		if !ok {
			return 0, fmt.Errorf("no endpoint %s", p.Endpoint)
		}
		m, err := e.Prober.Loopback(ctx, n, ep, e.Cfg.SmallMsgBytes, e.Cfg.LargeMsgBytes)
		if err != nil {
			return 0, err
		}
		if m.BandwidthBPS == 0 {
			return 0, fmt.Errorf("no bandwidth")
		}
		return m.BandwidthBPS, nil
	}
	out := map[string]model.BusyRecheck{}
	for _, plan := range plans {
		order := append([]analyzer.PathRef{plan.Path}, plan.Siblings...)
		var path, sibs []uint64
		var failed error
	rounds:
		for range analyzer.BusyRecheckRounds {
			for _, r := range order {
				v, err := read(r)
				if err != nil {
					if cerr := ctx.Err(); cerr != nil {
						return nil, cerr
					}
					failed = fmt.Errorf("%s: %w", r, err)
					break rounds
				}
				if r == plan.Path {
					path = append(path, v)
				} else {
					sibs = append(sibs, v)
				}
			}
		}
		if failed != nil {
			e.logf("hostping: busy recheck of %s abandoned, keeping the sweep verdict: %v", plan.Path, failed)
			continue
		}
		rc := analyzer.JudgeBusyRecheck(path, sibs, e.Cfg.AbnormalTh)
		rc.Rounds = analyzer.BusyRecheckRounds
		for _, s := range plan.Siblings {
			rc.Siblings = append(rc.Siblings, s.String())
		}
		out[plan.Path.String()] = rc
	}
	return out, nil
}

// recheckIdleBandwidth re-reads the groups of paths a sweep reads abnormal
// against the idle baseline (analyzer.PlanBWRecheck): for
// analyzer.BWRecheckRounds rounds, each round reads the group's
// representative and then each of its references on other RNICs once, so
// both sides share a time window. analyzer.JudgeBWRecheck decides the group
// from the re-reads alone. The result is keyed by group for
// analyzer.Input.BWRecheck; the sweep's measurements are left as they are.
//
// Idle runs recheck, and so do busy runs for the RNICs they judge against the
// idle baseline; calibration turns it off with SkipBWRecheck. A probe that
// fails, or returns no bandwidth, abandons that group's recheck and the group
// keeps its sweep verdict. Like the sweep, the context is checked before
// every probe.
func (e *Engine) recheckIdleBandwidth(ctx context.Context, req RunRequest, ms []model.PathMeasurement, ov baseline.Overlay) (map[string]model.BandwidthRecheck, error) {
	if e.SkipBWRecheck {
		return nil, nil
	}
	plans := analyzer.PlanBWRecheck(e.Graph, ms, req.Mode, req.BusyRNICs, e.Cfg, ov)
	if len(plans) == 0 {
		return nil, nil
	}
	out := map[string]model.BandwidthRecheck{}
	for _, plan := range plans {
		order := append([]analyzer.PathRef{plan.Path}, plan.Refs...)
		var path, refs []uint64
		var failed error
	rounds:
		for range analyzer.BWRecheckRounds {
			for _, r := range order {
				v, err := e.readBandwidth(ctx, r)
				if err != nil {
					if cerr := ctx.Err(); cerr != nil {
						return nil, cerr
					}
					failed = fmt.Errorf("%s: %w", r, err)
					break rounds
				}
				if r == plan.Path {
					path = append(path, v)
				} else {
					refs = append(refs, v)
				}
			}
		}
		if failed != nil {
			e.logf("hostping: bandwidth recheck of %s abandoned, keeping the sweep verdict: %v", plan.Group, failed)
			continue
		}
		rc := analyzer.JudgeBWRecheck(path, refs, plan.BaselineBPS, e.Cfg.AbnormalTh)
		rc.Path = plan.Path.String()
		rc.Rounds = analyzer.BWRecheckRounds
		for _, r := range plan.Refs {
			rc.Refs = append(rc.Refs, r.String())
		}
		out[plan.Group] = rc
	}
	return out, nil
}

// readBandwidth probes one path once for a recheck and returns its
// bandwidth; a reading without one is an error.
func (e *Engine) readBandwidth(ctx context.Context, p analyzer.PathRef) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, ok := e.Graph.RNIC(p.RNIC)
	if !ok {
		return 0, fmt.Errorf("no RNIC %s", p.RNIC)
	}
	ep, ok := e.Graph.Endpoint(p.Endpoint)
	if !ok {
		return 0, fmt.Errorf("no endpoint %s", p.Endpoint)
	}
	m, err := e.Prober.Loopback(ctx, n, ep, e.Cfg.SmallMsgBytes, e.Cfg.LargeMsgBytes)
	if err != nil {
		return 0, err
	}
	if m.BandwidthBPS == 0 {
		return 0, fmt.Errorf("no bandwidth")
	}
	return m.BandwidthBPS, nil
}

// mutual returns the run's mutual measurements: the scheduled probe's window
// when it is running and full, else one burst per pair from the prober.
func (e *Engine) mutual(ctx context.Context, pairs [][2]model.RNIC) ([]model.MutualMeasurement, error) {
	if e.Mutual != nil {
		if ms, ready := e.Mutual.Snapshot(); ready {
			return ms, nil
		}
	}
	var out []model.MutualMeasurement
	for _, pair := range pairs {
		a, b := pair[0], pair[1]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m, err := e.Prober.Mutual(ctx, a, b)
		if err != nil {
			if skippableProbe(err) {
				continue
			}
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// busyRepeated is the set of this RNIC's endpoints whose path is measured
// Cfg.BusySamples times instead of once, keyed by endpoint ID.
//
// A loaded RNIC cannot be judged against the idle baseline, so §4.3 compares
// its endpoints against each other. One reading per endpoint leaves that
// comparison as noisy as a single probe: the spread between two readings of the
// same healthy path clears --abnormal-th often enough to report a different
// sibling abnormal on every run. Classifying the median of several readings
// removes that, at the price of probe time on an RNIC that is already carrying
// service traffic — which is the overhead §4.3 wants kept small. So the extra
// probes are spent only where they can change a verdict:
//
//   - Busy mode only, and only on the RNICs the monitor reported loaded. Every
//     other RNIC is judged against the idle baseline, which does not compare
//     paths with each other, so an idle run probes each path exactly once.
//   - Only where the RNIC has a second endpoint at the same communication
//     distance. A distance holding one endpoint has no other reading to be
//     judged against and falls through to uncertain whatever it reads
//     (analyzer.busyRef), so repeating it buys nothing.
//
// This mirrors the analyzer's comparison rather than sharing it. The two can
// only disagree about a path that is measured once but compared after all,
// which is the single-sample behaviour this replaces, not a new failure.
func (e *Engine) busyRepeated(req RunRequest, rnic string, paths []model.Path) map[string]bool {
	if req.Mode != model.ModeBusy || !req.BusyRNICs[rnic] || busySampleCount(e.Cfg) < 2 {
		return nil
	}
	// In busy mode ProbePaths has already reduced paths to the busy probe set,
	// which is the set the sibling comparison draws on.
	siblings := map[model.Distance]int{}
	for _, p := range paths {
		siblings[p.Distance]++
	}
	out := map[string]bool{}
	for _, p := range paths {
		if siblings[p.Distance] > 1 {
			out[p.Endpoint] = true
		}
	}
	return out
}

// busySampleCount is Cfg.BusySamples as a usable probe count: a zero value
// (a Config built as a literal rather than from Defaults) and an explicit
// --busy-samples 0 both mean the single reading of §4.3 as written.
func busySampleCount(cfg config.Config) int {
	if cfg.BusySamples < 1 {
		return 1
	}
	return int(cfg.BusySamples)
}

// medianMeasurement reduces repeated readings of one path to the row the
// analyzer classifies.
//
// Each field takes its own median rather than one whole reading being picked by
// its bandwidth, because the analyzer reads them independently: bandwidth
// against the §4.3 sibling reference or the distance baseline, SmallLatNsP50
// against the same_switch latency baseline for ACS/ATS. Carrying one reading's
// outlier latency next to another's median bandwidth would leave half the noise
// in place. The result is therefore a summary of the samples rather than a
// single observation — every field is a level the path held in at least half of
// them. The status fields are left as the prober returned them, exactly as they
// are for a single reading: classifying the row is the analyzer's.
func medianMeasurement(ms []model.PathMeasurement) model.PathMeasurement {
	out := ms[0]
	if len(ms) == 1 {
		return out
	}
	out.BandwidthBPS = medianOf(ms, func(m model.PathMeasurement) uint64 { return m.BandwidthBPS })
	out.SmallLatNsP50 = medianOf(ms, func(m model.PathMeasurement) uint64 { return m.SmallLatNsP50 })
	out.SmallLatNsP99 = medianOf(ms, func(m model.PathMeasurement) uint64 { return m.SmallLatNsP99 })
	out.LargeLatNsP50 = medianOf(ms, func(m model.PathMeasurement) uint64 { return m.LargeLatNsP50 })
	out.Verbs = model.VerbNs{
		GPUAlloc:  medianOf(ms, func(m model.PathMeasurement) uint64 { return m.Verbs.GPUAlloc }),
		MRReg:     medianOf(ms, func(m model.PathMeasurement) uint64 { return m.Verbs.MRReg }),
		QPSetup:   medianOf(ms, func(m model.PathMeasurement) uint64 { return m.Verbs.QPSetup }),
		MemWarmup: medianOf(ms, func(m model.PathMeasurement) uint64 { return m.Verbs.MemWarmup }),
	}
	return out
}

// medianOf is the median of one field over the samples, averaging the two
// middle readings when there is an even number of them. Probe values are
// bandwidths and nanosecond latencies, far below the range where the sum of two
// uint64 could overflow.
func medianOf(ms []model.PathMeasurement, field func(model.PathMeasurement) uint64) uint64 {
	v := make([]uint64, len(ms))
	for i, m := range ms {
		v[i] = field(m)
	}
	slices.Sort(v)
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}

// markCrossRail sets CrossRail on every measurement whose RNICs sit on
// different rails. It is marked here, from the graph, rather than by each
// prober, so the live, simulated and test probers all agree on it.
func markCrossRail(g topology.Graph, ms []model.MutualMeasurement) {
	for i := range ms {
		a, okA := g.RNIC(ms[i].Src)
		b, okB := g.RNIC(ms[i].Dst)
		ms[i].CrossRail = okA && okB && topology.CrossRail(a, b)
	}
}

// reportMutualPairs says once that the mutual probe has nothing to measure.
// Without it an empty pair set is indistinguishable from a broken probe: no
// hostping.mutual.* series is exported and the ToN diagnoses (RNIC
// connectivity failure, RNIC packet drops) never fire, with nothing said. The
// fabric keys carry the reason. One key per RNIC under the default rail scope
// is a rail-optimized host, which --mutual-scope=host pairs across rails when
// they route to each other; "unknown" is a missing RoCEv2 GID, which no scope
// pairs.
//
// Like reportSkips this is keyed on the reported state, so a daemon polling
// every --metric-interval states a standing condition once per change, and a
// pair set that comes back clears the key. Unlike reportSkips it says nothing
// on recovery: the resumed hostping.mutual.* series is that signal, and the
// graph is fixed for the life of a process, so the agent cannot reach it.
func (e *Engine) reportMutualPairs(pairs [][2]model.RNIC) {
	if len(pairs) > 0 {
		e.mutualKey = ""
		return
	}
	keys := e.Graph.FabricKeys()
	scope := e.Cfg.MutualScope
	if scope == "" {
		scope = model.MutualScopeRail
	}
	key := fmt.Sprintf("%s %d %q", scope, len(e.Graph.RNICs), keys)
	if key == e.mutualKey {
		return
	}
	e.mutualKey = key
	need := "two RNICs with a RoCEv2 GID"
	if e.Cfg.MutualInfiniBand {
		need = "two RNICs of one link layer with a RoCEv2 GID or InfiniBand LID"
	}
	if scope == model.MutualScopeRail {
		need = "a shared IPv4 subnet or typed GID rail"
		if e.Cfg.MutualInfiniBand {
			need = "a shared IPv4 subnet, typed GID or InfiniBand subnet rail"
		}
		// Two or more probeable rails are what host scope would pair.
		rails := len(keys)
		if slices.Contains(keys, "unknown") {
			rails--
		}
		if rails > 1 {
			need += " (--mutual-scope=host pairs across rails that route to each other)"
		}
	}
	e.logf("hostping: mutual probe has no RNIC pairs (scope=%s rnics=%d fabrics=%q); pairing needs %s, so RNIC connectivity and packet drop diagnoses are unavailable",
		scope, len(e.Graph.RNICs), keys, need)
}

// computeModeSkip is the reason not to probe ep at all, or nil. Only a GPU
// whose compute mode was read and is not Default is held back: hostping's
// context on an EXCLUSIVE_PROCESS GPU would contend with the job that owns it
// (or lock it out, if hostping got there first), and a PROHIBITED GPU refuses
// every context. A mode that could not be read (no nvidia-smi, a non-NVIDIA
// GPU, an unrecognised value) is no reason to skip, so the GPU is probed
// exactly as before compute modes were read.
func computeModeSkip(ep model.Endpoint, modes map[string]string) error {
	if ep.Kind != model.EndpointGPU {
		return nil
	}
	mode := modes[ep.ID]
	if !model.ComputeModeBlocksProbe(mode) {
		return nil
	}
	return fmt.Errorf("%w: %s", model.ErrComputeMode, mode)
}

func skippableProbe(err error) bool {
	return errors.Is(err, model.ErrNoGDR) ||
		errors.Is(err, model.ErrNoDevice) ||
		errors.Is(err, model.ErrNoGID) ||
		errors.Is(err, model.ErrNoLID) ||
		errors.Is(err, model.ErrPortNotActive) ||
		errors.Is(err, model.ErrProbe)
}

// portFailFast reports whether err, the failure of a loopback from an RNIC
// none of whose probes has answered in this run, means the RNIC's remaining
// probes in this run would fail the same way. That holds for an InfiniBand
// port whose WRITEs came back with a port-level transport error in every
// addressing mode tried (model.ErrPortLoopback): each further probe would
// wait out the full hardware retry budget once per mode, only to be skipped.
// Local errors, GDR and compute-mode skips and setup failures say nothing
// about the other endpoints and keep being probed one by one, as do RoCE
// ports. The decision lasts one run, so a port that recovers is probed again
// on the next.
func portFailFast(n model.RNIC, err error) bool {
	return n.LinkLayer == model.LinkLayerInfiniBand && errors.Is(err, model.ErrPortLoopback)
}

// notProbedError is the skip reason of an endpoint left unprobed because an
// earlier loopback from the same RNIC failed at the port (portFailFast). It
// names that first failure and still wraps its sentinels.
func notProbedError(first error) error {
	return fmt.Errorf("not probed: an earlier loopback on this RNIC failed: %w", first)
}

type probeSkip struct {
	rnic string
	ep   string
	kind model.EndpointKind
	err  error
}

// skipGroup is every probe that failed the same way on the same endpoint kind.
// The reason is the probe's own error text, which carries the detail string
// ("NoGDR: no cuda") naming the cause.
type skipGroup struct {
	kind   model.EndpointKind
	err    error
	eps    []string // distinct endpoints, in probe order
	probes int
	ex     string // first rnic->endpoint pair, for the all-failed error
}

func groupSkips(skips []probeSkip) []skipGroup {
	var groups []skipGroup
	index := map[string]int{}
	for _, s := range skips {
		key := string(s.kind) + "\x00" + s.err.Error()
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, skipGroup{kind: s.kind, err: s.err, ex: s.rnic + "->" + s.ep})
		}
		groups[i].probes++
		if !slices.Contains(groups[i].eps, s.ep) {
			groups[i].eps = append(groups[i].eps, s.ep)
		}
	}
	return groups
}

func skippedEndpoints(groups []skipGroup) []model.EndpointSkip {
	if len(groups) == 0 {
		return nil
	}
	out := make([]model.EndpointSkip, 0, len(groups))
	for _, g := range groups {
		out = append(out, model.EndpointSkip{
			Kind:      g.kind,
			Reason:    g.err.Error(),
			Endpoints: slices.Clone(g.eps),
			Probes:    g.probes,
		})
	}
	return out
}

// reportSkips logs one aggregated line, but only when the set of (kind, reason)
// pairs differs from the last run's — including when it empties again. The
// daemon runs the engine on every trigger, so a standing condition (a host
// without a CUDA runtime) would otherwise repeat the same line forever. Counts
// are deliberately not part of that key: busy mode probes only affinitive
// paths, so they move between runs without the operator-visible state changing.
// Per-run counts stay in the report and in hostping.endpoint.skipped.
func (e *Engine) reportSkips(groups []skipGroup) {
	key := skipSignature(groups)
	if key == e.skipKey {
		return
	}
	e.skipKey = key
	if len(groups) == 0 {
		e.logf("hostping: endpoints skipped none (every endpoint registered again)")
		return
	}
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		parts = append(parts, fmt.Sprintf("kind=%s count=%d probes=%d reason=%q endpoints=%s",
			g.kind, len(g.eps), g.probes, g.err.Error(), strings.Join(g.eps, ",")))
	}
	e.logf("hostping: endpoints skipped %s", strings.Join(parts, "; "))
}

func skipSignature(groups []skipGroup) string {
	if len(groups) == 0 {
		return ""
	}
	keys := make([]string, 0, len(groups))
	for _, g := range groups {
		keys = append(keys, string(g.kind)+"\x00"+g.err.Error())
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x01")
}

// reportInfiniBandExcluded says once per process which InfiniBand RNICs the
// mutual probe leaves out (all of them, unless --mutual-infiniband). The graph
// is fixed for the life of the process, so once is enough.
func (e *Engine) reportInfiniBandExcluded() {
	if e.ibExcludedLogged || e.Cfg.MutualInfiniBand {
		return
	}
	e.ibExcludedLogged = true
	ids := e.Graph.InfiniBandRNICs()
	if len(ids) == 0 {
		return
	}
	e.logf("hostping: mutual probe leaves out %d InfiniBand RNIC(s) %v; --mutual-infiniband pairs them by subnet prefix, which cannot tell apart separate fabrics that share the default prefix",
		len(ids), ids)
}

// reportAddressing logs how the live loopback probes of an RNIC addressed its
// port (RoCEv2 GID, InfiniBand LID, or LID plus GRH), once per RNIC and again
// whenever it changes. The simulator reports no addressing and logs nothing.
func (e *Engine) reportAddressing(n model.RNIC, a model.PortAddressing) {
	if a.Mode == "" {
		return
	}
	if e.addrKey == nil {
		e.addrKey = map[string]model.PortAddressing{}
	}
	if prev, ok := e.addrKey[n.ID]; ok && prev == a {
		return
	}
	e.addrKey[n.ID] = a
	e.logf("hostping: loopback addressing rnic=%s device=%s link_layer=%s mode=%s port=%d lid=%d",
		n.ID, n.Device, n.LinkLayer, a.Mode, a.Port, a.LID)
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func noLoopbackError(skips []probeSkip) error {
	groups := groupSkips(skips)
	if len(groups) == 0 {
		return fmt.Errorf("no loopback measurements")
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].kind == groups[j].kind {
			return false
		}
		return groups[i].kind == model.EndpointMemory
	})
	joined := make([]error, 0, len(groups))
	for _, g := range groups {
		joined = append(joined, fmt.Errorf("%s %s x%d: %w", g.kind, g.ex, g.probes, g.err))
	}
	return fmt.Errorf("no loopback measurements: %w", errors.Join(joined...))
}

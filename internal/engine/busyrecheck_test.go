package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

const (
	busyFlat = 100_000_000_000
	busyLow  = 60_000_000_000
)

// busyHostGraph is a synthetic host whose RNICs all reach memory nodes
// mem1..memN as remote memory, so every RNIC's busy-probed remote_mem class
// holds mems paths.
func busyHostGraph(rnics, mems int) topology.Graph {
	var g topology.Graph
	for k := 1; k <= mems; k++ {
		g.Endpoints = append(g.Endpoints, model.Endpoint{ID: fmt.Sprintf("mem%d", k), Kind: model.EndpointMemory, Index: k, NUMA: k})
	}
	for i := range rnics {
		n := model.RNIC{ID: fmt.Sprintf("rnic%d", i), LineRate: 200_000_000_000, Fabric: "unknown"}
		g.RNICs = append(g.RNICs, n)
		for _, e := range g.Endpoints {
			g.Paths = append(g.Paths, model.Path{RNIC: n.ID, Endpoint: e.ID, Distance: model.DistanceRemoteMem})
		}
	}
	return g
}

// lowSweep is a reading sequence whose sweep (the first --busy-samples
// readings) is low and whose later readings are flat: noise that landed on the
// sweep only.
func lowSweep(cfg config.Config) []uint64 {
	out := make([]uint64, 0, busySampleCount(cfg)+1)
	for range busySampleCount(cfg) {
		out = append(out, busyLow)
	}
	return append(out, busyFlat)
}

func runBusyOn(t *testing.T, g topology.Graph, cfg config.Config, p Prober, mode model.RunMode) (model.Report, []string) {
	t.Helper()
	e := New(cfg, g, p)
	var logs []string
	e.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	req := RunRequest{Mode: mode}
	if mode == model.ModeBusy {
		req.BusyRNICs = map[string]bool{}
		for _, n := range g.RNICs {
			req.BusyRNICs[n.ID] = true
		}
	}
	rep, err := e.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return rep, logs
}

// One path's sweep readings land in the low tail on a healthy
// loaded RNIC. Re-read with its siblings it reads at their level, so the
// verdict is cleared: the path is uncertain with rnic_busy, the report records
// the recheck, and no cause or abnormal link is left.
func TestBusyRecheckClearsNoisySweep(t *testing.T) {
	g := busyHostGraph(1, 4)
	cfg := config.Defaults()
	p := &seqProber{flat: busyFlat, bw: map[string][]uint64{"rnic0->mem2": lowSweep(cfg)}}
	rep, _ := runBusyOn(t, g, cfg, p, model.ModeBusy)

	// The sweep alone flags the path; the report keeps its sweep reading.
	busy := map[string]bool{"rnic0": true}
	sweep := analyzer.ClassifyPaths(g, rep.Paths, model.ModeBusy, busy, cfg, baseline.Overlay{})
	if m := findPath(sweep, "rnic0", "mem2"); m.Status != model.PathAbnormal {
		t.Fatalf("precondition: the sweep should flag rnic0->mem2: %+v", m)
	}

	got := pathStatus(t, rep, "rnic0", "mem2")
	r := got.BusyRecheck
	if got.Status != model.PathUncertain || got.UncertainReason != model.UncertainRNICBusy || r == nil || r.Confirmed {
		t.Fatalf("rnic0->mem2 = %+v recheck %+v, want uncertain/rnic_busy and a cleared recheck", got, r)
	}
	if got.BandwidthBPS != busyLow {
		t.Errorf("bandwidth = %d, want the sweep median %d kept", got.BandwidthBPS, uint64(busyLow))
	}
	wantSibs := []string{"rnic0->mem1", "rnic0->mem3", "rnic0->mem4"}
	if strings.Join(r.Siblings, ",") != strings.Join(wantSibs, ",") || r.Rounds != analyzer.BusyRecheckRounds {
		t.Errorf("recheck = %+v, want siblings %v over %d rounds", r, wantSibs, analyzer.BusyRecheckRounds)
	}
	if r.PathBPS != busyFlat || r.SiblingBPS != busyFlat {
		t.Errorf("recheck medians = %d / %d, want both at the flat level", r.PathBPS, r.SiblingBPS)
	}
	if len(rep.Causes) != 0 {
		t.Errorf("a cleared verdict left causes %+v", rep.Causes)
	}
	for _, l := range rep.Links {
		if l.Status == model.LinkAbnormal || l.Status == model.LinkGray {
			t.Errorf("link %s is %s after the verdict was cleared", l.ID, l.Status)
		}
	}
	for _, m := range rep.Paths {
		if m.Status == model.PathAbnormal {
			t.Errorf("%s->%s still abnormal", m.RNIC, m.Endpoint)
		}
	}
	// Sweep plus one read per round, for the path and each sibling.
	sweepN := busySampleCount(cfg)
	if n := p.calls["rnic0->mem2"]; n != sweepN+analyzer.BusyRecheckRounds {
		t.Errorf("path read %d times, want %d", n, sweepN+analyzer.BusyRecheckRounds)
	}
	for _, s := range wantSibs {
		if n := p.calls[s]; n != sweepN+analyzer.BusyRecheckRounds {
			t.Errorf("%s read %d times, want %d", s, n, sweepN+analyzer.BusyRecheckRounds)
		}
	}
}

// A path that really is down stays low when re-read with its siblings: the
// verdict and its cause are kept.
func TestBusyRecheckConfirmsDegradedPath(t *testing.T) {
	g := busyHostGraph(1, 4)
	cfg := config.Defaults()
	p := &seqProber{flat: busyFlat, bw: map[string][]uint64{"rnic0->mem2": {busyLow}}}
	rep, _ := runBusyOn(t, g, cfg, p, model.ModeBusy)
	got := pathStatus(t, rep, "rnic0", "mem2")
	if got.Status != model.PathAbnormal || got.BusyRecheck == nil || !got.BusyRecheck.Confirmed {
		t.Fatalf("rnic0->mem2 = %+v recheck %+v, want abnormal and confirmed", got, got.BusyRecheck)
	}
	if got.BusyRecheck.PathBPS != busyLow {
		t.Errorf("recheck path median = %d, want %d", got.BusyRecheck.PathBPS, uint64(busyLow))
	}
	if len(rep.Causes) == 0 {
		t.Error("a confirmed verdict raised no cause")
	}
}

// In a class of three, one sibling reads near its unloaded level in the
// sweep, which flags the other two against the median of that high reading
// and each other. Re-read side by side, all three read level: both verdicts
// are cleared.
func TestBusyRecheckClearsPairFlaggedByHighSibling(t *testing.T) {
	g := busyHostGraph(1, 3)
	cfg := config.Defaults()
	high := make([]uint64, 0, busySampleCount(cfg)+1)
	for range busySampleCount(cfg) {
		high = append(high, 2*busyFlat)
	}
	high = append(high, busyFlat)
	p := &seqProber{flat: busyFlat, bw: map[string][]uint64{"rnic0->mem1": high}}
	rep, _ := runBusyOn(t, g, cfg, p, model.ModeBusy)
	for _, ep := range []string{"mem2", "mem3"} {
		got := pathStatus(t, rep, "rnic0", ep)
		if got.Status != model.PathUncertain || got.BusyRecheck == nil || got.BusyRecheck.Confirmed {
			t.Errorf("rnic0->%s = %+v recheck %+v, want rechecked and cleared", ep, got, got.BusyRecheck)
		}
	}
	if len(rep.Causes) != 0 {
		t.Errorf("cleared verdicts left causes %+v", rep.Causes)
	}
}

// A genuine drop of two paths of three, with the third reading high in the
// re-reads too, is confirmed for both.
func TestBusyRecheckConfirmsTwoOfThreeDegraded(t *testing.T) {
	g := busyHostGraph(1, 3)
	cfg := config.Defaults()
	const low = busyFlat / 2
	p := &seqProber{flat: busyFlat, bw: map[string][]uint64{"rnic0->mem2": {low}, "rnic0->mem3": {low}}}
	rep, _ := runBusyOn(t, g, cfg, p, model.ModeBusy)
	for _, ep := range []string{"mem2", "mem3"} {
		got := pathStatus(t, rep, "rnic0", ep)
		if got.Status != model.PathAbnormal || got.BusyRecheck == nil || !got.BusyRecheck.Confirmed {
			t.Errorf("rnic0->%s = %+v recheck %+v, want abnormal and confirmed", ep, got, got.BusyRecheck)
		}
	}
	if got := pathStatus(t, rep, "rnic0", "mem1"); got.Status == model.PathAbnormal {
		t.Errorf("the healthy path was flagged: %+v", got)
	}
}

// At most BusyRecheckMaxPaths paths are rechecked per run; the rest keep the
// sweep's verdict and cost no extra probe.
func TestBusyRecheckCap(t *testing.T) {
	n := analyzer.BusyRecheckMaxPaths + 2
	g := busyHostGraph(n, 3)
	cfg := config.Defaults()
	p := &seqProber{flat: busyFlat, bw: map[string][]uint64{}}
	for i := range n {
		p.bw[fmt.Sprintf("rnic%d->mem1", i)] = lowSweep(cfg)
	}
	rep, _ := runBusyOn(t, g, cfg, p, model.ModeBusy)
	cleared, kept := 0, 0
	for _, m := range rep.Paths {
		switch {
		case m.BusyRecheck != nil && !m.BusyRecheck.Confirmed:
			cleared++
		case m.Status == model.PathAbnormal && m.BusyRecheck == nil:
			kept++
		}
	}
	if cleared != analyzer.BusyRecheckMaxPaths || kept != n-analyzer.BusyRecheckMaxPaths {
		t.Fatalf("cleared %d kept %d, want %d and %d", cleared, kept, analyzer.BusyRecheckMaxPaths, n-analyzer.BusyRecheckMaxPaths)
	}
	sweep := len(g.Paths) * busySampleCount(cfg)
	// Each rechecked path reads itself and its two siblings once per round.
	want := sweep + analyzer.BusyRecheckMaxPaths*analyzer.BusyRecheckRounds*3
	if got := totalCalls(p); got != want {
		t.Errorf("%d loopback probes, want %d", got, want)
	}
}

// A recheck probe that fails abandons the path's recheck and keeps the sweep
// verdict, and the abandonment is logged.
func TestBusyRecheckProbeErrorKeepsVerdict(t *testing.T) {
	g := busyHostGraph(1, 4)
	cfg := config.Defaults()
	p := &seqProber{
		flat:    busyFlat,
		bw:      map[string][]uint64{"rnic0->mem2": {busyLow, busyLow, busyLow}},
		loopErr: map[string]error{"rnic0->mem2": fmt.Errorf("%w: setup failed", model.ErrProbe)},
	}
	rep, logs := runBusyOn(t, g, cfg, p, model.ModeBusy)
	got := pathStatus(t, rep, "rnic0", "mem2")
	if got.Status != model.PathAbnormal || got.BusyRecheck != nil {
		t.Fatalf("rnic0->mem2 = %+v, want the sweep's abnormal verdict and no recheck", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "busy recheck of rnic0->mem2 abandoned") {
		t.Errorf("abandonment not logged: %q", logs)
	}
}

// cancelOnRecheck cancels the run's context on the first recheck read.
type cancelOnRecheck struct {
	*seqProber
	after  int
	cancel context.CancelFunc
	n      int
}

func (c *cancelOnRecheck) Loopback(ctx context.Context, n model.RNIC, ep model.Endpoint, s, l uint32) (model.PathMeasurement, error) {
	c.n++
	if c.n == c.after {
		c.cancel()
	}
	return c.seqProber.Loopback(ctx, n, ep, s, l)
}

// Cancelling the run during the recheck ends the run with the context error.
func TestBusyRecheckHonoursCancel(t *testing.T) {
	g := busyHostGraph(1, 4)
	cfg := config.Defaults()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sweep := len(g.Paths) * busySampleCount(cfg)
	p := &cancelOnRecheck{
		seqProber: &seqProber{flat: busyFlat, bw: map[string][]uint64{"rnic0->mem2": lowSweep(cfg)}},
		after:     sweep + 1,
		cancel:    cancel,
	}
	e := New(cfg, g, p)
	e.Logf = func(string, ...any) {}
	_, err := e.Run(ctx, RunRequest{Mode: model.ModeBusy, BusyRNICs: map[string]bool{"rnic0": true}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if p.n != sweep+1 {
		t.Errorf("%d probes, want the sweep and the one that was cancelled", p.n)
	}
}

// Idle runs judge against the baseline and recheck nothing: every path is
// read exactly once.
func TestBusyRecheckNotInIdleMode(t *testing.T) {
	g := busyHostGraph(1, 4)
	cfg := config.Defaults()
	p := &seqProber{flat: busyFlat, bw: map[string][]uint64{"rnic0->mem2": {busyLow, busyFlat}}}
	rep, _ := runBusyOn(t, g, cfg, p, model.ModeIdle)
	for k, n := range p.calls {
		if n != 1 {
			t.Errorf("%s read %d times in an idle run, want once", k, n)
		}
	}
	for _, m := range rep.Paths {
		if m.BusyRecheck != nil {
			t.Errorf("%s->%s carries a busy recheck in an idle run", m.RNIC, m.Endpoint)
		}
	}
}

func findPath(ms []model.PathMeasurement, rnic, ep string) model.PathMeasurement {
	for _, m := range ms {
		if m.RNIC == rnic && m.Endpoint == ep {
			return m
		}
	}
	return model.PathMeasurement{}
}

func totalCalls(p *seqProber) int {
	t := 0
	for _, n := range p.calls {
		t += n
	}
	return t
}

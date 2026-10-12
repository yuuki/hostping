package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// bwWindowProber reads every path from the simulator and scales its bandwidth
// by scale(rnic, ep, n), where n counts the path's reads in the run so far (0
// is the sweep, 1 and up the rechecks). fail, when set, may fail a read
// instead.
type bwWindowProber struct {
	SimProber
	scale func(rnic, ep string, n int) float64
	fail  func(rnic, ep string, n int) error
	calls map[string]int
}

func newBWWindowProber(w *sim.World, scale func(rnic, ep string, n int) float64) *bwWindowProber {
	return &bwWindowProber{SimProber: SimProber{World: w}, scale: scale, calls: map[string]int{}}
}

func (p *bwWindowProber) Loopback(ctx context.Context, rnic model.RNIC, ep model.Endpoint, small, large uint32) (model.PathMeasurement, error) {
	key := rnic.ID + "->" + ep.ID
	n := p.calls[key]
	p.calls[key] = n + 1
	if p.fail != nil {
		if err := p.fail(rnic.ID, ep.ID, n); err != nil {
			return model.PathMeasurement{}, err
		}
	}
	m, err := p.SimProber.Loopback(ctx, rnic, ep, small, large)
	if err == nil && p.scale != nil {
		m.BandwidthBPS = uint64(float64(m.BandwidthBPS) * p.scale(rnic.ID, ep.ID, n))
	}
	return m, err
}

func (p *bwWindowProber) total() int {
	t := 0
	for _, n := range p.calls {
		t += n
	}
	return t
}

// windowDrop is the share of its level a memory path keeps in a slow window:
// well past --abnormal-th.
const windowDrop = 0.6

// memWindow drops the memory paths of the given RNICs to windowDrop on the
// reads for which slow(n) holds.
func memWindow(slow func(n int) bool, rnics ...string) func(string, string, int) float64 {
	return func(rnic, ep string, n int) float64 {
		if !strings.HasPrefix(ep, "mem") || !slow(n) {
			return 1
		}
		for _, r := range rnics {
			if r == rnic {
				return windowDrop
			}
		}
		return 1
	}
}

func sweepOnly(n int) bool { return n == 0 }

// idleSweep is the number of probes an idle sweep of g takes.
func idleSweep(g topology.Graph) int {
	n := 0
	for _, r := range g.RNICs {
		n += len(g.ProbePaths(r.ID, model.ModeIdle))
	}
	return n
}

func runBW(t *testing.T, g topology.Graph, p Prober, req RunRequest) (model.Report, []string) {
	t.Helper()
	e := New(config.Defaults(), g, p)
	var logs []string
	e.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	e.OneShot = true
	if req.Mode == "" {
		req.Mode = model.ModeIdle
	}
	rep, err := e.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return rep, logs
}

// sweepCauses is what the analyzer reports from the sweep's readings alone,
// without the recheck: the verdict the run would have without it.
func sweepCauses(g topology.Graph, rep model.Report, req RunRequest) []string {
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: req.Mode, BusyRNICs: req.BusyRNICs,
		Measurements: rep.Paths, OneShot: true,
	})
	var out []string
	for _, c := range res.Causes {
		out = append(out, c.Code)
	}
	return out
}

func hasCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}

// A slow window over the batches of two consecutive RNICs drops
// their memory paths below the baseline in the sweep, which reads as a failed
// root port. Re-read next to the same distance on the other RNICs, they read
// level and meet the baseline: every path of each group is normal again, the
// report records the recheck, and no cause is left.
func TestBWRecheckClearsSlowWindow(t *testing.T) {
	g := topology.Fig2()
	p := newBWWindowProber(sim.New(g), memWindow(sweepOnly, "rnic0", "rnic1"))
	req := RunRequest{Mode: model.ModeIdle}
	rep, _ := runBW(t, g, p, req)

	if sc := sweepCauses(g, rep, req); !hasCode(sc, model.CauseCPURootPortFailure) {
		t.Fatalf("precondition: the sweep alone should report a failed root port, got %v", sc)
	}
	if len(rep.Causes) != 0 {
		t.Errorf("a slow window left causes %+v", rep.Causes)
	}
	for _, m := range rep.Paths {
		if m.Status == model.PathAbnormal {
			t.Errorf("%s->%s still abnormal", m.RNIC, m.Endpoint)
		}
	}
	got := pathStatus(t, rep, "rnic0", "mem0")
	r := got.BWRecheck
	if got.Status != model.PathNormal || r == nil || r.Outcome != model.BWRecheckCleared {
		t.Fatalf("rnic0->mem0 = %+v recheck %+v, want normal and cleared", got, r)
	}
	wantRefs := []string{"rnic2->mem1", "rnic3->mem1"}
	if r.Path != "rnic0->mem0" || strings.Join(r.Refs, ",") != strings.Join(wantRefs, ",") || r.Rounds != analyzer.BWRecheckRounds {
		t.Errorf("recheck = %+v, want rnic0->mem0 against %v over %d rounds", r, wantRefs, analyzer.BWRecheckRounds)
	}
	if r.PathBPS == 0 || r.RefBPS == 0 || r.BaselineBPS == 0 {
		t.Errorf("recheck medians missing: %+v", r)
	}
	// The remote-memory group has its own recheck, against remote memory.
	if rr := pathStatus(t, rep, "rnic0", "mem1").BWRecheck; rr == nil || rr.Path != "rnic0->mem1" || strings.Join(rr.Refs, ",") != "rnic2->mem0,rnic3->mem0" {
		t.Errorf("rnic0->mem1 recheck = %+v, want its own against remote memory", rr)
	}
	// Four groups (two RNICs × local and remote memory), each re-reading its
	// path and two references in every round.
	sweep := idleSweep(g)
	if want := sweep + 4*analyzer.BWRecheckRounds*3; p.total() != want {
		t.Errorf("%d loopback probes, want %d", p.total(), want)
	}
	// The sweep's readings stay in the report.
	if got.BandwidthBPS >= r.PathBPS {
		t.Errorf("bandwidth %d, want the low sweep reading kept below the re-read %d", got.BandwidthBPS, r.PathBPS)
	}
}

// The whole host is in the slow state during the re-reads: the path reads
// with its references, but both sit below the baseline. The groups are
// uncertain with host_shift and no cause is reported.
func TestBWRecheckHostShift(t *testing.T) {
	g := topology.Fig2()
	scale := func(rnic, ep string, n int) float64 {
		if !strings.HasPrefix(ep, "mem") {
			return 1
		}
		if n == 0 && (rnic == "rnic2" || rnic == "rnic3") {
			return 1
		}
		return windowDrop
	}
	p := newBWWindowProber(sim.New(g), scale)
	rep, _ := runBW(t, g, p, RunRequest{Mode: model.ModeIdle})
	for _, rnic := range []string{"rnic0", "rnic1"} {
		for _, ep := range []string{"mem0", "mem1"} {
			got := pathStatus(t, rep, rnic, ep)
			if got.Status != model.PathUncertain || got.UncertainReason != model.UncertainHostShift || got.BWRecheck == nil || got.BWRecheck.Outcome != model.BWRecheckHostShift {
				t.Errorf("%s->%s = %+v recheck %+v, want uncertain/host_shift", rnic, ep, got, got.BWRecheck)
			}
		}
	}
	if len(rep.Causes) != 0 {
		t.Errorf("a host-wide shift left causes %+v", rep.Causes)
	}
}

// A path whose memory traffic really is degraded stays low in the re-reads
// while its references read high: the verdict is confirmed and the failed
// root port is still reported, naming the failing paths.
func TestBWRecheckConfirmsDegradedRootPort(t *testing.T) {
	g := topology.Fig2()
	p := newBWWindowProber(sim.New(g), memWindow(func(int) bool { return true }, "rnic0"))
	rep, _ := runBW(t, g, p, RunRequest{Mode: model.ModeIdle})
	got := pathStatus(t, rep, "rnic0", "mem0")
	if got.Status != model.PathAbnormal || got.BWRecheck == nil || got.BWRecheck.Outcome != model.BWRecheckConfirmed {
		t.Fatalf("rnic0->mem0 = %+v recheck %+v, want abnormal and confirmed", got, got.BWRecheck)
	}
	var rp *model.RootCause
	for i, c := range rep.Causes {
		if c.Code == model.CauseCPURootPortFailure {
			rp = &rep.Causes[i]
		}
	}
	if rp == nil {
		t.Fatalf("a confirmed root-port drop raised no %s: %+v", model.CauseCPURootPortFailure, rep.Causes)
	}
	if strings.Contains(rp.Evidence, "()") || !strings.Contains(rp.Evidence, "rnic0->mem0") {
		t.Errorf("evidence %q should name the failing paths through the root port", rp.Evidence)
	}
}

// In a busy run, the unloaded RNICs are judged against the idle baseline and
// rechecked the same way; the references come only from unloaded RNICs.
func TestBWRecheckBusyRunUnloadedRNICs(t *testing.T) {
	g := topology.Fig2()
	p := newBWWindowProber(sim.New(g), memWindow(sweepOnly, "rnic0", "rnic1"))
	req := RunRequest{Mode: model.ModeBusy, BusyRNICs: map[string]bool{"rnic3": true}}
	rep, _ := runBW(t, g, p, req)
	if sc := sweepCauses(g, rep, req); len(sc) == 0 {
		t.Fatal("precondition: the sweep alone should report a cause")
	}
	got := pathStatus(t, rep, "rnic0", "mem0")
	r := got.BWRecheck
	if got.Status != model.PathNormal || r == nil || r.Outcome != model.BWRecheckCleared {
		t.Fatalf("rnic0->mem0 = %+v recheck %+v, want normal and cleared", got, r)
	}
	for _, ref := range r.Refs {
		if strings.HasPrefix(ref, "rnic3->") {
			t.Errorf("reference %s is on the loaded RNIC", ref)
		}
	}
	for _, m := range rep.Paths {
		if m.RNIC == "rnic3" && m.BWRecheck != nil {
			t.Errorf("%s->%s on the loaded RNIC carries a bandwidth recheck", m.RNIC, m.Endpoint)
		}
	}
	if len(rep.Causes) != 0 {
		t.Errorf("a slow window left causes %+v", rep.Causes)
	}
}

// At most BWRecheckMaxGroups groups are rechecked per run; the rest keep the
// sweep's verdict and cost no extra probe.
func TestBWRecheckCap(t *testing.T) {
	const flat, low = 200_000_000_000, 100_000_000_000
	flagged := analyzer.BWRecheckMaxGroups + 2
	g := busyHostGraph(flagged+1, 1)
	p := &seqProber{flat: flat, bw: map[string][]uint64{}}
	for i := range flagged {
		p.bw[fmt.Sprintf("rnic%d->mem1", i)] = []uint64{low, flat}
	}
	rep, _ := runBW(t, g, p, RunRequest{Mode: model.ModeIdle})
	cleared, kept := 0, 0
	for _, m := range rep.Paths {
		switch {
		case m.BWRecheck != nil && m.BWRecheck.Outcome == model.BWRecheckCleared:
			cleared++
		case m.Status == model.PathAbnormal && m.BWRecheck == nil:
			kept++
		}
	}
	if cleared != analyzer.BWRecheckMaxGroups || kept != flagged-analyzer.BWRecheckMaxGroups {
		t.Fatalf("cleared %d kept %d, want %d and %d", cleared, kept, analyzer.BWRecheckMaxGroups, flagged-analyzer.BWRecheckMaxGroups)
	}
	// One reference (the only unflagged RNIC) per group.
	if want := len(g.Paths) + analyzer.BWRecheckMaxGroups*analyzer.BWRecheckRounds*2; totalCalls(p) != want {
		t.Errorf("%d loopback probes, want %d", totalCalls(p), want)
	}
}

// A recheck probe that fails abandons the group's recheck and keeps the sweep
// verdict, and the abandonment is logged.
func TestBWRecheckProbeErrorKeepsVerdict(t *testing.T) {
	g := topology.Fig2()
	p := newBWWindowProber(sim.New(g), memWindow(sweepOnly, "rnic0"))
	p.fail = func(rnic, ep string, n int) error {
		if rnic == "rnic0" && ep == "mem0" && n > 0 {
			return fmt.Errorf("%w: setup failed", model.ErrProbe)
		}
		return nil
	}
	rep, logs := runBW(t, g, p, RunRequest{Mode: model.ModeIdle})
	got := pathStatus(t, rep, "rnic0", "mem0")
	if got.Status != model.PathAbnormal || got.BWRecheck != nil {
		t.Fatalf("rnic0->mem0 = %+v, want the sweep's abnormal verdict and no recheck", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "bandwidth recheck of rnic0/local_mem abandoned") {
		t.Errorf("abandonment not logged: %q", logs)
	}
	// The other group of the RNIC was rechecked and cleared.
	if r := pathStatus(t, rep, "rnic0", "mem1").BWRecheck; r == nil || r.Outcome != model.BWRecheckCleared {
		t.Errorf("rnic0->mem1 recheck = %+v, want cleared", r)
	}
}

// Cancelling the run during the recheck ends the run with the context error.
func TestBWRecheckHonoursCancel(t *testing.T) {
	g := topology.Fig2()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sweep := idleSweep(g)
	inner := newBWWindowProber(sim.New(g), memWindow(sweepOnly, "rnic0"))
	n := 0
	inner.fail = func(string, string, int) error {
		n++
		if n == sweep+1 {
			cancel()
		}
		return nil
	}
	e := New(config.Defaults(), g, inner)
	e.Logf = func(string, ...any) {}
	_, err := e.Run(ctx, RunRequest{Mode: model.ModeIdle})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if inner.total() != sweep+1 {
		t.Errorf("%d probes, want the sweep and the one that was cancelled", inner.total())
	}
}

// SkipBWRecheck, which calibration sets, reads every path exactly once and
// keeps the sweep's verdict.
func TestBWRecheckSkipped(t *testing.T) {
	g := topology.Fig2()
	p := newBWWindowProber(sim.New(g), memWindow(sweepOnly, "rnic0"))
	e := New(config.Defaults(), g, p)
	e.Logf = func(string, ...any) {}
	e.SkipBWRecheck = true
	e.SkipLatencyRecheck = true
	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range p.calls {
		if n != 1 {
			t.Errorf("%s read %d times with the recheck skipped, want once", k, n)
		}
	}
	if got := pathStatus(t, rep, "rnic0", "mem0"); got.Status != model.PathAbnormal || got.BWRecheck != nil {
		t.Errorf("rnic0->mem0 = %+v, want the sweep's abnormal verdict", got)
	}
}

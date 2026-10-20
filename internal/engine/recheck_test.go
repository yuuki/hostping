package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// slowBump is a latency rise well past --abnormal-th and the sibling floor.
const slowBump = 2000

// firstReadSlow is a slow time window over the sweep batch of the given RNICs:
// their first reading of every path is slow, every later one is not.
func firstReadSlow(rnics ...string) func(string, string, int) uint64 {
	return func(rnic, _ string, n int) uint64 {
		for _, r := range rnics {
			if r == rnic && n == 0 {
				return slowBump
			}
		}
		return 0
	}
}

// runEngine runs a one-shot engine once, as hostpingctl diagnose does: a
// sibling latency verdict is reported there when its recheck confirmed it.
func runEngine(t *testing.T, cfg config.Config, g topology.Graph, p Prober, mode model.RunMode, skip bool) model.Report {
	t.Helper()
	e := New(cfg, g, p)
	e.SkipLatencyRecheck = skip
	e.OneShot = true
	req := RunRequest{Mode: mode}
	if mode == model.ModeBusy {
		req.BusyRNICs = map[string]bool{"rnic1": true}
	}
	return runWith(t, e, req)
}

func latencyCausesOf(rep model.Report) []model.RootCause {
	var out []model.RootCause
	for _, c := range rep.Causes {
		if c.Code == model.CausePathLatencyAbnormal {
			out = append(out, c)
		}
	}
	return out
}

func bandwidthOf(rep model.Report) map[string]string {
	out := map[string]string{}
	for _, m := range rep.Paths {
		out[m.RNIC+"->"+m.Endpoint] = fmt.Sprintf("%s/%s/%d", m.Status, m.UncertainReason, m.BandwidthBPS)
	}
	return out
}

func withoutLatency(cs []model.RootCause) []model.RootCause {
	var out []model.RootCause
	for _, c := range cs {
		if c.Code != model.CausePathLatencyAbnormal {
			out = append(out, c)
		}
	}
	return out
}

// One RNIC's whole sweep batch falls into a slow time window. Each
// of its paths reads high against siblings measured at other times, but
// re-read next to siblings it reads at their level, so the recheck clears
// every one and no cause is left. The sweep's readings stay in the report.
func TestLatencyRecheckClearsSlowWindow(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()

	before := runEngine(t, cfg, g, newWindowProber(g, firstReadSlow("rnic1")), model.ModeIdle, true)
	flagged := 0
	for _, m := range before.Paths {
		if m.LatencyStatus == model.LatencyAbnormal {
			flagged++
		}
	}
	if flagged == 0 {
		t.Fatal("precondition: without the recheck the slow window should read latency-abnormal")
	}
	if lc := latencyCausesOf(before); len(lc) != 0 {
		t.Errorf("one-shot run reported sibling verdicts no recheck confirmed: %+v", lc)
	}

	p := newWindowProber(g, firstReadSlow("rnic1"))
	rep := runEngine(t, cfg, g, p, model.ModeIdle, false)
	if lc := latencyCausesOf(rep); len(lc) != 0 {
		t.Fatalf("a slow time window left latency causes %+v", lc)
	}
	cleared := 0
	for _, m := range rep.Paths {
		if m.LatencyStatus == model.LatencyAbnormal {
			t.Errorf("%s->%s still latency-abnormal: %+v", m.RNIC, m.Endpoint, m)
		}
		if m.LatencyReason != model.LatencyRecheckCleared {
			if m.LatencyRecheck != nil {
				t.Errorf("%s->%s carries a recheck with reason %s", m.RNIC, m.Endpoint, m.LatencyReason)
			}
			continue
		}
		cleared++
		if m.RNIC != "rnic1" {
			t.Errorf("%s->%s cleared, but only rnic1 read slow", m.RNIC, m.Endpoint)
		}
		r := m.LatencyRecheck
		if m.LatencyStatus != model.LatencyNormal || r == nil || r.Confirmed || len(r.Siblings) != analyzer.LatencyRecheckReads {
			t.Errorf("%s->%s = %+v recheck %+v, want normal, cleared, with %d sibling reads", m.RNIC, m.Endpoint, m, r, analyzer.LatencyRecheckReads)
			continue
		}
		for _, s := range r.Siblings {
			if strings.HasPrefix(s, "rnic1->") {
				t.Errorf("%s->%s read against %s on its own RNIC while other RNICs had siblings", m.RNIC, m.Endpoint, s)
			}
		}
		// The sweep reading is kept; only the verdict changed.
		if m.SmallLatNsP50 < r.PathNsP50+slowBump {
			t.Errorf("%s->%s small_lat %d, want the slow sweep reading kept", m.RNIC, m.Endpoint, m.SmallLatNsP50)
		}
		if got := p.calls[m.RNIC+"->"+m.Endpoint]; got != 1+analyzer.LatencyRecheckReads {
			t.Errorf("%s->%s read %d times, want the sweep plus %d", m.RNIC, m.Endpoint, got, analyzer.LatencyRecheckReads)
		}
	}
	if cleared == 0 {
		t.Fatal("no path was cleared by the recheck")
	}
	// The recheck changes no bandwidth reading, status or cause.
	if !reflect.DeepEqual(bandwidthOf(rep), bandwidthOf(before)) {
		t.Errorf("bandwidth view changed by the recheck:\n got %v\nwant %v", bandwidthOf(rep), bandwidthOf(before))
	}
	if !reflect.DeepEqual(withoutLatency(rep.Causes), withoutLatency(before.Causes)) {
		t.Errorf("non-latency causes changed by the recheck:\n got %+v\nwant %+v", rep.Causes, before.Causes)
	}
	if !reflect.DeepEqual(rep.Links, before.Links) {
		t.Error("links changed by the recheck")
	}
}

// A path that is really slow stays slow when re-read next to its siblings:
// the verdict and its cause are kept, and the evidence carries the re-reads.
func TestLatencyRecheckConfirmsSlowPath(t *testing.T) {
	g := topology.Fig2()
	slow := g.ProbePaths("rnic1", model.ModeIdle)[0].Endpoint
	p := newWindowProber(g, func(rnic, ep string, _ int) uint64 {
		if rnic == "rnic1" && ep == slow {
			return slowBump
		}
		return 0
	})
	rep := runEngine(t, config.Defaults(), g, p, model.ModeIdle, false)
	m := pathOf(t, rep, "rnic1", slow)
	r := m.LatencyRecheck
	if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings || r == nil || !r.Confirmed {
		t.Fatalf("rnic1->%s = %+v recheck %+v, want abnormal/siblings, confirmed", slow, m, r)
	}
	if r.PathNsP50 < r.SiblingNsP50+slowBump {
		t.Errorf("recheck %+v, want the path %d ns above its siblings", r, slowBump)
	}
	lc := latencyCausesOf(rep)
	if len(lc) != 1 || !reflect.DeepEqual(lc[0].RNICs, []string{"rnic1"}) || !reflect.DeepEqual(lc[0].Endpoints, []string{slow}) {
		t.Fatalf("latency causes %+v, want one naming rnic1->%s", lc, slow)
	}
	if !strings.Contains(lc[0].Evidence, "recheck") {
		t.Errorf("evidence %q does not carry the recheck", lc[0].Evidence)
	}
}

// A verdict against a latency the operator set is not rechecked: that
// reference does not depend on when the siblings were read.
func TestLatencyRecheckSkipsUserBaseline(t *testing.T) {
	g := topology.Fig2()
	var target model.Path
	for _, p := range g.ProbePaths("rnic1", model.ModeIdle) {
		if p.Distance == model.DistanceLocalMem {
			target = p
			break
		}
	}
	if target.Endpoint == "" {
		t.Fatal("no local_mem path on rnic1")
	}
	n, _ := g.RNIC("rnic1")
	ep, _ := g.Endpoint(target.Endpoint)
	healthy, err := SimProber{World: sim.New(g)}.Loopback(context.Background(), n, ep, 1, 128*1024)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.BaselineSpecs = []string{fmt.Sprintf("local_mem=%dns", healthy.SmallLatNsP50)}

	p := newWindowProber(g, func(rnic, e string, n int) uint64 {
		if rnic == "rnic1" && e == target.Endpoint && n == 0 {
			return slowBump
		}
		return 0
	})
	rep := runEngine(t, cfg, g, p, model.ModeIdle, false)
	m := pathOf(t, rep, "rnic1", target.Endpoint)
	if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyByBaseline || m.LatencyRecheck != nil {
		t.Fatalf("rnic1->%s = %+v, want abnormal/user_baseline with no recheck", target.Endpoint, m)
	}
	if got := p.calls["rnic1->"+target.Endpoint]; got != 1 {
		t.Errorf("rnic1->%s read %d times, want only the sweep", target.Endpoint, got)
	}
	if len(latencyCausesOf(rep)) == 0 {
		t.Error("the user-baseline verdict lost its cause")
	}
}

// The recheck is capped per run: with two RNICs in a slow window there are
// more candidates than the cap, LatencyRecheckMaxPaths are rechecked, taken in
// turn from both RNICs, the rest keep their sweep verdict marked
// latency_recheck_skipped "cap", and the extra probes are bounded. The
// two RNICs sit on different NUMA nodes, so the locality check (a rise every
// RNIC of one node shares) keeps the verdicts the recheck does not reach.
func TestLatencyRecheckCap(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	skipP := newWindowProber(g, firstReadSlow("rnic0", "rnic2"))
	skipped := runEngine(t, cfg, g, skipP, model.ModeIdle, true)
	candidates := 0
	for _, m := range skipped.Paths {
		if m.LatencyStatus == model.LatencyAbnormal {
			candidates++
		}
	}
	if candidates <= analyzer.LatencyRecheckMaxPaths {
		t.Fatalf("precondition: %d candidates, want more than the cap %d", candidates, analyzer.LatencyRecheckMaxPaths)
	}

	p := newWindowProber(g, firstReadSlow("rnic0", "rnic2"))
	rep := runEngine(t, cfg, g, p, model.ModeIdle, false)
	rechecked, left := 0, 0
	perRNIC := map[string]int{}
	for _, m := range rep.Paths {
		if m.LatencyRecheck != nil {
			rechecked++
			perRNIC[m.RNIC]++
		} else if m.LatencyStatus == model.LatencyAbnormal {
			left++
			if m.LatencyRecheckSkipped != model.LatencyRecheckSkipCap {
				t.Errorf("%s->%s left out of the recheck with skip %q, want cap", m.RNIC, m.Endpoint, m.LatencyRecheckSkipped)
			}
		}
	}
	if want := analyzer.LatencyRecheckMaxPaths / 2; perRNIC["rnic0"] != want || perRNIC["rnic2"] != want {
		t.Errorf("rechecked per RNIC %v, want %d on each slow RNIC", perRNIC, want)
	}
	if rechecked != analyzer.LatencyRecheckMaxPaths || left != candidates-rechecked {
		t.Errorf("rechecked %d and left %d abnormal of %d candidates, want %d rechecked", rechecked, left, candidates, analyzer.LatencyRecheckMaxPaths)
	}
	if extra, want := p.total()-skipP.total(), 2*analyzer.LatencyRecheckReads*analyzer.LatencyRecheckMaxPaths; extra != want {
		t.Errorf("recheck spent %d probes, want %d", extra, want)
	}
}

// Busy runs judge no latency, so they recheck nothing and probe exactly as
// before.
func TestLatencyRecheckNotInBusyMode(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	skipP := newWindowProber(g, firstReadSlow("rnic1"))
	skipped := runEngine(t, cfg, g, skipP, model.ModeBusy, true)
	p := newWindowProber(g, firstReadSlow("rnic1"))
	rep := runEngine(t, cfg, g, p, model.ModeBusy, false)
	if !reflect.DeepEqual(p.calls, skipP.calls) {
		t.Errorf("busy run probed %v, want %v", p.calls, skipP.calls)
	}
	for _, m := range rep.Paths {
		if m.LatencyRecheck != nil || m.LatencyReason != model.LatencyBusy {
			t.Errorf("%s->%s = %+v, want unjudged busy with no recheck", m.RNIC, m.Endpoint, m)
		}
	}
	if !reflect.DeepEqual(rep.Causes, skipped.Causes) {
		t.Errorf("busy causes %+v, want %+v", rep.Causes, skipped.Causes)
	}
}

// A recheck that cannot be completed is no evidence: the path keeps its sweep
// verdict, and the run still succeeds.
func TestLatencyRecheckProbeErrorKeepsVerdict(t *testing.T) {
	g := topology.Fig2()
	slow := g.ProbePaths("rnic1", model.ModeIdle)[0].Endpoint
	p := newWindowProber(g, firstReadSlow("rnic1"))
	p.fail = func(rnic, ep string, n int) error {
		if rnic == "rnic1" && ep == slow && n == 2 {
			return fmt.Errorf("%w: setup failed", model.ErrProbe)
		}
		return nil
	}
	rep := runEngine(t, config.Defaults(), g, p, model.ModeIdle, false)
	m := pathOf(t, rep, "rnic1", slow)
	if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings || m.LatencyRecheck != nil {
		t.Fatalf("rnic1->%s = %+v, want the sweep's abnormal/siblings verdict with no recheck", slow, m)
	}
}

// Like the sweep, the recheck stops between probes when the run is cancelled.
func TestLatencyRecheckHonoursCancel(t *testing.T) {
	g := topology.Fig2()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newWindowProber(g, firstReadSlow("rnic1"))
	p.fail = func(_, _ string, n int) error {
		if n == 1 {
			cancel()
		}
		return nil
	}
	e := quietEngine(config.Defaults(), g, p)
	reread := 0
	_, err := e.Run(ctx, RunRequest{Mode: model.ModeIdle})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	for _, n := range p.calls {
		if n > 1 {
			reread++
		}
	}
	if reread != 1 {
		t.Errorf("%d paths re-read after the cancel, want only the one that saw it", reread)
	}
}

// A daemon engine carries a per-path latency streak from run to
// run. A path the recheck confirms is reported only once it has read
// abnormal against its siblings in analyzer.LatencyRuns consecutive idle
// runs; a busy run in between neither extends nor breaks the streak, and a
// discarded run is rolled back with the other streaks.
func TestLatencyStreakAcrossDaemonRuns(t *testing.T) {
	g := topology.Fig2()
	slow := g.ProbePaths("rnic1", model.ModeIdle)[0].Endpoint
	key := "rnic1->" + slow
	p := newWindowProber(g, func(rnic, ep string, _ int) uint64 {
		if rnic == "rnic1" && ep == slow {
			return slowBump
		}
		return 0
	})
	e := quietEngine(config.Defaults(), g, p)
	run := func(mode model.RunMode) model.Report {
		t.Helper()
		req := RunRequest{Mode: mode}
		if mode == model.ModeBusy {
			req.BusyRNICs = map[string]bool{"rnic1": true}
		}
		return runWith(t, e, req)
	}

	first := run(model.ModeIdle)
	m := pathOf(t, first, "rnic1", slow)
	if m.LatencyStatus != model.LatencyAbnormal || m.LatencyRecheck == nil || !m.LatencyRecheck.Confirmed || m.LatencyStreak != 1 {
		t.Fatalf("first run rnic1->%s = %+v, want abnormal, confirmed, streak 1", slow, m)
	}
	if lc := latencyCausesOf(first); len(lc) != 0 {
		t.Fatalf("first run reported %+v, want nothing until the rise persists", lc)
	}
	if e.Latency[key] != 1 || len(e.Latency) != 1 {
		t.Fatalf("streaks after one run = %v, want %s:1", e.Latency, key)
	}

	run(model.ModeBusy)
	if e.Latency[key] != 1 || len(e.Latency) != 1 {
		t.Fatalf("streaks after a busy run = %v, want them untouched", e.Latency)
	}

	before := e.SnapshotStreaks()
	run(model.ModeIdle)
	if e.Latency[key] != 2 {
		t.Fatalf("streaks after the second idle run = %v, want %s:2", e.Latency, key)
	}
	e.RestoreStreaks(before)
	if e.Latency[key] != 1 || len(e.Latency) != 1 {
		t.Fatalf("streaks after a rollback = %v, want %s:1", e.Latency, key)
	}

	var last model.Report
	for n := 2; n <= analyzer.LatencyRuns; n++ {
		last = run(model.ModeIdle)
		if m := pathOf(t, last, "rnic1", slow); m.LatencyStreak != n {
			t.Errorf("idle run %d streak = %d, want %d", n, m.LatencyStreak, n)
		}
		if lc := latencyCausesOf(last); n < analyzer.LatencyRuns && len(lc) != 0 {
			t.Fatalf("idle run %d reported %+v before the streak reached %d", n, lc, analyzer.LatencyRuns)
		}
	}
	lc := latencyCausesOf(last)
	if len(lc) != 1 || !reflect.DeepEqual(lc[0].Endpoints, []string{slow}) {
		t.Fatalf("idle run %d causes %+v, want one naming rnic1->%s", analyzer.LatencyRuns, lc, slow)
	}
	if !strings.Contains(lc[0].Evidence, fmt.Sprintf("%d consecutive idle runs", analyzer.LatencyRuns)) {
		t.Errorf("evidence %q does not say the rise persisted", lc[0].Evidence)
	}
}

// RestoreStreaks drops latency streaks of paths the graph does not have, as
// a state file could name.
func TestRestoreStreaksPrunesUnknownPaths(t *testing.T) {
	g := topology.Fig2()
	e := New(config.Defaults(), g, nil)
	ok := g.Paths[0].RNIC + "->" + g.Paths[0].Endpoint
	e.RestoreStreaks(Streaks{Latency: map[string]int{ok: 1, "rnic9->gpu0": 1, "garbage": 1}})
	if !reflect.DeepEqual(e.Latency, map[string]int{ok: 1}) {
		t.Errorf("latency streaks = %v, want only %s", e.Latency, ok)
	}
}

// sweepSlow sets the prober's bump and returns a reset to call before each
// daemon run: the prober's read counts restart, so read 0 is that run's sweep.
func sweepSlow(p *windowProber, slow func(rnic, ep string, n int) uint64) func() {
	p.bump = slow
	return p.reset
}

// Two RNICs fall into a slow window on every run, flagging more
// paths than the recheck's cap. The cap rotates, so every flagged path is
// rechecked (and cleared) within two runs, and a path past the cap never
// builds a streak from its unconfirmed readings: no run reports a cause. In
// probe order the last paths of the later RNIC used to be left out of every
// run and reported on the second.
func TestLatencyRecheckCapRotatesAcrossDaemonRuns(t *testing.T) {
	g := topology.Fig2()
	p := newWindowProber(g, nil)
	reset := sweepSlow(p, firstReadSlow("rnic0", "rnic2"))
	e := quietEngine(config.Defaults(), g, p)

	rechecked := map[string]bool{}
	flagged := map[string]bool{}
	for run := range 4 {
		reset()
		rep := runWith(t, e, RunRequest{Mode: model.ModeIdle})
		if lc := latencyCausesOf(rep); len(lc) != 0 {
			t.Fatalf("run %d reported %+v from a slow window", run, lc)
		}
		capped := 0
		for _, m := range rep.Paths {
			key := m.RNIC + "->" + m.Endpoint
			if m.LatencyRecheck != nil {
				rechecked[key] = true
				flagged[key] = true
			}
			if m.LatencyRecheckSkipped == model.LatencyRecheckSkipCap {
				capped++
				flagged[key] = true
				if m.LatencyStreak != 0 {
					t.Errorf("run %d: capped %s carries streak %d, want none", run, key, m.LatencyStreak)
				}
			}
		}
		if capped == 0 {
			t.Fatalf("precondition: run %d left nothing past the cap", run)
		}
		if len(e.Latency) != 0 {
			t.Errorf("run %d left streaks %v, want none", run, e.Latency)
		}
		if run == 1 {
			for k := range flagged {
				if !rechecked[k] {
					t.Errorf("%s not rechecked within two runs", k)
				}
			}
		}
	}
}

// A real fault on the last path of the later slow RNIC is past
// the cap in some runs. It holds its streak there and is not reported on
// those runs, but the rotating cap reaches it, its recheck confirms it, and it
// is reported at most one run later than an uncapped fault would be. A held
// streak survives SnapshotStreaks and RestoreStreaks like any other.
func TestLatencyRecheckCapReportsPersistentFault(t *testing.T) {
	g := topology.Fig2()
	probe := g.ProbePaths("rnic2", model.ModeIdle)
	fault := probe[len(probe)-1].Endpoint
	key := "rnic2->" + fault
	window := firstReadSlow("rnic0", "rnic2")
	p := newWindowProber(g, nil)
	reset := sweepSlow(p, func(rnic, ep string, n int) uint64 {
		b := window(rnic, ep, n)
		if rnic == "rnic2" && ep == fault {
			b += slowBump
		}
		return b
	})
	e := quietEngine(config.Defaults(), g, p)

	reportedAt, cappedSeen, heldChecked := -1, false, false
	for run := range 6 {
		prev := e.Latency[key]
		reset()
		rep := runWith(t, e, RunRequest{Mode: model.ModeIdle})
		m := pathOf(t, rep, "rnic2", fault)
		named := false
		for _, c := range latencyCausesOf(rep) {
			if slices.Contains(c.Endpoints, fault) && slices.Contains(c.RNICs, "rnic2") {
				named = true
			}
		}
		switch {
		case m.LatencyRecheckSkipped == model.LatencyRecheckSkipCap:
			cappedSeen = true
			if m.LatencyRecheck != nil || m.LatencyStreak != prev || e.Latency[key] != prev || named {
				t.Errorf("run %d: capped fault = %+v, streak %d, named %v; want held at %d and not reported", run, m, e.Latency[key], named, prev)
			}
			if prev > 0 {
				e.RestoreStreaks(e.SnapshotStreaks())
				if e.Latency[key] != prev {
					t.Errorf("run %d: held streak %d lost in a snapshot round trip: %v", run, prev, e.Latency)
				}
				heldChecked = true
			}
		case m.LatencyRecheck != nil && m.LatencyRecheck.Confirmed:
			if m.LatencyStreak != prev+1 {
				t.Errorf("run %d: confirmed fault streak %d, want %d", run, m.LatencyStreak, prev+1)
			}
			if named != (m.LatencyStreak >= analyzer.LatencyRuns) {
				t.Errorf("run %d: fault with streak %d named %v", run, m.LatencyStreak, named)
			}
			if named && reportedAt < 0 {
				reportedAt = run
			}
		default:
			t.Fatalf("run %d: fault = %+v, want capped or confirmed", run, m)
		}
	}
	if !cappedSeen {
		t.Error("precondition: the fault was never past the cap")
	}
	if reportedAt < 0 || reportedAt > analyzer.LatencyRuns {
		t.Errorf("fault first reported on run %d, want within %d runs", reportedAt, analyzer.LatencyRuns+1)
	}
	if !heldChecked {
		t.Error("no run held a non-zero streak past the cap")
	}
}

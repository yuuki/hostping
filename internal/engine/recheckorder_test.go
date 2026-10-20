package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// The three rechecks share one shape, a sweep and then paired re-reads, and
// the design pins the order of those re-reads (docs/design.md "Latency
// recheck", "Busy recheck", "Bandwidth recheck"): a sibling or reference is
// read right next to the path, or the two sides share no time window. The
// other recheck tests pin per-path read counts; these pin the exact order of
// every Loopback of a run, sweep and recheck, as literal slices, so a driver
// that keeps the counts but reorders the reads fails with a readable diff.

// fig2IdleSweep is the order an idle sweep of Fig.2 reads its paths in:
// Graph.RNICs in order, each RNIC's ProbePaths in order, once each.
var fig2IdleSweep = []string{
	"rnic0->gpu0", "rnic0->gpu1", "rnic0->gpu2", "rnic0->gpu3", "rnic0->gpu4", "rnic0->gpu5", "rnic0->gpu6", "rnic0->gpu7", "rnic0->mem0", "rnic0->mem1",
	"rnic1->gpu0", "rnic1->gpu1", "rnic1->gpu2", "rnic1->gpu3", "rnic1->gpu4", "rnic1->gpu5", "rnic1->gpu6", "rnic1->gpu7", "rnic1->mem0", "rnic1->mem1",
	"rnic2->gpu0", "rnic2->gpu1", "rnic2->gpu2", "rnic2->gpu3", "rnic2->gpu4", "rnic2->gpu5", "rnic2->gpu6", "rnic2->gpu7", "rnic2->mem0", "rnic2->mem1",
	"rnic3->gpu0", "rnic3->gpu1", "rnic3->gpu2", "rnic3->gpu3", "rnic3->gpu4", "rnic3->gpu5", "rnic3->gpu6", "rnic3->gpu7", "rnic3->mem0", "rnic3->mem1",
}

// sweepOrder is the order Run's sweep reads g in: Graph.RNICs × ProbePaths,
// each path samples times in a row.
func sweepOrder(g topology.Graph, mode model.RunMode, samples int) []string {
	var out []string
	for _, n := range g.RNICs {
		for _, p := range g.ProbePaths(n.ID, mode) {
			for range samples {
				out = append(out, n.ID+"->"+p.Endpoint)
			}
		}
	}
	return out
}

// The sweep literal is the graph's own order, so the recheck tests below
// start from where the sweep really ends.
func TestFig2IdleSweepLiteralMatchesGraph(t *testing.T) {
	assertOrder(t, "fig2 idle sweep", fig2IdleSweep, sweepOrder(topology.Fig2(), model.ModeIdle, 1))
}

// assertOrder compares two Loopback orders and reports the first read that
// differs, with both orders in full.
func assertOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	show := func(s []string) string {
		if len(s) == 0 {
			return "(none)"
		}
		return strings.Join(s, " ")
	}
	t.Errorf("%s: loopback order differs at read %d (got %d reads, want %d)\n got[%d:]  %s\nwant[%d:]  %s\n got  %s\nwant  %s",
		what, at, len(got), len(want),
		at, show(got[min(at, len(got)):]), at, show(want[min(at, len(want)):]),
		show(got), show(want))
}

// splitSweep cuts a run's Loopback order where its sweep ends, failing the
// test when the run read fewer paths than the sweep takes.
func splitSweep(t *testing.T, order []string, sweep int) (sweepReads, recheckReads []string) {
	t.Helper()
	if len(order) < sweep {
		t.Fatalf("run made %d loopbacks, fewer than the %d of its sweep: %s", len(order), sweep, strings.Join(order, " "))
	}
	return order[:sweep], order[sweep:]
}

// Latency recheck: each planned path is re-read LatencyRecheckReads times,
// every read followed by one sibling of its class, path, sibling, path,
// sibling, ..., so each pair shares a time window. The siblings are the
// class's paths judged normal on the other RNICs, one RNIC after another
// (analyzer.PlanLatencyRecheck), and the plans run one after the other in
// measurement order.
func TestLatencyRecheckOrderInterleavesPathAndSiblings(t *testing.T) {
	if analyzer.LatencyRecheckReads != 3 {
		t.Fatalf("the literal below assumes LatencyRecheckReads = 3, now %d", analyzer.LatencyRecheckReads)
	}
	g := topology.Fig2()
	// rnic1's two cross_root paths read slow on every read, so both are
	// planned and both are confirmed. Their class's normal paths on the other
	// RNICs are rnic0->gpu2,gpu3, rnic2->gpu6,gpu7 and rnic3->gpu4,gpu5.
	p := newWindowProber(g, func(rnic, ep string, _ int) uint64 {
		if rnic == "rnic1" && (ep == "gpu0" || ep == "gpu1") {
			return slowBump
		}
		return 0
	})
	e := New(config.Defaults(), g, p)
	e.OneShot = true
	rep := runWith(t, e, RunRequest{Mode: model.ModeIdle})
	for _, ep := range []string{"gpu0", "gpu1"} {
		if m := pathOf(t, rep, "rnic1", ep); m.LatencyRecheck == nil || !m.LatencyRecheck.Confirmed {
			t.Fatalf("precondition: rnic1->%s = %+v, want a confirmed latency recheck", ep, m)
		}
	}

	sweep, recheck := splitSweep(t, p.order, len(fig2IdleSweep))
	assertOrder(t, "sweep", sweep, fig2IdleSweep)
	assertOrder(t, "latency recheck", recheck, []string{
		// rnic1->gpu0: path, sibling, three times, one sibling per other RNIC.
		"rnic1->gpu0", "rnic0->gpu2",
		"rnic1->gpu0", "rnic2->gpu6",
		"rnic1->gpu0", "rnic3->gpu4",
		// rnic1->gpu1, against the same siblings.
		"rnic1->gpu1", "rnic0->gpu2",
		"rnic1->gpu1", "rnic2->gpu6",
		"rnic1->gpu1", "rnic3->gpu4",
	})
}

// Busy recheck: a flagged path on a loaded RNIC is re-read in
// BusyRecheckRounds rounds, each round the path and then every planned
// sibling once, in measurement order (analyzer.PlanBusyRecheck). The sweep
// before it reads each path --busy-samples times in a row.
func TestBusyRecheckOrderRoundsOfPathThenSiblings(t *testing.T) {
	cfg := config.Defaults()
	if analyzer.BusyRecheckRounds != 3 || busySampleCount(cfg) != 3 {
		t.Fatalf("the literal below assumes BusyRecheckRounds = 3 and --busy-samples 3, now %d and %d", analyzer.BusyRecheckRounds, busySampleCount(cfg))
	}
	g := busyHostGraph(1, 4)
	// rnic0->mem2's sweep readings are low and its re-reads flat, so it is
	// planned against its three siblings and cleared.
	p := &seqProber{flat: busyFlat, bw: map[string][]uint64{"rnic0->mem2": lowSweep(cfg)}}
	rep := runWith(t, New(cfg, g, p), RunRequest{Mode: model.ModeBusy, BusyRNICs: map[string]bool{"rnic0": true}})
	if m := pathOf(t, rep, "rnic0", "mem2"); m.BusyRecheck == nil || m.BusyRecheck.Confirmed {
		t.Fatalf("precondition: rnic0->mem2 = %+v, want a cleared busy recheck", m)
	}

	wantSweep := []string{
		"rnic0->mem1", "rnic0->mem1", "rnic0->mem1",
		"rnic0->mem2", "rnic0->mem2", "rnic0->mem2",
		"rnic0->mem3", "rnic0->mem3", "rnic0->mem3",
		"rnic0->mem4", "rnic0->mem4", "rnic0->mem4",
	}
	assertOrder(t, "busy sweep literal", wantSweep, sweepOrder(g, model.ModeBusy, busySampleCount(cfg)))
	sweep, recheck := splitSweep(t, p.order, len(wantSweep))
	assertOrder(t, "sweep", sweep, wantSweep)
	assertOrder(t, "busy recheck", recheck, []string{
		// Three rounds of the path and then its siblings.
		"rnic0->mem2", "rnic0->mem1", "rnic0->mem3", "rnic0->mem4",
		"rnic0->mem2", "rnic0->mem1", "rnic0->mem3", "rnic0->mem4",
		"rnic0->mem2", "rnic0->mem1", "rnic0->mem3", "rnic0->mem4",
	})
}

// Bandwidth recheck: each flagged group's representative is re-read in
// BWRecheckRounds rounds, each round the path and then every reference once,
// the reference to the same endpoint on another RNIC first, then one per
// further RNIC (analyzer.PlanBWRecheck). The groups run one after the other
// in measurement order.
func TestBWRecheckOrderRoundsOfPathThenRefs(t *testing.T) {
	if analyzer.BWRecheckRounds != 3 || analyzer.BWRecheckMaxRefs != 2 {
		t.Fatalf("the literal below assumes BWRecheckRounds = 3 and BWRecheckMaxRefs = 2, now %d and %d", analyzer.BWRecheckRounds, analyzer.BWRecheckMaxRefs)
	}
	g := topology.Fig2()
	// rnic0's memory paths read low in the sweep only: two groups, its local
	// and its remote memory, each cleared by the re-reads.
	p := newBWWindowProber(sim.New(g), memWindow(sweepOnly, "rnic0"))
	rep, _ := runBW(t, g, p, RunRequest{Mode: model.ModeIdle})
	for _, ep := range []string{"mem0", "mem1"} {
		if m := pathOf(t, rep, "rnic0", ep); m.BWRecheck == nil || m.BWRecheck.Outcome != model.BWRecheckCleared {
			t.Fatalf("precondition: rnic0->%s = %+v, want a cleared bandwidth recheck", ep, m)
		}
	}

	sweep, recheck := splitSweep(t, p.order, len(fig2IdleSweep))
	assertOrder(t, "sweep", sweep, fig2IdleSweep)
	assertOrder(t, "bandwidth recheck", recheck, []string{
		// rnic0/local_mem: the path, local memory of rnic1 (mem0, the same
		// endpoint), then local memory of rnic2 (mem1), three rounds.
		"rnic0->mem0", "rnic1->mem0", "rnic2->mem1",
		"rnic0->mem0", "rnic1->mem0", "rnic2->mem1",
		"rnic0->mem0", "rnic1->mem0", "rnic2->mem1",
		// rnic0/remote_mem: the path, remote memory of rnic1 (mem1, the same
		// endpoint), then remote memory of rnic2 (mem0), three rounds.
		"rnic0->mem1", "rnic1->mem1", "rnic2->mem0",
		"rnic0->mem1", "rnic1->mem1", "rnic2->mem0",
		"rnic0->mem1", "rnic1->mem1", "rnic2->mem0",
	})
}

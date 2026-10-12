package analyzer_test

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// latGraph is a synthetic host of nodes NUMA nodes with perNode RNICs each and
// one memory node on each of the first mems nodes: rnicI sits on node
// I/perNode, memK on node K, and an RNIC reaches memory on its own node as
// local_mem and the rest as remote_mem.
func latGraph(nodes, perNode, mems int) topology.Graph {
	var g topology.Graph
	for k := range mems {
		g.Endpoints = append(g.Endpoints, model.Endpoint{ID: fmt.Sprintf("mem%d", k), Kind: model.EndpointMemory, Index: k, NUMA: k})
	}
	for i := range nodes * perNode {
		n := model.RNIC{ID: fmt.Sprintf("rnic%d", i), NUMA: i / perNode}
		g.RNICs = append(g.RNICs, n)
		for _, e := range g.Endpoints {
			d := model.DistanceRemoteMem
			if e.NUMA == n.NUMA {
				d = model.DistanceLocalMem
			}
			g.Paths = append(g.Paths, model.Path{RNIC: n.ID, Endpoint: e.ID, Distance: d})
		}
	}
	return g
}

// latReadings reads every path of g at 1000 ns, and the paths named in slow
// ("rnic->endpoint") at 2000 ns, past --abnormal-th and the sibling floor.
func latReadings(g topology.Graph, slow ...string) []model.PathMeasurement {
	set := map[string]bool{}
	for _, s := range slow {
		set[s] = true
	}
	var ms []model.PathMeasurement
	for _, p := range g.Paths {
		m := model.PathMeasurement{RNIC: p.RNIC, Endpoint: p.Endpoint, SmallLatNsP50: 1000}
		if set[p.RNIC+"->"+p.Endpoint] {
			m.SmallLatNsP50 = 2000
		}
		ms = append(ms, m)
	}
	return ms
}

func planAt(g topology.Graph, ms []model.PathMeasurement, offset int) analyzer.LatencyRecheckSelection {
	return analyzer.PlanLatencyRecheck(g, ms, model.ModeIdle, config.Defaults(), baseline.Overlay{}, offset)
}

func refsOf(sel analyzer.LatencyRecheckSelection) []string {
	var out []string
	for _, p := range sel.Plans {
		out = append(out, p.Path.String())
	}
	return out
}

// With more candidates than the cap, one run takes one path per
// RNIC per pass rather than the first RNICs' every path, and the window moves
// with the offset so every candidate is rechecked within
// ⌈candidates / cap⌉ runs. One RNIC per NUMA node keeps the locality rule out.
func TestPlanLatencyRecheckFairOverRNICs(t *testing.T) {
	const rnics = 24
	g := latGraph(rnics, 1, 4)
	var slow []string
	for i := range rnics {
		// Every RNIC has one flagged remote path, the first four two more.
		slow = append(slow, fmt.Sprintf("rnic%d->mem%d", i, (i+1)%4))
		if i < 4 {
			slow = append(slow, fmt.Sprintf("rnic%d->mem%d", i, (i+2)%4), fmt.Sprintf("rnic%d->mem%d", i, (i+3)%4))
		}
	}
	ms := latReadings(g, slow...)
	if len(slow) <= analyzer.LatencyRecheckMaxPaths {
		t.Fatalf("precondition: %d candidates, want more than the cap", len(slow))
	}

	first := planAt(g, ms, 0)
	if len(first.Plans) != analyzer.LatencyRecheckMaxPaths || len(first.Plans)+len(first.Capped) != len(slow) {
		t.Fatalf("offset 0 planned %d and capped %d of %d candidates", len(first.Plans), len(first.Capped), len(slow))
	}
	perRNIC := map[string]int{}
	for _, p := range first.Plans {
		perRNIC[p.Path.RNIC]++
	}
	if len(perRNIC) != analyzer.LatencyRecheckMaxPaths {
		t.Errorf("offset 0 rechecked %v, want one path on each of %d RNICs", perRNIC, analyzer.LatencyRecheckMaxPaths)
	}

	covered := map[string]bool{}
	for run := range (len(slow) + analyzer.LatencyRecheckMaxPaths - 1) / analyzer.LatencyRecheckMaxPaths {
		sel := planAt(g, ms, run)
		for _, r := range refsOf(sel) {
			covered[r] = true
		}
		// Plans and Capped partition the candidates, in measurement order.
		all := append(refsOf(sel), func() []string {
			var out []string
			for _, c := range sel.Capped {
				out = append(out, c.String())
			}
			return out
		}()...)
		sort.Strings(all)
		want := append([]string(nil), slow...)
		sort.Strings(want)
		if !reflect.DeepEqual(all, want) {
			t.Errorf("offset %d: plans and capped %v, want the candidates %v", run, all, want)
		}
	}
	for _, s := range slow {
		if !covered[s] {
			t.Errorf("%s not rechecked within %d runs", s, (len(slow)+analyzer.LatencyRecheckMaxPaths-1)/analyzer.LatencyRecheckMaxPaths)
		}
	}

	// Within the cap, everything is planned whatever the offset.
	few := latReadings(g, slow[:analyzer.LatencyRecheckMaxPaths]...)
	for _, off := range []int{0, 1, 7} {
		if sel := planAt(g, few, off); len(sel.Plans) != analyzer.LatencyRecheckMaxPaths || len(sel.Capped) != 0 {
			t.Errorf("offset %d: %d candidates planned %d, capped %d", off, analyzer.LatencyRecheckMaxPaths, len(sel.Plans), len(sel.Capped))
		}
	}
}

// The paths the locality rule clears — every RNIC of one node
// reading high together — take no part of the cap; a per-RNIC fault next to
// them is still planned.
func TestPlanLatencyRecheckSkipsLocalityShift(t *testing.T) {
	g := latGraph(4, 2, 4)
	var slow []string
	for _, r := range []string{"rnic0", "rnic1"} {
		for k := range 4 {
			slow = append(slow, fmt.Sprintf("%s->mem%d", r, k))
		}
	}
	fault := "rnic4->mem0"
	ms := latReadings(g, append(slow, fault)...)
	for _, m := range analyzer.ClassifyLatency(g, ms, model.ModeIdle, config.Defaults(), baseline.Overlay{}) {
		if m.SmallLatNsP50 == 2000 && (m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings) {
			t.Fatalf("precondition: sweep judged %s->%s %s/%s, want abnormal/siblings", m.RNIC, m.Endpoint, m.LatencyStatus, m.LatencyReason)
		}
	}
	sel := planAt(g, ms, 0)
	if got := refsOf(sel); !reflect.DeepEqual(got, []string{fault}) || len(sel.Capped) != 0 {
		t.Errorf("planned %v, capped %v, want only %s", got, sel.Capped, fault)
	}

	// Analyze still clears the node-wide rise after the recheck.
	r := analyzer.Analyze(analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: ms})
	for _, m := range r.Paths {
		if m.RNIC == "rnic0" || m.RNIC == "rnic1" {
			if m.LatencyReason != model.LatencyLocalityShift {
				t.Errorf("%s->%s = %s/%s, want locality_shift", m.RNIC, m.Endpoint, m.LatencyStatus, m.LatencyReason)
			}
		}
	}
}

// A sibling verdict the recheck left out for its cap holds its
// streak and is never reported, however long the streak it holds; the mark is
// in the report. A path the recheck did reach, or a normal one, is not
// marked.
func TestLatencyRecheckCapHoldsStreak(t *testing.T) {
	g := topology.Fig2()
	ms := classPaths(t, g, 5)
	ms[0].SmallLatNsP50 = 3000
	key := ms[0].RNIC + "->" + ms[0].Endpoint
	other := ms[1].RNIC + "->" + ms[1].Endpoint
	skip := map[string]model.LatencyRecheckSkip{key: model.LatencyRecheckSkipCap, other: model.LatencyRecheckSkipCap}

	for _, held := range []int{0, 1, 5} {
		hits := map[string]int{}
		if held > 0 {
			hits[key] = held
		}
		for _, oneShot := range []bool{false, true} {
			r := analyzer.Analyze(analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: ms,
				LatencyHits: hits, OneShot: oneShot, LatencyRecheckSkipped: skip})
			m := r.Paths[0]
			if m.LatencyStatus != model.LatencyAbnormal || m.LatencyRecheckSkipped != model.LatencyRecheckSkipCap || m.LatencyStreak != held {
				t.Errorf("held %d one-shot %v: path = %+v, want abnormal, skipped cap, streak %d", held, oneShot, m, held)
			}
			if got, ok := r.LatencyHits[key]; got != held || ok != (held > 0) {
				t.Errorf("held %d: streaks %v, want %s held", held, r.LatencyHits, key)
			}
			if lc := latencyCauses(r.Causes); len(lc) != 0 {
				t.Errorf("held %d one-shot %v: capped path reported: %+v", held, oneShot, lc)
			}
			if n := r.Paths[1]; n.LatencyRecheckSkipped != "" || n.LatencyStatus != model.LatencyNormal {
				t.Errorf("normal path = %+v, want no mark", n)
			}
		}
	}

	// Once rechecked and confirmed, the held streak grows and is reported.
	r := analyzer.Analyze(analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: ms,
		LatencyHits:    map[string]int{key: analyzer.LatencyRuns - 1},
		LatencyRecheck: map[string]model.LatencyRecheck{key: {PathNsP50: 3000, SiblingNsP50: 1000, Confirmed: true}}})
	if m := r.Paths[0]; m.LatencyStreak != analyzer.LatencyRuns || m.LatencyRecheckSkipped != "" || len(latencyCauses(r.Causes)) != 1 {
		t.Errorf("confirmed path = %+v causes %+v, want streak %d and one cause", m, r.Causes, analyzer.LatencyRuns)
	}

	// Busy runs judge no latency: the mark is not set and streaks stay.
	r = analyzer.Analyze(analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeBusy, Measurements: ms,
		LatencyHits: map[string]int{key: 1}, LatencyRecheckSkipped: skip})
	if m := r.Paths[0]; m.LatencyRecheckSkipped != "" || r.LatencyHits[key] != 1 {
		t.Errorf("busy run path = %+v streaks %v, want no mark and the streak kept", m, r.LatencyHits)
	}
}

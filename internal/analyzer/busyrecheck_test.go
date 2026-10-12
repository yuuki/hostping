package analyzer_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// busyHost is a synthetic host whose RNICs all sit on NUMA node 0 and reach
// memory nodes mem1..memN, each on a node of its own, as remote memory: every
// RNIC's busy-probed remote_mem class holds mems paths.
func busyHost(rnics, mems int) topology.Graph {
	var g topology.Graph
	for k := 1; k <= mems; k++ {
		g.Endpoints = append(g.Endpoints, model.Endpoint{ID: fmt.Sprintf("mem%d", k), Kind: model.EndpointMemory, Index: k, NUMA: k})
	}
	for i := range rnics {
		n := model.RNIC{ID: fmt.Sprintf("rnic%d", i), NUMA: 0, LineRate: 200 * gbps}
		g.RNICs = append(g.RNICs, n)
		for _, e := range g.Endpoints {
			g.Paths = append(g.Paths, model.Path{RNIC: n.ID, Endpoint: e.ID, Distance: model.DistanceRemoteMem})
		}
	}
	return g
}

// busyReadings reads every path of g at 100 Gb/s, except the ones named in
// low ("rnic->endpoint"), which read at 60 Gb/s.
func busyReadings(g topology.Graph, low ...string) []model.PathMeasurement {
	var ms []model.PathMeasurement
	for _, p := range g.Paths {
		bw := uint64(100 * gbps)
		for _, l := range low {
			if l == p.RNIC+"->"+p.Endpoint {
				bw = 60 * gbps
			}
		}
		ms = append(ms, model.PathMeasurement{RNIC: p.RNIC, Endpoint: p.Endpoint, BandwidthBPS: bw})
	}
	return ms
}

func allBusy(g topology.Graph) map[string]bool {
	out := map[string]bool{}
	for _, n := range g.RNICs {
		out[n.ID] = true
	}
	return out
}

func planBusy(g topology.Graph, ms []model.PathMeasurement, mode model.RunMode, busy map[string]bool) []analyzer.BusyRecheckPlan {
	return analyzer.PlanBusyRecheck(g, ms, mode, busy, config.Defaults(), baseline.Overlay{})
}

// The plan takes exactly the paths the busy sibling rule flags, each with the
// other paths of its class, in measurement order.
func TestPlanBusyRecheckSiblings(t *testing.T) {
	g := busyHost(1, 4)
	ms := busyReadings(g, "rnic0->mem2")
	got := planBusy(g, ms, model.ModeBusy, allBusy(g))
	want := []analyzer.BusyRecheckPlan{{
		Path:     analyzer.PathRef{RNIC: "rnic0", Endpoint: "mem2"},
		Siblings: []analyzer.PathRef{{RNIC: "rnic0", Endpoint: "mem1"}, {RNIC: "rnic0", Endpoint: "mem3"}, {RNIC: "rnic0", Endpoint: "mem4"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %+v, want %+v", got, want)
	}

	// Siblings past BusyRecheckMaxSiblings are not read.
	g = busyHost(1, analyzer.BusyRecheckMaxSiblings+3)
	got = planBusy(g, busyReadings(g, "rnic0->mem1"), model.ModeBusy, allBusy(g))
	if len(got) != 1 || len(got[0].Siblings) != analyzer.BusyRecheckMaxSiblings {
		t.Fatalf("plan = %+v, want one path with %d siblings", got, analyzer.BusyRecheckMaxSiblings)
	}
}

// Nothing is planned outside a busy run or on an RNIC that is not loaded (it
// is judged against the idle baseline).
func TestPlanBusyRecheckSkips(t *testing.T) {
	g := busyHost(1, 4)
	ms := busyReadings(g, "rnic0->mem2")
	if got := planBusy(g, ms, model.ModeIdle, allBusy(g)); got != nil {
		t.Errorf("idle run planned %+v", got)
	}
	if got := planBusy(g, ms, model.ModeBusy, map[string]bool{}); got != nil {
		t.Errorf("unloaded RNIC planned %+v", got)
	}
}

// A class of three where one sibling reads high in the sweep flags the other
// two at once. Both are rechecked, each against every other path of the
// class, the other flagged one included: no share of flagged paths skips a
// class.
func TestPlanBusyRecheckMostOfClassFlagged(t *testing.T) {
	g := busyHost(1, 3)
	ms := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "mem1", BandwidthBPS: 200 * gbps},
		{RNIC: "rnic0", Endpoint: "mem2", BandwidthBPS: 100 * gbps},
		{RNIC: "rnic0", Endpoint: "mem3", BandwidthBPS: 100 * gbps},
	}
	judged := analyzer.ClassifyPaths(g, ms, model.ModeBusy, allBusy(g), config.Defaults(), baseline.Overlay{})
	flagged := 0
	for _, m := range judged {
		if m.Status == model.PathAbnormal {
			flagged++
		}
	}
	if flagged != 2 {
		t.Fatalf("precondition: %d of 3 flagged, want 2", flagged)
	}
	got := planBusy(g, ms, model.ModeBusy, allBusy(g))
	want := []analyzer.BusyRecheckPlan{
		{Path: analyzer.PathRef{RNIC: "rnic0", Endpoint: "mem2"}, Siblings: []analyzer.PathRef{{RNIC: "rnic0", Endpoint: "mem1"}, {RNIC: "rnic0", Endpoint: "mem3"}}},
		{Path: analyzer.PathRef{RNIC: "rnic0", Endpoint: "mem3"}, Siblings: []analyzer.PathRef{{RNIC: "rnic0", Endpoint: "mem1"}, {RNIC: "rnic0", Endpoint: "mem2"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %+v, want %+v", got, want)
	}
}

// At most BusyRecheckMaxPaths paths are planned per run, in measurement order.
func TestPlanBusyRecheckCap(t *testing.T) {
	n := analyzer.BusyRecheckMaxPaths + 2
	g := busyHost(n, 3)
	var low []string
	for i := range n {
		low = append(low, fmt.Sprintf("rnic%d->mem1", i))
	}
	got := planBusy(g, busyReadings(g, low...), model.ModeBusy, allBusy(g))
	if len(got) != analyzer.BusyRecheckMaxPaths {
		t.Fatalf("planned %d paths, want the cap %d", len(got), analyzer.BusyRecheckMaxPaths)
	}
	for i, p := range got {
		if want := fmt.Sprintf("rnic%d->mem1", i); p.Path.String() != want {
			t.Errorf("plan[%d] = %s, want %s", i, p.Path, want)
		}
	}
}

// Analyze applies a recheck before Algorithm 1 and the diagnoses: a cleared
// path is uncertain with rnic_busy and carries the recheck, a confirmed one
// stays abnormal, and a recheck for a path that is not a busy sibling verdict
// is ignored.
func TestAnalyzeAppliesBusyRecheck(t *testing.T) {
	g := busyHost(2, 3)
	busy := allBusy(g)
	ms := busyReadings(g, "rnic0->mem1", "rnic1->mem1")
	in := analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeBusy, BusyRNICs: busy, Measurements: ms}

	before := analyzer.Analyze(in)
	if len(before.Causes) == 0 {
		t.Fatal("precondition: the sweep's busy verdicts raise a cause")
	}

	cleared := analyzer.JudgeBusyRecheck([]uint64{100 * gbps, 98 * gbps, 101 * gbps}, []uint64{100 * gbps, 99 * gbps, 102 * gbps}, in.Cfg.AbnormalTh)
	confirmed := analyzer.JudgeBusyRecheck([]uint64{60 * gbps, 61 * gbps, 59 * gbps}, []uint64{100 * gbps, 99 * gbps, 102 * gbps}, in.Cfg.AbnormalTh)
	if cleared.Confirmed || !confirmed.Confirmed {
		t.Fatalf("precondition: cleared %+v confirmed %+v", cleared, confirmed)
	}
	in.BusyRecheck = map[string]model.BusyRecheck{
		"rnic0->mem1": cleared,
		"rnic1->mem1": confirmed,
		"rnic0->mem2": confirmed, // not flagged by the sweep: ignored
	}
	res := analyzer.Analyze(in)
	got := map[string]model.PathMeasurement{}
	for _, m := range res.Paths {
		got[m.RNIC+"->"+m.Endpoint] = m
	}
	if m := got["rnic0->mem1"]; m.Status != model.PathUncertain || m.UncertainReason != model.UncertainRNICBusy || m.BusyRecheck == nil || m.BusyRecheck.Confirmed {
		t.Errorf("cleared path = %+v, want uncertain/rnic_busy with the recheck", m)
	}
	if m := got["rnic0->mem1"]; m.BandwidthBPS != 60*gbps {
		t.Errorf("cleared path bandwidth = %d, want the sweep reading kept", m.BandwidthBPS)
	}
	if m := got["rnic1->mem1"]; m.Status != model.PathAbnormal || m.BusyRecheck == nil || !m.BusyRecheck.Confirmed {
		t.Errorf("confirmed path = %+v, want abnormal with the recheck", m)
	}
	if m := got["rnic0->mem2"]; m.BusyRecheck != nil || m.Status != model.PathUncertain {
		t.Errorf("unflagged path = %+v, want untouched", m)
	}
	for _, c := range res.Causes {
		for _, r := range c.RNICs {
			if r == "rnic0" {
				t.Errorf("cause %+v still names the cleared RNIC", c)
			}
		}
	}

	// Idle runs ignore a recheck map outright.
	idle := in
	idle.Mode = model.ModeIdle
	idle.BusyRecheck = nil
	want := analyzer.Analyze(idle)
	idle.BusyRecheck = in.BusyRecheck
	if gotIdle := analyzer.Analyze(idle); !reflect.DeepEqual(gotIdle.Paths, want.Paths) {
		t.Error("an idle run applied a busy recheck")
	}
}

// The decision compares the medians of the two sides' re-reads with the
// sweep's rule, degraded(path, siblings, th).
func TestJudgeBusyRecheck(t *testing.T) {
	th := config.Defaults().AbnormalTh
	sibs := []uint64{100 * gbps, 100 * gbps, 100 * gbps, 100 * gbps}
	cases := []struct {
		name string
		path []uint64
		want bool
	}{
		{"level with siblings", []uint64{100 * gbps, 100 * gbps, 100 * gbps}, false},
		{"one low tail reading", []uint64{50 * gbps, 100 * gbps, 100 * gbps}, false},
		{"low in most re-reads", []uint64{50 * gbps, 50 * gbps, 100 * gbps}, true},
	}
	for _, tc := range cases {
		r := analyzer.JudgeBusyRecheck(tc.path, sibs, th)
		if r.Confirmed != tc.want {
			t.Errorf("%s: %+v, want confirmed=%v", tc.name, r, tc.want)
		}
		if r.SiblingBPS != 100*gbps {
			t.Errorf("%s: sibling median %d", tc.name, r.SiblingBPS)
		}
	}
}

package analyzer_test

import (
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// classPaths returns n Fig.2 measurements of one distance, spread over the
// RNICs in turn, every one reading 1000 ns.
func classPaths(t *testing.T, g topology.Graph, n int) []model.PathMeasurement {
	t.Helper()
	byDist := map[model.Distance][][]model.PathMeasurement{}
	for _, d := range model.Distances() {
		byDist[d] = make([][]model.PathMeasurement, len(g.RNICs))
	}
	rnicIdx := map[string]int{}
	for i, r := range g.RNICs {
		rnicIdx[r.ID] = i
	}
	// Healthy simulator readings keep the bandwidth side normal.
	for _, m := range simMeasurements(t, g, "") {
		p, _ := g.Path(m.RNIC, m.Endpoint)
		m.SmallLatNsP50 = 1000
		ri := rnicIdx[m.RNIC]
		byDist[p.Distance][ri] = append(byDist[p.Distance][ri], m)
	}
	for _, d := range model.Distances() {
		var out []model.PathMeasurement
		for k := 0; len(out) < n; k++ {
			added := false
			for ri := range g.RNICs {
				if k < len(byDist[d][ri]) && len(out) < n {
					out = append(out, byDist[d][ri][k])
					added = true
				}
			}
			if !added {
				break
			}
		}
		if len(out) == n {
			return out
		}
	}
	t.Fatalf("no distance with %d paths", n)
	return nil
}

func plan(g topology.Graph, ms []model.PathMeasurement) []analyzer.LatencyRecheckPlan {
	return analyzer.PlanLatencyRecheck(g, ms, model.ModeIdle, config.Defaults(), baseline.Overlay{}, 0).Plans
}

// The plan takes exactly the sibling verdicts, and reads each against
// siblings on other RNICs, one RNIC after another.
func TestPlanLatencyRecheckSiblings(t *testing.T) {
	g := topology.Fig2()
	ms := classPaths(t, g, 5)
	ms[0].SmallLatNsP50 = 3000
	got := plan(g, ms)
	if len(got) != 1 || got[0].Path != (analyzer.PathRef{RNIC: ms[0].RNIC, Endpoint: ms[0].Endpoint}) {
		t.Fatalf("plan %+v, want only %s->%s", got, ms[0].RNIC, ms[0].Endpoint)
	}
	sibs := got[0].Siblings
	if len(sibs) != analyzer.LatencyRecheckReads {
		t.Fatalf("siblings %v, want %d", sibs, analyzer.LatencyRecheckReads)
	}
	seen := map[string]bool{}
	for _, s := range sibs {
		if s.RNIC == ms[0].RNIC {
			t.Errorf("sibling %s on the path's own RNIC", s)
		}
		if seen[s.RNIC] && len(seen) < len(g.RNICs)-1 {
			t.Errorf("siblings %v repeat an RNIC before using every other one", sibs)
		}
		seen[s.RNIC] = true
	}

	// Nothing flagged, nothing planned; busy runs plan nothing.
	if got := plan(g, classPaths(t, g, 5)); len(got) != 0 {
		t.Errorf("healthy class planned %+v", got)
	}
	if got := analyzer.PlanLatencyRecheck(g, ms, model.ModeBusy, config.Defaults(), baseline.Overlay{}, 0).Plans; len(got) != 0 {
		t.Errorf("busy run planned %+v", got)
	}
}

// A class with more than half of its paths flagged is not rechecked: the rise
// is class-wide. Half or less is. The flagged paths sit on RNICs of different
// NUMA nodes, so the locality rule leaves them to the recheck.
func TestPlanLatencyRecheckClassWideGuard(t *testing.T) {
	g := topology.Fig2()
	ms := classPaths(t, g, 5)
	for _, i := range []int{0, 2, 3} {
		ms[i].SmallLatNsP50 = 3000
	}
	judged := analyzer.ClassifyLatency(g, ms, model.ModeIdle, config.Defaults(), baseline.Overlay{})
	flagged := 0
	for _, m := range judged {
		if m.LatencyStatus == model.LatencyAbnormal {
			flagged++
		}
	}
	if flagged != 3 {
		t.Fatalf("precondition: %d of 5 flagged, want 3", flagged)
	}
	if got := plan(g, ms); len(got) != 0 {
		t.Errorf("class with 3 of 5 flagged planned %+v", got)
	}
	ms[3].SmallLatNsP50 = 1000
	if got := plan(g, ms); len(got) != 2 {
		t.Errorf("class with 2 of 5 flagged planned %+v, want both", got)
	}
}

// Analyze applies the recheck to the latency verdict alone: a cleared path is
// normal with recheck_cleared and leaves no cause; a confirmed one keeps its
// cause; a user-baseline verdict ignores a recheck.
func TestAnalyzeAppliesLatencyRecheck(t *testing.T) {
	g := topology.Fig2()
	ms := classPaths(t, g, 5)
	ms[0].SmallLatNsP50 = 3000
	key := ms[0].RNIC + "->" + ms[0].Endpoint
	// The path read abnormal in the runs before, so a verdict that stands is
	// reported.
	in := analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: ms, LatencyHits: map[string]int{key: analyzer.LatencyRuns - 1}}
	base := analyzer.Analyze(in)
	if len(latencyCauses(base.Causes)) != 1 {
		t.Fatalf("precondition: want one latency cause, got %+v", base.Causes)
	}

	in.LatencyRecheck = map[string]model.LatencyRecheck{key: analyzer.JudgeLatencyRecheck([]uint64{1000, 1010, 990}, []uint64{1000, 1000, 1000}, in.Cfg.AbnormalTh)}
	r := analyzer.Analyze(in)
	m := r.Paths[0]
	if m.LatencyStatus != model.LatencyNormal || m.LatencyReason != model.LatencyRecheckCleared || m.LatencyRecheck == nil || m.LatencyRecheck.Confirmed {
		t.Fatalf("cleared path = %+v", m)
	}
	if m.SmallLatNsP50 != 3000 || m.LatencyRefNs != base.Paths[0].LatencyRefNs {
		t.Errorf("cleared path lost its sweep reading or reference: %+v", m)
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("cleared path still reported: %+v", lc)
	}
	if bandwidthOf(r).paths[key] != bandwidthOf(base).paths[key] {
		t.Error("recheck changed the bandwidth verdict")
	}

	in.LatencyRecheck = map[string]model.LatencyRecheck{key: analyzer.JudgeLatencyRecheck([]uint64{3000, 3000, 3000}, []uint64{1000, 1000, 1000}, in.Cfg.AbnormalTh)}
	r = analyzer.Analyze(in)
	if m := r.Paths[0]; m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings || !m.LatencyRecheck.Confirmed {
		t.Fatalf("confirmed path = %+v", m)
	}
	lc := latencyCauses(r.Causes)
	if len(lc) != 1 || !strings.Contains(lc[0].Evidence, "recheck 3000ns vs 1000ns") {
		t.Errorf("confirmed causes %+v, want one whose evidence carries the recheck", lc)
	}

	// Against a user baseline the verdict stands whatever a recheck says.
	d, _ := g.Path(ms[0].RNIC, ms[0].Endpoint)
	in.Bases = baseline.Uniform(map[model.Distance]baseline.PathBaseline{d.Distance: {LatencyNs: 1000}})
	in.LatencyRecheck = map[string]model.LatencyRecheck{key: {PathNsP50: 1000, SiblingNsP50: 1000}}
	if m := analyzer.Analyze(in).Paths[0]; m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyByBaseline || m.LatencyRecheck != nil {
		t.Errorf("user-baseline path = %+v, want abnormal/user_baseline untouched", m)
	}
}

// The recheck applies the sweep's sibling rule to the re-reads: above
// Abnormal_th and the floor, or not.
func TestJudgeLatencyRecheck(t *testing.T) {
	th := config.Defaults().AbnormalTh
	for _, tc := range []struct {
		path, sib []uint64
		want      bool
	}{
		{[]uint64{2000, 2000, 2000}, []uint64{1000, 1000, 1000}, true},
		// Past Abnormal_th but under the floor.
		{[]uint64{1400, 1400, 1400}, []uint64{1000, 1000, 1000}, false},
		// One slow re-read is outvoted by the median.
		{[]uint64{3000, 1000, 1000}, []uint64{1000, 1000, 1000}, false},
		// Siblings that read slow in the same window clear the path.
		{[]uint64{2000, 2000, 2000}, []uint64{2000, 1900, 2100}, false},
	} {
		got := analyzer.JudgeLatencyRecheck(tc.path, tc.sib, th)
		if got.Confirmed != tc.want {
			t.Errorf("path %v vs siblings %v: confirmed %v, want %v", tc.path, tc.sib, got.Confirmed, tc.want)
		}
	}
}

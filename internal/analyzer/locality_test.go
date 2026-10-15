package analyzer_test

import (
	"reflect"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// flatPaths returns every healthy simulator path of g reading 1000 ns, with
// the paths of the given RNICs and to the given endpoints reading 2000 ns —
// past --abnormal-th and the sibling floor.
func flatPaths(t *testing.T, g topology.Graph, slow ...string) []model.PathMeasurement {
	t.Helper()
	ms := simMeasurements(t, g, "")
	for i := range ms {
		ms[i].SmallLatNsP50 = 1000
		for _, s := range slow {
			if ms[i].RNIC == s || ms[i].Endpoint == s {
				ms[i].SmallLatNsP50 = 2000
			}
		}
	}
	return ms
}

// analyzeLatency analyzes ms as consecutive idle runs that read the same,
// so a sibling verdict that survives has persisted long enough to be reported.
func analyzeLatency(g topology.Graph, ms []model.PathMeasurement) analyzer.Result {
	return analyzeIdle(g, ms)
}

// sweepFlagged asserts that the sweep alone flags every path from rnics
// against its siblings, so a later normal verdict is the locality check's.
func sweepFlagged(t *testing.T, g topology.Graph, ms []model.PathMeasurement, rnics ...string) {
	t.Helper()
	for _, m := range analyzer.ClassifyLatency(g, ms, model.ModeIdle, config.Defaults(), baseline.Overlay{}) {
		for _, r := range rnics {
			if m.RNIC == r && (m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings) {
				t.Fatalf("precondition: sweep judged %s->%s %s/%s, want abnormal/siblings", m.RNIC, m.Endpoint, m.LatencyStatus, m.LatencyReason)
			}
		}
	}
}

// Both RNICs of one NUMA node read high while the other node's read normal:
// the rise is the node's, so every path is cleared with locality_shift, the
// reference is recorded, and no cause or bandwidth verdict changes.
func TestLocalityShiftClearsNodeWideRise(t *testing.T) {
	g := topology.Fig2()
	ms := flatPaths(t, g, "rnic0", "rnic1")
	sweepFlagged(t, g, ms, "rnic0", "rnic1")
	r := analyzeLatency(g, ms)
	for _, m := range r.Paths {
		if m.RNIC != "rnic0" && m.RNIC != "rnic1" {
			if m.LatencyStatus != model.LatencyNormal || m.LatencyReason != model.LatencyBySiblings || m.LatencyLocality != nil {
				t.Errorf("%s->%s = %s/%s %+v, want normal/siblings", m.RNIC, m.Endpoint, m.LatencyStatus, m.LatencyReason, m.LatencyLocality)
			}
			continue
		}
		mate := "rnic1"
		if m.RNIC == "rnic1" {
			mate = "rnic0"
		}
		want := &model.LatencyLocality{NUMA: 0, RefNs: 2000, RNICs: []string{mate}}
		if m.LatencyStatus != model.LatencyNormal || m.LatencyReason != model.LatencyLocalityShift || !reflect.DeepEqual(m.LatencyLocality, want) {
			t.Errorf("%s->%s = %s/%s %+v, want normal/locality_shift %+v", m.RNIC, m.Endpoint, m.LatencyStatus, m.LatencyReason, m.LatencyLocality, want)
		}
		if m.SmallLatNsP50 != 2000 || m.LatencyRefNs != 1000 {
			t.Errorf("%s->%s lost its sweep reading or sibling reference: %+v", m.RNIC, m.Endpoint, m)
		}
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("node-wide rise still reported: %+v", lc)
	}
	if !reflect.DeepEqual(bandwidthOf(r), bandwidthOf(analyzeLatency(g, flatPaths(t, g)))) {
		t.Error("locality check changed the bandwidth verdict")
	}
}

// One RNIC of a node reads high while its node-mate reads normal: that is the
// RNIC's own rise, and the verdict and its cause stand.
func TestLocalityShiftKeepsSingleRNICRise(t *testing.T) {
	g := topology.Fig2()
	ms := flatPaths(t, g, "rnic0")
	sweepFlagged(t, g, ms, "rnic0")
	r := analyzeLatency(g, ms)
	for _, m := range r.Paths {
		if m.RNIC == "rnic0" && (m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings || m.LatencyLocality != nil) {
			t.Errorf("%s->%s = %s/%s, want abnormal/siblings", m.RNIC, m.Endpoint, m.LatencyStatus, m.LatencyReason)
		}
	}
	if lc := latencyCauses(r.Causes); len(lc) != 1 || !reflect.DeepEqual(lc[0].RNICs, []string{"rnic0"}) {
		t.Errorf("causes %+v, want one naming rnic0", lc)
	}
}

// Without another RNIC on the node — one RNIC per NUMA node, or a node that
// is not known — there is nothing to compare with, and the verdict stands.
func TestLocalityShiftNeedsANodeMate(t *testing.T) {
	unknown := topology.Fig2()
	unknown.RNICs[0].NUMA, unknown.RNICs[1].NUMA = -1, -1
	for name, g := range map[string]topology.Graph{
		"one RNIC per node": topology.Fig2SNC(),
		"unknown NUMA":      unknown,
	} {
		ms := flatPaths(t, g, "rnic0", "rnic1")
		sweepFlagged(t, g, ms, "rnic0")
		r := analyzeLatency(g, ms)
		for _, m := range r.Paths {
			if m.LatencyReason == model.LatencyLocalityShift || m.LatencyLocality != nil {
				t.Errorf("%s: %s->%s cleared by locality: %+v", name, m.RNIC, m.Endpoint, m)
			}
			if m.RNIC == "rnic0" && m.LatencyStatus != model.LatencyAbnormal {
				t.Errorf("%s: %s->%s = %s, want abnormal", name, m.RNIC, m.Endpoint, m.LatencyStatus)
			}
		}
		if len(latencyCauses(r.Causes)) == 0 {
			t.Errorf("%s: verdicts kept but no cause", name)
		}
	}
}

// A slow endpoint raises its paths from every RNIC. The node's RNICs often
// reach it at one distance (a node's memory), so they agree with each other,
// but the paths from the other node show the endpoint is the cause: kept.
func TestLocalityShiftKeepsEndpointRise(t *testing.T) {
	g := topology.Fig2()
	r := analyzeLatency(g, flatPaths(t, g, "mem0"))
	for _, m := range r.Paths {
		if m.Endpoint == "mem0" && (m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings) {
			t.Errorf("%s->mem0 = %s/%s, want abnormal/siblings", m.RNIC, m.LatencyStatus, m.LatencyReason)
		}
	}
	if lc := latencyCauses(r.Causes); len(lc) != 1 || !reflect.DeepEqual(lc[0].Endpoints, []string{"mem0"}) {
		t.Errorf("causes %+v, want one naming mem0", lc)
	}
}

// A verdict against a user baseline is never touched, even when the node's
// RNICs all read high.
func TestLocalityShiftLeavesUserBaseline(t *testing.T) {
	g := topology.Fig2()
	in := analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: flatPaths(t, g, "rnic0", "rnic1")}
	lat := map[model.Distance]baseline.PathBaseline{}
	for _, d := range model.Distances() {
		lat[d] = baseline.PathBaseline{LatencyNs: 1000}
	}
	in.Bases = baseline.Uniform(lat)
	for _, m := range analyzer.Analyze(in).Paths {
		if (m.RNIC == "rnic0" || m.RNIC == "rnic1") && (m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyByBaseline || m.LatencyLocality != nil) {
			t.Errorf("%s->%s = %s/%s, want abnormal/user_baseline", m.RNIC, m.Endpoint, m.LatencyStatus, m.LatencyReason)
		}
	}
}

// A path the recheck cleared keeps recheck_cleared and is not judged again; a
// path the recheck confirmed is still cleared by the node-wide rise, and
// keeps its recheck in the report.
func TestLocalityShiftAfterRecheck(t *testing.T) {
	g := topology.Fig2()
	ms := flatPaths(t, g, "rnic0", "rnic1")
	cfg := config.Defaults()
	cleared, confirmed := ms[0], ms[1]
	in := analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: ms,
		LatencyRecheck: map[string]model.LatencyRecheck{
			cleared.RNIC + "->" + cleared.Endpoint:     analyzer.JudgeLatencyRecheck([]uint64{1000, 1000, 1000}, []uint64{1000, 1000, 1000}, cfg),
			confirmed.RNIC + "->" + confirmed.Endpoint: analyzer.JudgeLatencyRecheck([]uint64{2000, 2000, 2000}, []uint64{1000, 1000, 1000}, cfg),
		}}
	r := analyzer.Analyze(in)
	if m := r.Paths[0]; m.RNIC != "rnic0" || m.LatencyReason != model.LatencyRecheckCleared || m.LatencyLocality != nil {
		t.Errorf("recheck-cleared path = %+v, want recheck_cleared without a locality reference", m)
	}
	if m := r.Paths[1]; m.RNIC != "rnic0" || m.LatencyStatus != model.LatencyNormal || m.LatencyReason != model.LatencyLocalityShift ||
		m.LatencyLocality == nil || m.LatencyRecheck == nil || !m.LatencyRecheck.Confirmed {
		t.Errorf("recheck-confirmed path = %+v, want locality_shift keeping its recheck", m)
	}
}

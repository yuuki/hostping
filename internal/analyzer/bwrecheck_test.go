package analyzer_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// fig2Readings reads every path of Fig.2 from the healthy simulator, then
// scales the paths named in low ("rnic->endpoint") to 0.6 of their level, past
// --abnormal-th.
func fig2Readings(t *testing.T, low ...string) (topology.Graph, []model.PathMeasurement) {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	var ms []model.PathMeasurement
	for _, p := range g.Paths {
		m, err := w.Loopback(p.RNIC, p.Endpoint, config.Defaults().SmallMsgBytes, config.Defaults().LargeMsgBytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range low {
			if l == p.RNIC+"->"+p.Endpoint {
				m.BandwidthBPS = m.BandwidthBPS * 6 / 10
			}
		}
		ms = append(ms, m)
	}
	return g, ms
}

func planBW(g topology.Graph, ms []model.PathMeasurement, mode model.RunMode, busy map[string]bool) []analyzer.BWRecheckPlan {
	return analyzer.PlanBWRecheck(g, ms, mode, busy, config.Defaults(), baseline.Overlay{})
}

func planSummary(plans []analyzer.BWRecheckPlan) []string {
	var out []string
	for _, p := range plans {
		var refs []string
		for _, r := range p.Refs {
			refs = append(refs, r.String())
		}
		out = append(out, p.Group+" "+p.Path.String()+" ["+strings.Join(refs, ",")+"]")
	}
	return out
}

func TestJudgeBWRecheck(t *testing.T) {
	const base = 200 * gbps
	cases := []struct {
		name       string
		path, refs uint64
		want       model.BWRecheckOutcome
	}{
		{"path low against its references", 100 * gbps, 200 * gbps, model.BWRecheckConfirmed},
		{"path level with references at the baseline", 190 * gbps, 200 * gbps, model.BWRecheckCleared},
		{"path level with references, both below the baseline", 120 * gbps, 125 * gbps, model.BWRecheckHostShift},
		// Boundary: the references' median just clears the
		// baseline threshold (0.8 × 200 = 160) and the path just misses it,
		// but the path is within --abnormal-th of the references: one host
		// state on both sides of the line, not a fault of the path's own.
		{"references just above the baseline threshold, path just below and level with them", 155 * gbps, 165 * gbps, model.BWRecheckHostShift},
		{"path below the baseline, within th of references well above it", 155 * gbps, 190 * gbps, model.BWRecheckHostShift},
		{"path just past th below references that meet the baseline", 150 * gbps, 190 * gbps, model.BWRecheckConfirmed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := analyzer.JudgeBWRecheck([]uint64{tc.path, tc.path, tc.path}, []uint64{tc.refs, tc.refs}, base, config.Defaults().AbnormalTh)
			if rc.Outcome != tc.want || rc.PathBPS != tc.path || rc.RefBPS != tc.refs || rc.BaselineBPS != base {
				t.Errorf("JudgeBWRecheck = %+v, want outcome %s", rc, tc.want)
			}
		})
	}
}

// The memory paths of two RNICs drop: one group per RNIC and distance, its
// lowest path re-read against the same distance on RNICs that met the
// baseline, one per RNIC.
func TestPlanBWRecheckGroups(t *testing.T) {
	g, ms := fig2Readings(t, "rnic0->mem0", "rnic0->mem1", "rnic1->mem0", "rnic1->mem1")
	got := planSummary(planBW(g, ms, model.ModeIdle, nil))
	want := []string{
		"rnic0/local_mem rnic0->mem0 [rnic2->mem1,rnic3->mem1]",
		"rnic0/remote_mem rnic0->mem1 [rnic2->mem0,rnic3->mem0]",
		"rnic1/local_mem rnic1->mem0 [rnic2->mem1,rnic3->mem1]",
		"rnic1/remote_mem rnic1->mem1 [rnic2->mem0,rnic3->mem0]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plans = %q, want %q", got, want)
	}
}

// A reference to the representative's own endpoint is read first.
func TestPlanBWRecheckPrefersSameEndpoint(t *testing.T) {
	g, ms := fig2Readings(t, "rnic0->mem0")
	got := planSummary(planBW(g, ms, model.ModeIdle, nil))
	want := []string{"rnic0/local_mem rnic0->mem0 [rnic1->mem0,rnic2->mem1]"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plans = %q, want %q", got, want)
	}
}

// In a busy run, a loaded RNIC's paths are judged against its siblings, so
// they are neither planned nor read as references.
func TestPlanBWRecheckBusyRun(t *testing.T) {
	g, ms := fig2Readings(t, "rnic0->mem0", "rnic3->mem1")
	busy := map[string]bool{"rnic3": true}
	got := planSummary(planBW(g, ms, model.ModeBusy, busy))
	want := []string{"rnic0/local_mem rnic0->mem0 [rnic1->mem0,rnic2->mem1]"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plans = %q, want %q", got, want)
	}
}

// A class that dropped on every RNIC has nothing to read against.
func TestPlanBWRecheckNoReference(t *testing.T) {
	g, ms := fig2Readings(t, "rnic0->mem0", "rnic1->mem0", "rnic2->mem1", "rnic3->mem1")
	if got := planBW(g, ms, model.ModeIdle, nil); len(got) != 0 {
		t.Errorf("plans = %q, want none", planSummary(got))
	}
}

// Each outcome reaches every path of its group: confirmed stays abnormal,
// host_shift is uncertain with host_shift, and cleared is judged as a path
// that met its baseline — uncertain non_affinitive for a cross-root GPU, which
// a baseline alone never makes normal.
func TestAnalyzeAppliesBWRecheck(t *testing.T) {
	g, ms := fig2Readings(t, "rnic0->mem0", "rnic1->mem0", "rnic0->gpu2", "rnic0->gpu3")
	healthy := map[string]uint64{}
	for _, m := range ms {
		healthy[m.RNIC+"->"+m.Endpoint] = m.BandwidthBPS
	}
	cfg := config.Defaults()
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg, Mode: model.ModeIdle, Measurements: ms,
		BWRecheck: map[string]model.BandwidthRecheck{
			"rnic0/local_mem":  {Path: "rnic0->mem0", Outcome: model.BWRecheckConfirmed},
			"rnic1/local_mem":  {Path: "rnic1->mem0", Outcome: model.BWRecheckHostShift},
			"rnic0/cross_root": {Path: "rnic0->gpu2", PathBPS: 2 * healthy["rnic2->gpu0"], Outcome: model.BWRecheckCleared},
		},
	})
	want := map[string]string{
		"rnic0->mem0": "abnormal/none",
		"rnic1->mem0": "uncertain/host_shift",
		"rnic0->gpu2": "uncertain/non_affinitive",
		"rnic0->gpu3": "uncertain/non_affinitive",
	}
	for _, m := range res.Paths {
		k := m.RNIC + "->" + m.Endpoint
		w, ok := want[k]
		if !ok {
			if m.BWRecheck != nil {
				t.Errorf("%s carries a recheck it was not in", k)
			}
			continue
		}
		if got := m.Status.String() + "/" + m.UncertainReason.String(); got != w || m.BWRecheck == nil {
			t.Errorf("%s = %s recheck %+v, want %s with the recheck", k, got, m.BWRecheck, w)
		}
	}
}

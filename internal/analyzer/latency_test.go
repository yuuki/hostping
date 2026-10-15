package analyzer_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func latencyCauses(causes []model.RootCause) []model.RootCause {
	var out []model.RootCause
	for _, c := range causes {
		if c.Code == model.CausePathLatencyAbnormal {
			out = append(out, c)
		}
	}
	return out
}

func withoutLatencyCauses(causes []model.RootCause) []model.RootCause {
	var out []model.RootCause
	for _, c := range causes {
		if c.Code != model.CausePathLatencyAbnormal {
			out = append(out, c)
		}
	}
	return out
}

// bandwidthView is everything the bandwidth side of the analyzer decides: the
// path statuses and reasons, and Algorithm 1's links.
type bandwidthView struct {
	paths map[string][2]string
	links []model.LinkState
}

func bandwidthOf(r analyzer.Result) bandwidthView {
	v := bandwidthView{paths: map[string][2]string{}, links: r.Links}
	for _, p := range r.Paths {
		v.paths[p.RNIC+"->"+p.Endpoint] = [2]string{p.Status.String(), p.UncertainReason.String()}
	}
	return v
}

// A hop that adds latency without costing bandwidth (Eq.1 cancels host latency
// out of the bandwidth) is invisible to the bandwidth status. Every path to
// the GPU behind it rises well past its same-distance siblings, so each is
// latency-abnormal, and the four collapse into one cause naming the GPU.
func TestLatencySiblingOutlier(t *testing.T) {
	g := topology.Fig2()
	healthy := analyzeIdle(g, simMeasurements(t, g, ""))
	got := analyzeIdle(g, simMeasurements(t, g, "lat:pcie.gpu3"))

	for _, p := range got.Paths {
		want := model.LatencyNormal
		if p.Endpoint == "gpu3" {
			want = model.LatencyAbnormal
		}
		if p.LatencyStatus != want || p.LatencyReason != model.LatencyBySiblings {
			t.Errorf("%s->%s latency %s/%s, want %s/siblings", p.RNIC, p.Endpoint, p.LatencyStatus, p.LatencyReason, want)
		}
		if p.LatencyRefNs == 0 {
			t.Errorf("%s->%s judged without a reference", p.RNIC, p.Endpoint)
		}
	}

	lc := latencyCauses(got.Causes)
	if len(lc) != 1 {
		t.Fatalf("want 1 %s cause, got %+v", model.CausePathLatencyAbnormal, lc)
	}
	c := lc[0]
	if !reflect.DeepEqual(c.Endpoints, []string{"gpu3"}) ||
		!reflect.DeepEqual(c.RNICs, []string{"rnic0", "rnic1", "rnic2", "rnic3"}) ||
		c.Confidence != "low" {
		t.Errorf("cause = %+v, want gpu3 from every RNIC at low confidence", c)
	}

	// The bandwidth side and every other cause are exactly what a healthy
	// host gets: latency never reaches Algorithm 1.
	if !reflect.DeepEqual(bandwidthOf(got), bandwidthOf(healthy)) {
		t.Errorf("bandwidth verdicts changed:\n got %+v\nwant %+v", bandwidthOf(got), bandwidthOf(healthy))
	}
	if !reflect.DeepEqual(withoutLatencyCauses(got.Causes), healthy.Causes) {
		t.Errorf("other causes changed: got %+v, want %+v", withoutLatencyCauses(got.Causes), healthy.Causes)
	}
}

// A slow RNIC raises every path from it; those paths spread over several
// endpoints and distances, and are reported as one cause for that RNIC.
func TestLatencySlowRNICGroupedPerRNIC(t *testing.T) {
	g := topology.Fig2()
	lc := latencyCauses(analyzeIdle(g, simMeasurements(t, g, "lat:pcie.rnic2")).Causes)
	if len(lc) != 1 {
		t.Fatalf("want 1 %s cause, got %+v", model.CausePathLatencyAbnormal, lc)
	}
	if !reflect.DeepEqual(lc[0].RNICs, []string{"rnic2"}) || len(lc[0].Endpoints) != len(g.Endpoints) {
		t.Errorf("cause = %+v, want rnic2 and all %d endpoints", lc[0], len(g.Endpoints))
	}
}

// Uniform latencies within each distance judge every path normal and
// produce no cause, even though the distances differ from each other.
func TestLatencyUniformNoCause(t *testing.T) {
	g := topology.Fig2()
	r := analyzeIdle(g, simMeasurements(t, g, ""))
	for _, p := range r.Paths {
		if p.LatencyStatus != model.LatencyNormal {
			t.Errorf("%s->%s latency %s/%s, want normal", p.RNIC, p.Endpoint, p.LatencyStatus, p.LatencyReason)
		}
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("healthy host got %+v", lc)
	}
}

// A user latency baseline wins over the siblings. Setting local_mem below
// what the host measures makes every local_mem path abnormal against it,
// reported once for the distance at medium confidence; bandwidth is untouched.
func TestLatencyUserBaselineExceeded(t *testing.T) {
	g := topology.Fig2()
	ms := simMeasurements(t, g, "")
	bases := map[model.Distance]baseline.PathBaseline{model.DistanceLocalMem: {LatencyNs: 500}}
	r := analyzer.Analyze(analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: ms, Bases: baseline.Uniform(bases)})
	healthy := analyzeIdle(g, ms)

	for _, p := range r.Paths {
		path, _ := g.Path(p.RNIC, p.Endpoint)
		if path.Distance != model.DistanceLocalMem {
			if p.LatencyReason != model.LatencyBySiblings {
				t.Errorf("%s->%s reason %s, want siblings", p.RNIC, p.Endpoint, p.LatencyReason)
			}
			continue
		}
		if p.LatencyStatus != model.LatencyAbnormal || p.LatencyReason != model.LatencyByBaseline || p.LatencyRefNs != 500 {
			t.Errorf("%s->%s latency %s/%s ref=%d, want abnormal/user_baseline ref=500", p.RNIC, p.Endpoint, p.LatencyStatus, p.LatencyReason, p.LatencyRefNs)
		}
	}
	lc := latencyCauses(r.Causes)
	if len(lc) != 1 {
		t.Fatalf("want 1 %s cause, got %+v", model.CausePathLatencyAbnormal, lc)
	}
	if lc[0].Confidence != "medium" || len(lc[0].RNICs) != 4 || !reflect.DeepEqual(lc[0].Endpoints, []string{"mem0", "mem1"}) {
		t.Errorf("cause = %+v, want every local_mem path at medium confidence", lc[0])
	}
	if !reflect.DeepEqual(bandwidthOf(r), bandwidthOf(healthy)) {
		t.Errorf("a latency baseline changed bandwidth verdicts")
	}
}

// withLat returns the healthy Fig.2 measurements with rnic0->mem0's
// small-message latency (local_mem, whose siblings read 1000 ns) set to lat.
func withLat(t *testing.T, g topology.Graph, lat uint64) []model.PathMeasurement {
	t.Helper()
	ms := simMeasurements(t, g, "")
	for i := range ms {
		if ms[i].RNIC == "rnic0" && ms[i].Endpoint == "mem0" {
			if ms[i].SmallLatNsP50 != 1000 {
				t.Fatalf("setup: rnic0->mem0 reads %d ns, want 1000", ms[i].SmallLatNsP50)
			}
			ms[i].SmallLatNsP50 = lat
		}
	}
	return ms
}

func latencyOf(t *testing.T, r analyzer.Result, rnic, ep string) model.PathMeasurement {
	t.Helper()
	for _, p := range r.Paths {
		if p.RNIC == rnic && p.Endpoint == ep {
			return p
		}
	}
	t.Fatalf("no path %s->%s", rnic, ep)
	return model.PathMeasurement{}
}

// A sibling reference is only as steady as the host's paths are alike, and a
// small-message p50 of a µs puts Abnormal_th at a few hundred ns. A path 25%
// above its sibling median but less than the absolute floor above it stays
// normal; one past both is abnormal.
func TestLatencySiblingFloor(t *testing.T) {
	g := topology.Fig2()
	for _, tc := range []struct {
		lat  uint64
		want model.LatencyStatus
	}{
		{1250, model.LatencyNormal},   // +25%, +250 ns: under the floor
		{1600, model.LatencyAbnormal}, // +60%, +600 ns: past both
	} {
		r := analyzeIdle(g, withLat(t, g, tc.lat))
		p := latencyOf(t, r, "rnic0", "mem0")
		if p.LatencyStatus != tc.want || p.LatencyReason != model.LatencyBySiblings || p.LatencyRefNs != 1000 {
			t.Errorf("%d ns: latency %s/%s ref=%d, want %s/siblings ref=1000", tc.lat, p.LatencyStatus, p.LatencyReason, p.LatencyRefNs, tc.want)
		}
		if got := len(latencyCauses(r.Causes)); (tc.want == model.LatencyAbnormal) != (got == 1) {
			t.Errorf("%d ns: %d latency causes", tc.lat, got)
		}
	}
}

// --latency-floor moves the floor on a platform whose healthy paths jitter by
// more than 500 ns between runs: a path +60% and +600 ns above its siblings is
// abnormal under the default but normal under a 1 µs floor, and one past the
// raised floor is still abnormal. A zero floor leaves Abnormal_th alone.
func TestLatencySiblingFloorConfigurable(t *testing.T) {
	g := topology.Fig2()
	for _, tc := range []struct {
		floor time.Duration
		lat   uint64
		want  model.LatencyStatus
	}{
		{time.Microsecond, 1600, model.LatencyNormal},
		{time.Microsecond, 2100, model.LatencyAbnormal},
		{0, 1250, model.LatencyAbnormal},
		{0, 1150, model.LatencyNormal},
	} {
		cfg := config.Defaults()
		cfg.LatencyFloor = tc.floor
		r := analyzer.Analyze(analyzer.Input{Graph: g, Cfg: cfg, Mode: model.ModeIdle, Measurements: withLat(t, g, tc.lat)})
		if p := latencyOf(t, r, "rnic0", "mem0"); p.LatencyStatus != tc.want {
			t.Errorf("floor %v, %d ns: latency %s/%s, want %s", tc.floor, tc.lat, p.LatencyStatus, p.LatencyReason, tc.want)
		}
	}
}

// The floor guards the sibling reference only. Against a latency the operator
// measured and set, Abnormal_th alone decides, so the same +250 ns is abnormal.
func TestLatencyUserBaselineHasNoFloor(t *testing.T) {
	g := topology.Fig2()
	bases := map[model.Distance]baseline.PathBaseline{model.DistanceLocalMem: {LatencyNs: 1000}}
	r := analyzer.Analyze(analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: withLat(t, g, 1250), Bases: baseline.Uniform(bases)})
	p := latencyOf(t, r, "rnic0", "mem0")
	if p.LatencyStatus != model.LatencyAbnormal || p.LatencyReason != model.LatencyByBaseline {
		t.Errorf("latency %s/%s, want abnormal/user_baseline", p.LatencyStatus, p.LatencyReason)
	}
}

// With no latency baseline, a distance needs three other paths to take a
// median from. Fig.2 has four local_mem paths; with one of them missing, the
// rest have only two siblings each and stay unknown, however far apart.
func TestLatencyTooFewSiblings(t *testing.T) {
	g := topology.Fig2()
	var ms []model.PathMeasurement
	for _, m := range simMeasurements(t, g, "lat:pcie.rnic0") {
		p, _ := g.Path(m.RNIC, m.Endpoint)
		if p.Distance == model.DistanceLocalMem && m.RNIC != "rnic3" {
			ms = append(ms, m)
		}
	}
	if len(ms) != 3 {
		t.Fatalf("setup: %d local_mem paths, want 3", len(ms))
	}
	r := analyzeIdle(g, ms)
	for _, p := range r.Paths {
		if p.LatencyStatus != model.LatencyUnknown || p.LatencyReason != model.LatencyFewSiblings || p.LatencyRefNs != 0 {
			t.Errorf("%s->%s latency %s/%s ref=%d, want unknown/few_siblings", p.RNIC, p.Endpoint, p.LatencyStatus, p.LatencyReason, p.LatencyRefNs)
		}
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("unjudged paths produced %+v", lc)
	}
}

// Busy mode judges no latency: service traffic inflates Tproc on a loaded
// RNIC. Even a real latency fault and a user baseline produce nothing.
func TestLatencyBusyModeNoVerdict(t *testing.T) {
	g := topology.Fig2()
	bases := map[model.Distance]baseline.PathBaseline{model.DistanceSameSwitch: {LatencyNs: 100}}
	r := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeBusy,
		BusyRNICs:    map[string]bool{"rnic1": true},
		Measurements: simMeasurements(t, g, "lat:pcie.gpu3"),
		Bases:        baseline.Uniform(bases),
	})
	for _, p := range r.Paths {
		if p.LatencyStatus != model.LatencyUnknown || p.LatencyReason != model.LatencyBusy {
			t.Errorf("%s->%s latency %s/%s, want unknown/busy", p.RNIC, p.Endpoint, p.LatencyStatus, p.LatencyReason)
		}
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("busy run produced %+v", lc)
	}
}

// ATS off sends rnic0's GPUDirect traffic through the root complex, which is
// what raises its GPU latencies; ats_disabled already names that, so those
// paths get no second cause even where their bandwidth stays normal. The
// same RNIC's memory paths are not GPUDirect, so latency there is still
// reported.
func TestLatencyExplainedByATSNotRepeated(t *testing.T) {
	g := topology.Fig2()
	r := runFault(t, "ats:rnic0;lat:pcie.rnic0", model.ModeIdle)

	ats, gpuLatAbn := hasCause(r, model.CauseATSDisabled), 0
	for _, p := range r.Paths {
		e, _ := g.Endpoint(p.Endpoint)
		if p.RNIC == "rnic0" && e.Kind == model.EndpointGPU && p.LatencyStatus == model.LatencyAbnormal && p.Status != model.PathAbnormal {
			gpuLatAbn++
		}
	}
	if !ats || gpuLatAbn == 0 {
		t.Fatalf("setup: ats cause %v, latency-only GPU paths %d; want both", ats, gpuLatAbn)
	}

	lc := latencyCauses(r.Causes)
	if len(lc) != 1 {
		t.Fatalf("want 1 %s cause, got %+v", model.CausePathLatencyAbnormal, lc)
	}
	if !reflect.DeepEqual(lc[0].RNICs, []string{"rnic0"}) || !reflect.DeepEqual(lc[0].Endpoints, []string{"mem0", "mem1"}) {
		t.Errorf("cause = %+v, want rnic0 memory paths only", lc[0])
	}
}

// A path whose bandwidth is already abnormal belongs to the bandwidth causes;
// its latency adds no second one.
func TestLatencyOnBandwidthAbnormalPathNotRepeated(t *testing.T) {
	g := topology.Fig2()
	r := analyzeIdle(g, simMeasurements(t, g, "gpu_pcie:gpu3;lat:pcie.gpu3"))
	for _, p := range r.Paths {
		if p.Endpoint == "gpu3" && (p.Status != model.PathAbnormal || p.LatencyStatus != model.LatencyAbnormal) {
			t.Fatalf("setup: %s->gpu3 %s/%s, want abnormal on both", p.RNIC, p.Status, p.LatencyStatus)
		}
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("bandwidth-abnormal paths repeated as %+v", lc)
	}
}

// ClassifyLatency writes only the latency fields.
func TestClassifyLatencyLeavesBandwidthFields(t *testing.T) {
	g := topology.Fig2()
	in := analyzer.ClassifyPaths(g, simMeasurements(t, g, "lat:pcie.gpu3;rnic_pcie:rnic1"), model.ModeIdle, nil, config.Defaults(), baseline.Overlay{})
	out := analyzer.ClassifyLatency(g, in, model.ModeIdle, config.Defaults(), baseline.Overlay{})
	for i := range out {
		got := out[i]
		got.LatencyStatus, got.LatencyReason, got.LatencyRefNs = in[i].LatencyStatus, in[i].LatencyReason, in[i].LatencyRefNs
		if !reflect.DeepEqual(got, in[i]) {
			t.Errorf("path %d changed beyond latency: %+v -> %+v", i, in[i], out[i])
		}
	}
}

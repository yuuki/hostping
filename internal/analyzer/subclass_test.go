package analyzer_test

import (
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// In Fig.2, rnic0->gpu2/gpu3 and rnic1->gpu0/gpu1 cross root ports rp.0 and
// rp.1; rnic2->gpu6/gpu7 and rnic3->gpu4/gpu5 cross rp.2 and rp.3. A learned
// sub-class lists {rp.0, rp.1} as the fast pair of cross_root.
func fastCrossRoot(bw, lat, fastBW, fastLat uint64) map[model.Distance]baseline.PathBaseline {
	return map[model.Distance]baseline.PathBaseline{
		model.DistanceCrossRoot: {BandwidthBPS: bw, LatencyNs: lat, SubClasses: []baseline.SubClass{{
			Name: "fast", BandwidthBPS: fastBW, LatencyNs: fastLat,
			RootPortPairs: []model.RootPortPair{model.NewRootPortPair("rp.1", "rp.0")},
		}}},
	}
}

func byPath(ms []model.PathMeasurement) map[string]model.PathMeasurement {
	out := map[string]model.PathMeasurement{}
	for _, m := range ms {
		out[m.RNIC+"->"+m.Endpoint] = m
	}
	return out
}

// A fast-pair path that falls to the slow level is abnormal against its
// sub-class, while a slow-pair path at that level is judged against the
// distance as before.
func TestClassifyPathsUsesSubClassBaseline(t *testing.T) {
	g := topology.Fig2()
	ms := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "gpu2", BandwidthBPS: 125_000_000_000},
		{RNIC: "rnic0", Endpoint: "gpu3", BandwidthBPS: 196_000_000_000},
		{RNIC: "rnic2", Endpoint: "gpu6", BandwidthBPS: 125_000_000_000},
	}
	got := byPath(analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), baseline.Uniform(fastCrossRoot(125_000_000_000, 0, 197_000_000_000, 0))))
	if m := got["rnic0->gpu2"]; m.Status != model.PathAbnormal || m.BaselineClass != "cross_root/fast" {
		t.Errorf("degraded fast path = %+v, want abnormal against cross_root/fast", m)
	}
	if m := got["rnic0->gpu3"]; m.Status == model.PathAbnormal || m.BaselineClass != "cross_root/fast" {
		t.Errorf("healthy fast path = %+v", m)
	}
	if m := got["rnic2->gpu6"]; m.Status == model.PathAbnormal || m.BaselineClass != "" {
		t.Errorf("slow-pair path = %+v, want judged against cross_root itself", m)
	}

	// Without the sub-class the degraded fast path hides at the slow level.
	plain := fastCrossRoot(125_000_000_000, 0, 197_000_000_000, 0)
	cr := plain[model.DistanceCrossRoot]
	cr.SubClasses = nil
	plain[model.DistanceCrossRoot] = cr
	if m := byPath(analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), baseline.Uniform(plain)))["rnic0->gpu2"]; m.Status == model.PathAbnormal || m.BaselineClass != "" {
		t.Errorf("without a sub-class = %+v", m)
	}
}

// The latency verdict narrows the same way: a set sub-class latency is the
// reference of its paths, and with no latency set the sibling median is drawn
// from the path's own class, so a distance at two latency levels does not
// make its slow half abnormal against the fast half.
func TestClassifyLatencyUsesSubClass(t *testing.T) {
	g := topology.Fig2()
	all := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "gpu2", SmallLatNsP50: 1000},
		{RNIC: "rnic0", Endpoint: "gpu3", SmallLatNsP50: 1000},
		{RNIC: "rnic1", Endpoint: "gpu0", SmallLatNsP50: 1000},
		{RNIC: "rnic1", Endpoint: "gpu1", SmallLatNsP50: 2200}, // fast pair, slow latency
		{RNIC: "rnic2", Endpoint: "gpu6", SmallLatNsP50: 2200},
		{RNIC: "rnic2", Endpoint: "gpu7", SmallLatNsP50: 2200},
		{RNIC: "rnic3", Endpoint: "gpu4", SmallLatNsP50: 2200},
		{RNIC: "rnic3", Endpoint: "gpu5", SmallLatNsP50: 2200},
	}

	got := byPath(analyzer.ClassifyLatency(g, all, model.ModeIdle, config.Defaults(), baseline.Uniform(fastCrossRoot(0, 2200, 197_000_000_000, 1000))))
	if m := got["rnic1->gpu1"]; m.LatencyStatus != model.LatencyAbnormal || m.LatencyRefNs != 1000 || m.LatencyReason != model.LatencyByBaseline {
		t.Errorf("fast path at the slow latency = %+v, want abnormal against the sub-class's 1000ns", m)
	}
	if m := got["rnic2->gpu6"]; m.LatencyStatus != model.LatencyNormal || m.LatencyRefNs != 2200 {
		t.Errorf("slow-pair path = %+v, want normal against cross_root's 2200ns", m)
	}

	// No latency set anywhere: sibling medians per class.
	got = byPath(analyzer.ClassifyLatency(g, all, model.ModeIdle, config.Defaults(), baseline.Uniform(fastCrossRoot(125_000_000_000, 0, 197_000_000_000, 0))))
	for k, m := range got {
		want := model.LatencyNormal
		if k == "rnic1->gpu1" {
			want = model.LatencyAbnormal
		}
		if m.LatencyStatus != want || m.LatencyReason != model.LatencyBySiblings {
			t.Errorf("%s = %+v, want %s by siblings of its class", k, m, want)
		}
	}
}

// Busy mode compares a loaded RNIC's path only with siblings of the same
// distance and sub-class. Fig.2's busy probe set holds no cross-root-port
// GPU path, so the test marks rnic0's cross_socket paths affinitive (busy
// probed); {rp.0, rp.2} is their fast pair.
func TestBusyRefGroupsBySubClass(t *testing.T) {
	g := topology.Fig2()
	for i, p := range g.Paths {
		if p.RNIC == "rnic0" && p.Distance == model.DistanceCrossSocket {
			g.Paths[i].Affinitive = true
		}
	}
	bases := map[model.Distance]baseline.PathBaseline{
		model.DistanceCrossSocket: {BandwidthBPS: 116_000_000_000, SubClasses: []baseline.SubClass{{
			Name: "fast", BandwidthBPS: 197_000_000_000,
			RootPortPairs: []model.RootPortPair{model.NewRootPortPair("rp.0", "rp.2")},
		}}},
	}
	busy := map[string]bool{"rnic0": true}
	run := func(gpu5 uint64) map[string]model.PathMeasurement {
		ms := []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu4", BandwidthBPS: 197_000_000_000},
			{RNIC: "rnic0", Endpoint: "gpu5", BandwidthBPS: gpu5},
			{RNIC: "rnic0", Endpoint: "gpu6", BandwidthBPS: 116_000_000_000},
			{RNIC: "rnic0", Endpoint: "gpu7", BandwidthBPS: 116_000_000_000},
		}
		return byPath(analyzer.ClassifyPaths(g, ms, model.ModeBusy, busy, config.Defaults(), baseline.Uniform(bases)))
	}

	// Healthy: the slow paths are not held to the fast ones.
	for k, m := range run(197_000_000_000) {
		if m.Status == model.PathAbnormal {
			t.Errorf("healthy %s = %+v: compared across sub-classes", k, m)
		}
	}
	// A fast path at the slow level is abnormal against its fast sibling.
	got := run(116_000_000_000)
	if m := got["rnic0->gpu5"]; m.Status != model.PathAbnormal || m.BaselineClass != "cross_socket/fast" {
		t.Errorf("degraded fast path = %+v", m)
	}
	if m := got["rnic0->gpu4"]; m.Status == model.PathAbnormal {
		t.Errorf("its healthy sibling = %+v", m)
	}
}

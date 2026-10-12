package analyzer_test

import (
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// fig2SlowRNIC is Fig.2 with rnic3 a lower-line-rate RNIC in a narrow slot
// next to faster RNICs: it derives a Fig.4 table of its own.
func fig2SlowRNIC() topology.Graph {
	g := topology.Fig2()
	for i := range g.RNICs {
		if g.RNICs[i].ID == "rnic3" {
			g.RNICs[i].LineRate = 100 * gbps
			g.RNICs[i].PCIeCapBPS = 64 * gbps
		}
	}
	return g
}

// A baseline file calibrated on the fast RNICs puts every path of
// the slow RNIC far below its distance's baseline. A "tables" section for the
// slow RNIC's table judges its paths against its own values, and a distance
// the section leaves out falls back to the RNIC's own Fig.4 default, not to
// the top-level value measured on the other table.
func TestSectionServesTheSlowRNIC(t *testing.T) {
	g := fig2SlowRNIC()
	slow, _ := g.RNIC("rnic3")
	top := map[model.Distance]baseline.PathBaseline{
		model.DistanceLocalMem:  {BandwidthBPS: 197 * gbps},
		model.DistanceRemoteMem: {BandwidthBPS: 190 * gbps},
	}
	ov := baseline.Overlay{
		Distances: top,
		Tables: map[string]map[model.Distance]baseline.PathBaseline{
			baseline.RNICTable(slow): {model.DistanceLocalMem: {BandwidthBPS: 60 * gbps}},
		},
	}
	// rnic3's local memory is mem1 and its remote memory mem0.
	healthy := []model.PathMeasurement{
		{RNIC: "rnic3", Endpoint: "mem1", BandwidthBPS: 60 * gbps, SmallLatNsP50: 1000},
		{RNIC: "rnic3", Endpoint: "mem0", BandwidthBPS: 58 * gbps, SmallLatNsP50: 1400},
		{RNIC: "rnic2", Endpoint: "mem1", BandwidthBPS: 197 * gbps, SmallLatNsP50: 1000},
	}
	status := func(ov baseline.Overlay, ms []model.PathMeasurement) map[string]model.PathStatus {
		out := map[string]model.PathStatus{}
		for _, m := range analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), ov) {
			out[m.RNIC+"->"+m.Endpoint] = m.Status
		}
		return out
	}

	// Precondition: one value per distance for every RNIC.
	got := status(baseline.Uniform(top), healthy)
	if got["rnic3->mem1"] != model.PathAbnormal || got["rnic3->mem0"] != model.PathAbnormal {
		t.Fatalf("precondition: the slow RNIC should read abnormal against the fast table's values, got %v", got)
	}

	got = status(ov, healthy)
	for k, s := range got {
		if s != model.PathNormal {
			t.Errorf("healthy %s = %s against its own section, want normal", k, s)
		}
	}

	degraded := []model.PathMeasurement{{RNIC: "rnic3", Endpoint: "mem1", BandwidthBPS: 30 * gbps, SmallLatNsP50: 1000}}
	if s := status(ov, degraded)["rnic3->mem1"]; s != model.PathAbnormal {
		t.Fatalf("degraded slow path = %s, want abnormal", s)
	}
}

package analyzer_test

import (
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// fig2At400G is the Fig.2 world rewired with 400G RNICs: the wire outruns the
// ~252 Gb/s PCIe hop feeding it. That is the shape of a current 400G host, where
// host-DRAM loopback measures ~266–278 Gb/s.
func fig2At400G(t *testing.T) topology.Graph {
	t.Helper()
	g := topology.Fig2()
	for i := range g.RNICs {
		g.RNICs[i].LineRate = 400_000_000_000
		if g.RNICs[i].PCIeCapBPS != 252_000_000_000 {
			t.Fatalf("Fig.2 should carry the PCIe hop capacity on %s: %d", g.RNICs[i].ID, g.RNICs[i].PCIeCapBPS)
		}
	}
	return g
}

// A healthy 400G host must come back normal with no flags. Before the clamp the
// default local_mem/remote_mem baseline was 0.985×400G = 394 Gb/s, so every
// memory path went abnormal and each RNIC tripped the "all affinitive paths
// abnormal" rule at high confidence.
func TestClassify400GHealthyMemoryIsNormalByDefault(t *testing.T) {
	g := fig2At400G(t)
	ms := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 274_000_000_000, SmallLatNsP50: 3000},
		{RNIC: "rnic0", Endpoint: "mem1", BandwidthBPS: 266_000_000_000, SmallLatNsP50: 3000},
	}

	for _, p := range analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), baseline.Overlay{}) {
		if p.Status != model.PathNormal {
			t.Fatalf("healthy 400G memory loopback must be normal with no --baseline: %+v", p)
		}
	}
}

// The clamp must not blind the abnormal test: a hop that lost most of its
// bandwidth still falls under the derived baseline.
func TestClassify400GDegradedMemoryStaysAbnormal(t *testing.T) {
	g := fig2At400G(t)
	ms := []model.PathMeasurement{
		// An x16 hop trained down to x4, and a half-width one.
		{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 63_000_000_000, SmallLatNsP50: 3200},
		{RNIC: "rnic0", Endpoint: "mem1", BandwidthBPS: 133_000_000_000, SmallLatNsP50: 3200},
	}

	for _, p := range analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), baseline.Overlay{}) {
		if p.Status != model.PathAbnormal {
			t.Fatalf("a degraded 400G memory path must stay abnormal: %+v", p)
		}
	}
}

// The paper's 200G/Gen4 hosts keep the Fig.4 behaviour: 197 Gb/s baseline, so
// 190 Gb/s is normal and 150 Gb/s is abnormal.
func TestClassify200GUnchanged(t *testing.T) {
	g := topology.Fig2()
	ms := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 190_000_000_000, SmallLatNsP50: 1000},
		{RNIC: "rnic0", Endpoint: "mem1", BandwidthBPS: 150_000_000_000, SmallLatNsP50: 1400},
	}

	got := analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), baseline.Overlay{})
	for _, p := range got {
		switch p.Endpoint {
		case "mem0":
			if p.Status != model.PathNormal {
				t.Fatalf("200G 190Gbps local_mem: %+v", p)
			}
		case "mem1":
			if p.Status != model.PathAbnormal {
				t.Fatalf("200G 150Gbps remote_mem: %+v", p)
			}
		}
	}
}

// --baseline / --baseline-file stay absolute: they override the clamped default
// in both directions.
func TestClassify400GUserBaselineOverridesClamp(t *testing.T) {
	g := fig2At400G(t)
	ms := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 274_000_000_000, SmallLatNsP50: 3000},
		{RNIC: "rnic0", Endpoint: "mem1", BandwidthBPS: 266_000_000_000, SmallLatNsP50: 3000},
	}
	cfg := config.Defaults()

	// A floor above what the bus can carry still marks healthy paths abnormal.
	ov := map[model.Distance]baseline.PathBaseline{
		model.DistanceLocalMem:  {BandwidthBPS: 394_000_000_000},
		model.DistanceRemoteMem: {BandwidthBPS: 394_000_000_000},
	}
	for _, p := range analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, cfg, baseline.Uniform(ov)) {
		if p.Status != model.PathAbnormal {
			t.Fatalf("an explicit 394Gbps floor must win over the clamp: %+v", p)
		}
	}

	// The documented 270 Gbps override keeps working.
	ov = map[model.Distance]baseline.PathBaseline{
		model.DistanceLocalMem:  {BandwidthBPS: 270_000_000_000},
		model.DistanceRemoteMem: {BandwidthBPS: 270_000_000_000},
	}
	for _, p := range analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, cfg, baseline.Uniform(ov)) {
		if p.Status != model.PathNormal {
			t.Fatalf("user 270G baseline must accept measured ~270G: %+v", p)
		}
	}

	// Only the overridden distance moves; the rest stay on the clamped default.
	got := analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, cfg, baseline.Uniform(map[model.Distance]baseline.PathBaseline{
		model.DistanceLocalMem: {BandwidthBPS: 394_000_000_000},
	}))
	for _, p := range got {
		switch p.Endpoint {
		case "mem0":
			if p.Status != model.PathAbnormal {
				t.Fatalf("overridden local_mem: %+v", p)
			}
		case "mem1":
			if p.Status != model.PathNormal {
				t.Fatalf("unset remote_mem should stay on the clamped default: %+v", p)
			}
		}
	}
}

// An RNIC whose hop capacity is unknown (a hand-written topology with no
// pcie_rnic link) keeps the pre-clamp behaviour rather than silently relaxing.
func TestClassify400GWithoutHopCapacityKeepsLineRateBaseline(t *testing.T) {
	g := fig2At400G(t)
	for i := range g.RNICs {
		g.RNICs[i].PCIeCapBPS = 0
	}
	ms := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 274_000_000_000, SmallLatNsP50: 3000},
	}

	for _, p := range analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), baseline.Overlay{}) {
		if p.Status != model.PathAbnormal {
			t.Fatalf("unknown hop capacity must still judge against 0.985×line: %+v", p)
		}
	}
}

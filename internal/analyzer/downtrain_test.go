package analyzer_test

import (
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/topology"
)

const (
	pcieGen4 = "16.0 GT/s PCIe"
	pcieGen5 = "32.0 GT/s PCIe"
)

// fig2Gen5 is Fig.2 rewired as a 400G host whose every PCIe hop is a Gen5 x16
// link (~504 Gb/s), the capacity discovery now reads from sysfs.
func fig2Gen5() topology.Graph {
	g := topology.Fig2()
	gen5x16 := pci.EffectiveBPS(32, 16)
	for i := range g.Links {
		switch g.Links[i].Kind {
		case "pcie_rnic", "pcie_gpu", "pcie_switch", "root_port":
			g.Links[i].CapacityBPS = gen5x16
		}
	}
	for i := range g.RNICs {
		g.RNICs[i].LineRate = 400 * gbps
		g.RNICs[i].PCIeCapBPS = gen5x16
	}
	return g
}

// gen5Link is a Gen5 x16 device under a Gen5 x16 port, trained to speed x width.
func gen5Link(speed string, width int) model.PCIeConfig {
	return model.PCIeConfig{
		Speed: speed, Width: width,
		MaxSpeed: pcieGen5, MaxWidth: 16,
		PortMaxSpeed: pcieGen5, PortMaxWidth: 16,
	}
}

// healthyGen5PCIe is every RNIC and GPU link at its capability.
func healthyGen5PCIe(g topology.Graph) map[string]model.PCIeConfig {
	out := map[string]model.PCIeConfig{}
	for _, n := range g.RNICs {
		out[n.ID] = gen5Link(pcieGen5, 16)
	}
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			out[e.ID] = gen5Link(pcieGen5, 16)
		}
	}
	return out
}

// gen5Measurements is one idle full-mesh run: host DRAM at mem, affinitive
// GPUs at gdr, everything else at a reduced rate that clears ❸/❹.
func gen5Measurements(g topology.Graph, mem, gdr uint64) []model.PathMeasurement {
	var ms []model.PathMeasurement
	for _, p := range g.Paths {
		e, _ := g.Endpoint(p.Endpoint)
		bw := mem
		if e.Kind == model.EndpointGPU {
			bw = gdr
			if !p.Affinitive {
				bw = 200 * gbps
			}
		}
		ms = append(ms, model.PathMeasurement{RNIC: p.RNIC, Endpoint: p.Endpoint, BandwidthBPS: bw, SmallLatNsP50: 900})
	}
	return ms
}

func causesOf(res analyzer.Result, code string) []model.RootCause {
	var out []model.RootCause
	for _, c := range res.Causes {
		if c.Code == code {
			out = append(out, c)
		}
	}
	return out
}

// A Gen5 hop must not lift the default baseline above the regime Fig.4 was
// measured in: host-DRAM loopback of about 270 Gb/s is what a healthy 400G RNIC
// in a Gen5 slot reaches, and scaling 0.985 × min(400G, 504G) = 394 Gb/s would
// mark every memory path abnormal. With every link at its capability the host
// is clean.
func TestGen5HopHealthyHostStaysNormal(t *testing.T) {
	g := fig2Gen5()
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle,
		Measurements: gen5Measurements(g, 270*gbps, 273*gbps),
		PCIe:         healthyGen5PCIe(g),
	})
	for _, m := range res.Paths {
		if m.Status == model.PathAbnormal {
			t.Fatalf("healthy Gen5 host has an abnormal path: %+v", m)
		}
		p, _ := g.Path(m.RNIC, m.Endpoint)
		if p.Affinitive && m.Status != model.PathNormal {
			t.Fatalf("affinitive path must be normal on a healthy Gen5 host: %+v", m)
		}
	}
	if len(res.Causes) != 0 {
		t.Fatalf("healthy Gen5 host must have no causes: %v", codes(model.Report{Causes: res.Causes}))
	}
}

// NSDI'23 failure #1 on a Gen5 host: the RNIC's slot trained to Gen4 x16, so
// loopback reads about 250 Gb/s. That clears the Fig.4-bounded default, so no
// path is abnormal and the bandwidth rules stay silent; the negotiated link is
// below what both ends can train to and names the RNIC anyway.
func TestGen5DownTrainedRNICWithoutBandwidthSymptom(t *testing.T) {
	g := fig2Gen5()
	pcie := healthyGen5PCIe(g)
	pcie["rnic0"] = gen5Link(pcieGen4, 16)
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle,
		Measurements: gen5Measurements(g, 250*gbps, 250*gbps),
		PCIe:         pcie,
	})
	for _, m := range res.Paths {
		if m.Status == model.PathAbnormal {
			t.Fatalf("250 Gb/s must not be abnormal against the Fig.4 default: %+v", m)
		}
	}
	got := causesOf(res, model.CauseRNICPCIeFailure)
	if len(got) != 1 || len(got[0].RNICs) != 1 || got[0].RNICs[0] != "rnic0" {
		t.Fatalf("want one rnic_pcie_failure on rnic0, got %+v", res.Causes)
	}
	if got[0].Confidence != "medium" {
		t.Fatalf("config space alone is medium, got %s", got[0].Confidence)
	}
	if len(res.Causes) != 1 {
		t.Fatalf("only rnic0 is down-trained: %v", codes(model.Report{Causes: res.Causes}))
	}
}

// The same down-train with an affinitive path from the RNIC already abnormal:
// both halves of the evidence agree and the cause is high.
func TestGen5DownTrainedRNICWithAbnormalPathIsHigh(t *testing.T) {
	g := fig2Gen5()
	pcie := healthyGen5PCIe(g)
	pcie["rnic0"] = gen5Link(pcieGen4, 16)
	ms := gen5Measurements(g, 250*gbps, 250*gbps)
	for i := range ms {
		if ms[i].RNIC == "rnic0" && ms[i].Endpoint == "gpu0" {
			ms[i].BandwidthBPS = 150 * gbps
		}
	}
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: ms, PCIe: pcie,
	})
	got := causesOf(res, model.CauseRNICPCIeFailure)
	if len(got) != 1 || got[0].Confidence != "high" {
		t.Fatalf("want one high rnic_pcie_failure, got %+v", got)
	}
}

// A 200G RNIC whose Gen5 slot trained to Gen4 x16 still carries its line rate:
// the link is not the bottleneck, which is where healthyPCIeSpeed draws the line.
func TestDownTrainStillCarryingLineRateIsNotACause(t *testing.T) {
	g := fig2Gen5()
	for i := range g.RNICs {
		g.RNICs[i].LineRate = 200 * gbps
	}
	pcie := healthyGen5PCIe(g)
	pcie["rnic0"] = gen5Link(pcieGen4, 16)
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle,
		Measurements: gen5Measurements(g, 197*gbps, 197*gbps),
		PCIe:         pcie,
	})
	if len(res.Causes) != 0 {
		t.Fatalf("a down-train that still carries the line rate is not a cause: %+v", res.Causes)
	}
}

// GPUs drop link speed when idle and retrain on load, and hostping reads config
// space on an idle host, so a low GPU link speed is not evidence. A narrower
// link is.
func TestGPUDownTrainIsWidthOnly(t *testing.T) {
	g := fig2Gen5()

	pcie := healthyGen5PCIe(g)
	pcie["gpu0"] = gen5Link("2.5 GT/s PCIe", 16) // power-saving idle speed
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle,
		Measurements: gen5Measurements(g, 270*gbps, 273*gbps), PCIe: pcie,
	})
	if len(res.Causes) != 0 {
		t.Fatalf("an idle GPU at low link speed is healthy: %+v", res.Causes)
	}

	pcie["gpu0"] = gen5Link(pcieGen5, 8)
	res = analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle,
		Measurements: gen5Measurements(g, 270*gbps, 273*gbps), PCIe: pcie,
	})
	got := causesOf(res, model.CauseGPUPCIeFailure)
	if len(got) != 1 || len(got[0].Endpoints) != 1 || got[0].Endpoints[0] != "gpu0" || got[0].Confidence != "medium" {
		t.Fatalf("want one medium gpu_pcie_failure on gpu0, got %+v", res.Causes)
	}
}

package analyzer_test

import (
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func TestGPURowsUseFig4DistanceNotLocalMem(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	// 140 Gbps is > Fig.4 cross-root 125.5G (not abnormal) and
	// < local/same-switch ~197G by more than AbnormalTh=20% (would be
	// abnormal if the GPU row were mis-keyed as LocalMem).
	// Pass nil overlay: ForRNIC applies Fig.4×200G, not user Defaults-as-overlay.
	ms := []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu2", BandwidthBPS: 140_000_000_000, SmallLatNsP50: 2200,
	}}
	out := analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, cfg, baseline.Overlay{})
	if len(out) != 1 {
		t.Fatalf("paths %d", len(out))
	}
	if out[0].Status == model.PathAbnormal {
		t.Fatalf("cross-root GPU at 140G must use Fig.4 125.5G, not LocalMem; status=%s", out[0].Status)
	}
	p, _ := g.Path("rnic0", "gpu2")
	if p.Distance != model.DistanceCrossRoot {
		t.Fatalf("fixture distance %d", p.Distance)
	}

	ms2 := []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu4", BandwidthBPS: 130_000_000_000, SmallLatNsP50: 2400,
	}}
	out2 := analyzer.ClassifyPaths(g, ms2, model.ModeIdle, nil, cfg, baseline.Overlay{})
	if out2[0].Status == model.PathAbnormal {
		t.Fatalf("cross-socket GPU at 130G must use Fig.4 116.4G, not LocalMem; status=%s", out2[0].Status)
	}

	// 100G is ≥20% below CrossRoot 125.5G (abnormal) but only ~14% below
	// CrossSocket 116.4G (would stay non-abnormal if the keys were swapped).
	ms3 := []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu2", BandwidthBPS: 100_000_000_000, SmallLatNsP50: 2200,
	}}
	out3 := analyzer.ClassifyPaths(g, ms3, model.ModeIdle, nil, cfg, baseline.Overlay{})
	if out3[0].Status != model.PathAbnormal {
		t.Fatalf("cross-root GPU at 100G must use Fig.4 125.5G, not CrossSocket; status=%s", out3[0].Status)
	}
}

func TestDiagnoseMisconfigFiresFromLiveGPURows(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg, Mode: model.ModeIdle,
		Measurements: []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 8000},
			{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000},
		},
		PCIe: map[string]model.PCIeConfig{
			"rnic0": {ACSKnown: true, ACSEnabled: true, ATSKnown: true, ATSEnabled: true, Speed: "16GT/s", Width: 16},
		},
	})
	if !hasCause(model.Report{Causes: res.Causes}, model.CauseACSEnabled) {
		t.Fatalf("want acs_enabled from GPU row + ACS bit, got %v", res.Causes)
	}

	res2 := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg, Mode: model.ModeIdle,
		Measurements: []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 8000},
			{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000},
		},
		PCIe: map[string]model.PCIeConfig{
			"rnic0": {ACSKnown: true, ACSEnabled: false, ATSKnown: true, ATSEnabled: false, Speed: "16GT/s", Width: 16},
		},
	})
	if !hasCause(model.Report{Causes: res2.Causes}, model.CauseATSDisabled) {
		t.Fatalf("want ats_disabled from GPU row + ATS off, got %v", res2.Causes)
	}
}

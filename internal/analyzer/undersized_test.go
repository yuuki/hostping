package analyzer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// gen5Narrow is an RNIC link trained at speed x width under the given device
// and upstream port maximum widths, all at Gen5 speed.
func gen5Narrow(width, devMax, portMax int) model.PCIeConfig {
	return model.PCIeConfig{
		Speed: pcieGen5, Width: width,
		MaxSpeed: pcieGen5, MaxWidth: devMax,
		PortMaxSpeed: pcieGen5, PortMaxWidth: portMax,
	}
}

// abnormalRNIC0 is the idle Gen5 mesh with every rnic0 path far below its
// baseline, so all of rnic0's affinitive paths are abnormal.
func abnormalRNIC0(g topology.Graph) []model.PathMeasurement {
	ms := gen5Measurements(g, 270*gbps, 273*gbps)
	for i := range ms {
		if ms[i].RNIC == "rnic0" {
			ms[i].BandwidthBPS = 100 * gbps
		}
	}
	return ms
}

func analyzeRNIC0(g topology.Graph, ms []model.PathMeasurement, cfg model.PCIeConfig) analyzer.Result {
	pcie := healthyGen5PCIe(g)
	pcie["rnic0"] = cfg
	return analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle,
		Measurements: ms, PCIe: pcie,
	})
}

func wantUndersized(t *testing.T, res analyzer.Result, n int) {
	t.Helper()
	got := causesOf(res, model.CauseRNICPCIeUndersized)
	if len(got) != n {
		t.Fatalf("want %d rnic_pcie_undersized, got %+v", n, res.Causes)
	}
	for _, c := range got {
		if c.Confidence != "low" || len(c.RNICs) != 1 || c.RNICs[0] != "rnic0" {
			t.Fatalf("rnic_pcie_undersized must be low on rnic0: %+v", c)
		}
		for _, want := range []string{"x4", "max", "port max", "trained at its maximum", "126 Gb/s", "400 Gb/s line rate"} {
			if !strings.Contains(c.Evidence, want) {
				t.Fatalf("evidence %q missing %q", c.Evidence, want)
			}
		}
	}
}

// A 400G RNIC whose own maximum is x4 (~126 Gb/s at Gen5) under a x16 port
// trained exactly at that maximum: a slot narrower than the line rate by
// design, not a fault. Its paths are healthy.
func TestRNICAtMaxUndersizedIsAdvisoryOnly(t *testing.T) {
	g := fig2Gen5()
	res := analyzeRNIC0(g, gen5Measurements(g, 270*gbps, 273*gbps), gen5Narrow(4, 4, 16))
	if got := causesOf(res, model.CauseRNICPCIeFailure); len(got) != 0 {
		t.Fatalf("a link at its maximum is not rnic_pcie_failure: %+v", got)
	}
	wantUndersized(t, res, 1)
	if len(res.Causes) != 1 {
		t.Fatalf("only the advisory is expected: %v", codes(model.Report{Causes: res.Causes}))
	}
}

// The same at-max link with every affinitive path abnormal: something caps the
// RNIC, but the link did not down-train, so the bandwidth rule must not claim
// PCIe evidence. It stays the medium "likely failed" verdict.
func TestRNICAtMaxUndersizedWithAbnormalPathsIsMedium(t *testing.T) {
	g := fig2Gen5()
	res := analyzeRNIC0(g, abnormalRNIC0(g), gen5Narrow(4, 4, 16))
	got := causesOf(res, model.CauseRNICPCIeFailure)
	if len(got) != 1 || got[0].Confidence != "medium" {
		t.Fatalf("want one medium rnic_pcie_failure, got %+v", got)
	}
	if !strings.Contains(got[0].Evidence, "at its maximum") {
		t.Fatalf("evidence must say the link is at its maximum: %q", got[0].Evidence)
	}
	wantUndersized(t, res, 1)
}

// Width below both maxima and below the line rate is a down-train: the
// existing confidence rules apply, high with abnormal affinitive paths and
// medium from config space alone. No advisory: the link is not at its maximum.
func TestRNICDownTrainedUndersizedKeepsConfidence(t *testing.T) {
	g := fig2Gen5()
	for _, tc := range []struct {
		name string
		ms   []model.PathMeasurement
		want model.Confidence
	}{
		{"abnormal paths", abnormalRNIC0(g), "high"},
		{"config space only", gen5Measurements(g, 270*gbps, 273*gbps), "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := analyzeRNIC0(g, tc.ms, gen5Narrow(4, 16, 16))
			got := causesOf(res, model.CauseRNICPCIeFailure)
			if len(got) != 1 || got[0].Confidence != tc.want {
				t.Fatalf("want one %s rnic_pcie_failure, got %+v", tc.want, got)
			}
			wantUndersized(t, res, 0)
		})
	}
}

// Width below both maxima while the link still carries the line rate: a 400G
// RNIC at Gen5 x16 under x32 maxima is not the bottleneck (unchanged).
func TestRNICDownTrainedCarryingLineRateIsNothing(t *testing.T) {
	g := fig2Gen5()
	res := analyzeRNIC0(g, gen5Measurements(g, 270*gbps, 273*gbps), gen5Narrow(16, 32, 32))
	if len(res.Causes) != 0 {
		t.Fatalf("a down-train that carries the line rate is no cause: %v", codes(model.Report{Causes: res.Causes}))
	}
}

// Unknown maxima give no down-train evidence: abnormal paths stay medium, and
// with no symptom nothing is reported, advisory included.
func TestRNICUndersizedUnknownMaxima(t *testing.T) {
	g := fig2Gen5()
	cfg := model.PCIeConfig{Speed: pcieGen5, Width: 4}
	res := analyzeRNIC0(g, abnormalRNIC0(g), cfg)
	got := causesOf(res, model.CauseRNICPCIeFailure)
	if len(got) != 1 || got[0].Confidence != "medium" {
		t.Fatalf("want one medium rnic_pcie_failure, got %+v", got)
	}
	if !strings.Contains(got[0].Evidence, "maximum is unread") {
		t.Fatalf("evidence must say the maximum is unread: %q", got[0].Evidence)
	}
	wantUndersized(t, res, 0)

	res = analyzeRNIC0(g, gen5Measurements(g, 270*gbps, 273*gbps), cfg)
	if len(res.Causes) != 0 {
		t.Fatalf("unknown maxima without a symptom is no cause: %v", codes(model.Report{Causes: res.Causes}))
	}
}

// runWorld runs the simulator after replacing rnic0's PCIe config.
func runWorld(t *testing.T, fault string, mode model.RunMode, rnic0 *model.PCIeConfig) model.Report {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault(fault); err != nil {
		t.Fatal(err)
	}
	if rnic0 != nil {
		w.PCIe["rnic0"] = *rnic0
	}
	e := engine.New(config.Defaults(), g, sim.Prober{World: w})
	rep, err := e.Run(context.Background(), engine.RunRequest{
		Trigger: model.TriggerManual, Mode: mode, BusyRNICs: w.Busy, Util: w.LinkUtil, PCIe: w.PCIe,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// The simulator's rnic_pcie fault (x4 under a x16 device maximum) is a
// down-train and stays rnic_pcie_failure [high] in idle and busy runs.
func TestSimRNICPCIeFaultStaysHigh(t *testing.T) {
	for _, tc := range []struct {
		fault string
		mode  model.RunMode
	}{
		{"rnic_pcie:rnic0", model.ModeIdle},
		{"rnic_pcie:rnic0;busy:rnic0", model.ModeBusy},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			rep := runWorld(t, tc.fault, tc.mode, nil)
			c, ok := causeFor(rep, model.CauseRNICPCIeFailure, "rnic0")
			if !ok || c.Confidence != "high" {
				t.Fatalf("want rnic_pcie_failure [high] on rnic0, got %+v", rep.Causes)
			}
			if hasCause(rep, model.CauseRNICPCIeUndersized) {
				t.Fatalf("a down-trained link is not undersized: %v", codes(rep))
			}
		})
	}
}

// The busy equal-throttle fallback had the same capacity-only branch: an
// RNIC at its maximum must not get rnic_pcie_failure [high] from it.
func TestBusyEqualThrottleAtMaxIsNotFailure(t *testing.T) {
	atMax := model.PCIeConfig{
		Speed: pcieGen4, Width: 4, MaxSpeed: pcieGen4, MaxWidth: 4,
		PortMaxSpeed: pcieGen4, PortMaxWidth: 16,
	}
	rep := runWorld(t, "rnic_pcie:rnic0;busy:rnic0", model.ModeBusy, &atMax)
	if _, ok := causeFor(rep, model.CauseRNICPCIeFailure, "rnic0"); ok {
		t.Fatalf("a link at its maximum is not rnic_pcie_failure: %+v", rep.Causes)
	}
	c, ok := causeFor(rep, model.CauseRNICPCIeUndersized, "rnic0")
	if !ok || c.Confidence != "low" {
		t.Fatalf("want rnic_pcie_undersized [low] on rnic0, got %+v", rep.Causes)
	}
}

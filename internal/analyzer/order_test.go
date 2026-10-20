package analyzer_test

import (
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// multiATSInput makes every RNIC report an abnormal affinitive GPU path with
// ATS off, so diagnoseMisconfig emits one ats_disabled cause per RNIC from a
// map range. Without a tie-break beyond the cause code, the order of those
// causes follows Go's randomized map iteration.
func multiATSInput() analyzer.Input {
	off := model.PCIeConfig{ACSKnown: true, ACSEnabled: false, ATSKnown: true, ATSEnabled: false, Speed: "16GT/s", Width: 16}
	return analyzer.Input{
		Graph: topology.Fig2(), Cfg: config.Defaults(), Mode: model.ModeIdle,
		Measurements: []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 8000},
			{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000},
			{RNIC: "rnic1", Endpoint: "gpu2", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 8000},
			{RNIC: "rnic1", Endpoint: "mem0", BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000},
			{RNIC: "rnic2", Endpoint: "gpu4", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 8000},
			{RNIC: "rnic2", Endpoint: "mem1", BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000},
			{RNIC: "rnic3", Endpoint: "gpu6", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 8000},
			{RNIC: "rnic3", Endpoint: "mem1", BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000},
		},
		PCIe: map[string]model.PCIeConfig{"rnic0": off, "rnic1": off, "rnic2": off, "rnic3": off},
	}
}

func causeOrder(causes []model.RootCause) []string {
	out := make([]string, 0, len(causes))
	for _, c := range causes {
		out = append(out, string(c.Confidence)+"/"+c.Code+"/"+strings.Join(c.RNICs, ",")+
			"/"+strings.Join(c.Links, ",")+"/"+strings.Join(c.Endpoints, ","))
	}
	return out
}

func TestAnalyzeCauseOrderIsStable(t *testing.T) {
	want := causeOrder(analyzer.Analyze(multiATSInput()).Causes)
	if len(want) < 2 {
		t.Fatalf("fixture must produce several same-code causes, got %v", want)
	}
	for i := 0; i < 100; i++ {
		got := causeOrder(analyzer.Analyze(multiATSInput()).Causes)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("run %d reordered causes:\nwant %v\ngot  %v", i, want, got)
		}
	}
}

func TestAnalyzeCausesSortByConfidenceThenSubject(t *testing.T) {
	causes := analyzer.Analyze(multiATSInput()).Causes
	var rnics []string
	for _, c := range causes {
		if c.Code == model.CauseATSDisabled {
			rnics = append(rnics, strings.Join(c.RNICs, ","))
		}
	}
	want := []string{"rnic0", "rnic1", "rnic2", "rnic3"}
	if strings.Join(rnics, ",") != strings.Join(want, ",") {
		t.Fatalf("ats_disabled causes want %v, got %v", want, rnics)
	}

	rank := map[model.Confidence]int{model.ConfidenceHigh: 0, model.ConfidenceMedium: 1, model.ConfidenceLow: 2}
	for i := 1; i < len(causes); i++ {
		if rank[causes[i-1].Confidence] > rank[causes[i].Confidence] {
			t.Fatalf("confidence out of rank order at %d: %v", i, causeOrder(causes))
		}
	}
}

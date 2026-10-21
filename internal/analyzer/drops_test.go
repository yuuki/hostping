package analyzer_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func dropCauses(t *testing.T, in analyzer.Input) []model.RootCause {
	t.Helper()
	in.Graph = topology.Fig2()
	in.Cfg = config.Defaults()
	var out []model.RootCause
	for _, c := range analyzer.Analyze(in).Causes {
		if c.Code == model.CauseRNICPacketDrops {
			out = append(out, c)
		}
	}
	return out
}

func lossy(src, dst string, loss float64) model.MutualMeasurement {
	return model.MutualMeasurement{Src: src, Dst: dst, Connected: true, LossRatio: loss, RTTnsP50: 4000}
}

func clean(src, dst string) model.MutualMeasurement {
	return model.MutualMeasurement{Src: src, Dst: dst, Connected: true, RTTnsP50: 4000}
}

// One faulty RNIC shows loss on every pair it is probed over. That is one
// RNIC dropping packets, not one fault per pair.
func TestDropsReportedOncePerRNIC(t *testing.T) {
	causes := dropCauses(t, analyzer.Input{Mutual: []model.MutualMeasurement{
		lossy("rnic0", "rnic1", 0.05),
		clean("rnic0", "rnic2"),
		clean("rnic0", "rnic3"),
		lossy("rnic1", "rnic2", 0.05),
		lossy("rnic1", "rnic3", 0.05),
		clean("rnic2", "rnic3"),
	}})
	if len(causes) != 1 {
		t.Fatalf("want 1 drop cause, got %d: %+v", len(causes), causes)
	}
	if got := causes[0].RNICs; len(got) != 1 || got[0] != "rnic1" {
		t.Errorf("rnics = %v, want [rnic1]", got)
	}
	for _, peer := range []string{"rnic0", "rnic2", "rnic3"} {
		if !strings.Contains(causes[0].Evidence, peer) {
			t.Errorf("evidence %q does not list the affected pair with %s", causes[0].Evidence, peer)
		}
	}
}

// Two dropping RNICs on separate rails stay two causes.
func TestDropsTwoFaultyRNICs(t *testing.T) {
	causes := dropCauses(t, analyzer.Input{Mutual: []model.MutualMeasurement{
		lossy("rnic0", "rnic1", 0.05),
		lossy("rnic1", "rnic2", 0.05),
		lossy("rnic3", "rnic4", 0.07),
		lossy("rnic4", "rnic5", 0.07),
	}})
	if len(causes) != 2 {
		t.Fatalf("want 2 drop causes, got %d: %+v", len(causes), causes)
	}
	for i, want := range []string{"rnic1", "rnic4"} {
		if got := causes[i].RNICs; len(got) != 1 || got[0] != want {
			t.Errorf("cause %d rnics = %v, want [%s]", i, got, want)
		}
	}
}

// A single dropping pair cannot be attributed to one side, so both RNICs are
// named — still one cause.
func TestDropsSinglePairNamesBothRNICs(t *testing.T) {
	causes := dropCauses(t, analyzer.Input{Mutual: []model.MutualMeasurement{
		lossy("rnic0", "rnic1", 0.05),
		clean("rnic2", "rnic3"),
	}})
	if len(causes) != 1 {
		t.Fatalf("want 1 drop cause, got %d: %+v", len(causes), causes)
	}
	if got := causes[0].RNICs; len(got) != 2 || got[0] != "rnic0" || got[1] != "rnic1" {
		t.Errorf("rnics = %v, want [rnic0 rnic1]", got)
	}
}

// No RNIC is common to a lossy triangle, so nothing is attributed and the
// pairs are reported as they were measured.
func TestDropsUnattributableGroupStaysPerPair(t *testing.T) {
	causes := dropCauses(t, analyzer.Input{Mutual: []model.MutualMeasurement{
		lossy("rnic0", "rnic1", 0.05),
		lossy("rnic1", "rnic2", 0.05),
		lossy("rnic0", "rnic2", 0.05),
	}})
	if len(causes) != 3 {
		t.Fatalf("want 3 drop causes for an unattributable triangle, got %d: %+v", len(causes), causes)
	}
}

// A hardware discard/OOB delta is already per RNIC: it must be reported once,
// not once per mutual pair that RNIC takes part in.
func TestCounterDropDeltaReportedOnce(t *testing.T) {
	reliable := func(src, dst string, counter uint64) model.MutualMeasurement {
		return model.MutualMeasurement{Src: src, Dst: dst, Connected: true, Reliable: true, LossRatio: 0.5, CounterDrops: counter}
	}
	causes := dropCauses(t, analyzer.Input{
		Mutual: []model.MutualMeasurement{
			reliable("rnic0", "rnic1", 12),
			reliable("rnic1", "rnic2", 12),
			reliable("rnic1", "rnic3", 12),
			reliable("rnic0", "rnic2", 0),
		},
		DropDelta: map[string]model.RNICDrops{"rnic1": faultDrops("rx_discards_phy", 12)},
	})
	if len(causes) != 1 {
		t.Fatalf("want 1 drop cause, got %d: %+v", len(causes), causes)
	}
	if got := causes[0].RNICs; len(got) != 1 || got[0] != "rnic1" {
		t.Errorf("rnics = %v, want [rnic1]", got)
	}
	if !strings.Contains(causes[0].Evidence, "delta=12") {
		t.Errorf("evidence %q should report the delta once, undoubled", causes[0].Evidence)
	}
}

// Counter deltas without any mutual probe keep their per-RNIC form, and name
// each counter that moved.
func TestCounterDropDeltaWithoutMutual(t *testing.T) {
	causes := dropCauses(t, analyzer.Input{DropDelta: map[string]model.RNICDrops{
		"rnic0": {Fault: map[string]uint64{"rx_prio3_discards": 2, "out_of_buffer": 1}},
		"rnic2": {},
	}})
	if len(causes) != 1 {
		t.Fatalf("want 1 drop cause, got %d: %+v", len(causes), causes)
	}
	if want := "rnic0 discard/OOB delta=3 (out_of_buffer=1, rx_prio3_discards=2)"; causes[0].Evidence != want {
		t.Errorf("evidence = %q, want %q", causes[0].Evidence, want)
	}
}

// Lossy-class drops are not rnic_packet_drops: they are the advisory
// rnic_lossy_class_discards at low confidence, naming the counters and the
// configured lossless priorities.
func TestLossyClassDiscardsAreAdvisory(t *testing.T) {
	cfg := config.Defaults()
	cfg.LosslessPriorities = []int{3, 4}
	res := analyzer.Analyze(analyzer.Input{
		Graph: topology.Fig2(), Cfg: cfg,
		DropDelta: map[string]model.RNICDrops{
			"rnic0": {Lossy: map[string]uint64{"rx_prio0_buf_discard": 300}},
			"rnic1": {Fault: map[string]uint64{"rx_prio3_discards": 7}, Lossy: map[string]uint64{"rx_prio0_discards": 5}},
		},
	})
	var drops, lossy []model.RootCause
	for _, c := range res.Causes {
		switch c.Code {
		case model.CauseRNICPacketDrops:
			drops = append(drops, c)
		case model.CauseRNICLossyClassDiscards:
			lossy = append(lossy, c)
		}
	}
	if len(drops) != 1 || drops[0].RNICs[0] != "rnic1" || drops[0].Evidence != "rnic1 discard/OOB delta=7 (rx_prio3_discards=7)" {
		t.Fatalf("rnic_packet_drops = %+v, want rnic1's lossless drops only", drops)
	}
	if len(lossy) != 2 {
		t.Fatalf("want 2 lossy-class causes, got %+v", lossy)
	}
	for _, c := range lossy {
		if c.Confidence != model.ConfidenceLow {
			t.Errorf("%s confidence = %s, want low", c.RNICs, c.Confidence)
		}
	}
	if want := "rnic0 lossy-class delta=300 (rx_prio0_buf_discard=300); lossless priorities 3,4"; lossy[0].Evidence != want {
		t.Errorf("evidence = %q, want %q", lossy[0].Evidence, want)
	}
}

func faultDrops(counter string, n uint64) model.RNICDrops {
	return model.RNICDrops{Fault: map[string]uint64{counter: n}}
}

// --drop-ignore-rnic also leaves the RNIC's mutual-probe loss undiagnosed
// (the --sim drops fault), not only its counter deltas.
func TestDropIgnoredRNICProbeLossNotDiagnosed(t *testing.T) {
	cfg := config.Defaults()
	cfg.DropIgnoreRNICs = []string{"rnic1"}
	res := analyzer.Analyze(analyzer.Input{
		Graph: topology.Fig2(), Cfg: cfg,
		Mutual: []model.MutualMeasurement{
			lossy("rnic0", "rnic1", 0.05),
			lossy("rnic1", "rnic2", 0.05),
			lossy("rnic2", "rnic3", 0.05),
		},
	})
	for _, c := range res.Causes {
		if c.Code == model.CauseRNICPacketDrops && slices.Contains(c.RNICs, "rnic1") {
			t.Fatalf("ignored rnic1 diagnosed: %+v", c)
		}
	}
	if !slices.ContainsFunc(res.Causes, func(c model.RootCause) bool { return c.Code == model.CauseRNICPacketDrops }) {
		t.Fatalf("the pair without rnic1 must still be diagnosed: %+v", res.Causes)
	}
}

package analyzer_test

import (
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
		DropDelta: map[string]uint64{"rnic1": 12},
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

// Counter deltas without any mutual probe keep their per-RNIC form.
func TestCounterDropDeltaWithoutMutual(t *testing.T) {
	causes := dropCauses(t, analyzer.Input{DropDelta: map[string]uint64{"rnic0": 3, "rnic2": 0}})
	if len(causes) != 1 {
		t.Fatalf("want 1 drop cause, got %d: %+v", len(causes), causes)
	}
	if causes[0].Evidence != "rnic0 discard/OOB delta=3" {
		t.Errorf("evidence = %q", causes[0].Evidence)
	}
}

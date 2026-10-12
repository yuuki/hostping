package calibrate

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func fig2(t *testing.T) topology.Graph {
	t.Helper()
	g, err := topology.Named("fig2")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func pm(rnic, ep string, bw, lat uint64) model.PathMeasurement {
	return model.PathMeasurement{RNIC: rnic, Endpoint: ep, BandwidthBPS: bw, SmallLatNsP50: lat, Status: model.PathNormal}
}

func idleRun(ms ...model.PathMeasurement) model.Report {
	return model.Report{Mode: model.ModeIdle, Paths: ms}
}

func distResult(t *testing.T, r Result, d model.Distance) DistanceResult {
	t.Helper()
	for _, x := range r.Distances {
		if x.Distance == d {
			return x
		}
	}
	t.Fatalf("no row for %s", d.Key())
	return DistanceResult{}
}

// In Fig.2, rnic0's local memory is mem0, rnic1's is mem0, rnic2's and
// rnic3's is mem1.
func TestSummarizeMedianOfPerRunMedians(t *testing.T) {
	g := fig2(t)
	reps := []model.Report{
		// Per-run medians of local_mem: 200, 190, 100 (a disturbed run).
		idleRun(pm("rnic0", "mem0", 200, 1000), pm("rnic1", "mem0", 210, 1100), pm("rnic2", "mem1", 50, 900)),
		idleRun(pm("rnic0", "mem0", 190, 1000), pm("rnic1", "mem0", 195, 1000), pm("rnic2", "mem1", 180, 1000)),
		idleRun(pm("rnic0", "mem0", 100, 2000), pm("rnic1", "mem0", 100, 2000), pm("rnic2", "mem1", 100, 2000)),
	}
	r := Summarize(g, reps, Options{AbnormalTh: 0.2})
	lm := distResult(t, r, model.DistanceLocalMem)
	if lm.BandwidthBPS != 190 {
		t.Fatalf("local_mem baseline = %d, want 190 (median of 200, 190, 100)", lm.BandwidthBPS)
	}
	if lm.LatencyNs != 0 {
		t.Fatalf("latency set without Options.Latency: %d", lm.LatencyNs)
	}
	if lm.Paths != 3 || lm.Runs != 3 || lm.BW.N != 9 {
		t.Fatalf("paths=%d runs=%d n=%d", lm.Paths, lm.Runs, lm.BW.N)
	}
	if lm.BW.Min != 50 || lm.BW.Max != 210 {
		t.Fatalf("spread = %+v", lm.BW)
	}
	if got := r.Baselines(); len(got.Distances) != 1 {
		t.Fatalf("baselines = %+v, want local_mem only", got)
	}

	r = Summarize(g, reps, Options{Latency: true})
	if lm := distResult(t, r, model.DistanceLocalMem); lm.LatencyNs != 1000 {
		t.Fatalf("local_mem latency = %d, want 1000 (median of 1000, 1000, 2000)", lm.LatencyNs)
	}
}

func TestSummarizeSkipsReadingsThatAreNotIdleBandwidth(t *testing.T) {
	g := fig2(t)
	noPath := pm("rnic0", "nowhere", 1, 1)
	noPath.Status, noPath.UncertainReason = model.PathUncertain, model.UncertainNoPath
	busy := pm("rnic0", "mem0", 2, 1)
	busy.Status, busy.UncertainReason = model.PathUncertain, model.UncertainRNICBusy
	zero := pm("rnic1", "mem0", 0, 1)
	// Abnormal and non-affinitive readings are idle bandwidth: they count.
	abn := pm("rnic2", "mem1", 150, 1)
	abn.Status = model.PathAbnormal
	nonAff := pm("rnic0", "gpu2", 120, 1)
	nonAff.Status, nonAff.UncertainReason = model.PathUncertain, model.UncertainNonAffinitive
	busyRun := model.Report{Mode: model.ModeBusy, Paths: []model.PathMeasurement{pm("rnic3", "mem1", 3, 1)}}

	r := Summarize(g, []model.Report{idleRun(noPath, busy, zero, abn, nonAff), busyRun}, Options{})
	lm := distResult(t, r, model.DistanceLocalMem)
	if lm.BandwidthBPS != 150 || lm.BW.N != 1 {
		t.Fatalf("local_mem = %+v, want only the abnormal reading 150", lm)
	}
	if cr := distResult(t, r, model.DistanceCrossRoot); cr.BandwidthBPS != 120 {
		t.Fatalf("cross_root = %+v, want the non-affinitive reading", cr)
	}
	b := r.Baselines().Distances
	if _, ok := b[model.DistanceSameSwitch]; ok {
		t.Fatalf("unmeasured same_switch written: %+v", b)
	}
	if len(b) != 2 {
		t.Fatalf("baselines = %+v", b)
	}
}

func TestSummarizeFlagsOutlierPaths(t *testing.T) {
	g := fig2(t)
	var reps []model.Report
	for i := 0; i < 3; i++ {
		reps = append(reps, idleRun(
			pm("rnic0", "gpu0", 197, 1), pm("rnic0", "gpu1", 197, 1),
			pm("rnic1", "gpu2", 197, 1), pm("rnic1", "gpu3", 60, 1),
		))
	}
	r := Summarize(g, reps, Options{AbnormalTh: 0.2})
	if ss := distResult(t, r, model.DistanceSameSwitch); ss.BandwidthBPS != 197 {
		t.Fatalf("same_switch = %d; one slow path must not move the median", ss.BandwidthBPS)
	}
	if len(r.Outliers) != 1 || r.Outliers[0].RNIC != "rnic1" || r.Outliers[0].Endpoint != "gpu3" {
		t.Fatalf("outliers = %+v", r.Outliers)
	}
	var buf bytes.Buffer
	r.WriteTable(&buf)
	out := buf.String()
	for _, want := range []string{"WARNING: rnic1->gpu3 (same_switch)", "no measured path", "multi_switch", "4×3 (n=12)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

func TestMarshalFileRoundTripsThroughLoader(t *testing.T) {
	in := map[model.Distance]baseline.PathBaseline{
		model.DistanceSameSwitch:  {BandwidthBPS: 197_008_172_851},
		model.DistanceCrossSocket: {BandwidthBPS: 116_404_973_357, LatencyNs: 2400},
		model.DistanceRemoteMem:   {LatencyNs: 1400},
	}
	data, err := MarshalFile(baseline.Uniform(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(data), "same_switch") > strings.Index(string(data), "remote_mem") {
		t.Fatalf("keys not in Fig.4 order:\n%s", data)
	}
	path := filepath.Join(t.TempDir(), "baselines.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := baseline.LoadFile(path)
	if err != nil {
		t.Fatalf("loader rejected calibrate output: %v\n%s", err, data)
	}
	if len(got.Distances) != len(in) {
		t.Fatalf("round trip = %+v, want %+v", got, in)
	}
	for d, v := range in {
		if !reflect.DeepEqual(got.Distances[d], v) {
			t.Fatalf("%s: round trip %+v, want %+v", d.Key(), got.Distances[d], v)
		}
	}
}

func TestMarshalFileRefusesEmpty(t *testing.T) {
	if _, err := MarshalFile(baseline.Overlay{}); err == nil {
		t.Fatal("empty table marshalled; the loader would reject it")
	}
}

func TestSummarizeNotesSeveralRateTables(t *testing.T) {
	g := fig2(t)
	rep := idleRun(pm("rnic0", "mem0", 197, 1))
	rep.Baselines = []model.BaselineTable{{LineRateBPS: 200e9}, {LineRateBPS: 400e9, PCIeCapBPS: 252e9}}
	r := Summarize(g, []model.Report{rep}, Options{})
	var buf bytes.Buffer
	r.WriteTable(&buf)
	if !strings.Contains(buf.String(), "2 different Fig.4 tables") {
		t.Fatalf("table:\n%s", buf.String())
	}
}

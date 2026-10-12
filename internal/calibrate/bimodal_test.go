package calibrate

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
)

func medians(bps ...uint64) []pathMedian {
	out := make([]pathMedian, len(bps))
	for i, v := range bps {
		out[i] = pathMedian{ref: PathRef{RNIC: fmt.Sprintf("rnic%d", i), Endpoint: "gpu"}, bps: v}
	}
	return out
}

func TestSplitBimodal(t *testing.T) {
	cases := []struct {
		name       string
		bps        []uint64
		th         float64
		slow, fast int // cluster sizes; 0, 0 when not bimodal
	}{
		{"two levels", []uint64{125, 197, 126, 124, 198, 125}, 0.2, 4, 2},
		{"order does not matter", []uint64{197, 125, 198, 124}, 0.2, 2, 2},
		{"fast majority", []uint64{125, 124, 197, 196, 198}, 0.2, 2, 3},
		{"unimodal", []uint64{120, 122, 125, 127, 130}, 0.2, 0, 0},
		{"gradual spread is not two levels", []uint64{100, 110, 120, 130}, 0.2, 0, 0},
		{"single fast outlier", []uint64{125, 125, 126, 124, 197}, 0.2, 0, 0},
		{"single slow outlier", []uint64{60, 125, 125, 126, 124}, 0.2, 0, 0},
		// The largest gap overall isolates the 60; the largest allowed one
		// still separates 125 from 197.
		{"outlier beside two levels", []uint64{60, 125, 125, 197, 197}, 0.2, 3, 2},
		// 100 vs 124: the lower level is 19.4% below, inside th.
		{"gap within th", []uint64{100, 100, 124, 124}, 0.2, 0, 0},
		{"gap just past th", []uint64{100, 100, 126, 126}, 0.2, 2, 2},
		{"too few paths", []uint64{100, 200, 200}, 0.2, 0, 0},
		{"th off", []uint64{100, 100, 200, 200}, 0, 0, 0},
		{"zero lower level", []uint64{0, 0, 200, 200}, 0.2, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slow, fast, ok := splitBimodal(medians(c.bps...), c.th)
			if want := c.slow > 0; ok != want {
				t.Fatalf("bimodal = %v, want %v (slow=%v fast=%v)", ok, want, slow, fast)
			}
			if ok && (len(slow) != c.slow || len(fast) != c.fast) {
				t.Fatalf("split %d/%d, want %d/%d", len(slow), len(fast), c.slow, c.fast)
			}
		})
	}
}

// simReadings runs one idle round of every Fig.2 path on w.
func simReadings(t *testing.T, w *sim.World, fast map[PathRef]bool, fastW *sim.World) model.Report {
	t.Helper()
	var rep model.Report
	rep.Mode = model.ModeIdle
	for _, p := range w.Graph.Paths {
		src := w
		if fast[PathRef{p.RNIC, p.Endpoint}] {
			src = fastW
		}
		m, err := src.Loopback(p.RNIC, p.Endpoint, 64, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		m.Status = model.PathNormal
		rep.Paths = append(rep.Paths, m)
	}
	return rep
}

// A two-level cross_root on the simulator: a fixed minority of cross_root paths reads
// at the same_switch rate, the rest at the cross_root rate, so the median —
// the baseline — is the slow level.
func TestSummarizeWarnsAboutBimodalDistanceOnSim(t *testing.T) {
	g := fig2(t)
	slowW := sim.New(g)
	fastW := sim.New(g)
	fastW.Bases[model.DistanceCrossRoot] = fastW.Bases[model.DistanceSameSwitch]
	fast := map[PathRef]bool{{"rnic0", "gpu2"}: true, {"rnic2", "gpu6"}: true}
	var reps []model.Report
	for i := 0; i < 3; i++ {
		reps = append(reps, simReadings(t, slowW, fast, fastW))
	}
	r := Summarize(g, reps, Options{AbnormalTh: 0.2})
	if len(r.Bimodal) != 1 {
		t.Fatalf("bimodal = %+v, want cross_root only", r.Bimodal)
	}
	b := r.Bimodal[0]
	if b.Distance != model.DistanceCrossRoot || len(b.Fast) != 2 || len(b.Slow) != 6 {
		t.Fatalf("bimodal = %+v", b)
	}
	if !b.FastUnflagged(0.2) {
		t.Fatalf("a slow-majority baseline should not flag a fast path at the slow level: %+v", b)
	}
	// The warning changes nothing: the baseline is still the slow level.
	if cr := distResult(t, r, model.DistanceCrossRoot); cr.BandwidthBPS != b.SlowBPS || b.BaselineBPS != b.SlowBPS {
		t.Fatalf("cross_root baseline %d, bimodal %+v; want the slow level", cr.BandwidthBPS, b)
	}
	if len(r.Outliers) != 0 {
		t.Fatalf("outliers = %+v; fast paths are not outliers", r.Outliers)
	}
	var buf bytes.Buffer
	r.WriteTable(&buf)
	out := buf.String()
	for _, want := range []string{
		"WARNING: cross_root reads at two levels: 2 paths at",
		"fast paths: rnic0->gpu2, rnic2->gpu6",
		"a fast path that degrades to the slow level stays normal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	t.Logf("\n%s", out)
}

func TestSummarizeDoesNotWarnOnUnimodalOrSingleOutlierOnSim(t *testing.T) {
	g := fig2(t)
	slowW := sim.New(g)
	fastW := sim.New(g)
	fastW.Bases[model.DistanceCrossRoot] = fastW.Bases[model.DistanceSameSwitch]
	for name, fast := range map[string]map[PathRef]bool{
		"healthy":             nil,
		"one fast cross_root": {{"rnic0", "gpu2"}: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := Summarize(g, []model.Report{simReadings(t, slowW, fast, fastW)}, Options{AbnormalTh: 0.2})
			if len(r.Bimodal) != 0 {
				t.Fatalf("bimodal = %+v", r.Bimodal)
			}
			var buf bytes.Buffer
			r.WriteTable(&buf)
			if strings.Contains(buf.String(), "two levels") {
				t.Fatalf("table:\n%s", buf.String())
			}
		})
	}
}

func TestBimodalFastMajorityPointsAtSlowOutliers(t *testing.T) {
	g := fig2(t)
	// Fig.2 has eight cross_root paths; six fast and two slow put the
	// median, and so the baseline, at the fast level.
	var ms []model.PathMeasurement
	for _, p := range g.Paths {
		if p.Distance != model.DistanceCrossRoot {
			continue
		}
		bw := uint64(197)
		if p.RNIC == "rnic0" {
			bw = 125
		}
		ms = append(ms, pm(p.RNIC, p.Endpoint, bw, 1))
	}
	r := Summarize(g, []model.Report{idleRun(ms...)}, Options{AbnormalTh: 0.2})
	if len(r.Bimodal) != 1 || r.Bimodal[0].FastUnflagged(0.2) {
		t.Fatalf("bimodal = %+v", r.Bimodal)
	}
	if len(r.Outliers) != 2 {
		t.Fatalf("outliers = %+v, want the two slow rnic0 paths", r.Outliers)
	}
	var buf bytes.Buffer
	r.WriteTable(&buf)
	if !strings.Contains(buf.String(), "sits at the fast level, so the slow paths diagnose abnormal") {
		t.Fatalf("table:\n%s", buf.String())
	}
}

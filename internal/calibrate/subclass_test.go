package calibrate

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// The paths of Fig.2 across root ports rp.0 and rp.1.
var rp01Paths = map[PathRef]bool{
	{"rnic0", "gpu2"}: true, {"rnic0", "gpu3"}: true, {"rnic1", "gpu0"}: true, {"rnic1", "gpu1"}: true,
}

func simWorlds(t *testing.T, g topology.Graph) (slow, fast *sim.World) {
	t.Helper()
	slow = sim.New(g)
	fast = sim.New(g)
	fast.Bases[model.DistanceCrossRoot] = fast.Bases[model.DistanceSameSwitch]
	return slow, fast
}

// A split along root-port pairs becomes a sub-class: cross_root keeps the
// slow level from the slow paths alone, and "fast" lists the fast pair with
// the fast paths' level, for bandwidth and latency.
func TestSummarizeLearnsSubClassFromPureSplit(t *testing.T) {
	g := fig2(t)
	slowW, fastW := simWorlds(t, g)
	var reps []model.Report
	for range 3 {
		reps = append(reps, simReadings(t, slowW, rp01Paths, fastW))
	}
	r := Summarize(g, reps, Options{AbnormalTh: 0.2, Latency: true})
	cr := distResult(t, r, model.DistanceCrossRoot)
	slowBW := slowW.PathBW(mustPath(t, g, "rnic2", "gpu6"))
	fastBW := fastW.PathBW(mustPath(t, g, "rnic0", "gpu2"))
	if !near(cr.BandwidthBPS, slowBW) || cr.Paths != 4 || cr.Runs != 3 || cr.BW.N != 12 {
		t.Fatalf("cross_root = %+v, want the slow level %d over 4 paths", cr, slowBW)
	}
	if len(cr.SubClasses) != 1 {
		t.Fatalf("sub-classes = %+v", cr.SubClasses)
	}
	sc := cr.SubClasses[0]
	if sc.Name != "fast" || !near(sc.BandwidthBPS, fastBW) || sc.Paths != 4 ||
		!reflect.DeepEqual(sc.RootPortPairs, []model.RootPortPair{model.NewRootPortPair("rp.0", "rp.1")}) {
		t.Fatalf("sub-class = %+v", sc)
	}
	if sc.LatencyNs == 0 || cr.LatencyNs == 0 || sc.LatencyNs >= cr.LatencyNs {
		t.Fatalf("latency: cross_root %d, fast %d", cr.LatencyNs, sc.LatencyNs)
	}
	if len(r.Outliers) != 0 {
		t.Fatalf("outliers = %+v", r.Outliers)
	}
	if len(r.Bimodal) != 1 || r.Bimodal[0].SubClass != "fast" || r.Bimodal[0].BaselineBPS != cr.BandwidthBPS {
		t.Fatalf("bimodal = %+v", r.Bimodal)
	}

	var buf bytes.Buffer
	r.WriteTable(&buf)
	out := buf.String()
	for _, want := range []string{
		"\ncross_root/fast ",
		"sub-class cross_root/fast: paths across root-port pairs rp.0+rp.1",
		"NOTE: cross_root reads at two levels: 4 paths at",
		"wrote sub-class cross_root/fast",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WARNING") || strings.Contains(out, "stays normal") {
		t.Errorf("a learned sub-class closes the blind spot; no warning wanted:\n%s", out)
	}
	t.Logf("\n%s", out)

	// The file carries the sub-class and loads back unchanged.
	data, err := MarshalFile(r.Baselines())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := baseline.LoadFile(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	if !reflect.DeepEqual(got.Distances[model.DistanceCrossRoot], r.Baselines().Distances[model.DistanceCrossRoot]) {
		t.Fatalf("round trip %+v, want %+v", got.Distances[model.DistanceCrossRoot], r.Baselines().Distances[model.DistanceCrossRoot])
	}
}

// A split that cuts across a root-port pair, or whose fast cluster holds a
// path crossing no pair, cannot be written as a sub-class: the distance keeps
// one baseline, and the warning says why.
func TestSummarizeRefusesSubClassOffPairs(t *testing.T) {
	g := fig2(t)
	slowW, fastW := simWorlds(t, g)

	t.Run("mixed pair", func(t *testing.T) {
		fast := map[PathRef]bool{{"rnic0", "gpu2"}: true, {"rnic0", "gpu3"}: true, {"rnic1", "gpu0"}: true}
		r := Summarize(g, []model.Report{simReadings(t, slowW, fast, fastW)}, Options{AbnormalTh: 0.2})
		if len(r.Bimodal) != 1 || r.Bimodal[0].SubClass != "" {
			t.Fatalf("bimodal = %+v", r.Bimodal)
		}
		if b := r.Bimodal[0]; !reflect.DeepEqual(b.MixedPairs, []model.RootPortPair{model.NewRootPortPair("rp.0", "rp.1")}) {
			t.Fatalf("mixed pairs = %v", b.MixedPairs)
		}
		if cr := distResult(t, r, model.DistanceCrossRoot); len(cr.SubClasses) != 0 || cr.Paths != 8 {
			t.Fatalf("cross_root = %+v", cr)
		}
		var buf bytes.Buffer
		r.WriteTable(&buf)
		for _, want := range []string{"WARNING: cross_root reads at two levels", "the split does not follow root-port pairs, so no sub-class was written (root-port pairs with paths at both levels: rp.0+rp.1)"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("table lacks %q:\n%s", want, buf.String())
			}
		}
	})

	t.Run("fast path without a pair", func(t *testing.T) {
		// rnic0's paths to gpu2 and gpu3 lose their second root port, so
		// they cross no pair; rp.0+rp.1 itself stays pure.
		g := fig2(t)
		g.Paths = slices.Clone(g.Paths)
		for i, p := range g.Paths {
			if p.RNIC == "rnic0" && (p.Endpoint == "gpu2" || p.Endpoint == "gpu3") {
				g.Paths[i].LinkIDs = slices.DeleteFunc(slices.Clone(p.LinkIDs), func(id string) bool { return id == "rp.1" })
			}
		}
		r := Summarize(g, []model.Report{simReadings(t, slowW, rp01Paths, fastW)}, Options{AbnormalTh: 0.2})
		if len(r.Bimodal) != 1 || r.Bimodal[0].SubClass != "" || len(r.Bimodal[0].Unpaired) != 2 {
			t.Fatalf("bimodal = %+v", r.Bimodal)
		}
		var buf bytes.Buffer
		r.WriteTable(&buf)
		if !strings.Contains(buf.String(), "fast paths that cross no root-port pair: rnic0->gpu2, rnic0->gpu3") {
			t.Errorf("table:\n%s", buf.String())
		}
	})
}

func mustPath(t *testing.T, g topology.Graph, rnic, ep string) model.Path {
	t.Helper()
	p, ok := g.Path(rnic, ep)
	if !ok {
		t.Fatalf("no path %s->%s", rnic, ep)
	}
	return p
}

// near is within 1% (the simulator's Eq.1 round trip).
func near(got, want uint64) bool {
	return got >= want*99/100 && got <= want*101/100
}

func subHost(name string, fastBW uint64, pairs ...model.RootPortPair) MergeInput {
	b := baseline.PathBaseline{BandwidthBPS: 125}
	if len(pairs) > 0 {
		b.SubClasses = []baseline.SubClass{{Name: "fast", BandwidthBPS: fastBW, RootPortPairs: pairs}}
	}
	return MergeInput{Name: name, Bases: baseline.Uniform(map[model.Distance]baseline.PathBaseline{model.DistanceCrossRoot: b})}
}

// A pair joins the merged sub-class when at least half of the files with the
// distance list it; the files that disagree are named either way.
func TestMergeSubClassMajority(t *testing.T) {
	p1 := model.NewRootPortPair("rp.0000:aa:01.0", "rp.0000:bb:01.0")
	p2 := model.NewRootPortPair("rp.0000:cc:01.0", "rp.0000:dd:01.0")
	p3 := model.NewRootPortPair("rp.0000:aa:01.0", "rp.0000:cc:01.0")
	in := []MergeInput{
		subHost("a", 200, p1, p2),
		subHost("b", 190, p1, p2),
		subHost("c", 180, p1),
		subHost("d", 170, p1, p3),
		// A file without cross_root has no say.
		{Name: "e", Bases: baseline.Uniform(map[model.Distance]baseline.PathBaseline{model.DistanceLocalMem: {BandwidthBPS: 197}})},
	}
	r, err := Merge(in, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	cr := r.Baselines().Distances[model.DistanceCrossRoot]
	if len(cr.SubClasses) != 1 {
		t.Fatalf("cross_root = %+v", cr)
	}
	sc := cr.SubClasses[0]
	// p1: 4/4, p2: 2/4 (kept), p3: 1/4 (left out).
	if !reflect.DeepEqual(sc.RootPortPairs, []model.RootPortPair{p1, p2}) {
		t.Fatalf("merged pairs = %v", sc.RootPortPairs)
	}
	// Sorted 170, 180, 190, 200: p25 = 177.5 -> 178.
	if sc.BandwidthBPS != 178 {
		t.Fatalf("sub-class bandwidth = %d, want 178", sc.BandwidthBPS)
	}
	var buf bytes.Buffer
	r.WriteTable(&buf)
	out := buf.String()
	for _, want := range []string{
		"\ncross_root/fast ",
		"sub-class cross_root/fast: root-port pairs rp.0000:aa:01.0+rp.0000:bb:01.0, rp.0000:cc:01.0+rp.0000:dd:01.0",
		"WARNING: files disagree on root-port pair rp.0000:cc:01.0+rp.0000:dd:01.0 for cross_root/fast: listed by a, b; not by c, d. Kept",
		"WARNING: files disagree on root-port pair rp.0000:aa:01.0+rp.0000:cc:01.0 for cross_root/fast: listed by d; not by a, b, c. Left out",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "rp.0000:aa:01.0+rp.0000:bb:01.0 for") {
		t.Errorf("every file agrees on p1:\n%s", out)
	}

	// A sub-class one file in four carries does not survive.
	r, err = Merge([]MergeInput{subHost("a", 200, p3), subHost("b", 0), subHost("c", 0), subHost("d", 0)}, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	if subs := r.Baselines().Distances[model.DistanceCrossRoot].SubClasses; len(subs) != 0 {
		t.Fatalf("minority sub-class kept: %+v", subs)
	}
}

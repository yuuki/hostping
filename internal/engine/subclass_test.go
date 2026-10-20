package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/calibrate"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// A two-level distance end to end in the simulator: Fig.2 with the P2P route between
// rp.0 and rp.1 at full rate, so cross_root reads at two levels. Calibrate
// learns the fast pair as a sub-class; against that file the healthy host has
// no bottleneck, while a fast path that falls to the slow cross_root level —
// normal against one per-distance baseline — is abnormal.
func TestSubClassCalibrateThenDiagnose(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault("fast_rp:rp.0,rp.1"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	var reps []model.Report
	for range 3 {
		reps = append(reps, runIdle(t, cfg, g, SimProber{World: w}))
	}
	res := calibrate.Summarize(g, reps, calibrate.Options{Latency: true, AbnormalTh: cfg.AbnormalTh})
	data, err := calibrate.MarshalFile(res.Baselines())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "class.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	bases, err := baseline.LoadFile(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	cr := bases.Distances[model.DistanceCrossRoot]
	if len(cr.SubClasses) != 1 || cr.SubClasses[0].Name != "fast" ||
		len(cr.SubClasses[0].RootPortPairs) != 1 || cr.SubClasses[0].RootPortPairs[0] != model.NewRootPortPair("rp.0", "rp.1") {
		t.Fatalf("cross_root sub-classes = %+v\n%s", cr.SubClasses, data)
	}
	slow := cr.BandwidthBPS
	if fast := cr.SubClasses[0].BandwidthBPS; fast <= slow {
		t.Fatalf("fast %d not above slow %d", fast, slow)
	}

	cfg.BaselineFile = path
	rep := runIdle(t, cfg, g, SimProber{World: w})
	if len(rep.Causes) != 0 {
		t.Fatalf("healthy host against its own file: causes %+v", rep.Causes)
	}
	fastPaths := 0
	for _, m := range rep.Paths {
		if m.Status == model.PathAbnormal || m.LatencyStatus == model.LatencyAbnormal {
			t.Fatalf("healthy path judged abnormal: %+v", m)
		}
		if m.BaselineClass == "cross_root/fast" {
			fastPaths++
		}
	}
	if fastPaths != 4 {
		t.Fatalf("%d paths judged as cross_root/fast, want 4 (rnic0->gpu2,gpu3 and rnic1->gpu0,gpu1)", fastPaths)
	}

	// One fast path falls to the slow level: the blind spot of one baseline per distance.
	deg := degradeProber(w, "rnic0", "gpu2", slow)
	noFile := config.Defaults()
	if m := pathOf(t, runIdle(t, noFile, g, deg), "rnic0", "gpu2"); m.Status == model.PathAbnormal {
		t.Fatalf("precondition: without the sub-class the path should hide at the slow level, got %+v", m)
	}
	rep = runIdle(t, cfg, g, deg)
	if m := pathOf(t, rep, "rnic0", "gpu2"); m.Status != model.PathAbnormal || m.BaselineClass != "cross_root/fast" {
		t.Fatalf("degraded fast path = %+v, want abnormal against cross_root/fast", m)
	}
	for _, m := range rep.Paths {
		if m.Status == model.PathAbnormal && (m.RNIC != "rnic0" || m.Endpoint != "gpu2") {
			t.Fatalf("only the degraded path should be abnormal, also %s->%s", m.RNIC, m.Endpoint)
		}
	}
	if len(rep.Causes) == 0 {
		t.Fatal("an abnormal path with no cause")
	}

	// The platform loses the fast route altogether: every fast path drops.
	w.FastPairs = map[model.RootPortPair]bool{}
	rep = runIdle(t, cfg, g, SimProber{World: w})
	for _, ep := range []struct{ rnic, ep string }{{"rnic0", "gpu2"}, {"rnic0", "gpu3"}, {"rnic1", "gpu0"}, {"rnic1", "gpu1"}} {
		if m := pathOf(t, rep, ep.rnic, ep.ep); m.Status != model.PathAbnormal || m.LatencyStatus != model.LatencyAbnormal {
			t.Errorf("%s->%s = %+v, want abnormal bandwidth and latency", ep.rnic, ep.ep, m)
		}
	}
}

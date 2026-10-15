package analyzer_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// A sibling latency verdict is reported only once the same path
// has read abnormal in analyzer.LatencyRuns consecutive idle runs.

// slowOne returns the healthy Fig.2 measurements with the small-message
// latency of rnic->ep raised well past its siblings.
func slowOne(t *testing.T, g topology.Graph, rnic, ep string) []model.PathMeasurement {
	t.Helper()
	ms := simMeasurements(t, g, "")
	found := false
	for i := range ms {
		if ms[i].RNIC == rnic && ms[i].Endpoint == ep {
			ms[i].SmallLatNsP50 += 2000
			found = true
		}
	}
	if !found {
		t.Fatalf("setup: no path %s->%s", rnic, ep)
	}
	return ms
}

// otherLocalMem is a local_mem path on another RNIC and to another endpoint
// than rnic0->mem0.
func otherLocalMem(t *testing.T, g topology.Graph) (string, string) {
	t.Helper()
	for _, p := range g.Paths {
		if p.Distance == model.DistanceLocalMem && p.RNIC != "rnic0" && p.Endpoint != "mem0" {
			return p.RNIC, p.Endpoint
		}
	}
	t.Fatal("setup: no second local_mem path")
	return "", ""
}

type streakRun struct {
	mode    model.RunMode
	ms      []model.PathMeasurement
	oneShot bool
	recheck map[string]model.LatencyRecheck
	bases   baseline.Overlay
}

func runStreak(g topology.Graph, hits map[string]int, r streakRun) analyzer.Result {
	in := analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: r.mode, Measurements: r.ms,
		LatencyHits: hits, OneShot: r.oneShot, LatencyRecheck: r.recheck, Bases: r.bases}
	if r.mode == model.ModeBusy {
		in.BusyRNICs = map[string]bool{"rnic1": true}
	}
	return analyzer.Analyze(in)
}

// A different path flagged in each run is never reported, though each one
// keeps latency_status abnormal with a streak of one.
func TestLatencyStreakDifferentPathEachRun(t *testing.T) {
	g := topology.Fig2()
	r2, e2 := otherLocalMem(t, g)
	r := runStreak(g, nil, streakRun{mode: model.ModeIdle, ms: slowOne(t, g, "rnic0", "mem0")})
	if m := latencyOf(t, r, "rnic0", "mem0"); m.LatencyStatus != model.LatencyAbnormal || m.LatencyStreak != 1 {
		t.Fatalf("first run rnic0->mem0 = %s streak %d, want abnormal streak 1", m.LatencyStatus, m.LatencyStreak)
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Fatalf("first run reported %+v", lc)
	}
	r = runStreak(g, r.LatencyHits, streakRun{mode: model.ModeIdle, ms: slowOne(t, g, r2, e2)})
	if m := latencyOf(t, r, r2, e2); m.LatencyStatus != model.LatencyAbnormal || m.LatencyStreak != 1 {
		t.Fatalf("second run %s->%s = %s streak %d, want abnormal streak 1", r2, e2, m.LatencyStatus, m.LatencyStreak)
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("one-off readings on different paths reported %+v", lc)
	}
	if want := map[string]int{r2 + "->" + e2: 1}; !reflect.DeepEqual(r.LatencyHits, want) {
		t.Errorf("streaks = %v, want %v", r.LatencyHits, want)
	}
}

// The same path is reported once it read abnormal in LatencyRuns consecutive
// idle runs, not before, and the evidence says so.
func TestLatencyStreakSamePathPersists(t *testing.T) {
	g := topology.Fig2()
	ms := slowOne(t, g, "rnic0", "mem0")
	var r analyzer.Result
	for run := 1; run < analyzer.LatencyRuns; run++ {
		r = runStreak(g, r.LatencyHits, streakRun{mode: model.ModeIdle, ms: ms})
		if lc := latencyCauses(r.Causes); len(lc) != 0 {
			t.Fatalf("run %d reported %+v before the streak reached %d", run, lc, analyzer.LatencyRuns)
		}
	}
	r = runStreak(g, r.LatencyHits, streakRun{mode: model.ModeIdle, ms: ms})
	if m := latencyOf(t, r, "rnic0", "mem0"); m.LatencyStreak != analyzer.LatencyRuns {
		t.Errorf("streak = %d, want %d", m.LatencyStreak, analyzer.LatencyRuns)
	}
	lc := latencyCauses(r.Causes)
	if len(lc) != 1 || !reflect.DeepEqual(lc[0].RNICs, []string{"rnic0"}) || !reflect.DeepEqual(lc[0].Endpoints, []string{"mem0"}) {
		t.Fatalf("causes %+v, want one naming rnic0->mem0", lc)
	}
	if !strings.Contains(lc[0].Evidence, fmt.Sprintf("%d consecutive idle runs", analyzer.LatencyRuns)) {
		t.Errorf("evidence %q does not carry the streak", lc[0].Evidence)
	}
}

// An idle run that judges the path normal, or cannot judge it, resets its
// streak; so does one that does not measure it.
func TestLatencyStreakResets(t *testing.T) {
	g := topology.Fig2()
	slow := slowOne(t, g, "rnic0", "mem0")
	healthy := simMeasurements(t, g, "")
	noReading := slowOne(t, g, "rnic0", "mem0")
	var missing []model.PathMeasurement
	for i := range noReading {
		if noReading[i].RNIC == "rnic0" && noReading[i].Endpoint == "mem0" {
			noReading[i].SmallLatNsP50 = 0
			continue
		}
		missing = append(missing, noReading[i])
	}
	for _, tc := range []struct {
		name string
		ms   []model.PathMeasurement
	}{
		{"normal", healthy},
		{"no reading", noReading},
		{"not measured", missing},
	} {
		var r analyzer.Result
		for range analyzer.LatencyRuns - 1 {
			r = runStreak(g, r.LatencyHits, streakRun{mode: model.ModeIdle, ms: slow})
		}
		r = runStreak(g, r.LatencyHits, streakRun{mode: model.ModeIdle, ms: tc.ms})
		if len(r.LatencyHits) != 0 {
			t.Errorf("%s: streaks = %v, want none", tc.name, r.LatencyHits)
		}
		for range analyzer.LatencyRuns - 1 {
			r = runStreak(g, r.LatencyHits, streakRun{mode: model.ModeIdle, ms: slow})
			if lc := latencyCauses(r.Causes); len(lc) != 0 {
				t.Errorf("%s: a broken streak was reported: %+v", tc.name, lc)
			}
		}
	}
}

// A busy run judges no latency and leaves every streak as it was, so the next
// idle run continues it.
func TestLatencyStreakBusyKeeps(t *testing.T) {
	g := topology.Fig2()
	slow := slowOne(t, g, "rnic0", "mem0")
	r := runStreak(g, nil, streakRun{mode: model.ModeIdle, ms: slow})
	hits := r.LatencyHits
	r = runStreak(g, hits, streakRun{mode: model.ModeBusy, ms: simMeasurements(t, g, "")})
	if !reflect.DeepEqual(r.LatencyHits, hits) {
		t.Fatalf("busy run changed streaks %v -> %v", hits, r.LatencyHits)
	}
	for range analyzer.LatencyRuns - 1 {
		r = runStreak(g, r.LatencyHits, streakRun{mode: model.ModeIdle, ms: slow})
	}
	if lc := latencyCauses(r.Causes); len(lc) != 1 {
		t.Errorf("streak across a busy run: causes %+v, want one", lc)
	}
}

// A verdict against a user baseline needs no streak: it is reported on the
// first run, and keeps no streak.
func TestLatencyStreakNotNeededForUserBaseline(t *testing.T) {
	g := topology.Fig2()
	bases := baseline.Uniform(map[model.Distance]baseline.PathBaseline{model.DistanceLocalMem: {LatencyNs: 1000}})
	r := runStreak(g, nil, streakRun{mode: model.ModeIdle, ms: withLat(t, g, 1500), bases: bases})
	m := latencyOf(t, r, "rnic0", "mem0")
	if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyByBaseline || m.LatencyStreak != 0 {
		t.Fatalf("rnic0->mem0 = %s/%s streak %d, want abnormal/user_baseline, no streak", m.LatencyStatus, m.LatencyReason, m.LatencyStreak)
	}
	if lc := latencyCauses(r.Causes); len(lc) != 1 || lc[0].Confidence != "medium" {
		t.Errorf("causes %+v, want one at medium confidence", lc)
	}
	if len(r.LatencyHits) != 0 {
		t.Errorf("streaks = %v, want none", r.LatencyHits)
	}
}

// A one-shot run carries no streaks: a sibling verdict the paired recheck
// confirmed is reported, one it did not reach is not.
func TestLatencyStreakOneShot(t *testing.T) {
	g := topology.Fig2()
	ms := slowOne(t, g, "rnic0", "mem0")
	cfg := config.Defaults()

	r := runStreak(g, nil, streakRun{mode: model.ModeIdle, ms: ms, oneShot: true})
	if m := latencyOf(t, r, "rnic0", "mem0"); m.LatencyStatus != model.LatencyAbnormal {
		t.Fatalf("setup: rnic0->mem0 %s, want abnormal", m.LatencyStatus)
	}
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("unconfirmed one-shot verdict reported %+v", lc)
	}

	rc := map[string]model.LatencyRecheck{"rnic0->mem0": analyzer.JudgeLatencyRecheck([]uint64{3000, 3000, 3000}, []uint64{1000, 1000, 1000}, cfg)}
	r = runStreak(g, nil, streakRun{mode: model.ModeIdle, ms: ms, oneShot: true, recheck: rc})
	lc := latencyCauses(r.Causes)
	if len(lc) != 1 || !reflect.DeepEqual(lc[0].Endpoints, []string{"mem0"}) {
		t.Fatalf("confirmed one-shot verdict: causes %+v, want one naming mem0", lc)
	}
	if !strings.Contains(lc[0].Evidence, "one-shot run") {
		t.Errorf("evidence %q does not say it rests on one run", lc[0].Evidence)
	}

	// The same confirmed reading in a daemon's first run waits for more runs.
	r = runStreak(g, nil, streakRun{mode: model.ModeIdle, ms: ms, recheck: rc})
	if lc := latencyCauses(r.Causes); len(lc) != 0 {
		t.Errorf("daemon's first run reported %+v", lc)
	}
}

// Analyze copies the streaks it is given rather than updating them in place.
func TestLatencyStreakInputUnchanged(t *testing.T) {
	g := topology.Fig2()
	hits := map[string]int{"rnic0->mem0": 1}
	runStreak(g, hits, streakRun{mode: model.ModeIdle, ms: slowOne(t, g, "rnic0", "mem0")})
	runStreak(g, hits, streakRun{mode: model.ModeBusy, ms: simMeasurements(t, g, "")})
	if !reflect.DeepEqual(hits, map[string]int{"rnic0->mem0": 1}) {
		t.Errorf("Analyze changed its input streaks: %v", hits)
	}
}

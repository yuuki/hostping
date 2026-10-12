package analyzer_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// simMeasurements probes every Fig.2 path in the simulated world after fault,
// which is where the verb timings come from.
func simMeasurements(t *testing.T, g topology.Graph, fault string) []model.PathMeasurement {
	t.Helper()
	w := sim.New(g)
	if err := w.ApplyFault(fault); err != nil {
		t.Fatal(err)
	}
	var ms []model.PathMeasurement
	for _, n := range g.RNICs {
		for _, ep := range g.Endpoints {
			if _, ok := g.Path(n.ID, ep.ID); !ok {
				continue
			}
			m, err := w.Loopback(n.ID, ep.ID, 1, 128*1024)
			if err != nil {
				t.Fatal(err)
			}
			ms = append(ms, m)
		}
	}
	return ms
}

func slowMRCauses(causes []model.RootCause) []model.RootCause {
	var out []model.RootCause
	for _, c := range causes {
		if c.Code == model.CauseGPUMRRegistrationSlow {
			out = append(out, c)
		}
	}
	return out
}

// analyzeIdle analyzes ms as analyzer.LatencyRuns consecutive idle runs that
// read the same and returns the last: the steady state, past the runs
// gpu_mr_registration_slow and a sibling path_latency_abnormal need before
// they report.
func analyzeIdle(g topology.Graph, ms []model.PathMeasurement) analyzer.Result {
	var r analyzer.Result
	for range analyzer.LatencyRuns {
		r = analyzer.Analyze(analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: ms,
			SlowMRHits: r.SlowMRHits, LatencyHits: r.LatencyHits})
	}
	return r
}

// analyzeOnce is one run that starts from the slow-registration streaks hits.
func analyzeOnce(g topology.Graph, mode model.RunMode, ms []model.PathMeasurement, hits map[string]int) analyzer.Result {
	return analyzer.Analyze(analyzer.Input{Graph: g, Cfg: config.Defaults(), Mode: mode, Measurements: ms, SlowMRHits: hits})
}

// A GPU whose registration is far slower than its siblings' is reported, and
// only that GPU. It is slow from every RNIC that reached it, so the finding is
// medium rather than low confidence.
func TestSlowMROutlierReported(t *testing.T) {
	g := topology.Fig2()
	got := slowMRCauses(analyzeIdle(g, simMeasurements(t, g, "slow_mr:gpu3")).Causes)
	if len(got) != 1 {
		t.Fatalf("want 1 %s cause, got %+v", model.CauseGPUMRRegistrationSlow, got)
	}
	if !reflect.DeepEqual(got[0].Endpoints, []string{"gpu3"}) {
		t.Errorf("endpoints = %v, want [gpu3]", got[0].Endpoints)
	}
	if got[0].Confidence != "medium" {
		t.Errorf("confidence = %s, want medium", got[0].Confidence)
	}
}

// The finding is advisory: path statuses, uncertain reasons and link states are
// exactly what the same measurements give without the slow registration.
func TestSlowMRDoesNotChangeStatus(t *testing.T) {
	g := topology.Fig2()
	healthy := analyzeIdle(g, simMeasurements(t, g, ""))
	slow := analyzeIdle(g, simMeasurements(t, g, "slow_mr:gpu3"))
	for i := range healthy.Paths {
		h, s := healthy.Paths[i], slow.Paths[i]
		if h.Status != s.Status || h.UncertainReason != s.UncertainReason {
			t.Errorf("%s->%s: status %s/%s became %s/%s", h.RNIC, h.Endpoint, h.Status, h.UncertainReason, s.Status, s.UncertainReason)
		}
	}
	if !reflect.DeepEqual(healthy.Links, slow.Links) {
		t.Errorf("links changed:\n healthy %+v\n    slow %+v", healthy.Links, slow.Links)
	}
}

func TestSlowMRUniformNotReported(t *testing.T) {
	g := topology.Fig2()
	if got := slowMRCauses(analyzeIdle(g, simMeasurements(t, g, "")).Causes); len(got) != 0 {
		t.Fatalf("uniform registration times reported: %+v", got)
	}
}

func gpuMeas(rnic, ep string, mrNs uint64) model.PathMeasurement {
	return model.PathMeasurement{RNIC: rnic, Endpoint: ep, EndpointKind: model.EndpointGPU, BandwidthBPS: 190_000_000_000, SmallLatNsP50: 2000, Verbs: model.VerbNs{MRReg: mrNs}}
}

// Three GPUs leave the outlier two siblings: not enough to call a median.
func TestSlowMRTooFewSiblings(t *testing.T) {
	g := topology.Fig2()
	ms := []model.PathMeasurement{
		gpuMeas("rnic0", "gpu0", 150_000),
		gpuMeas("rnic0", "gpu1", 150_000),
		gpuMeas("rnic1", "gpu2", 50_000_000),
	}
	if got := slowMRCauses(analyzeIdle(g, ms).Causes); len(got) != 0 {
		t.Fatalf("judged with two siblings: %+v", got)
	}
	// A third sibling is enough.
	ms = append(ms, gpuMeas("rnic1", "gpu3", 150_000))
	if got := slowMRCauses(analyzeIdle(g, ms).Causes); len(got) != 1 {
		t.Fatalf("want 1 cause with three siblings, got %+v", got)
	}
}

// Many times the median on a µs-scale value is jitter, not a failing GPU.
func TestSlowMRBelowFloorNotReported(t *testing.T) {
	g := topology.Fig2()
	ms := []model.PathMeasurement{
		gpuMeas("rnic0", "gpu0", 20_000),
		gpuMeas("rnic0", "gpu1", 20_000),
		gpuMeas("rnic1", "gpu2", 20_000),
		gpuMeas("rnic1", "gpu3", 400_000), // 20× but only 0.38 ms over
	}
	if got := slowMRCauses(analyzeIdle(g, ms).Causes); len(got) != 0 {
		t.Fatalf("sub-floor excess reported: %+v", got)
	}
}

// A single slow reading of a GPU probed from several RNICs is summarised away
// by its median; slow on one probe only is low confidence.
func TestSlowMRConfidence(t *testing.T) {
	g := topology.Fig2()
	base := []model.PathMeasurement{
		gpuMeas("rnic0", "gpu0", 150_000),
		gpuMeas("rnic0", "gpu1", 150_000),
		gpuMeas("rnic1", "gpu2", 150_000),
	}
	one := append(append([]model.PathMeasurement{}, base...), gpuMeas("rnic1", "gpu3", 6_000_000))
	got := slowMRCauses(analyzeIdle(g, one).Causes)
	if len(got) != 1 || got[0].Confidence != "low" {
		t.Fatalf("single slow reading: want one low-confidence cause, got %+v", got)
	}
	mixed := append(append([]model.PathMeasurement{}, one...),
		gpuMeas("rnic0", "gpu3", 150_000), gpuMeas("rnic2", "gpu3", 150_000))
	if got := slowMRCauses(analyzeIdle(g, mixed).Causes); len(got) != 0 {
		t.Fatalf("one slow probe out of three reported: %+v", got)
	}
}

// busyProbeMR gives each GPU the single reading busy mode's reduced probe set
// yields: one probe from its affinitive RNIC. gpu3 took several times the
// others, as a healthy GPU's registration can while the host is saturated.
func busyProbeMR() []model.PathMeasurement {
	return []model.PathMeasurement{
		gpuMeas("rnic0", "gpu0", 700_000),
		gpuMeas("rnic0", "gpu1", 700_000),
		gpuMeas("rnic1", "gpu2", 700_000),
		gpuMeas("rnic1", "gpu3", 3_500_000),
	}
}

// Busy mode judges no registration time: the workload's contention, not the
// GPU, sets it, and one reading per GPU leaves no median to absorb that.
func TestSlowMRBusyModeNotReported(t *testing.T) {
	g := topology.Fig2()
	r := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeBusy,
		BusyRNICs:    map[string]bool{"rnic0": true, "rnic1": true},
		Measurements: busyProbeMR(),
	})
	if got := slowMRCauses(r.Causes); len(got) != 0 {
		t.Fatalf("busy run reported %+v", got)
	}
	// Even a GPU slow from every RNIC is left to idle runs.
	r = analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeBusy,
		Measurements: simMeasurements(t, g, "slow_mr:gpu3"),
	})
	if got := slowMRCauses(r.Causes); len(got) != 0 {
		t.Fatalf("busy run reported %+v", got)
	}
}

// The same timings on an idle run are judged: the gate is the mode, not the
// data. A GPU slow from every RNIC is reported at medium confidence.
func TestSlowMRIdleModeStillReported(t *testing.T) {
	g := topology.Fig2()
	got := slowMRCauses(analyzeIdle(g, busyProbeMR()).Causes)
	if len(got) != 1 || got[0].Confidence != "low" {
		t.Fatalf("idle single reading: want one low-confidence cause, got %+v", got)
	}
	var ms []model.PathMeasurement
	for _, n := range g.RNICs {
		for _, gpu := range []string{"gpu0", "gpu1", "gpu2", "gpu3"} {
			mr := uint64(700_000)
			if gpu == "gpu3" {
				mr = 5_000_000
			}
			ms = append(ms, gpuMeas(n.ID, gpu, mr))
		}
	}
	got = slowMRCauses(analyzeIdle(g, ms).Causes)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Endpoints, []string{"gpu3"}) || got[0].Confidence != "medium" {
		t.Fatalf("idle slow GPU: want one medium-confidence cause on gpu3, got %+v", got)
	}
}

// fourProbeGPU3 gives gpu0..gpu2 healthy registrations from every Fig.2 RNIC
// and gpu3 the given readings, one per RNIC in order.
func fourProbeGPU3(gpu3 ...uint64) []model.PathMeasurement {
	rnics := []string{"rnic0", "rnic1", "rnic2", "rnic3"}
	var ms []model.PathMeasurement
	for i, n := range rnics {
		for _, gpu := range []string{"gpu0", "gpu1", "gpu2"} {
			ms = append(ms, gpuMeas(n, gpu, 500_000))
		}
		if i < len(gpu3) {
			ms = append(ms, gpuMeas(n, "gpu3", gpu3[i]))
		}
	}
	return ms
}

// Healthy GPUs register slowly now and then, one reading at a time. Two such
// spikes on the same GPU out of four readings do not move its lower median,
// where a plain median would average them into a slow GPU; a GPU slow on three
// of its four probes still crosses.
func TestSlowMRSporadicSpikesDoNotCross(t *testing.T) {
	g := topology.Fig2()
	two := fourProbeGPU3(500_000, 20_000_000, 500_000, 20_000_000)
	if r := analyzeIdle(g, two); len(slowMRCauses(r.Causes)) != 0 || len(r.SlowMRHits) != 0 {
		t.Fatalf("two spikes of four reported: causes=%+v hits=%v", slowMRCauses(r.Causes), r.SlowMRHits)
	}
	three := fourProbeGPU3(500_000, 20_000_000, 20_000_000, 20_000_000)
	got := slowMRCauses(analyzeIdle(g, three).Causes)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Endpoints, []string{"gpu3"}) || got[0].Confidence != "low" {
		t.Fatalf("three slow probes of four: want one low-confidence cause on gpu3, got %+v", got)
	}
	if !strings.Contains(got[0].Evidence, "lower median of 4 probes") {
		t.Errorf("evidence %q does not name the statistic and sample count", got[0].Evidence)
	}
}

// The same GPU must cross in two consecutive idle runs: one run only starts
// its streak, the second reports it, and an idle run in between that judges
// the GPU healthy starts the count over.
func TestSlowMRNeedsTwoConsecutiveIdleRuns(t *testing.T) {
	g := topology.Fig2()
	slow := fourProbeGPU3(20_000_000, 20_000_000, 20_000_000, 20_000_000)
	healthy := fourProbeGPU3(500_000, 500_000, 500_000, 500_000)

	r1 := analyzeOnce(g, model.ModeIdle, slow, nil)
	if got := slowMRCauses(r1.Causes); len(got) != 0 {
		t.Fatalf("reported after one run: %+v", got)
	}
	if r1.SlowMRHits["gpu3"] != 1 || len(r1.SlowMRHits) != 1 {
		t.Fatalf("hits after one run = %v, want gpu3:1", r1.SlowMRHits)
	}
	r2 := analyzeOnce(g, model.ModeIdle, slow, r1.SlowMRHits)
	got := slowMRCauses(r2.Causes)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Endpoints, []string{"gpu3"}) || got[0].Confidence != "medium" {
		t.Fatalf("second consecutive run: want one medium-confidence cause on gpu3, got %+v", got)
	}
	if !strings.Contains(got[0].Evidence, "crossed in 2 consecutive idle runs") {
		t.Errorf("evidence %q does not say how many runs crossed", got[0].Evidence)
	}
	if r1.SlowMRHits["gpu3"] != 1 {
		t.Errorf("Analyze changed its input streaks: %v", r1.SlowMRHits)
	}

	// Interrupted: slow, healthy, slow does not report.
	r := analyzeOnce(g, model.ModeIdle, slow, nil)
	r = analyzeOnce(g, model.ModeIdle, healthy, r.SlowMRHits)
	if len(r.SlowMRHits) != 0 {
		t.Fatalf("a healthy idle run kept the streak: %v", r.SlowMRHits)
	}
	r = analyzeOnce(g, model.ModeIdle, slow, r.SlowMRHits)
	if got := slowMRCauses(r.Causes); len(got) != 0 || r.SlowMRHits["gpu3"] != 1 {
		t.Fatalf("interrupted streak: causes=%+v hits=%v", got, r.SlowMRHits)
	}
}

// Busy runs do not judge registration, so they neither extend nor reset a
// streak: slow, busy, busy, slow is two consecutive idle runs.
func TestSlowMRBusyRunKeepsStreak(t *testing.T) {
	g := topology.Fig2()
	slow := fourProbeGPU3(20_000_000, 20_000_000, 20_000_000, 20_000_000)
	healthy := fourProbeGPU3(500_000, 500_000, 500_000, 500_000)
	r := analyzeOnce(g, model.ModeIdle, slow, nil)
	for _, ms := range [][]model.PathMeasurement{slow, healthy} {
		b := analyzeOnce(g, model.ModeBusy, ms, r.SlowMRHits)
		if !reflect.DeepEqual(b.SlowMRHits, r.SlowMRHits) {
			t.Fatalf("busy run changed the streaks %v -> %v", r.SlowMRHits, b.SlowMRHits)
		}
		r = b
	}
	if got := slowMRCauses(analyzeOnce(g, model.ModeIdle, slow, r.SlowMRHits).Causes); len(got) != 1 {
		t.Fatalf("slow, busy, busy, slow: want one cause, got %+v", got)
	}
}

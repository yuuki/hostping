package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// A GPU whose compute mode is not Default is never touched, and says so the
// way every other skip does: in the report, in the per-kind count behind
// hostping.endpoint.skipped, and in the engine's log line. The other GPUs and
// every memory path are probed and diagnosed as usual.
func TestEngineSkipsNonDefaultComputeModeGPUs(t *testing.T) {
	g := topology.Fig2()
	p := &flatProber{}
	var sink logSink
	e := New(config.Defaults(), g, p)
	e.Logf = sink.logf

	rep := runWith(t, e, RunRequest{
		Mode: model.ModeIdle,
		GPUComputeMode: map[string]string{
			"gpu0": model.ComputeModeExclusiveProcess,
			"gpu1": model.ComputeModeProhibited,
			"gpu2": model.ComputeModeDefault,
		},
	})
	for _, id := range []string{"gpu0", "gpu1"} {
		if n := p.byEndpoint(id); n != 0 {
			t.Errorf("%s probed %d times, want 0", id, n)
		}
	}
	for _, id := range []string{"gpu2", "gpu3", "mem0"} {
		if p.byEndpoint(id) == 0 {
			t.Errorf("%s was not probed", id)
		}
	}
	var memPaths, gpu2Paths int
	for _, ph := range rep.Paths {
		switch {
		case ph.Endpoint == "gpu0" || ph.Endpoint == "gpu1":
			t.Fatalf("skipped GPU %s has a path in the report", ph.Endpoint)
		case ph.EndpointKind == model.EndpointMemory:
			memPaths++
		case ph.Endpoint == "gpu2":
			gpu2Paths++
		}
	}
	if memPaths == 0 || gpu2Paths == 0 {
		t.Fatalf("memory paths=%d gpu2 paths=%d, want both diagnosed", memPaths, gpu2Paths)
	}

	reasons := map[string]model.EndpointSkip{}
	for _, s := range rep.SkippedEndpoints {
		reasons[s.Reason] = s
	}
	for reason, ep := range map[string]string{
		"compute_mode: Exclusive_Process": "gpu0",
		"compute_mode: Prohibited":        "gpu1",
	} {
		s, ok := reasons[reason]
		if !ok {
			t.Fatalf("no skip with reason %q in %+v", reason, rep.SkippedEndpoints)
		}
		if s.Kind != model.EndpointGPU || len(s.Endpoints) != 1 || s.Endpoints[0] != ep {
			t.Errorf("skip %q = %+v, want gpu %s", reason, s, ep)
		}
		if s.Probes != len(g.RNICs) {
			t.Errorf("skip %q probes = %d, want one per RNIC (%d)", reason, s.Probes, len(g.RNICs))
		}
	}
	if len(rep.SkippedEndpoints) != 2 {
		t.Errorf("want exactly the two compute-mode skips, got %+v", rep.SkippedEndpoints)
	}
	if got := rep.SkippedEndpointCount(model.EndpointGPU); got != 2 {
		t.Errorf("skipped gpu endpoints = %d, want 2", got)
	}
	if got := rep.SkippedEndpointCount(model.EndpointMemory); got != 0 {
		t.Errorf("skipped memory endpoints = %d, want 0", got)
	}
	if len(sink.lines) != 1 || !strings.Contains(sink.lines[0], "compute_mode: Exclusive_Process") ||
		!strings.Contains(sink.lines[0], "compute_mode: Prohibited") {
		t.Fatalf("skip log = %v, want one line naming both compute modes", sink.lines)
	}
}

// A compute mode that was not read (no nvidia-smi, an AMD GPU) or not
// recognised must not hold a GPU back: the run probes it exactly as before.
func TestEngineProbesGPUsWithUnknownComputeMode(t *testing.T) {
	for name, modes := range map[string]map[string]string{
		"none":    nil,
		"empty":   {"gpu0": ""},
		"garbage": {"gpu0": "[N/A]", "gpu1": "exclusive process"},
		"default": {"gpu0": model.ComputeModeDefault},
	} {
		t.Run(name, func(t *testing.T) {
			p := &flatProber{}
			rep := runWith(t, New(config.Defaults(), topology.Fig2(), p), RunRequest{Mode: model.ModeIdle, GPUComputeMode: modes})
			if len(rep.SkippedEndpoints) != 0 {
				t.Fatalf("skips = %+v, want none", rep.SkippedEndpoints)
			}
			if p.byEndpoint("gpu0") == 0 || p.byEndpoint("gpu1") == 0 {
				t.Fatalf("probed = %v, want gpu0 and gpu1 probed", p.calls)
			}
		})
	}
}

// Only GPU endpoints have a compute mode: a mode keyed by a memory endpoint's
// ID (which nothing produces, but nothing prevents) is ignored.
func TestComputeModeSkipIgnoresMemoryEndpoints(t *testing.T) {
	modes := map[string]string{"mem0": model.ComputeModeProhibited, "gpu0": model.ComputeModeExclusiveProcess}
	if err := computeModeSkip(model.Endpoint{ID: "mem0", Kind: model.EndpointMemory}, modes); err != nil {
		t.Fatalf("memory endpoint skipped: %v", err)
	}
	err := computeModeSkip(model.Endpoint{ID: "gpu0", Kind: model.EndpointGPU}, modes)
	if !errors.Is(err, model.ErrComputeMode) {
		t.Fatalf("err = %v, want ErrComputeMode", err)
	}
}

// The simulated world's compute_mode fault reaches the engine the way the
// monitor's reading does, so --sim exercises the skip end to end.
func TestEngineSimComputeModeFault(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault("compute_mode:gpu3=Prohibited"); err != nil {
		t.Fatal(err)
	}
	e := quietEngine(config.Defaults(), g, SimProber{World: w})
	rep := runWith(t, e, RunRequest{Mode: model.ModeIdle, GPUComputeMode: w.ComputeMode})
	if len(rep.SkippedEndpoints) != 1 || rep.SkippedEndpoints[0].Endpoints[0] != "gpu3" ||
		rep.SkippedEndpoints[0].Reason != "compute_mode: Prohibited" {
		t.Fatalf("skips = %+v, want gpu3 compute_mode: Prohibited", rep.SkippedEndpoints)
	}
	if len(rep.Causes) != 0 {
		t.Fatalf("a skipped healthy GPU must not be diagnosed: %+v", rep.Causes)
	}
}

// releaseProber counts ReleaseGPU calls.
type releaseProber struct {
	stubProber
	released int
}

func (p *releaseProber) ReleaseGPU() { p.released++ }

// The run hands back GPU contexts once at its end, also when it fails, so an
// idle daemon holds none between runs.
func TestEngineReleasesGPUAfterEveryRun(t *testing.T) {
	p := &releaseProber{}
	e := quietEngine(config.Defaults(), topology.Fig2(), p)
	runWith(t, e, RunRequest{Mode: model.ModeIdle})
	if p.released != 1 {
		t.Fatalf("released %d times after one run, want 1", p.released)
	}

	p.loop = func(model.RNIC, model.Endpoint) (model.PathMeasurement, error) {
		return model.PathMeasurement{}, errors.New("boom")
	}
	if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err == nil {
		t.Fatal("want the probe error")
	}
	if p.released != 2 {
		t.Fatalf("released %d times after a failed run, want 2", p.released)
	}
}

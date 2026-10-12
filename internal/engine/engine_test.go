package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

type stubProber struct {
	loop func(model.RNIC, model.Endpoint) (model.PathMeasurement, error)
	mut  func(model.RNIC, model.RNIC) (model.MutualMeasurement, error)
}

func (s stubProber) Loopback(_ context.Context, n model.RNIC, ep model.Endpoint, _, _ uint32) (model.PathMeasurement, error) {
	if s.loop != nil {
		return s.loop(n, ep)
	}
	return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000}, nil
}

func (s stubProber) Mutual(_ context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	if s.mut != nil {
		return s.mut(src, dst)
	}
	return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: true}, nil
}

func TestEngineSkipsNoGDRAndNoDevice(t *testing.T) {
	g := topology.Fig2()
	e := New(config.Defaults(), g, stubProber{
		loop: func(n model.RNIC, ep model.Endpoint) (model.PathMeasurement, error) {
			if ep.Kind == model.EndpointGPU {
				return model.PathMeasurement{}, model.ErrNoGDR
			}
			if n.ID == "rnic3" {
				return model.PathMeasurement{}, model.ErrNoDevice
			}
			return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000}, nil
		},
		mut: func(_, _ model.RNIC) (model.MutualMeasurement, error) {
			return model.MutualMeasurement{}, model.ErrNoGID
		},
	})
	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Paths) == 0 {
		t.Fatal("memory paths should remain after GDR skip")
	}
	for _, p := range rep.Paths {
		if p.Endpoint == "gpu0" {
			t.Fatal("GPU path should have been skipped")
		}
	}
}

// A mutual probe that fails to set up (missing device, no GID, allocation)
// must be skipped, not turned into a connectivity root cause.
func TestEngineSkipsMutualSetupFailure(t *testing.T) {
	g := topology.Fig2()
	e := New(config.Defaults(), g, stubProber{
		mut: func(model.RNIC, model.RNIC) (model.MutualMeasurement, error) {
			return model.MutualMeasurement{}, model.ErrNoDevice
		},
	})
	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Mutual) != 0 {
		t.Fatalf("mutual setup failure must be skipped, got %+v", rep.Mutual)
	}
	if hasCause(rep, model.CauseRNICConnectFailure) {
		t.Fatalf("a missing device is not a connectivity failure: %v", causeCodes(rep))
	}
}

func TestEngineFailsClosedWhenEveryPathSkips(t *testing.T) {
	g := topology.Fig2()
	e := New(config.Defaults(), g, stubProber{
		loop: func(model.RNIC, model.Endpoint) (model.PathMeasurement, error) {
			return model.PathMeasurement{}, model.ErrNoDevice
		},
	})
	_, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err == nil || !errors.Is(err, model.ErrNoDevice) {
		t.Fatalf("want ErrNoDevice wrap, got %v", err)
	}
}

// A healthy 400G host must need no flags. The RNICs are 400G behind the Fig.2
// x16 hops, the shape of a current 400G host: host-DRAM loopback lands at
// ~270 Gb/s and 0.985x400G would ask for 394 Gb/s. Judged against the wire,
// every memory path went abnormal and every RNIC was reported as a failed
// PCIe link at high confidence.
func TestEngineIdle400GHealthyDRAMNeedsNoBaselineFlags(t *testing.T) {
	g := topology.Fig2()
	for i := range g.RNICs {
		g.RNICs[i].LineRate = 400_000_000_000
	}
	loop := func(n model.RNIC, ep model.Endpoint) (model.PathMeasurement, error) {
		if ep.Kind == model.EndpointGPU {
			return model.PathMeasurement{}, model.ErrNoGDR
		}
		return model.PathMeasurement{
			RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 270_000_000_000, SmallLatNsP50: 1400,
		}, nil
	}

	e := New(config.Defaults(), g, stubProber{loop: loop})
	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Causes) != 0 {
		t.Fatalf("healthy 400G DRAM must not be diagnosed with no flags: %v", causeCodes(rep))
	}
	for _, p := range rep.Paths {
		if p.Status == model.PathAbnormal {
			t.Fatalf("abnormal path on a healthy host: %+v", p)
		}
	}
	for _, tab := range rep.Baselines {
		if tab.LineRateBPS != 400_000_000_000 {
			t.Fatalf("reported line rate must stay the wire speed: %d", tab.LineRateBPS)
		}
		if tab.DerivedFromBPS != 252_000_000_000 {
			t.Fatalf("baseline should derive from the PCIe hop: %d", tab.DerivedFromBPS)
		}
		for _, entry := range tab.Entries {
			if entry.BandwidthSource != model.BaselineSourceDefault || entry.LatencySource != model.BaselineSourceDefault {
				t.Fatalf("no flags were passed, so every field is a default: %+v", entry)
			}
		}
	}

	// An explicit floor still wins, in both directions. Above the bus it marks
	// the same healthy paths abnormal and diagnoses #1 as before.
	cfg := config.Defaults()
	cfg.BaselineSpecs = []string{"local_mem=394Gbps", "remote_mem=394Gbps"}
	e = New(cfg, g, stubProber{loop: loop})
	rep, err = e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if !hasCause(rep, model.CauseRNICPCIeFailure) {
		t.Fatalf("an explicit 394Gbps floor must still diagnose #1 on 270G DRAM, got %v", causeCodes(rep))
	}

	cfg = config.Defaults()
	cfg.BaselineSpecs = []string{"local_mem=270Gbps", "remote_mem=270Gbps"}
	e = New(cfg, g, stubProber{loop: loop})
	rep, err = e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Causes) != 0 {
		t.Fatalf("user 270G baseline must not mark DRAM paths abnormal: %v", causeCodes(rep))
	}
	for _, p := range rep.Paths {
		if p.Status == model.PathAbnormal {
			t.Fatalf("abnormal path %+v", p)
		}
	}
	if len(rep.Baselines) == 0 {
		t.Fatal("diagnose report must include effective baselines")
	}
	var sawLocalUser, sawRemoteUser, sawSwitchDefault bool
	for _, tab := range rep.Baselines {
		if tab.LineRateBPS != 400_000_000_000 {
			t.Fatalf("line rate %d", tab.LineRateBPS)
		}
		for _, e := range tab.Entries {
			switch {
			case e.Distance == "local_mem" && e.BandwidthSource == model.BaselineSourceUser && e.BandwidthBPS == 270_000_000_000:
				sawLocalUser = true
			case e.Distance == "remote_mem" && e.BandwidthSource == model.BaselineSourceUser:
				sawRemoteUser = true
			case e.Distance == "same_switch" && e.BandwidthSource == model.BaselineSourceDefault:
				sawSwitchDefault = true
			}
		}
	}
	if !sawLocalUser || !sawRemoteUser || !sawSwitchDefault {
		t.Fatalf("effective table missing user/default tags: %+v", rep.Baselines)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "baselines.json")
	if err := os.WriteFile(p, []byte(`{"local_mem":{"bandwidth":"394Gbps"},"remote_mem":{"bandwidth":"394Gbps"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = config.Defaults()
	cfg.BaselineFile = p
	e = New(cfg, g, stubProber{loop: loop})
	rep, err = e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if !hasCause(rep, model.CauseRNICPCIeFailure) {
		t.Fatalf("baseline-file must apply over the derived default: %v", causeCodes(rep))
	}
}

func TestEngineBaselineFileErrorBeforeProbe(t *testing.T) {
	calls := 0
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.BaselineFile = filepath.Join(t.TempDir(), "missing.json")
	e := New(cfg, g, stubProber{
		loop: func(model.RNIC, model.Endpoint) (model.PathMeasurement, error) {
			calls++
			return model.PathMeasurement{}, nil
		},
	})
	_, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err == nil {
		t.Fatal("missing baseline file must fail")
	}
	if calls != 0 {
		t.Fatalf("probed %d times before file error", calls)
	}
}

func hasCause(rep model.Report, code string) bool {
	for _, c := range rep.Causes {
		if c.Code == code {
			return true
		}
	}
	return false
}

func causeCodes(rep model.Report) []string {
	var out []string
	for _, c := range rep.Causes {
		out = append(out, c.Code)
	}
	return out
}

func TestEngineNoMeasurementsSurfacesMemorySkip(t *testing.T) {
	g := topology.Fig2()
	e := New(config.Defaults(), g, stubProber{
		loop: func(_ model.RNIC, ep model.Endpoint) (model.PathMeasurement, error) {
			if ep.Kind == model.EndpointGPU {
				return model.PathMeasurement{}, model.ErrNoGDR
			}
			return model.PathMeasurement{}, model.ErrNoGID
		},
	})
	_, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err == nil {
		t.Fatal("expected empty-mesh error")
	}
	if !errors.Is(err, model.ErrNoGID) {
		t.Fatalf("memory skip hidden behind NoGDR: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "memory") || !strings.Contains(got, "gpu") {
		t.Fatalf("want both endpoint kinds in summary, got %s", got)
	}
}

// logSink captures the aggregated skip line the engine would otherwise write
// through the standard logger.
type logSink struct{ lines []string }

func (s *logSink) logf(format string, args ...any) {
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

// gdrProber fails every GPU endpoint with the probe's own NoGDR detail string
// and answers every memory endpoint, which is a GPU host whose CUDA libraries
// are missing: the mesh shrinks to the memory paths and nothing else changes.
func gdrProber(detail string) stubProber {
	return stubProber{
		loop: func(n model.RNIC, ep model.Endpoint) (model.PathMeasurement, error) {
			if ep.Kind == model.EndpointGPU {
				return model.PathMeasurement{}, fmt.Errorf("%w: %s", model.ErrNoGDR, detail)
			}
			return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000}, nil
		},
	}
}

// A partial skip — every GPU gone, memory intact — must name itself once, with
// the reason the probe reported. Before this it reached no logging path at all
// and the only visible difference was the path count.
func TestEnginePartialSkipIsReportedOnceWithReason(t *testing.T) {
	g := topology.Fig2()
	var sink logSink
	e := New(config.Defaults(), g, gdrProber("NoGDR: no cuda"))
	e.Logf = sink.logf

	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Paths) == 0 {
		t.Fatal("memory paths should remain after the GDR skip")
	}
	if len(sink.lines) != 1 {
		t.Fatalf("want one skip line, got %d: %v", len(sink.lines), sink.lines)
	}
	line := sink.lines[0]
	for _, want := range []string{"kind=gpu", "count=8", "probes=32", "NoGDR: no cuda", "gpu0", "gpu7"} {
		if !strings.Contains(line, want) {
			t.Errorf("skip line %q missing %q", line, want)
		}
	}

	// The JSON report carries the same skip, so one-shot diagnose output is
	// self-describing even when nobody reads stderr.
	if len(rep.SkippedEndpoints) != 1 {
		t.Fatalf("want one skip group in the report, got %+v", rep.SkippedEndpoints)
	}
	s := rep.SkippedEndpoints[0]
	if s.Kind != model.EndpointGPU {
		t.Errorf("skip kind = %q, want gpu", s.Kind)
	}
	if !strings.Contains(s.Reason, "NoGDR: no cuda") {
		t.Errorf("skip reason = %q, want the probe detail", s.Reason)
	}
	if len(s.Endpoints) != 8 || s.Endpoints[0] != "gpu0" {
		t.Errorf("skip endpoints = %v, want gpu0..gpu7", s.Endpoints)
	}
	if s.Probes != 32 {
		t.Errorf("skipped probes = %d, want 32 (4 RNICs × 8 GPUs)", s.Probes)
	}

	// A daemon polls every --metric-interval; a standing condition must not be
	// logged again on every run.
	for i := 0; i < 3; i++ {
		rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.SkippedEndpoints) != 1 {
			t.Fatalf("run %d dropped the skip from the report: %+v", i+2, rep.SkippedEndpoints)
		}
	}
	if len(sink.lines) != 1 {
		t.Fatalf("repeated runs re-logged the same reason: %v", sink.lines)
	}
}

// Logging on change means both directions: a new reason and a recovery are
// each worth exactly one line.
func TestEngineLogsSkipReasonChangeAndRecovery(t *testing.T) {
	g := topology.Fig2()
	var sink logSink
	detail := "NoGDR: no cuda"
	gdr := true
	e := New(config.Defaults(), g, stubProber{
		loop: func(n model.RNIC, ep model.Endpoint) (model.PathMeasurement, error) {
			if ep.Kind == model.EndpointGPU && gdr {
				return model.PathMeasurement{}, fmt.Errorf("%w: %s", model.ErrNoGDR, detail)
			}
			return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000}, nil
		},
	})
	e.Logf = sink.logf

	run := func() {
		t.Helper()
		if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	detail = "NoGDR: missing gpu"
	run()
	gdr = false
	run()
	run()

	if len(sink.lines) != 3 {
		t.Fatalf("want three lines (two reasons, one recovery), got %v", sink.lines)
	}
	if !strings.Contains(sink.lines[0], "no cuda") || !strings.Contains(sink.lines[1], "missing gpu") {
		t.Errorf("reason change not reported: %v", sink.lines[:2])
	}
	if !strings.Contains(sink.lines[2], "none") {
		t.Errorf("recovery line = %q, want the cleared line", sink.lines[2])
	}
}

// Busy mode probes only affinitive paths, so the skipped counts legitimately
// move between runs. The line reports the same condition, so it must not be
// logged again just because the numbers changed.
func TestEngineDoesNotRelogSkipWhenOnlyCountsChange(t *testing.T) {
	g := topology.Fig2()
	var sink logSink
	e := New(config.Defaults(), g, gdrProber("NoGDR: no cuda"))
	e.Logf = sink.logf

	if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
		t.Fatal(err)
	}
	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeBusy, BusyRNICs: map[string]bool{"rnic0": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SkippedEndpoints) != 1 {
		t.Fatalf("busy run lost the skip: %+v", rep.SkippedEndpoints)
	}
	if got := rep.SkippedEndpoints[0].Probes; got == 32 {
		t.Fatalf("busy mode probed the full mesh (%d), test proves nothing", got)
	}
	if len(sink.lines) != 1 {
		t.Fatalf("a count change re-logged the same reason: %v", sink.lines)
	}
}

// The all-failed case keeps its aggregated error, which is the only carrier of
// the reasons when there is no report to attach them to.
func TestEngineAllSkippedKeepsAggregatedErrorAndLogsNothing(t *testing.T) {
	g := topology.Fig2()
	var sink logSink
	e := New(config.Defaults(), g, stubProber{
		loop: func(_ model.RNIC, ep model.Endpoint) (model.PathMeasurement, error) {
			if ep.Kind == model.EndpointGPU {
				return model.PathMeasurement{}, fmt.Errorf("%w: %s", model.ErrNoGDR, "NoGDR: no cuda")
			}
			return model.PathMeasurement{}, model.ErrNoGID
		},
	})
	e.Logf = sink.logf

	_, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err == nil {
		t.Fatal("expected the empty-mesh error")
	}
	if !errors.Is(err, model.ErrNoGDR) || !errors.Is(err, model.ErrNoGID) {
		t.Fatalf("aggregated error lost a cause: %v", err)
	}
	got := err.Error()
	for _, want := range []string{"no loopback measurements", "memory rnic0->mem0 x8", "gpu rnic0->gpu0 x32", "NoGDR: no cuda"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q missing %q", got, want)
		}
	}
	if len(sink.lines) != 0 {
		t.Fatalf("all-failed run logged a skip line as well as failing: %v", sink.lines)
	}
}

// A rail-optimized host puts every RNIC on a rail of its own, so no two share a
// fabric key and the mutual probe has nothing to measure. Saying so is the only
// way an operator can tell that apart from a broken probe: the ToN diagnoses
// simply never fire and no hostping.mutual.* series is exported.
//
// The keys here are opaque on purpose. Discovery writes "v4:<subnet>" or
// "gid:<prefix>", but pairing is string equality, so the shape of a real
// deployment's keys is not what is under test.
func TestEngineReportsEmptyMutualPairSet(t *testing.T) {
	rails := topology.Fig2()
	for i := range rails.RNICs {
		rails.RNICs[i].Fabric = fmt.Sprintf("rail%d", i)
	}
	var sink logSink
	e := New(config.Defaults(), rails, stubProber{})
	e.Logf = sink.logf

	// A daemon runs the engine every poll; the report must not repeat.
	for i := 0; i < 3; i++ {
		rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Mutual) != 0 {
			t.Fatalf("run %d: distinct fabrics must not pair: %+v", i, rep.Mutual)
		}
	}
	if len(sink.lines) != 1 {
		t.Fatalf("want one report over three runs, got %v", sink.lines)
	}
	line := sink.lines[0]
	if !strings.Contains(line, "no RNIC pairs") {
		t.Fatalf("report %q does not name the condition", line)
	}
	for i := range rails.RNICs {
		if key := rails.RNICs[i].Fabric; !strings.Contains(line, key) {
			t.Fatalf("report %q must name fabric %q", line, key)
		}
	}

	// "Once" means until the condition changes, not once per process: a
	// topology that pairs again clears it, and losing the pairs re-reports.
	e.Graph = topology.Fig2()
	if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
		t.Fatal(err)
	}
	e.Graph = rails
	if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
		t.Fatal(err)
	}
	if len(sink.lines) != 2 {
		t.Fatalf("want a second report after the pair set came back and went, got %v", sink.lines)
	}
}

// On a rail-optimized host rail scope says why there is nothing to measure and
// points at host scope; host scope then probes every pair across rails, marks
// them cross-rail, and an RNIC cut off onto an isolated subnet (ToN #14) is
// named as cross-rail unreachable rather than down or a ToR anomaly.
func TestEngineHostScopeFindsIsolatedRNIC(t *testing.T) {
	g := topology.Fig2Rails()
	w := sim.New(g)
	if err := w.ApplyFault("isolate:rnic3"); err != nil {
		t.Fatal(err)
	}

	var sink logSink
	e := New(config.Defaults(), g, SimProber{World: w})
	e.Logf = sink.logf
	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Mutual) != 0 || len(sink.lines) != 1 {
		t.Fatalf("rail scope: want no pairs and one report, got %d pairs, %v", len(rep.Mutual), sink.lines)
	}
	if !strings.Contains(sink.lines[0], "--mutual-scope=host") {
		t.Fatalf("report %q must point at host scope", sink.lines[0])
	}

	cfg := config.Defaults()
	cfg.MutualScope = model.MutualScopeHost
	e = New(cfg, g, SimProber{World: w})
	e.Logf = sink.logf
	rep, err = e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Mutual) != 6 {
		t.Fatalf("host scope on 4 rails: want 6 pairs, got %d", len(rep.Mutual))
	}
	for _, m := range rep.Mutual {
		if !m.CrossRail {
			t.Fatalf("%s-%s must be marked cross-rail", m.Src, m.Dst)
		}
	}
	var found bool
	for _, c := range rep.Causes {
		switch c.Code {
		case model.CauseRNICCrossRailUnreachable:
			if strings.Join(c.RNICs, ",") != "rnic3" || c.Confidence != "medium" {
				t.Fatalf("want rnic3 at medium, got %+v", c)
			}
			found = true
		case model.CauseRNICConnectFailure, model.CauseToRSwitchAnomaly:
			t.Fatalf("cross-rail failures must not read as %s: %+v", c.Code, c)
		}
	}
	if !found {
		t.Fatalf("isolated rnic3 not reported: %+v", rep.Causes)
	}
}

// The Fig.2 (--sim) world pairs every distinct device, so the daemon that runs
// it must stay quiet and keep measuring its mutual pairs.
func TestEngineSilentWhenMutualPairsExist(t *testing.T) {
	var sink logSink
	e := New(config.Defaults(), topology.Fig2(), stubProber{})
	e.Logf = sink.logf

	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Mutual) == 0 {
		t.Fatal("Fig.2 must produce mutual measurements")
	}
	if len(sink.lines) != 0 {
		t.Fatalf("a topology with pairs must report nothing: %v", sink.lines)
	}
}

// seqProber replays one bandwidth sequence per path, so a single path can be
// given the spread a probe shows on a loaded RNIC while its sibling stays flat.
// A path that runs out of readings repeats its last one, and a path with no
// sequence reads flat. loopErr fails a path after that many readings.
type seqProber struct {
	flat    uint64
	bw      map[string][]uint64 // "rnic->endpoint" → readings, in probe order
	loopErr map[string]error    // "rnic->endpoint" → error once bw is exhausted
	calls   map[string]int
}

func (p *seqProber) Loopback(_ context.Context, n model.RNIC, ep model.Endpoint, _, _ uint32) (model.PathMeasurement, error) {
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	key := n.ID + "->" + ep.ID
	i := p.calls[key]
	p.calls[key] = i + 1
	seq := p.bw[key]
	if i >= len(seq) {
		if err := p.loopErr[key]; err != nil {
			return model.PathMeasurement{}, err
		}
	}
	bw := p.flat
	if len(seq) > 0 {
		bw = seq[min(i, len(seq)-1)]
	}
	return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: bw, SmallLatNsP50: 1000}, nil
}

func (p *seqProber) Mutual(_ context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: true}, nil
}

func pathStatus(t *testing.T, rep model.Report, rnic, ep string) model.PathMeasurement {
	t.Helper()
	for _, p := range rep.Paths {
		if p.RNIC == rnic && p.Endpoint == ep {
			return p
		}
	}
	t.Fatalf("%s->%s missing from the report", rnic, ep)
	return model.PathMeasurement{}
}

func busyRun(t *testing.T, cfg config.Config, p Prober) model.Report {
	t.Helper()
	e := New(cfg, topology.Fig2(), p)
	e.Logf = func(string, ...any) {}
	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeBusy, BusyRNICs: map[string]bool{"rnic0": true}})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// Busy mode judges a loaded RNIC's path against its siblings at the same
// distance, so one reading from the tail of the probe's own spread was enough
// to report a healthy path abnormal — a different path on every run.
// Classifying the median of several readings holds that path undecided, which
// is what an unbroken path measured on a loaded RNIC is.
func TestEngineBusyMedianHoldsNoisySiblingUndecided(t *testing.T) {
	const flat = 190_000_000_000
	// Two affinitive GPUs sit at one distance and read alike; gpu1's first
	// probe lands 30% low, twice the threshold, and its repeats do not.
	noisy := func() *seqProber {
		return &seqProber{
			flat: flat,
			bw:   map[string][]uint64{"rnic0->gpu1": {flat * 7 / 10, flat, flat}},
		}
	}

	rep := busyRun(t, config.Defaults(), noisy())
	for _, p := range rep.Paths {
		if p.Status == model.PathAbnormal {
			t.Fatalf("sample noise on a healthy busy host reported abnormal: %+v", p)
		}
	}
	got := pathStatus(t, rep, "rnic0", "gpu1")
	if got.UncertainReason != model.UncertainRNICBusy {
		t.Fatalf("a path the loaded RNIC cannot decide must say so: %+v", got)
	}
	if got.BandwidthBPS != flat {
		t.Fatalf("classified bandwidth = %d, want the median %d", got.BandwidthBPS, uint64(flat))
	}

	// The same readings with the repeat turned off are the behaviour the
	// median replaced, so the case above is not passing for some other reason: the
	// single low reading flags the path, and only the busy recheck,
	// whose re-reads are not low, takes the verdict back.
	cfg := config.Defaults()
	cfg.BusySamples = 1
	rep = busyRun(t, cfg, noisy())
	got = pathStatus(t, rep, "rnic0", "gpu1")
	if r := got.BusyRecheck; r == nil || r.Confirmed || got.Status != model.PathUncertain {
		t.Fatalf("--busy-samples 1 is the single-reading comparison, want it flagged and cleared by the recheck: %+v", got)
	}
}

// The median must not cost the comparison its sensitivity: a path that really
// is down against its sibling reads low in every sample, not just the tail.
func TestEngineBusyMedianStillFlagsDegradedSibling(t *testing.T) {
	const flat = 190_000_000_000
	rep := busyRun(t, config.Defaults(), &seqProber{
		flat: flat,
		bw: map[string][]uint64{"rnic0->gpu1": {
			flat * 55 / 100, flat * 60 / 100, flat * 65 / 100,
		}},
	})
	if got := pathStatus(t, rep, "rnic0", "gpu1"); got.Status != model.PathAbnormal {
		t.Fatalf("a sibling degraded in every sample must stay abnormal: %+v", got)
	}
	if got := pathStatus(t, rep, "rnic0", "gpu0"); got.Status == model.PathAbnormal {
		t.Fatalf("the reference sibling must not be flagged with it: %+v", got)
	}
}

// §4.3 wants the probe overhead on a loaded RNIC small, so the repeat is spent
// only where it can change a verdict: busy mode, an RNIC the monitor reported
// loaded, and a distance that has a second endpoint to compare against.
func TestEngineRepeatsOnlyComparableBusyPaths(t *testing.T) {
	cfg := config.Defaults()
	want := int(cfg.BusySamples)
	if want < 2 {
		t.Fatalf("default --busy-samples = %d leaves nothing to test", want)
	}

	p := &seqProber{flat: 190_000_000_000}
	busyRun(t, cfg, p)
	for key, got := range p.calls {
		// rnic0 is the loaded RNIC and its two affinitive GPUs share the
		// same_switch distance; its memory paths are alone at theirs.
		repeat := key == "rnic0->gpu0" || key == "rnic0->gpu1"
		if repeat && got != want {
			t.Errorf("%s probed %d times, want %d", key, got, want)
		}
		// A path no sibling comparison can decide pays for no repeat.
		if !repeat && got != 1 {
			t.Errorf("%s probed %d times, want 1", key, got)
		}
	}

	// An idle run classifies against the baseline and never compares paths with
	// each other, so it must keep probing each path exactly once.
	idle := &seqProber{flat: 190_000_000_000}
	e := New(cfg, topology.Fig2(), idle)
	e.Logf = func(string, ...any) {}
	if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
		t.Fatal(err)
	}
	for key, got := range idle.calls {
		if got != 1 {
			t.Errorf("idle run probed %s %d times, want 1", key, got)
		}
	}
}

// Repeating a probe must not change what "skipped" means: a path is skipped
// when it produced no reading at all, and it is counted once however many
// readings it was going to take.
func TestEngineBusySkipAccountingWithRepeats(t *testing.T) {
	p := &seqProber{
		flat: 190_000_000_000,
		// gpu0 never answers; gpu1 answers once and then fails, which is a
		// measured path, not a skipped one.
		bw:      map[string][]uint64{"rnic0->gpu1": {190_000_000_000}},
		loopErr: map[string]error{"rnic0->gpu0": model.ErrNoGDR, "rnic0->gpu1": model.ErrProbe},
	}
	rep := busyRun(t, config.Defaults(), p)

	if got := p.calls["rnic0->gpu0"]; got != 1 {
		t.Errorf("a path the prober cannot complete was probed %d times, want 1", got)
	}
	if len(rep.SkippedEndpoints) != 1 {
		t.Fatalf("want one skip group, got %+v", rep.SkippedEndpoints)
	}
	s := rep.SkippedEndpoints[0]
	if len(s.Endpoints) != 1 || s.Endpoints[0] != "gpu0" {
		t.Errorf("skipped endpoints = %v, want gpu0 alone", s.Endpoints)
	}
	if s.Probes != 1 {
		t.Errorf("skipped probes = %d, want 1 (one path, counted once)", s.Probes)
	}
	if got := pathStatus(t, rep, "rnic0", "gpu1"); got.BandwidthBPS != 190_000_000_000 {
		t.Errorf("a path that answered before failing must keep its readings: %+v", got)
	}
}

// The row handed to the analyzer takes a median per field rather than one
// reading picked whole, because the analyzer reads the fields independently:
// bandwidth against the sibling reference, small-message latency against the
// ACS/ATS baseline. A reading picked by its bandwidth would carry that
// reading's outlier latency with it.
func TestMedianMeasurementIsPerField(t *testing.T) {
	ms := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 100, SmallLatNsP50: 900, SmallLatNsP99: 4000, LargeLatNsP50: 8000,
			Verbs: model.VerbNs{GPUAlloc: 300, MRReg: 9000, QPSetup: 200}},
		{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 60, SmallLatNsP50: 1000, SmallLatNsP99: 9000, LargeLatNsP50: 8200,
			Verbs: model.VerbNs{GPUAlloc: 400, MRReg: 100, QPSetup: 250}},
		{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 98, SmallLatNsP50: 8000, SmallLatNsP99: 4100, LargeLatNsP50: 8100,
			Verbs: model.VerbNs{GPUAlloc: 350, MRReg: 120, QPSetup: 900}},
	}
	got := medianMeasurement(ms)
	want := model.PathMeasurement{
		RNIC: "rnic0", Endpoint: "gpu0",
		BandwidthBPS: 98, SmallLatNsP50: 1000, SmallLatNsP99: 4100, LargeLatNsP50: 8100,
		Verbs: model.VerbNs{GPUAlloc: 350, MRReg: 120, QPSetup: 250},
	}
	if got != want {
		t.Fatalf("median = %+v, want %+v", got, want)
	}

	// An even number of readings has no middle one, so the two in the middle
	// are averaged.
	got = medianMeasurement(ms[:2])
	want = model.PathMeasurement{
		RNIC: "rnic0", Endpoint: "gpu0",
		BandwidthBPS: 80, SmallLatNsP50: 950, SmallLatNsP99: 6500, LargeLatNsP50: 8100,
		Verbs: model.VerbNs{GPUAlloc: 350, MRReg: 4550, QPSetup: 225},
	}
	if got != want {
		t.Fatalf("even median = %+v, want %+v", got, want)
	}

	// One reading is itself, untouched.
	if got := medianMeasurement(ms[1:2]); got != ms[1] {
		t.Fatalf("single reading = %+v, want %+v", got, ms[1])
	}
}

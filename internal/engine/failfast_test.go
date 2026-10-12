package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// wcRetryExceeded is how the live prober reports an InfiniBand loopback whose
// WRITEs ran out of transport retries in every addressing mode it tried.
var wcRetryExceeded = model.PortLoopbackError(
	fmt.Errorf("%w: Wc: transport retry counter exceeded (12)", model.ErrProbe))

// countProber counts loopbacks per RNIC and lets fail decide each one's
// outcome from the RNIC and the RNIC's probe count so far (0 = first).
type countProber struct {
	fail  func(n model.RNIC, i int) error
	calls map[string]int
}

func (p *countProber) Loopback(_ context.Context, n model.RNIC, ep model.Endpoint, _, _ uint32) (model.PathMeasurement, error) {
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	i := p.calls[n.ID]
	p.calls[n.ID] = i + 1
	if p.fail != nil {
		if err := p.fail(n, i); err != nil {
			return model.PathMeasurement{}, err
		}
	}
	return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000}, nil
}

func (p *countProber) Mutual(_ context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: true}, nil
}

func failFastGraph(linkLayer string) topology.Graph {
	g := topology.Fig2()
	g.RNICs[0].LinkLayer = linkLayer
	return g
}

func runFailFast(t *testing.T, g topology.Graph, p *countProber) model.Report {
	t.Helper()
	e := New(config.Defaults(), g, p)
	e.Logf = func(string, ...any) {}
	rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// skippedOf returns, per endpoint of rnic's probe paths, the skip reason the
// report gives it; an endpoint that was measured is absent.
func skippedOf(rep model.Report, g topology.Graph, rnic string) map[string]string {
	eps := map[string]bool{}
	for _, p := range g.ProbePaths(rnic, model.ModeIdle) {
		eps[p.Endpoint] = true
	}
	measured := map[string]bool{}
	for _, p := range rep.Paths {
		if p.RNIC == rnic {
			measured[p.Endpoint] = true
		}
	}
	out := map[string]string{}
	for _, s := range rep.SkippedEndpoints {
		for _, ep := range s.Endpoints {
			if eps[ep] && !measured[ep] {
				out[ep] = s.Reason
			}
		}
	}
	return out
}

// An InfiniBand port that cannot loop back costs one probe per run, not one
// per endpoint: its other endpoints are reported skipped with a reason that
// names the first failure, and the other RNICs are probed as usual.
func TestEngineInfiniBandPortFailureStopsTheRNIC(t *testing.T) {
	g := failFastGraph(model.LinkLayerInfiniBand)
	p := &countProber{fail: func(n model.RNIC, _ int) error {
		if n.ID == "rnic0" {
			return wcRetryExceeded
		}
		return nil
	}}
	rep := runFailFast(t, g, p)
	if got := p.calls["rnic0"]; got != 1 {
		t.Fatalf("rnic0 probed %d times, want 1", got)
	}
	paths := g.ProbePaths("rnic0", model.ModeIdle)
	if len(paths) < 2 {
		t.Fatalf("fixture needs several rnic0 paths, has %d", len(paths))
	}
	if got := p.calls["rnic1"]; got != len(g.ProbePaths("rnic1", model.ModeIdle)) {
		t.Fatalf("rnic1 probed %d times, want every path", got)
	}
	skipped := skippedOf(rep, g, "rnic0")
	if len(skipped) != len(paths) {
		t.Fatalf("skipped %d of rnic0's %d endpoints: %v", len(skipped), len(paths), skipped)
	}
	first := paths[0].Endpoint
	if skipped[first] != wcRetryExceeded.Error() {
		t.Errorf("first endpoint reason %q, want the probe's own error", skipped[first])
	}
	want := "not probed: an earlier loopback on this RNIC failed: " + wcRetryExceeded.Error()
	for _, p := range paths[1:] {
		if skipped[p.Endpoint] != want {
			t.Errorf("%s reason %q, want %q", p.Endpoint, skipped[p.Endpoint], want)
		}
	}
	// The reason stays a probe failure, so the run-failed error and the
	// skip report keep classifying it as before.
	if err := notProbedError(wcRetryExceeded); !errors.Is(err, model.ErrProbe) {
		t.Errorf("not-probed reason must wrap ErrProbe: %v", err)
	}

	// The decision lasts one run: the next run probes the port again.
	e := New(config.Defaults(), g, p)
	e.Logf = func(string, ...any) {}
	for i := 0; i < 2; i++ {
		if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.calls["rnic0"]; got != 3 {
		t.Fatalf("rnic0 probed %d times over three runs, want once per run", got)
	}
}

// An RNIC that answered once in this run has a port that can loop back, so a
// later failure is about that endpoint and the rest are still probed.
func TestEngineInfiniBandFailureAfterSuccessKeepsProbing(t *testing.T) {
	g := failFastGraph(model.LinkLayerInfiniBand)
	p := &countProber{fail: func(n model.RNIC, i int) error {
		if n.ID == "rnic0" && i > 0 {
			return wcRetryExceeded
		}
		return nil
	}}
	rep := runFailFast(t, g, p)
	paths := g.ProbePaths("rnic0", model.ModeIdle)
	if got := p.calls["rnic0"]; got != len(paths) {
		t.Fatalf("rnic0 probed %d times, want all %d paths", got, len(paths))
	}
	for ep, reason := range skippedOf(rep, g, "rnic0") {
		if strings.HasPrefix(reason, "not probed") {
			t.Errorf("%s: %q", ep, reason)
		}
	}
}

// Only an InfiniBand port fails fast; a RoCE RNIC reporting the same error,
// and an InfiniBand RNIC with a local error, keep probing every endpoint.
func TestEngineFailFastScope(t *testing.T) {
	local := fmt.Errorf("%w: Wc: local protection error (4)", model.ErrProbe)
	for _, tc := range []struct {
		name      string
		linkLayer string
		err       error
	}{
		{"roce port-level error", model.LinkLayerEthernet, wcRetryExceeded},
		{"unknown link layer", "", wcRetryExceeded},
		{"infiniband local error", model.LinkLayerInfiniBand, local},
		{"infiniband setup failure", model.LinkLayerInfiniBand, fmt.Errorf("%w: Qp", model.ErrProbe)},
		{"infiniband no gdr", model.LinkLayerInfiniBand, model.ErrNoGDR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := failFastGraph(tc.linkLayer)
			p := &countProber{fail: func(n model.RNIC, _ int) error {
				if n.ID == "rnic0" {
					return tc.err
				}
				return nil
			}}
			rep := runFailFast(t, g, p)
			paths := g.ProbePaths("rnic0", model.ModeIdle)
			if got := p.calls["rnic0"]; got != len(paths) {
				t.Fatalf("rnic0 probed %d times, want all %d paths", got, len(paths))
			}
			skipped := skippedOf(rep, g, "rnic0")
			if len(skipped) != len(paths) {
				t.Fatalf("skipped %d of %d endpoints", len(skipped), len(paths))
			}
			for ep, reason := range skipped {
				if reason != tc.err.Error() {
					t.Errorf("%s reason %q, want %q", ep, reason, tc.err.Error())
				}
			}
		})
	}
}

func TestPortLoopbackErrorKeepsTextAndSentinels(t *testing.T) {
	if !errors.Is(wcRetryExceeded, model.ErrPortLoopback) || !errors.Is(wcRetryExceeded, model.ErrProbe) {
		t.Fatalf("want both sentinels: %v", wcRetryExceeded)
	}
	if got, want := wcRetryExceeded.Error(), "probe failed: Wc: transport retry counter exceeded (12)"; got != want {
		t.Fatalf("text %q, want %q", got, want)
	}
	if !skippableProbe(wcRetryExceeded) {
		t.Fatal("a port-level loopback failure must stay skippable")
	}
	if model.PortLoopbackError(nil) != nil {
		t.Fatal("nil stays nil")
	}
}

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

// An InfiniBand RNIC without a usable LID is a skip with its own reason, like
// a RoCE RNIC without a RoCEv2 GID: the rest of the mesh is still measured.
func TestEngineSkipsInfiniBandPortWithoutLID(t *testing.T) {
	for _, sentinel := range []error{model.ErrNoLID, model.ErrPortNotActive} {
		g := topology.Fig2()
		var sink logSink
		e := New(config.Defaults(), g, stubProber{
			loop: func(n model.RNIC, ep model.Endpoint) (model.PathMeasurement, error) {
				if n.ID == "rnic1" {
					return model.PathMeasurement{}, fmt.Errorf("%w: synthetic", sentinel)
				}
				return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000}, nil
			},
		})
		e.Logf = sink.logf
		rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
		if err != nil {
			t.Fatalf("%v: %v", sentinel, err)
		}
		if len(rep.SkippedEndpoints) == 0 {
			t.Fatalf("%v: want skipped endpoints", sentinel)
		}
		for _, s := range rep.SkippedEndpoints {
			if !strings.HasPrefix(s.Reason, sentinel.Error()) {
				t.Errorf("%v: skip reason %q", sentinel, s.Reason)
			}
		}
		for _, p := range rep.Paths {
			if p.RNIC == "rnic1" {
				t.Fatalf("%v: rnic1 must be skipped, got %+v", sentinel, p)
			}
		}
		if !skippableProbe(fmt.Errorf("wrapped: %w", sentinel)) {
			t.Errorf("%v must be skippable", sentinel)
		}
	}
	if !errors.Is(fmt.Errorf("%w: x", model.ErrNoLID), model.ErrNoLID) {
		t.Fatal("sentinel wrapping")
	}
}

// Which addressing a live probe used (RoCEv2 GID or InfiniBand LID) is
// logged once per RNIC, and again only when it changes.
func TestEngineLogsLoopbackAddressingOncePerRNIC(t *testing.T) {
	g := topology.Fig2()
	g.RNICs[0].LinkLayer = model.LinkLayerInfiniBand
	mode := model.AddrIBLID
	var sink logSink
	e := New(config.Defaults(), g, stubProber{
		loop: func(n model.RNIC, ep model.Endpoint) (model.PathMeasurement, error) {
			m := model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000}
			if n.ID == "rnic0" {
				m.Addressing = model.PortAddressing{Mode: mode, Port: 1, LID: 7}
			}
			return m, nil
		},
	})
	e.Logf = sink.logf
	count := func() int {
		n := 0
		for _, l := range sink.lines {
			if strings.Contains(l, "loopback addressing rnic=rnic0") {
				n++
			}
		}
		return n
	}
	for i := 0; i < 2; i++ {
		if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(); got != 1 {
		t.Fatalf("addressing logged %d times over two runs, want 1: %q", got, sink.lines)
	}
	want := "link_layer=InfiniBand mode=ib_lid port=1 lid=7"
	found := false
	for _, l := range sink.lines {
		if strings.Contains(l, want) {
			found = true
		}
		if strings.Contains(l, "rnic=rnic1 ") {
			t.Fatalf("an RNIC with no reported addressing (simulator) must log nothing: %q", l)
		}
	}
	if !found {
		t.Fatalf("want %q in %q", want, sink.lines)
	}
	mode = model.AddrIBLIDGRH
	if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 2 {
		t.Fatalf("a changed addressing must be logged again, got %d lines", got)
	}
}

// InfiniBand RNICs are never mutual-probed without --mutual-infiniband, in
// any scope, and the engine names them once. RoCE pairs are unaffected.
func TestEngineLeavesInfiniBandOutOfMutualByDefault(t *testing.T) {
	ibGraph := func() topology.Graph {
		g := topology.Fig2()
		for i := range g.RNICs {
			g.RNICs[i].Fabric = "ib:fe80000000000000"
			g.RNICs[i].LinkLayer = model.LinkLayerInfiniBand
		}
		return g
	}
	for _, scope := range []model.MutualScope{model.MutualScopeRail, model.MutualScopeHost} {
		cfg := config.Defaults()
		cfg.MutualScope = scope
		var probed int
		var sink logSink
		e := New(cfg, ibGraph(), stubProber{
			mut: func(src, dst model.RNIC) (model.MutualMeasurement, error) {
				probed++
				return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: false, LossRatio: 1}, nil
			},
		})
		e.Logf = sink.logf
		for i := 0; i < 2; i++ {
			rep, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle})
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Mutual) != 0 {
				t.Fatalf("scope %s: InfiniBand pairs measured: %+v", scope, rep.Mutual)
			}
		}
		if probed != 0 {
			t.Fatalf("scope %s: %d InfiniBand mutual probes ran", scope, probed)
		}
		n := 0
		for _, l := range sink.lines {
			if strings.Contains(l, "mutual probe leaves out") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("scope %s: exclusion logged %d times, want 1: %q", scope, n, sink.lines)
		}
	}

	cfg := config.Defaults()
	cfg.MutualInfiniBand = true
	var probed int
	e := New(cfg, ibGraph(), stubProber{
		mut: func(src, dst model.RNIC) (model.MutualMeasurement, error) {
			probed++
			return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: true}, nil
		},
	})
	e.Logf = func(string, ...any) {}
	if _, err := e.Run(context.Background(), RunRequest{Mode: model.ModeIdle}); err != nil {
		t.Fatal(err)
	}
	if probed == 0 {
		t.Fatal("--mutual-infiniband must pair InfiniBand RNICs sharing a subnet prefix")
	}
}

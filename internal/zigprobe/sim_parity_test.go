package zigprobe_test

import (
	"context"
	"testing"

	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
	"github.com/yuuki/hostping/internal/zigprobe"
)

// The Eq.1 world is modelled twice: sim.World in Go, which the unit tests use,
// and loopbackSim/hostping_mutual in Zig, which production --sim goes through.
// They used to disagree on the synthetic p99 (Zig reported p50 for loopback
// and p50+10% for mutual), so `hostpingctl diagnose --sim` emitted different
// numbers than any Go test ever checked. This locks the two together for every
// path and pair of the Fig.2 topology; if it fails, sim.zig and
// internal/sim/world.go have drifted apart again.
func TestZigSimMatchesGoSim(t *testing.T) {
	g := topology.Fig2()
	for _, fault := range []string{"", "rnic_pcie:rnic0", "acs", "ats:rnic0", "drops:rnic1", "connect_fail:rnic0", "slow_mr:gpu3", "flap:rnic1", "tor", "lat:pcie.gpu3", "isolate:rnic3"} {
		t.Run("fault="+fault, func(t *testing.T) {
			w := sim.New(g)
			if err := w.ApplyFault(fault); err != nil {
				t.Fatal(err)
			}
			p := zigprobe.Prober{Sim: w, Graph: g, Iters: 64, TimeoutMS: 1000}
			for _, n := range g.RNICs {
				for _, ep := range g.Endpoints {
					if _, ok := g.Path(n.ID, ep.ID); !ok {
						continue
					}
					got, err := p.Loopback(context.Background(), n, ep, 1, 128*1024)
					if err != nil {
						t.Fatalf("%s->%s: %v", n.ID, ep.ID, err)
					}
					want, err := w.Loopback(n.ID, ep.ID, 1, 128*1024)
					if err != nil {
						t.Fatal(err)
					}
					if got.SmallLatNsP50 != want.SmallLatNsP50 ||
						got.SmallLatNsP99 != want.SmallLatNsP99 ||
						got.LargeLatNsP50 != want.LargeLatNsP50 ||
						got.BandwidthBPS != want.BandwidthBPS ||
						got.Verbs != want.Verbs {
						t.Fatalf("%s->%s loopback\n zig %+v\n  go %+v", n.ID, ep.ID, got, want)
					}
				}
			}
			for _, src := range g.RNICs {
				for _, dst := range g.RNICs {
					if src.ID == dst.ID {
						continue
					}
					got, err := p.Mutual(context.Background(), src, dst)
					if err != nil {
						t.Fatalf("%s->%s: %v", src.ID, dst.ID, err)
					}
					want := w.Mutual(src.ID, dst.ID)
					if got.Connected != want.Connected || got.RTTnsP50 != want.RTTnsP50 ||
						got.RTTnsP99 != want.RTTnsP99 || got.LossRatio != want.LossRatio ||
						got.Sent != want.Sent || got.Recv != want.Recv {
						t.Fatalf("%s->%s mutual\n zig %+v\n  go %+v", src.ID, dst.ID, got, want)
					}
				}
			}
		})
	}
}

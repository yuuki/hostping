package agent

import (
	"context"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// The --sim daemon runs the probe schedule over the simulated world: a
// flapping RNIC loses a quarter of its probes, which the window reports as
// that RNIC's timeout ratio and raises the alarm.
func TestSimMutualLoopFlap(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault("flap:rnic0"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.MutualTransport = model.MutualTransportUD
	cfg.MutualInterval = time.Millisecond
	cfg.MutualTimeout = 5 * time.Millisecond
	cfg.MutualWindow = 400 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	l, done := startMutualLoop(ctx, cfg, g, w)
	if l == nil {
		cancel()
		t.Fatal("Fig.2 has pairs: the loop must start")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ms, ready := l.Snapshot(); ready && len(ms) > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the window never filled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	ms, _ := l.Snapshot()
	cancel()
	<-done
	var lossy, clean int
	for _, m := range ms {
		r, ok := m.TimeoutRatio()
		if !ok || m.Reliable {
			t.Fatalf("simulated window: %+v", m)
		}
		if m.Src == "rnic0" || m.Dst == "rnic0" {
			if r > 0 {
				lossy++
			}
		} else if r != 0 {
			t.Fatalf("pair without rnic0 must be clean: %+v", m)
		} else {
			clean++
		}
	}
	if lossy == 0 || clean == 0 {
		t.Fatalf("want lossy rnic0 pairs and clean others, got %+v", ms)
	}
}

// The schedule is opt-in: by default the daemon keeps the per-run burst.
func TestMutualLoopOffByDefault(t *testing.T) {
	cfg := config.Defaults()
	if l, _ := startMutualLoop(context.Background(), cfg, topology.Fig2(), sim.New(topology.Fig2())); l != nil {
		t.Fatal("--mutual-interval 0 must not start the loop")
	}
}

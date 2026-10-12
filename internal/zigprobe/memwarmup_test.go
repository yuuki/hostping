package zigprobe

import (
	"context"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// --mem-warmup reaches the C request of a memory endpoint and never that of a
// GPU endpoint, whose buffers are device memory.
func TestMemWarmupReachesMemoryRequestOnly(t *testing.T) {
	g := topology.Fig2()
	rnic := g.RNICs[0]
	var mem, gpu model.Endpoint
	for _, ep := range g.Endpoints {
		switch {
		case ep.Kind == model.EndpointMemory && mem.ID == "":
			mem = ep
		case ep.Kind == model.EndpointGPU && gpu.ID == "":
			gpu = ep
		}
	}
	if mem.ID == "" || gpu.ID == "" {
		t.Fatal("Fig.2 needs a memory and a GPU endpoint")
	}

	p := Prober{Graph: g, MemWarmup: 50 * time.Millisecond}
	_, ns, err := loopbackReqFields(p, rnic, mem)
	if err != nil {
		t.Fatal(err)
	}
	if ns != uint64(50*time.Millisecond) {
		t.Errorf("memory endpoint mem_warmup_ns = %d, want %d", ns, 50*time.Millisecond)
	}
	_, ns, err = loopbackReqFields(p, rnic, gpu)
	if err != nil {
		t.Fatal(err)
	}
	if ns != 0 {
		t.Errorf("GPU endpoint mem_warmup_ns = %d, want 0", ns)
	}

	_, ns, _ = loopbackReqFields(Prober{Graph: g}, rnic, mem)
	if ns != 0 {
		t.Errorf("off by default: mem_warmup_ns = %d, want 0", ns)
	}
}

// The zero Prober uses the configured defaults: 512 loopback WRITE pairs and
// a 64-round-trip mutual burst.
func TestProberIterDefaults(t *testing.T) {
	g := topology.Fig2()
	var mem model.Endpoint
	for _, ep := range g.Endpoints {
		if ep.Kind == model.EndpointMemory {
			mem = ep
			break
		}
	}
	iters, _, err := loopbackReqFields(Prober{Graph: g}, g.RNICs[0], mem)
	if err != nil {
		t.Fatal(err)
	}
	if iters != 512 {
		t.Errorf("loopback iters = %d, want 512", iters)
	}
	if got := mutualReqIters(Prober{}, g.RNICs[0], g.RNICs[1]); got != 64 {
		t.Errorf("mutual iters = %d, want 64", got)
	}
	p := Prober{Iters: 7, MutualIters: 9}
	if got := mutualReqIters(p, g.RNICs[0], g.RNICs[1]); got != 9 {
		t.Errorf("mutual iters = %d, want MutualIters 9, not Iters", got)
	}
}

// The simulator ignores the warm-up: same reading, no mem_warmup reported.
func TestMemWarmupLeavesSimUnchanged(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	var mem model.Endpoint
	for _, ep := range g.Endpoints {
		if ep.Kind == model.EndpointMemory {
			mem = ep
			break
		}
	}
	off := Prober{Sim: w, Graph: g, TimeoutMS: 1000}
	on := off
	on.MemWarmup = 100 * time.Millisecond
	a, err := off.Loopback(context.Background(), g.RNICs[0], mem, 1, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	b, err := on.Loopback(context.Background(), g.RNICs[0], mem, 1, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("warm-up changed the simulated reading:\n off %+v\n  on %+v", a, b)
	}
	if b.Verbs.MemWarmup != 0 {
		t.Errorf("simulated mem_warmup = %d, want 0", b.Verbs.MemWarmup)
	}
}

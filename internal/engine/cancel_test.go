package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// blockingProber models the Zig prober: it ignores ctx and only returns after
// its own timeout, so the probe loop is the only place that can honour ctx.
type blockingProber struct {
	block time.Duration
	onRun func()
	loops atomic.Int32
	muts  atomic.Int32
}

func (p *blockingProber) Loopback(_ context.Context, n model.RNIC, ep model.Endpoint, _, _ uint32) (model.PathMeasurement, error) {
	p.loops.Add(1)
	time.Sleep(p.block)
	p.onRun()
	return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: 197_000_000_000, SmallLatNsP50: 1000}, nil
}

func (p *blockingProber) Mutual(_ context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	p.muts.Add(1)
	time.Sleep(p.block)
	p.onRun()
	return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: true}, nil
}

func TestEngineAbortsLoopbackLoopOnCancel(t *testing.T) {
	g := topology.Fig2()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &blockingProber{block: 20 * time.Millisecond}
	var once sync.Once
	p.onRun = func() { once.Do(cancel) }
	e := New(config.Defaults(), g, p)

	start := time.Now()
	_, err := e.Run(ctx, RunRequest{Mode: model.ModeIdle})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if n := p.loops.Load(); n != 1 {
		t.Fatalf("engine kept probing after cancel: %d loopback probes", n)
	}
	if n := p.muts.Load(); n != 0 {
		t.Fatalf("engine ran %d mutual probes after cancel", n)
	}
	// Stopping must cost at most the one probe already in flight.
	if elapsed > 10*p.block {
		t.Fatalf("engine took %v to stop after cancel", elapsed)
	}
}

func TestEngineAbortsMutualLoopOnCancel(t *testing.T) {
	g := topology.Fig2()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &blockingProber{}
	// Let every loopback through, then cancel during the first mutual probe.
	p.onRun = func() {
		if p.muts.Load() > 0 {
			cancel()
		}
	}
	e := New(config.Defaults(), g, p)

	_, err := e.Run(ctx, RunRequest{Mode: model.ModeIdle})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if n := p.muts.Load(); n != 1 {
		t.Fatalf("engine kept probing after cancel: %d mutual probes", n)
	}
}

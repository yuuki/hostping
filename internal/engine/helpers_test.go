package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// The fakes and run helpers the engine tests share. A fake that counts its
// loopbacks embeds probeRecorder, so a test can pin both how often a path was
// read (calls) and the order the engine read the paths in (order).

const (
	flatBPS   = 197_000_000_000
	flatLatNs = 1000
)

// flatMeasurement is the healthy reading a flat fake returns for a path.
func flatMeasurement(n model.RNIC, ep model.Endpoint) model.PathMeasurement {
	return model.PathMeasurement{RNIC: n.ID, Endpoint: ep.ID, BandwidthBPS: flatBPS, SmallLatNsP50: flatLatNs}
}

// connected is the healthy reading a flat fake returns for a mutual pair.
func connected(src, dst model.RNIC) model.MutualMeasurement {
	return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: true}
}

// probeRecorder records the loopbacks of the fake that embeds it: calls
// counts the reads per path, keyed "rnic->endpoint", and order lists every
// read in the order the engine made them.
type probeRecorder struct {
	mu    sync.Mutex
	calls map[string]int
	order []string
}

// record notes one read of rnic->ep and returns how many reads of that path
// came before it (0 is the sweep's first).
func (r *probeRecorder) record(rnic, ep string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	key := rnic + "->" + ep
	n := r.calls[key]
	r.calls[key] = n + 1
	r.order = append(r.order, key)
	return n
}

// reset forgets the reads so far, so read 0 is the next run's sweep.
func (r *probeRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = map[string]int{}
	r.order = nil
}

// total is the number of loopbacks so far.
func (r *probeRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.order)
}

// byRNIC is the number of loopbacks on rnic so far.
func (r *probeRecorder) byRNIC(rnic string) int {
	return r.count(func(key string) bool { return strings.HasPrefix(key, rnic+"->") })
}

// byEndpoint is the number of loopbacks to ep so far, from any RNIC.
func (r *probeRecorder) byEndpoint(ep string) int {
	return r.count(func(key string) bool { return strings.HasSuffix(key, "->"+ep) })
}

func (r *probeRecorder) count(ok func(key string) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, key := range r.order {
		if ok(key) {
			n++
		}
	}
	return n
}

// loopbackHook may edit or fail a fake's reading of rnic->ep: n is the
// path's read index in the run (0 is the sweep, 1 and up the rechecks) and m
// the reading, which the hook may change in place. An error fails the read.
type loopbackHook func(rnic, ep string, n int, m *model.PathMeasurement) error

// flatProber answers every loopback with flatMeasurement, passed through hook
// when one is set, and every mutual probe as connected. It records every
// loopback.
type flatProber struct {
	probeRecorder
	hook loopbackHook
}

func (p *flatProber) Loopback(_ context.Context, n model.RNIC, ep model.Endpoint, _, _ uint32) (model.PathMeasurement, error) {
	i := p.record(n.ID, ep.ID)
	m := flatMeasurement(n, ep)
	if p.hook != nil {
		if err := p.hook(n.ID, ep.ID, i, &m); err != nil {
			return model.PathMeasurement{}, err
		}
	}
	return m, nil
}

func (p *flatProber) Mutual(_ context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	return connected(src, dst), nil
}

// countProber answers flat and lets fail decide each loopback's outcome from
// the RNIC and the RNIC's probe count so far (0 = first), whatever the
// endpoint.
func countProber(fail func(rnic string, i int) error) *flatProber {
	p := &flatProber{}
	p.hook = func(rnic, _ string, _ int, _ *model.PathMeasurement) error {
		// The read being decided is already recorded.
		return fail(rnic, p.byRNIC(rnic)-1)
	}
	return p
}

// simHookProber reads every path from the simulator, passed through hook when
// one is set, and records every loopback.
type simHookProber struct {
	sim.Prober
	probeRecorder
	hook loopbackHook
}

func newSimHookProber(w *sim.World, hook loopbackHook) *simHookProber {
	return &simHookProber{Prober: sim.Prober{World: w}, hook: hook}
}

func (p *simHookProber) Loopback(ctx context.Context, n model.RNIC, ep model.Endpoint, small, large uint32) (model.PathMeasurement, error) {
	i := p.record(n.ID, ep.ID)
	m, err := p.Prober.Loopback(ctx, n, ep, small, large)
	if err != nil {
		return m, err
	}
	if p.hook != nil {
		if err := p.hook(n.ID, ep.ID, i, &m); err != nil {
			return model.PathMeasurement{}, err
		}
	}
	return m, nil
}

// degradeProber reads one path at a fixed bandwidth and every other path
// from the simulator: a transit fault that touches that path alone.
func degradeProber(w *sim.World, rnic, ep string, bps uint64) *simHookProber {
	return newSimHookProber(w, func(r, e string, _ int, m *model.PathMeasurement) error {
		if r == rnic && e == ep {
			m.BandwidthBPS = bps
		}
		return nil
	})
}

// windowProber reads every path from the simulator and, where set, adds
// bump(rnic, ep, n) to its small-message latency and scales its bandwidth by
// scale(rnic, ep, n), where n counts the path's reads in the run so far (0 is
// the sweep, 1 and up the rechecks): a time window over some reads. fail,
// when set, may fail a read instead.
type windowProber struct {
	simHookProber
	bump  func(rnic, ep string, n int) uint64
	scale func(rnic, ep string, n int) float64
	fail  func(rnic, ep string, n int) error
}

func newWindowProber(g topology.Graph, bump func(rnic, ep string, n int) uint64) *windowProber {
	p := &windowProber{bump: bump}
	p.init(sim.New(g))
	return p
}

func newBWWindowProber(w *sim.World, scale func(rnic, ep string, n int) float64) *windowProber {
	p := &windowProber{scale: scale}
	p.init(w)
	return p
}

func (p *windowProber) init(w *sim.World) {
	p.World = w
	p.hook = func(rnic, ep string, n int, m *model.PathMeasurement) error {
		if p.fail != nil {
			if err := p.fail(rnic, ep, n); err != nil {
				return err
			}
		}
		if p.bump != nil {
			m.SmallLatNsP50 += p.bump(rnic, ep, n)
		}
		if p.scale != nil {
			m.BandwidthBPS = uint64(float64(m.BandwidthBPS) * p.scale(rnic, ep, n))
		}
		return nil
	}
}

// cancelAfter cancels the run's context on its after-th loopback and counts
// the loopbacks in n.
type cancelAfter struct {
	Prober
	after  int
	cancel context.CancelFunc
	n      int
}

func (c *cancelAfter) Loopback(ctx context.Context, n model.RNIC, ep model.Endpoint, s, l uint32) (model.PathMeasurement, error) {
	c.n++
	if c.n == c.after {
		c.cancel()
	}
	return c.Prober.Loopback(ctx, n, ep, s, l)
}

// logSink captures the aggregated lines the engine would otherwise write
// through the standard logger.
type logSink struct{ lines []string }

func (s *logSink) logf(format string, args ...any) {
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

// quietEngine is New with the engine's log silenced.
func quietEngine(cfg config.Config, g topology.Graph, p Prober) *Engine {
	e := New(cfg, g, p)
	e.Logf = func(string, ...any) {}
	return e
}

// runWith runs e once and fails the test on an error. An engine with no Logf
// yet is silenced first, so the run writes nothing through the standard
// logger; a sink a test set is kept.
func runWith(t *testing.T, e *Engine, req RunRequest) model.Report {
	t.Helper()
	if e.Logf == nil {
		e.Logf = func(string, ...any) {}
	}
	rep, err := e.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// runIdle runs a fresh engine once in idle mode.
func runIdle(t *testing.T, cfg config.Config, g topology.Graph, p Prober) model.Report {
	t.Helper()
	return runWith(t, New(cfg, g, p), RunRequest{Mode: model.ModeIdle})
}

// pathOf returns rep's row for rnic->ep, failing the test when there is none.
func pathOf(t *testing.T, rep model.Report, rnic, ep string) model.PathMeasurement {
	t.Helper()
	for _, m := range rep.Paths {
		if m.RNIC == rnic && m.Endpoint == ep {
			return m
		}
	}
	t.Fatalf("no path %s->%s in the report", rnic, ep)
	return model.PathMeasurement{}
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

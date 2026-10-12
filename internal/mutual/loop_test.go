package mutual

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.t) {
		c.t = t
	}
}

type scheduled struct {
	at time.Time
	r  Reply
}

// fakeTransport answers probes on the fake clock. Wait advances the clock to
// the next reply (or to the end of its budget), so Run is deterministic.
type fakeTransport struct {
	clk *fakeClock
	// delay is the reply time of a probe; ok=false loses it.
	delay       func(src, dst int) (time.Duration, bool)
	unavailable map[int]bool
	noRoute     map[int]bool
	stopAt      time.Time
	cancel      context.CancelFunc

	inflight []scheduled
	sends    int
	closed   bool
}

func ns(t time.Time) uint64 { return uint64(t.Sub(epoch)) }

func (f *fakeTransport) Send(src, dst int, seq uint32) (uint64, error) {
	now := f.clk.Now()
	if f.unavailable[src] || f.unavailable[dst] {
		return 0, fmt.Errorf("%w: test", ErrUnavailable)
	}
	if f.noRoute[src] || f.noRoute[dst] {
		return ns(now), fmt.Errorf("%w: test", ErrNoRoute)
	}
	f.sends++
	if d, ok := f.delay(src, dst); ok {
		at := now.Add(d)
		f.inflight = append(f.inflight, scheduled{at: at, r: Reply{Src: src, Seq: seq, AtNs: ns(at)}})
	}
	return ns(now), nil
}

func (f *fakeTransport) Wait(spin, block time.Duration) ([]Reply, error) {
	now := f.clk.Now()
	deadline := now.Add(spin + block)
	first := time.Time{}
	for _, s := range f.inflight {
		if !s.at.After(deadline) && (first.IsZero() || s.at.Before(first)) {
			first = s.at
		}
	}
	if first.IsZero() {
		f.clk.set(deadline)
	} else {
		f.clk.set(first)
	}
	now = f.clk.Now()
	var out []Reply
	rest := f.inflight[:0]
	for _, s := range f.inflight {
		if !s.at.After(now) {
			out = append(out, s.r)
		} else {
			rest = append(rest, s)
		}
	}
	f.inflight = rest
	if !f.stopAt.IsZero() && !now.Before(f.stopAt) {
		f.cancel()
	}
	return out, nil
}

func (f *fakeTransport) Close() { f.closed = true }

func rnics(n int) []model.RNIC {
	out := make([]model.RNIC, n)
	for i := range out {
		out[i] = model.RNIC{ID: fmt.Sprintf("rnic%d", i), Device: fmt.Sprintf("dev%d", i)}
	}
	return out
}

func allPairs(rs []model.RNIC) [][2]model.RNIC {
	var out [][2]model.RNIC
	for i := range rs {
		for j := i + 1; j < len(rs); j++ {
			out = append(out, [2]model.RNIC{rs[i], rs[j]})
		}
	}
	return out
}

func tonConfig() Config {
	return Config{Interval: 100 * time.Millisecond, Timeout: 500 * time.Millisecond, Window: 20 * time.Second, Spin: time.Millisecond, TimeoutTh: 0.1, MinProbes: 10}
}

func healthy(time.Duration) func(int, int) (time.Duration, bool) {
	return func(int, int) (time.Duration, bool) { return 8 * time.Microsecond, true }
}

// runFor runs the loop on the fake clock until d has passed.
func runFor(t *testing.T, cfg Config, n int, ft *fakeTransport, d time.Duration) (*Loop, []model.RNIC) {
	t.Helper()
	rs := rnics(n)
	ft.clk = &fakeClock{t: epoch}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ft.stopAt = epoch.Add(d)
	ft.cancel = cancel
	i := 0
	intn := func(k int) int { i++; return i % k }
	l := New(cfg, rs, allPairs(rs), ft, ft.clk.Now, intn, nil)
	l.Run(ctx)
	if !ft.closed {
		t.Fatal("Run must close the transport")
	}
	return l, rs
}

func byPair(ms []model.MutualMeasurement) map[string]model.MutualMeasurement {
	out := map[string]model.MutualMeasurement{}
	for _, m := range ms {
		out[m.Src+"-"+m.Dst] = m
	}
	return out
}

// Every RNIC sends one probe per interval, so a full ToN window holds about
// window/interval probes per RNIC, spread over its peers; a healthy host
// answers all of them.
func TestLoopHealthyWindow(t *testing.T) {
	ft := &fakeTransport{delay: healthy(0)}
	l, _ := runFor(t, tonConfig(), 3, ft, 21*time.Second)
	ms, ready := l.Snapshot()
	if !ready {
		t.Fatal("a full window must be ready")
	}
	if len(ms) != 3 {
		t.Fatalf("want 3 pairs, got %+v", ms)
	}
	var sent uint32
	for _, m := range ms {
		if !m.Connected || m.Recv != m.Sent || m.Sent == 0 {
			t.Fatalf("healthy pair must answer every probe: %+v", m)
		}
		if r, ok := m.TimeoutRatio(); !ok || r != 0 {
			t.Fatalf("timeout ratio: %v %v", r, ok)
		}
		if m.RTTnsP50 != uint64(8*time.Microsecond) || !m.Reliable || m.LossRatio != 0 {
			t.Fatalf("rtt/loss: %+v", m)
		}
		sent += m.Sent
	}
	// 3 RNICs × 200 rounds in 20 s, give or take the round at each edge.
	if sent < 3*199 || sent > 3*201 {
		t.Fatalf("want ~600 probes in the window, got %d", sent)
	}
	if l.Alarm() {
		t.Fatal("healthy window must not alarm")
	}
}

func TestLoopNotReadyBeforeWindow(t *testing.T) {
	ft := &fakeTransport{delay: healthy(0)}
	l, _ := runFor(t, tonConfig(), 3, ft, 5*time.Second)
	if ms, ready := l.Snapshot(); ready || ms != nil {
		t.Fatalf("a partial window must not be used: %v %+v", ready, ms)
	}
	if l.Alarm() {
		t.Fatal("a partial window must not alarm")
	}
}

// An RNIC whose probes are all lost times out on every pair; the others stay
// clean, and the window raises the alarm.
func TestLoopLossyRNIC(t *testing.T) {
	ft := &fakeTransport{delay: func(src, dst int) (time.Duration, bool) {
		return 8 * time.Microsecond, src != 2 && dst != 2
	}}
	l, _ := runFor(t, tonConfig(), 3, ft, 21*time.Second)
	ms, _ := l.Snapshot()
	p := byPair(ms)
	for _, k := range []string{"rnic0-rnic2", "rnic1-rnic2"} {
		m := p[k]
		if r, ok := m.TimeoutRatio(); !ok || r != 1 || m.Sent == 0 || !m.Connected {
			t.Fatalf("%s: want ratio 1, got %+v", k, m)
		}
	}
	if r, _ := p["rnic0-rnic1"].TimeoutRatio(); r != 0 {
		t.Fatalf("rnic0-rnic1 must be clean: %+v", p["rnic0-rnic1"])
	}
	if !l.Alarm() {
		t.Fatal("a pair above Timeout_th must alarm")
	}
}

// Isolated losses are what the RC probe used to hide: every lost probe is
// exactly one timeout.
func TestLoopIsolatedLossCounts(t *testing.T) {
	k := 0
	ft := &fakeTransport{delay: func(int, int) (time.Duration, bool) {
		k++
		return 8 * time.Microsecond, k%20 != 0 // 5% loss
	}}
	l, _ := runFor(t, tonConfig(), 2, ft, 21*time.Second)
	ms, _ := l.Snapshot()
	if len(ms) != 1 {
		t.Fatalf("want one pair, got %+v", ms)
	}
	r, _ := ms[0].TimeoutRatio()
	if r < 0.04 || r > 0.06 {
		t.Fatalf("want ~5%% timeouts, got %v (%+v)", r, ms[0])
	}
	if l.Alarm() {
		t.Fatal("5% is below Timeout_th")
	}
}

// A reply that arrives after its probe timed out is a timeout, and it must not
// be credited to the next probe of the same pair.
func TestLoopLateReplyIsTimeout(t *testing.T) {
	ft := &fakeTransport{delay: func(int, int) (time.Duration, bool) { return 700 * time.Millisecond, true }}
	l, _ := runFor(t, tonConfig(), 2, ft, 21*time.Second)
	ms, _ := l.Snapshot()
	if len(ms) != 1 || ms[0].Recv != 0 || ms[0].Sent == 0 {
		t.Fatalf("late replies must all be timeouts: %+v", ms)
	}
}

// A reply just inside the timeout counts, with its RTT.
func TestLoopSlowReplyWithinTimeout(t *testing.T) {
	ft := &fakeTransport{delay: func(int, int) (time.Duration, bool) { return 300 * time.Millisecond, true }}
	l, _ := runFor(t, tonConfig(), 2, ft, 21*time.Second)
	ms, _ := l.Snapshot()
	if len(ms) != 1 || ms[0].Recv != ms[0].Sent || ms[0].RTTnsP50 != uint64(300*time.Millisecond) {
		t.Fatalf("slow replies inside the timeout count: %+v", ms)
	}
}

// An endpoint that could not be opened measures nothing: its pairs are left
// out rather than reported as timeouts.
func TestLoopUnavailableEndpointNotCounted(t *testing.T) {
	ft := &fakeTransport{delay: healthy(0), unavailable: map[int]bool{2: true}}
	l, _ := runFor(t, tonConfig(), 3, ft, 21*time.Second)
	ms, _ := l.Snapshot()
	p := byPair(ms)
	if _, ok := p["rnic0-rnic2"]; ok {
		t.Fatalf("unavailable endpoint must not be measured: %+v", ms)
	}
	if m := p["rnic0-rnic1"]; m.Sent == 0 || m.Recv != m.Sent {
		t.Fatalf("rnic0-rnic1: %+v", m)
	}
}

// No route to the peer is a pair that never connected.
func TestLoopNoRouteIsNotConnected(t *testing.T) {
	ft := &fakeTransport{delay: healthy(0), noRoute: map[int]bool{1: true}}
	l, _ := runFor(t, tonConfig(), 2, ft, 21*time.Second)
	ms, _ := l.Snapshot()
	if len(ms) != 1 || ms[0].Connected {
		t.Fatalf("want a not-connected pair: %+v", ms)
	}
	if r, ok := ms[0].TimeoutRatio(); !ok || r != 1 {
		t.Fatalf("ratio %v %v", r, ok)
	}
	if !l.Alarm() {
		t.Fatal("a dead pair must alarm")
	}
}

// Snapshot is called from the agent while Run records outcomes.
func TestLoopSnapshotConcurrent(t *testing.T) {
	rs := rnics(4)
	clk := &fakeClock{t: epoch}
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTransport{clk: clk, delay: healthy(0), stopAt: epoch.Add(30 * time.Second), cancel: cancel}
	l := New(tonConfig(), rs, allPairs(rs), ft, clk.Now, nil, nil)
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	for {
		select {
		case <-done:
			if _, ready := l.Snapshot(); !ready {
				t.Fatal("30s of probing must fill the window")
			}
			return
		default:
			l.Snapshot()
			l.Alarm()
		}
	}
}

// Under --mutual-scope=host a pair may cross rails; the window marks it as the
// engine marks a burst, so the analyzer's cross-rail rules apply to it.
func TestLoopMarksCrossRailPairs(t *testing.T) {
	rs := rnics(3)
	rs[0].Fabric, rs[1].Fabric, rs[2].Fabric = "railA", "railA", "railB"
	clk := &fakeClock{t: epoch}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ft := &fakeTransport{clk: clk, delay: healthy(0), stopAt: epoch.Add(21 * time.Second), cancel: cancel}
	l := New(tonConfig(), rs, allPairs(rs), ft, clk.Now, nil, nil)
	l.Run(ctx)
	ms, _ := l.Snapshot()
	p := byPair(ms)
	if p["rnic0-rnic1"].CrossRail || !p["rnic0-rnic2"].CrossRail || !p["rnic1-rnic2"].CrossRail {
		t.Fatalf("cross-rail marks: %+v", ms)
	}
}

func TestWindowPrunesByAge(t *testing.T) {
	w := pairWindow{src: "a", dst: "b"}
	win := 20 * time.Second
	w.add(outcome{at: epoch, ok: true, rttNs: 10}, win)
	w.add(outcome{at: epoch.Add(10 * time.Second)}, win)
	// Out of order: resolved later, sent earlier.
	w.add(outcome{at: epoch.Add(5 * time.Second), ok: true, rttNs: 30}, win)
	m, ok := w.measurement(false)
	if !ok || m.Sent != 3 || m.Recv != 2 || m.RTTnsP50 != 10 || m.RTTnsP99 != 10 {
		t.Fatalf("got %+v", m)
	}
	w.add(outcome{at: epoch.Add(22 * time.Second), ok: true, rttNs: 20}, win)
	m, _ = w.measurement(false)
	if m.Sent != 3 || m.Recv != 2 {
		t.Fatalf("the probe sent at 0s is older than the window: %+v", m)
	}
	w.prune(epoch.Add(30 * time.Second))
	if _, ok := w.measurement(false); ok {
		t.Fatal("an empty window has no measurement")
	}
}

func TestWindowLossIsDrop(t *testing.T) {
	w := pairWindow{src: "a", dst: "b"}
	for i := 0; i < 4; i++ {
		w.add(outcome{at: epoch.Add(time.Duration(i) * time.Second), ok: i != 0}, time.Minute)
	}
	m, _ := w.measurement(true)
	if m.Reliable || m.LossRatio != 0.25 {
		t.Fatalf("simulated loss must read as drop loss: %+v", m)
	}
	m, _ = w.measurement(false)
	if !m.Reliable || m.LossRatio != 0 {
		t.Fatalf("live loss is timeouts only: %+v", m)
	}
}

// The peer choice is random per round, so every pair of a small host is
// covered many times within one window.
func TestLoopRandomPeersCoverEveryPair(t *testing.T) {
	rs := rnics(4)
	clk := &fakeClock{t: epoch}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ft := &fakeTransport{clk: clk, delay: healthy(0), stopAt: epoch.Add(21 * time.Second), cancel: cancel}
	l := New(tonConfig(), rs, allPairs(rs), ft, clk.Now, nil, nil)
	l.Run(ctx)
	ms, _ := l.Snapshot()
	if len(ms) != 6 {
		t.Fatalf("want all 6 pairs, got %d", len(ms))
	}
	for _, m := range ms {
		if m.Sent < 50 {
			t.Fatalf("pair %s-%s probed only %d times", m.Src, m.Dst, m.Sent)
		}
	}
}

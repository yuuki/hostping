package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

// fakeClock is the injectable clock for the rate-limit and backoff tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

const smiOut = "00000000:17:00.0, 42\n00000000:65:00.0, 3\n"

// utilOf is the utilization half of the cache's sample, which is all these
// tests look at.
func utilOf(c *gpuUtilCache, ctx context.Context, every time.Duration) map[string]float64 {
	return c.get(ctx, every).util
}

func TestGPUUtilCacheRateLimitsNvidiaSMI(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	c := &gpuUtilCache{
		now: clk.now,
		run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			calls++
			if name != "nvidia-smi" {
				t.Fatalf("ran %q, want nvidia-smi", name)
			}
			return []byte(smiOut), nil
		},
	}
	ctx := context.Background()

	// The first call of the process must sample (one-shot/warmup path).
	got := utilOf(c, ctx, 10*time.Second)
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if got["0000:17:00.0"] != 42 || got["0000:65:00.0"] != 3 {
		t.Fatalf("util=%v", got)
	}

	// Nine 1s polls inside the interval must not fork again.
	for i := 0; i < 9; i++ {
		clk.add(time.Second)
		if u := utilOf(c, ctx, 10*time.Second)["0000:17:00.0"]; u != 42 {
			t.Fatalf("cached util=%v, want 42", u)
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d after 9 polls, want 1", calls)
	}

	// Past the interval it refreshes exactly once.
	clk.add(time.Second)
	utilOf(c, ctx, 10*time.Second)
	if calls != 2 {
		t.Fatalf("calls=%d after interval, want 2", calls)
	}
}

func TestGPUUtilCacheCachesFailure(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	c := &gpuUtilCache{
		now: clk.now,
		run: func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return nil, errors.New("nvidia-smi: not found")
		},
	}
	ctx := context.Background()
	if u := utilOf(c, ctx, 10*time.Second); u != nil {
		t.Fatalf("util=%v, want nil", u)
	}
	clk.add(5 * time.Second)
	if u := utilOf(c, ctx, 10*time.Second); u != nil {
		t.Fatalf("util=%v, want nil", u)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 (failure must be cached too)", calls)
	}
	clk.add(6 * time.Second)
	utilOf(c, ctx, 10*time.Second)
	if calls != 2 {
		t.Fatalf("calls=%d after interval, want 2", calls)
	}
}

func TestGPUUtilCacheZeroIntervalAlwaysSamples(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	c := &gpuUtilCache{
		now: clk.now,
		run: func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return []byte(smiOut), nil
		},
	}
	utilOf(c, context.Background(), 0)
	utilOf(c, context.Background(), 0)
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

const ethtoolPauseOut = "NIC statistics:\n" +
	"     tx_prio0_pause_duration: 7\n" +
	"     rx_prio0_pause_duration: 5\n" +
	"     rx_discards_phy: 1\n"

func TestEthtoolScanAccumulatesPauseDuration(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	g := &ethtoolGate{
		now: clk.now,
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls++
			if name != "ethtool" || len(args) != 2 || args[0] != "-S" || args[1] != "eth0" {
				t.Fatalf("ran %q %v", name, args)
			}
			return []byte(ethtoolPauseOut), nil
		},
	}
	var c model.RNICCounters
	g.scan(context.Background(), "eth0", &c, true)
	if c.TxPauseDurationUs != 7 || c.RxPauseDurationUs != 5 {
		t.Fatalf("tx=%d rx=%d, want 7/5", c.TxPauseDurationUs, c.RxPauseDurationUs)
	}
	// A device that answers keeps being polled every time.
	clk.add(time.Second)
	g.scan(context.Background(), "eth0", &c, true)
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func TestEthtoolScanBacksOffWhenUnavailable(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	g := &ethtoolGate{
		now: clk.now,
		run: func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return nil, errors.New("exec: ethtool: not found")
		},
	}
	var c model.RNICCounters
	ctx := context.Background()
	g.scan(ctx, "eth0", &c, true)
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	// 29 one-second polls inside the first backoff window fork nothing.
	for i := 0; i < 29; i++ {
		clk.add(time.Second)
		g.scan(ctx, "eth0", &c, true)
	}
	if calls != 1 {
		t.Fatalf("calls=%d during backoff, want 1", calls)
	}
	clk.add(time.Second)
	g.scan(ctx, "eth0", &c, true)
	if calls != 2 {
		t.Fatalf("calls=%d after backoff, want 2", calls)
	}
	// The window doubles after each consecutive failure.
	clk.add(ethtoolRetryMin)
	g.scan(ctx, "eth0", &c, true)
	if calls != 2 {
		t.Fatalf("calls=%d, want 2 (backoff must have doubled)", calls)
	}
	clk.add(ethtoolRetryMin)
	g.scan(ctx, "eth0", &c, true)
	if calls != 3 {
		t.Fatalf("calls=%d after doubled backoff, want 3", calls)
	}
	if got := g.backoff["eth0"]; got != 4*ethtoolRetryMin {
		t.Fatalf("backoff=%v, want %v", got, 4*ethtoolRetryMin)
	}
}

func TestEthtoolScanBackoffIsPerNetdevAndCapped(t *testing.T) {
	clk := newFakeClock()
	g := &ethtoolGate{
		now: clk.now,
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("no such device")
		},
	}
	var c model.RNICCounters
	ctx := context.Background()
	g.scan(ctx, "eth0", &c, true)
	if !g.allow("eth1") {
		t.Fatal("eth1 must not inherit eth0 backoff")
	}
	for i := 0; i < 20; i++ {
		clk.add(ethtoolRetryMax)
		g.scan(ctx, "eth0", &c, true)
	}
	if got := g.backoff["eth0"]; got != ethtoolRetryMax {
		t.Fatalf("backoff=%v, want cap %v", got, ethtoolRetryMax)
	}
}

func TestEthtoolScanBacksOffWithoutPauseCounters(t *testing.T) {
	clk := newFakeClock()
	calls := 0
	g := &ethtoolGate{
		now: clk.now,
		run: func(context.Context, string, ...string) ([]byte, error) {
			calls++
			return []byte("NIC statistics:\n     rx_packets: 12\n     tx_packets: 9\n"), nil
		},
	}
	var c model.RNICCounters
	ctx := context.Background()
	g.scan(ctx, "eth0", &c, true)
	clk.add(time.Second)
	g.scan(ctx, "eth0", &c, true)
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 (device has no pause counters)", calls)
	}
	// A successful read later clears the backoff.
	clk.add(ethtoolRetryMin)
	g.run = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return []byte(ethtoolPauseOut), nil
	}
	g.scan(ctx, "eth0", &c, true)
	if calls != 2 || c.TxPauseDurationUs != 7 {
		t.Fatalf("calls=%d tx=%d", calls, c.TxPauseDurationUs)
	}
	if _, ok := g.retryAt["eth0"]; ok {
		t.Fatal("backoff must be cleared after a successful read")
	}
}

func TestEthtoolScanSkipsEmptyNetdev(t *testing.T) {
	g := &ethtoolGate{run: func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("must not fork for an empty netdev")
		return nil, nil
	}}
	var c model.RNICCounters
	g.scan(context.Background(), "", &c, true)
}

func TestRunWithTimeoutKillsHungCommand(t *testing.T) {
	start := time.Now()
	_, err := runWithTimeout(context.Background(), 50*time.Millisecond, "sleep", "30")
	if err == nil {
		t.Fatal("want error from the timed-out command")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("runWithTimeout took %v; the child was not killed", d)
	}
}

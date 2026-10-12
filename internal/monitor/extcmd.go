package monitor

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// nvidia-smi and ethtool are forked from the poll loop, which ticks every
// MetricInterval (1s by default). nvidia-smi costs hundreds of milliseconds
// and can hang on a wedged driver, so every external command is bounded and
// the GPU query is rate-limited to gpuUtilCache's interval.
const (
	// execTimeout bounds a single external sampler command.
	execTimeout = 2 * time.Second
	// DefaultGPUUtilInterval is the minimum period between nvidia-smi runs.
	// GPU utilization only feeds the idle/busy classification, whose consumer
	// (the idle full-mesh run) fires every IdleInterval (5m by default).
	DefaultGPUUtilInterval = 10 * time.Second
	// ethtoolRetryMin/Max bound the backoff after ethtool -S yields no pause
	// counters for a netdev.
	ethtoolRetryMin = 30 * time.Second
	ethtoolRetryMax = 5 * time.Minute
)

// runner runs an external sampler binary. Injectable for tests.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func runBounded(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runWithTimeout(ctx, execTimeout, name, args...)
}

// runWithTimeout kills the child once the timeout elapses; a hung binary must
// not stall the poll loop.
func runWithTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// gpuUtilCache rate-limits nvidia-smi and serves the last sample in between.
// The compute mode rides on the same query, so it costs no extra fork and
// comes from the same snapshot that decided whether a run starts.
type gpuUtilCache struct {
	mu sync.Mutex
	// run and now are nil outside tests.
	run   runner
	now   func() time.Time
	at    time.Time
	valid bool
	smi   nvidiaSample
}

var gpuUtil = &gpuUtilCache{}

func (c *gpuUtilCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *gpuUtilCache) runner() runner {
	if c.run != nil {
		return c.run
	}
	return runBounded
}

// get returns GPU utilization by normalized BDF, refreshing at most once per
// every. A failed query is cached too, so a missing nvidia-smi is not probed
// every second. The first call after process start always samples.
func (c *gpuUtilCache) get(ctx context.Context, every time.Duration) map[string]float64 {
	return c.sample(ctx, every).util
}

// sample is get with the compute modes read alongside the utilization.
func (c *gpuUtilCache) sample(ctx context.Context, every time.Duration) nvidiaSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.valid && every > 0 && now.Sub(c.at) < every {
		return c.smi
	}
	c.smi = nvidiaGPUsByBDF(ctx, c.runner())
	c.at = now
	c.valid = true
	return c.smi
}

// ethtoolGate suppresses ethtool -S per netdev once it is known not to answer.
// The pause counters it reports are a fallback for vendors that do not export
// them under hw_counters, so a device that has none must not be re-forked on
// every poll.
type ethtoolGate struct {
	mu sync.Mutex
	// run and now are nil outside tests.
	run     runner
	now     func() time.Time
	backoff map[string]time.Duration
	retryAt map[string]time.Time
	// why is the failure behind each backoff, for the log.
	why map[string]string
}

var ethtool = &ethtoolGate{}

func (g *ethtoolGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *ethtoolGate) runner() runner {
	if g.run != nil {
		return g.run
	}
	return runBounded
}

func (g *ethtoolGate) allow(netdev string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	at, ok := g.retryAt[netdev]
	return !ok || !g.clock().Before(at)
}

func (g *ethtoolGate) succeeded(netdev string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.retryAt, netdev)
	delete(g.backoff, netdev)
	delete(g.why, netdev)
}

// lastWhy is the failure that put netdev in backoff.
func (g *ethtoolGate) lastWhy(netdev string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.why[netdev]
}

func (g *ethtoolGate) failed(netdev, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.backoff == nil {
		g.backoff = map[string]time.Duration{}
		g.retryAt = map[string]time.Time{}
	}
	if g.why == nil {
		g.why = map[string]string{}
	}
	g.why[netdev] = why
	d := g.backoff[netdev] * 2
	if d < ethtoolRetryMin {
		d = ethtoolRetryMin
	}
	if d > ethtoolRetryMax {
		d = ethtoolRetryMax
	}
	g.backoff[netdev] = d
	g.retryAt[netdev] = g.clock().Add(d)
}

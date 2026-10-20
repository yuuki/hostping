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

// rateLimited serves the last result of an external query for every after it
// ran, so a command that costs a fork (and can hang on a wedged driver) is
// not run on each 1 s poll. The first call of the process always runs the
// query, so --once is never served stale; a failed query is cached like any
// other result, so a missing binary is not probed every second; every <= 0
// runs the query on each call. Q is the query the cache refreshes.
type rateLimited[T any, Q query[T]] struct {
	mu sync.Mutex
	// run and now are nil outside tests.
	run   runner
	now   func() time.Time
	at    time.Time
	valid bool
	last  T
}

// query is what a rateLimited cache refreshes. It is a stateless type, so the
// cache's zero value (a test's literal) already carries it.
type query[T any] interface {
	// defaultRunner is the runner the query forks with when none is injected.
	defaultRunner() runner
	fetch(ctx context.Context, run runner) T
}

func (c *rateLimited[T, Q]) get(ctx context.Context, every time.Duration) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	if c.valid && every > 0 && now.Sub(c.at) < every {
		return c.last
	}
	var q Q
	run := c.run
	if run == nil {
		run = q.defaultRunner()
	}
	c.last = q.fetch(ctx, run)
	c.at = now
	c.valid = true
	return c.last
}

// gpuUtilCache rate-limits nvidia-smi and serves the last sample in between.
// The compute mode rides on the same query, so it costs no extra fork and
// comes from the same snapshot that decided whether a run starts.
type gpuUtilCache = rateLimited[nvidiaSample, nvidiaUtilQuery]

var gpuUtil = &gpuUtilCache{}

// nvidiaUtilQuery is the GPU utilization and compute mode query by normalized
// BDF (nvidiaGPUsByBDF).
type nvidiaUtilQuery struct{}

func (nvidiaUtilQuery) defaultRunner() runner { return runBounded }
func (nvidiaUtilQuery) fetch(ctx context.Context, run runner) nvidiaSample {
	return nvidiaGPUsByBDF(ctx, run)
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

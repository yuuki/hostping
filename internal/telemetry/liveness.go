package telemetry

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Liveness bounds for /healthz.
const (
	// LivenessPolls is how many --metric-intervals the poll loop may go
	// without completing a poll before /healthz turns 503.
	LivenessPolls = 10
	// LivenessFloor is the shortest stall /healthz reports, so a short
	// --metric-interval does not turn one slow sysfs or nvidia-smi read
	// into a failed health check.
	LivenessFloor = 30 * time.Second
	// LivenessRunBudget is how long one engine run may hold the poll loop
	// before /healthz reports it. The loop is synchronous, so no poll
	// completes during a run, and a full mesh whose probes time out one by
	// one legitimately takes minutes; a run this long is stuck.
	LivenessRunBudget = 30 * time.Minute
)

// Liveness tracks whether the daemon's poll loop is still turning, for
// /healthz. The loop calls Polled after every poll that returns (a poll that
// returned an error still proves the loop is alive; hostping.monitor.poll.errors
// counts those) and brackets every engine run with RunStarted and RunEnded.
//
// It deliberately says nothing about whether runs succeed: a host that is
// never idle legitimately never runs, and a failing run is a metric
// (hostping.engine.run.failed) for an alert, not a reason to restart the
// process. All methods are safe on a nil *Liveness, which is always healthy.
type Liveness struct {
	now    func() time.Time
	maxAge time.Duration

	mu      sync.Mutex
	last    time.Time // last poll or run end; the tracker's creation before either
	runFrom time.Time // start of the engine run in progress, zero between runs
}

// NewLiveness returns a tracker that reports a stall after
// max(LivenessPolls × metricInterval, LivenessFloor) without a poll. Its
// creation counts as the first tick, which is the start-up grace: the first
// poll is one interval away and the monitor may still be warming up. now is
// the clock (time.Now when nil).
func NewLiveness(metricInterval time.Duration, now func() time.Time) *Liveness {
	if now == nil {
		now = time.Now
	}
	maxAge := LivenessPolls * metricInterval
	if maxAge < LivenessFloor {
		maxAge = LivenessFloor
	}
	return &Liveness{now: now, maxAge: maxAge, last: now()}
}

// MaxAge is the longest the loop may go without a poll before Check fails.
func (l *Liveness) MaxAge() time.Duration {
	if l == nil {
		return 0
	}
	return l.maxAge
}

// Polled records that a monitor poll returned.
func (l *Liveness) Polled() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.last = l.now()
	l.mu.Unlock()
}

// RunStarted records that the loop is about to block in an engine run.
func (l *Liveness) RunStarted() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.runFrom = l.now()
	l.mu.Unlock()
}

// RunEnded records that the engine run returned, whatever its outcome. It
// counts as a tick: the next poll is at most one interval away.
func (l *Liveness) RunEnded() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.runFrom = time.Time{}
	l.last = l.now()
	l.mu.Unlock()
}

// Check returns nil while the loop is alive, else why it is not.
func (l *Liveness) Check() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if !l.runFrom.IsZero() {
		if d := now.Sub(l.runFrom); d > LivenessRunBudget {
			return fmt.Errorf("engine run in progress for %s (limit %s)", d.Round(time.Second), LivenessRunBudget)
		}
		return nil
	}
	if d := now.Sub(l.last); d > l.maxAge {
		return fmt.Errorf("no monitor poll completed for %s (limit %s)", d.Round(time.Second), l.maxAge)
	}
	return nil
}

// ServeHTTP is /healthz: 200 "ok" while the loop is alive, 503 with the
// reason otherwise.
func (l *Liveness) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := l.Check(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintln(w, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

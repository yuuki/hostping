// Package mutual runs the ToN'24 §IV-A-2 mutual probe on its own schedule:
// every RNIC probes one random peer every interval with a small UD request,
// a probe that gets no reply within the probe timeout counts as a timeout, and
// each pair's timeout ratio is aggregated over a sliding window. The engine
// reads the window instead of probing pairs itself.
package mutual

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// ErrUnavailable is returned by Transport.Send when an endpoint of the probe
// is not open (no device, no RoCEv2 GID, allocation failure). It is local
// setup, not evidence about the path, so the probe is not counted at all.
var ErrUnavailable = errors.New("mutual probe endpoint unavailable")

// ErrNoRoute is returned by Transport.Send when the probe cannot be
// addressed to the peer at all. It is counted as a connectivity failure.
var ErrNoRoute = errors.New("no route to mutual probe peer")

// Reply is one answered probe as the transport returns it: the endpoint that
// sent the request, the probe's sequence number and when the reply was
// received, on the transport's clock (the same clock as Send's return).
type Reply struct {
	Src  int
	Seq  uint32
	AtNs uint64
}

// Transport is the probe's data path. Endpoints are indices into the RNIC
// list the loop was built with. It is used from one goroutine only.
type Transport interface {
	// Send posts probe seq from src to dst and returns when it was posted.
	// Any error other than ErrUnavailable counts the probe as lost.
	Send(src, dst int, seq uint32) (sentNs uint64, err error)
	// Wait answers the requests reaching every endpoint and collects replies.
	// It spins for up to spin, then sleeps for up to block, and returns as
	// soon as at least one reply is in.
	Wait(spin, block time.Duration) ([]Reply, error)
	Close()
}

// Config is the probe schedule. ToN'24 §IV-A-2: every 100 ms, 500 ms
// timeout, 20 s window.
type Config struct {
	Interval time.Duration
	Timeout  time.Duration
	Window   time.Duration
	// Spin is how long the loop busy-polls for replies right after sending a
	// round. A healthy intra-host round trip is microseconds, so this keeps
	// its RTT exact; later replies are awaited asleep.
	Spin time.Duration
	// LossIsDrop marks a transport whose lost probes are injected drops (the
	// simulator), reported as loss_ratio; a live probe's losses are timeouts.
	LossIsDrop bool
	// TimeoutTh and MinProbes define Alarm.
	TimeoutTh float64
	MinProbes int
}

// DefaultSpin bounds the per-round busy-poll.
const DefaultSpin = time.Millisecond

// seqMask is the 24-bit sequence space the UD tag carries (zig/src/udtag.zig).
const seqMask = 1<<24 - 1

type probe struct {
	src, dst int
	pair     int
	at       time.Time
	sentNs   uint64
}

// Loop is the scheduler. Run owns the transport; Snapshot and Alarm may be
// called from any goroutine.
type Loop struct {
	cfg   Config
	t     Transport
	peers [][]int // per RNIC index: the RNICs it may probe
	pairs map[[2]int]int
	now   func() time.Time
	intn  func(int) int
	logf  func(string, ...any)

	// Owned by the Run goroutine.
	pending map[uint32]probe
	seq     uint32
	lastErr string

	mu      sync.Mutex
	win     []pairWindow
	started time.Time
}

// New builds a loop over rnics probing the given pairs, as
// topology.Graph.MutualPairs returns them for the configured scope. now
// and intn default to the wall clock and math/rand; tests inject both.
func New(cfg Config, rnics []model.RNIC, pairs [][2]model.RNIC, t Transport, now func() time.Time, intn func(int) int, logf func(string, ...any)) *Loop {
	if now == nil {
		now = time.Now
	}
	if intn == nil {
		intn = rand.IntN
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if cfg.Spin < 0 {
		cfg.Spin = 0
	}
	idx := map[string]int{}
	for i, n := range rnics {
		idx[n.ID] = i
	}
	l := &Loop{
		cfg: cfg, t: t, now: now, intn: intn, logf: logf,
		peers:   make([][]int, len(rnics)),
		pairs:   map[[2]int]int{},
		pending: map[uint32]probe{},
	}
	for _, p := range pairs {
		a, okA := idx[p[0].ID]
		b, okB := idx[p[1].ID]
		if !okA || !okB || a == b {
			continue
		}
		key := pairKey(a, b)
		if _, dup := l.pairs[key]; dup {
			continue
		}
		l.pairs[key] = len(l.win)
		l.win = append(l.win, pairWindow{src: p[0].ID, dst: p[1].ID, crossRail: topology.CrossRail(p[0], p[1])})
		l.peers[a] = append(l.peers[a], b)
		l.peers[b] = append(l.peers[b], a)
	}
	return l
}

func pause(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func pairKey(a, b int) [2]int {
	if a > b {
		a, b = b, a
	}
	return [2]int{a, b}
}

// Run probes until ctx is done, then closes the transport.
func (l *Loop) Run(ctx context.Context) {
	defer l.t.Close()
	start := l.now()
	l.mu.Lock()
	l.started = start
	l.mu.Unlock()
	next := start
	var spinUntil time.Time
	for ctx.Err() == nil {
		now := l.now()
		if !now.Before(next) {
			l.round(now)
			spinUntil = now.Add(l.cfg.Spin)
			next = next.Add(l.cfg.Interval)
			if !next.After(now) {
				// A round overran the interval (a slow send, a stalled
				// host): skip the missed rounds rather than burst them.
				next = now.Add(l.cfg.Interval)
			}
		}
		wake := next
		if d, ok := l.nextDeadline(); ok && d.Before(wake) {
			wake = d
		}
		spin := time.Duration(0)
		if len(l.pending) > 0 && spinUntil.After(now) {
			spin = spinUntil.Sub(now)
		}
		block := wake.Sub(now) - spin
		if block < 0 {
			block = 0
		}
		reps, err := l.t.Wait(spin, block)
		if err != nil {
			// A failing poll returns at once; wait out the block instead of
			// spinning on it, and say so once per distinct error.
			if msg := err.Error(); msg != l.lastErr {
				l.lastErr = msg
				l.logf("hostping: mutual probe wait: %v", err)
			}
			pause(ctx, block)
		}
		l.complete(reps)
		l.expire(l.now())
	}
}

// round sends one probe from every RNIC that has a peer to a random one.
func (l *Loop) round(now time.Time) {
	for src, peers := range l.peers {
		if len(peers) == 0 {
			continue
		}
		dst := peers[l.intn(len(peers))]
		l.seq = (l.seq + 1) & seqMask
		seq := l.seq
		pair := l.pairs[pairKey(src, dst)]
		sentNs, err := l.t.Send(src, dst, seq)
		switch {
		case err == nil:
			l.pending[seq] = probe{src: src, dst: dst, pair: pair, at: now, sentNs: sentNs}
		case errors.Is(err, ErrUnavailable):
			// Nothing was measured.
		default:
			l.record(pair, outcome{at: now, connFail: errors.Is(err, ErrNoRoute)})
		}
	}
}

// complete credits replies to the probes that are still pending. A reply for
// a probe that already timed out, or with a tag that does not match the
// sender, is a stale arrival and counts for nothing.
func (l *Loop) complete(reps []Reply) {
	for _, r := range reps {
		p, ok := l.pending[r.Seq]
		if !ok || p.src != r.Src {
			continue
		}
		delete(l.pending, r.Seq)
		rtt := uint64(0)
		if r.AtNs > p.sentNs {
			rtt = r.AtNs - p.sentNs
		}
		l.record(p.pair, outcome{at: p.at, ok: time.Duration(rtt) < l.cfg.Timeout, rttNs: rtt})
	}
}

// expire resolves every probe whose timeout has passed as a timeout.
func (l *Loop) expire(now time.Time) {
	for seq, p := range l.pending {
		if now.Sub(p.at) >= l.cfg.Timeout {
			delete(l.pending, seq)
			l.record(p.pair, outcome{at: p.at})
		}
	}
}

func (l *Loop) nextDeadline() (time.Time, bool) {
	var d time.Time
	found := false
	for _, p := range l.pending {
		t := p.at.Add(l.cfg.Timeout)
		if !found || t.Before(d) {
			d, found = t, true
		}
	}
	return d, found
}

func (l *Loop) record(pair int, o outcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.win[pair].add(o, l.cfg.Window)
}

// Snapshot returns one measurement per pair probed within the window, in
// topology order. ready is false until the loop has run for a whole window:
// a partly filled window rests on a handful of probes per pair, where one
// lost probe reads as a total outage.
func (l *Loop) Snapshot() (ms []model.MutualMeasurement, ready bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started.IsZero() || now.Sub(l.started) < l.cfg.Window {
		return nil, false
	}
	cutoff := now.Add(-l.cfg.Window)
	for i := range l.win {
		l.win[i].prune(cutoff)
		if m, ok := l.win[i].measurement(l.cfg.LossIsDrop); ok {
			ms = append(ms, m)
		}
	}
	return ms, true
}

// Alarm reports whether a full window shows a pair, with at least MinProbes
// probes, whose timeout ratio exceeds TimeoutTh. The monitor uses it to start
// an engine run between its own triggers, so the diagnosis follows the probe
// rather than waiting for the next idle or PFC run.
func (l *Loop) Alarm() bool {
	ms, ready := l.Snapshot()
	if !ready {
		return false
	}
	for _, m := range ms {
		if !m.Connected {
			return true
		}
		if int(m.Sent) < l.cfg.MinProbes {
			continue
		}
		if r, ok := m.TimeoutRatio(); ok && r > l.cfg.TimeoutTh {
			return true
		}
	}
	return false
}

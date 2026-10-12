package mutual

import (
	"slices"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

// outcome is one resolved probe of a pair.
type outcome struct {
	at time.Time // when the probe was sent
	ok bool      // a reply came back within the probe timeout
	// connFail marks a probe that could not be sent because there is no route
	// to the peer (no address handle): a connectivity failure, not a setup one.
	connFail bool
	rttNs    uint64
}

// pairWindow is the sliding window of one unordered RNIC pair: every probe
// either RNIC sent to the other, in send order, no older than the window.
type pairWindow struct {
	src, dst string // graph order, as in topology.Graph.MutualPairs
	// crossRail marks a pair whose RNICs sit on different rails, exactly as
	// the engine marks a burst (topology.CrossRail), so the analyzer's
	// cross-rail rules read the window the same way.
	crossRail bool
	obs       []outcome
}

// add records o and forgets outcomes that fell out of the window. Probes
// resolve out of order (a reply beats an earlier probe's timeout), so o is
// inserted by send time.
func (w *pairWindow) add(o outcome, window time.Duration) {
	i := len(w.obs)
	for i > 0 && w.obs[i-1].at.After(o.at) {
		i--
	}
	w.obs = slices.Insert(w.obs, i, o)
	w.prune(w.obs[len(w.obs)-1].at.Add(-window))
}

// prune drops outcomes sent before cutoff.
func (w *pairWindow) prune(cutoff time.Time) {
	i := 0
	for i < len(w.obs) && w.obs[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		w.obs = slices.Delete(w.obs, 0, i)
	}
}

// measurement aggregates the window into the row the analyzer reads: Sent is
// every resolved probe, Recv the ones answered in time, so TimeoutRatio is
// 1 − Recv/Sent over the window (ToN'24 §IV-A-2). ok is false when the
// window holds no probe of this pair.
func (w *pairWindow) measurement(lossIsDrop bool) (model.MutualMeasurement, bool) {
	if len(w.obs) == 0 {
		return model.MutualMeasurement{}, false
	}
	m := model.MutualMeasurement{Src: w.src, Dst: w.dst, Sent: uint32(len(w.obs)), CrossRail: w.crossRail}
	var rtts []uint64
	conn := 0
	for _, o := range w.obs {
		if o.connFail {
			conn++
		}
		if o.ok {
			m.Recv++
			rtts = append(rtts, o.rttNs)
		}
	}
	// A UD probe has no connection to lose; the pair "never connected" only
	// when no probe could even be addressed to the peer.
	m.Connected = conn < len(w.obs)
	if !m.Connected {
		m.LossRatio = 1
	}
	if len(rtts) > 0 {
		slices.Sort(rtts)
		m.RTTnsP50 = pct(rtts, 50)
		m.RTTnsP99 = pct(rtts, 99)
	}
	// The live probe's losses are timeouts, and drops are read from the RNIC
	// counters (Reliable). The simulator's losses are its injected drops.
	m.Reliable = !lossIsDrop
	if lossIsDrop && m.Connected {
		m.LossRatio = 1 - float64(m.Recv)/float64(m.Sent)
	}
	return m, true
}

// pct matches the Zig probe's percentile: the nearest-rank sample at or below
// pct of the sorted samples.
func pct(sorted []uint64, p int) uint64 {
	return sorted[(len(sorted)-1)*p/100]
}

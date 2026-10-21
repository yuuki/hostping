package monitor

import (
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// dropDeltas is each drop counter's delta from prev to cur, counters that did
// not move left out. The caller differences only samples that read the same
// counters (sameCounters).
func dropDeltas(prev, cur map[string]uint64) map[string]uint64 {
	var out map[string]uint64
	for name, v := range cur {
		if d := dropCounterDelta(v, prev[name]); d > 0 {
			if out == nil {
				out = map[string]uint64{}
			}
			out[name] = d
		}
	}
	return out
}

// dropCounterDelta is counterDelta for one drop counter, which is also reset
// (driver reload, a counter cleared by hand) rather than only wrapped. A
// decrease is a wrap only from the upper half of the 32- or 64-bit range;
// anything else is a reset, which yields no delta and becomes the new
// baseline. Read as a wrap, a reset from 100 to 0 was a delta of 2^32.
func dropCounterDelta(cur, prev uint64) uint64 {
	switch {
	case cur >= prev:
		return cur - prev
	case prev >= 1<<31 && prev <= 0xffffffff && cur <= 0xffffffff,
		prev >= 1<<63:
		return counterDelta(cur, prev)
	}
	return 0
}

// sameCounters reports whether two samples read the same drop counters. One
// that appears or vanishes while the sources stay the same cannot be
// differenced: the poll's drops are unread, as on a source change, instead of
// reading as no drops.
func sameCounters(prev, cur map[string]uint64) bool {
	if len(prev) != len(cur) {
		return false
	}
	for name := range cur {
		if _, ok := prev[name]; !ok {
			return false
		}
	}
	return true
}

// dropAggregates maps a port-wide drop counter to the prefix and suffix of the
// per-priority counters it sums: the ConnectX rx_discards_phy is the sum of
// rx_prioN_discards. Where those counters exist, the aggregate only counts
// for what they do not account for (settleResidual), so a discard is neither
// counted twice nor, on a lossy priority, counted against the RNIC through
// the aggregate.
var dropAggregates = map[string]struct{ prefix, suffix string }{
	"rx_discards_phy": {"rx_prio", "_discards"},
}

// splitDrops files one RNIC's per-counter deltas as fault or lossy-class
// drops. cur is the sample's counters, residual the RNIC's aggregate state
// across polls (settleResidual). A per-priority counter (rx_prio0_buf_discard)
// is lossy when lossless says PFC does not protect its priority; every other
// counter, and the part of an aggregate its per-priority counters leave
// unexplained, is a fault.
func splitDrops(cur, deltas map[string]uint64, lossless func(int) bool, residual map[string]int64) model.RNICDrops {
	var d model.RNICDrops
	add := func(m *map[string]uint64, name string, v uint64) {
		if *m == nil {
			*m = map[string]uint64{}
		}
		(*m)[name] = v
	}
	for name, v := range deltas {
		if _, ok := dropAggregates[name]; ok {
			continue
		}
		if p, ok := counterPriority(name); ok && !lossless(p) {
			add(&d.Lossy, name, v)
			continue
		}
		add(&d.Fault, name, v)
	}
	for name, agg := range dropAggregates {
		if _, ok := cur[name]; !ok {
			continue
		}
		v := deltas[name]
		var parts uint64
		hasParts := false
		for n := range cur {
			if mid, ok := strings.CutPrefix(n, agg.prefix); ok {
				if mid, ok = strings.CutSuffix(mid, agg.suffix); ok && isDigits(mid) {
					hasParts = true
					parts += deltas[n]
				}
			}
		}
		if hasParts {
			v = settleResidual(residual, name, signedDelta(v)-signedDelta(parts))
		}
		if v > 0 {
			add(&d.Fault, name, v)
		}
	}
	return d
}

// settleResidual is the part of an aggregate's delta its per-priority
// counters do not account for, counted only once it has held for two polls.
// The driver reads the aggregate and the per-priority counters apart, so one
// sample can catch a discard in one and not yet in the other: a residual of
// +k followed by -k (or -k then +k) is that skew, not an unexplained discard.
// residual[name] carries the running balance: what stands in both this poll
// and the last is counted and taken off it, and a negative balance is kept
// only from the latest poll, so it offsets the next one and no more.
func settleResidual(residual map[string]int64, name string, s int64) uint64 {
	r := residual[name]
	next := r + s
	if floor := min(s, 0); next < floor {
		next = floor
	}
	var out uint64
	if settled := min(r, next); settled > 0 {
		out = uint64(settled)
		next -= settled
	}
	residual[name] = next
	return out
}

// signedDelta is a delta as an int64, capped so a difference of two cannot
// overflow.
func signedDelta(v uint64) int64 {
	return int64(min(v, 1<<62))
}

// counterPriority is the 802.1p priority a per-priority counter names, the N
// of "prioN_" (rx_prio3_discards, rx_prio0_buf_discard), if it names one.
func counterPriority(name string) (int, bool) {
	i := strings.Index(name, "prio")
	if i < 0 || (i > 0 && name[i-1] != '_') {
		return 0, false
	}
	rest := name[i+len("prio"):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 || (j < len(rest) && rest[j] != '_') {
		return 0, false
	}
	p, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	return p, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

package monitor

import (
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// dropDeltas is each drop counter's delta from prev to cur, counters that did
// not move left out. A counter missing from either sample yields none: one
// that appears is baselined, one that vanishes says nothing.
func dropDeltas(prev, cur map[string]uint64) map[string]uint64 {
	var out map[string]uint64
	for name, v := range cur {
		p, ok := prev[name]
		if !ok {
			continue
		}
		if d := counterDelta(v, p); d > 0 {
			if out == nil {
				out = map[string]uint64{}
			}
			out[name] = d
		}
	}
	return out
}

// dropAggregates maps a port-wide drop counter to the prefix and suffix of the
// per-priority counters it sums: the ConnectX rx_discards_phy is the sum of
// rx_prioN_discards. Its delta is only kept for what they do not account for,
// so a discard is neither counted twice nor, on a lossy priority, counted
// against the RNIC through the aggregate.
var dropAggregates = map[string]struct{ prefix, suffix string }{
	"rx_discards_phy": {"rx_prio", "_discards"},
}

// splitDrops files one RNIC's per-counter deltas as fault or lossy-class
// drops. A per-priority counter (rx_prio0_buf_discard) is lossy when
// lossless says PFC does not protect its priority; every other counter, and
// the part of an aggregate its per-priority counters leave unexplained, is a
// fault.
func splitDrops(deltas map[string]uint64, lossless func(int) bool) model.RNICDrops {
	var d model.RNICDrops
	add := func(m *map[string]uint64, name string, v uint64) {
		if *m == nil {
			*m = map[string]uint64{}
		}
		(*m)[name] = v
	}
	for name, v := range deltas {
		if agg, ok := dropAggregates[name]; ok {
			var parts uint64
			for n, pv := range deltas {
				mid, ok := strings.CutPrefix(n, agg.prefix)
				if !ok {
					continue
				}
				if mid, ok = strings.CutSuffix(mid, agg.suffix); ok && isDigits(mid) {
					parts += pv
				}
			}
			if v <= parts {
				continue
			}
			v -= parts
		}
		if p, ok := counterPriority(name); ok && !lossless(p) {
			add(&d.Lossy, name, v)
			continue
		}
		add(&d.Fault, name, v)
	}
	return d
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

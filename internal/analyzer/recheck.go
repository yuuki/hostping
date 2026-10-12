package analyzer

import (
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// A sibling verdict compares paths that were not measured at the same time.
// The engine probes RNIC by RNIC, so a host whose memory latency drifts
// between states over seconds can put the whole batch of one RNIC in a slower
// state, and its every path then reads above the siblings measured before or
// after it; a single path can also land in the tail of its own jitter. Both
// read as a slow path. Before such a verdict is reported, the engine re-reads
// the path in turn with siblings of its class (path, sibling, path, sibling,
// ...), so the two sides share a time window, and keeps the verdict only if
// the path still reads high against them. The plan and the decision live here,
// next to ClassifyLatency, so the engine only probes.

// LatencyRecheckReads is how many times a rechecked path is re-read, each
// read followed by one sibling read.
const LatencyRecheckReads = 3

// LatencyRecheckMaxPaths caps the paths rechecked in one run, so a run spends
// at most 2 × LatencyRecheckReads × LatencyRecheckMaxPaths extra probes on it.
// It is sized to cover every path of one RNIC, the batch a slow time window
// hits. Candidates past the cap keep their sweep verdict but are marked
// LatencyRecheckSkipCap: in that run they neither extend their latency streak
// nor become a cause (latencyStreaks, latencyReportable), and
// PlanLatencyRecheck rotates its selection so a later run rechecks them.
const LatencyRecheckMaxPaths = 16

// latencyRecheckMaxShare is the share of a class's judged paths that may be
// flagged for the class to be rechecked. Past it the rise is class-wide rather
// than a path's: the paths left to read against are too few to stand for the
// class, and the grouping in diagnoseLatency judges a class-wide rise. One
// slow RNIC among two is exactly half of a class and still rechecked.
const latencyRecheckMaxShare = 0.5

// PathRef names one RNIC→endpoint path.
type PathRef struct {
	RNIC, Endpoint string
}

func (p PathRef) String() string { return p.RNIC + "->" + p.Endpoint }

// LatencyRecheckPlan is one path to recheck and the siblings to read
// interleaved with it: Siblings[i] is read right after the path's i-th re-read.
type LatencyRecheckPlan struct {
	Path     PathRef
	Siblings []PathRef
}

// LatencyRecheckSelection is what PlanLatencyRecheck chose: the paths to
// recheck, and the candidates left out because of LatencyRecheckMaxPaths.
type LatencyRecheckSelection struct {
	Plans []LatencyRecheckPlan
	// Capped are the candidates past the cap, in measurement order. The
	// engine hands them to Analyze (Input.LatencyRecheckSkipped), so their
	// unconfirmed sweep verdicts neither extend a streak nor become a cause.
	Capped []PathRef
}

// PlanLatencyRecheck returns the paths whose sweep verdict, from
// ClassifyLatency on the same arguments, is abnormal against the sibling median
// (LatencyBySiblings), and for each LatencyRecheckReads siblings of its class.
// A verdict against a user baseline is not rechecked: the operator's reference
// does not move with the time a path was measured in.
//
// A class in which more than latencyRecheckMaxShare of the judged paths are
// flagged is not rechecked at all. Busy runs judge no latency and plan
// nothing.
//
// A candidate whose rise the other RNICs on its NUMA node share — one the
// locality shift clears on the sweep verdicts (localityShifts) — is not
// planned: Analyze clears it after the recheck anyway, so rechecking it would
// only spend the cap. Pre-applying the rule here cannot disagree with
// Analyze, which runs the same rule after applyLatencyRecheck: a path left out
// here is not rechecked, so it is still flagged when the rule runs again; its
// locality siblings' sweep readings are the same; and the recheck only clears
// verdicts, which can only make fewer endpoints read slow from the other
// nodes (endpointSlowElsewhere). Every path left out here is cleared there
// too. The class share and the siblings are still taken from the sweep
// verdicts: a path the locality rule would clear still reads high, and is no
// sibling to read against.
//
// When more candidates remain than LatencyRecheckMaxPaths, the selection is
// fair over RNICs and over runs. The candidates are ordered round-robin
// across RNICs — each RNIC's first candidate, in measurement order, then each
// one's second, and so on — and LatencyRecheckMaxPaths consecutive ones of
// that order are taken, starting at offset × LatencyRecheckMaxPaths and
// wrapping around. One run so spreads the cap over every RNIC with
// candidates, and a caller whose offset advances by one per run walks the
// window over every candidate in ⌈candidates / LatencyRecheckMaxPaths⌉ runs
// while the candidates stay the same, instead of starving the RNICs probed
// last. The rest are returned as Capped. A one-shot run passes 0. Plans and
// Capped are in measurement order.
//
// Siblings are the class's paths that were judged normal, those on other
// RNICs first — one per RNIC in turn, so the reads spread over RNICs — then
// those on the path's own RNIC. A class with fewer such siblings than
// LatencyRecheckReads reads them again in the same order.
func PlanLatencyRecheck(g topology.Graph, ms []model.PathMeasurement, mode model.RunMode, cfg config.Config, bases baseline.Overlay, offset int) LatencyRecheckSelection {
	if mode == model.ModeBusy {
		return LatencyRecheckSelection{}
	}
	judged := ClassifyLatency(g, ms, mode, cfg, bases)
	shifted := localityShifts(g, judged, bases, cfg.AbnormalTh)
	byClass := map[latClass][]int{}
	classOf := map[int]latClass{}
	for i, m := range judged {
		if m.LatencyStatus == model.LatencyUnknown || m.LatencyReason != model.LatencyBySiblings {
			continue
		}
		c, _, _ := latencyClassOf(g, m, bases)
		byClass[c] = append(byClass[c], i)
		classOf[i] = c
	}
	// normalOf is, per class that may be rechecked, its paths judged normal.
	normalOf := map[latClass][]int{}
	for c, idx := range byClass {
		var flagged, normal []int
		for _, i := range idx {
			if judged[i].LatencyStatus == model.LatencyAbnormal {
				flagged = append(flagged, i)
			} else {
				normal = append(normal, i)
			}
		}
		if len(flagged) == 0 || len(normal) == 0 || float64(len(flagged)) > latencyRecheckMaxShare*float64(len(idx)) {
			continue
		}
		normalOf[c] = normal
	}
	var cands []int
	for i, m := range judged {
		if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings {
			continue
		}
		if _, ok := normalOf[classOf[i]]; !ok {
			continue
		}
		if _, ok := shifted[i]; ok {
			continue
		}
		cands = append(cands, i)
	}
	chosen := recheckWindow(judged, cands, offset)
	var sel LatencyRecheckSelection
	for _, i := range cands {
		m := judged[i]
		ref := PathRef{m.RNIC, m.Endpoint}
		if !chosen[i] {
			sel.Capped = append(sel.Capped, ref)
			continue
		}
		sibs := recheckSiblings(judged, normalOf[classOf[i]], m.RNIC)
		plan := LatencyRecheckPlan{Path: ref}
		for k := range LatencyRecheckReads {
			plan.Siblings = append(plan.Siblings, sibs[k%len(sibs)])
		}
		sel.Plans = append(sel.Plans, plan)
	}
	return sel
}

// recheckWindow returns which candidates (indices into ms) PlanLatencyRecheck
// rechecks: every one when they fit LatencyRecheckMaxPaths, otherwise
// LatencyRecheckMaxPaths consecutive ones of their round-robin order across
// RNICs, starting at offset × LatencyRecheckMaxPaths and wrapping around.
func recheckWindow(ms []model.PathMeasurement, cands []int, offset int) map[int]bool {
	chosen := map[int]bool{}
	if len(cands) <= LatencyRecheckMaxPaths {
		for _, i := range cands {
			chosen[i] = true
		}
		return chosen
	}
	var rnics []string
	byRNIC := map[string][]int{}
	for _, i := range cands {
		r := ms[i].RNIC
		if _, ok := byRNIC[r]; !ok {
			rnics = append(rnics, r)
		}
		byRNIC[r] = append(byRNIC[r], i)
	}
	order := make([]int, 0, len(cands))
	for k := 0; len(order) < len(cands); k++ {
		for _, r := range rnics {
			if k < len(byRNIC[r]) {
				order = append(order, byRNIC[r][k])
			}
		}
	}
	n := len(order)
	start := ((offset%n + n) % n) * LatencyRecheckMaxPaths % n
	for k := range LatencyRecheckMaxPaths {
		chosen[order[(start+k)%n]] = true
	}
	return chosen
}

// recheckSiblings orders the normal siblings of a path on rnic: other RNICs
// first, taking one path per RNIC in turn, then rnic's own paths.
func recheckSiblings(ms []model.PathMeasurement, normal []int, rnic string) []PathRef {
	var rnics []string
	byRNIC := map[string][]PathRef{}
	var own []PathRef
	for _, i := range normal {
		m := ms[i]
		ref := PathRef{m.RNIC, m.Endpoint}
		if m.RNIC == rnic {
			own = append(own, ref)
			continue
		}
		if _, ok := byRNIC[m.RNIC]; !ok {
			rnics = append(rnics, m.RNIC)
		}
		byRNIC[m.RNIC] = append(byRNIC[m.RNIC], ref)
	}
	var out []PathRef
	for k := 0; ; k++ {
		added := false
		for _, n := range rnics {
			if k < len(byRNIC[n]) {
				out = append(out, byRNIC[n][k])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return append(out, own...)
}

// JudgeLatencyRecheck decides a recheck from the path's re-read small-message
// p50s and the interleaved sibling re-reads: the verdict holds when the median
// of the path's reads exceeds the median of the siblings' by more than th and
// by at least latencyFloorNs, exactly as the sweep judged it against the
// sibling median. Siblings is left for the caller to fill.
func JudgeLatencyRecheck(path, siblings []uint64, th float64) model.LatencyRecheck {
	p, s := medianU64(path), medianU64(siblings)
	return model.LatencyRecheck{PathNsP50: p, SiblingNsP50: s, Confirmed: siblingLatencyHigh(p, s, th)}
}

// applyLatencyRecheck attaches each recheck, keyed by "rnic->endpoint", to its
// path when the path's verdict is the sibling one it was planned for. A path
// the recheck did not confirm becomes normal with LatencyRecheckCleared; a
// confirmed one stays abnormal against siblings. Nothing else changes.
func applyLatencyRecheck(paths []model.PathMeasurement, rc map[string]model.LatencyRecheck) []model.PathMeasurement {
	if len(rc) == 0 {
		return paths
	}
	for i := range paths {
		m := &paths[i]
		if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings {
			continue
		}
		r, ok := rc[m.RNIC+"->"+m.Endpoint]
		if !ok {
			continue
		}
		m.LatencyRecheck = &r
		if !r.Confirmed {
			m.LatencyStatus, m.LatencyReason = model.LatencyNormal, model.LatencyRecheckCleared
		}
	}
	return paths
}

// markLatencyRecheckSkipped sets LatencyRecheckSkipped on each path named in
// skipped (keyed by "rnic->endpoint") whose verdict, after the recheck and the
// locality shift, is still abnormal against its siblings with no recheck
// behind it. A path the locality shift cleared needs no mark: it is normal.
func markLatencyRecheckSkipped(paths []model.PathMeasurement, skipped map[string]model.LatencyRecheckSkip) []model.PathMeasurement {
	if len(skipped) == 0 {
		return paths
	}
	for i := range paths {
		m := &paths[i]
		if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings || m.LatencyRecheck != nil {
			continue
		}
		if why, ok := skipped[latencyKey(*m)]; ok {
			m.LatencyRecheckSkipped = why
		}
	}
	return paths
}

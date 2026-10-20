package analyzer

import (
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// An idle verdict holds a path's reading to a baseline measured at another
// time. The engine reads every path of an RNIC in one batch and the RNICs one
// after another, so on a host whose memory bandwidth drifts between states
// over seconds to minutes, a slow state over the batches of a few RNICs drops
// their paths of one distance together below the baseline, and Algorithm 1
// turns that into failed root ports and RNIC PCIe links. Before such a verdict
// is reported, the engine re-reads one path of each flagged group (one RNIC's
// paths of one distance class) in rounds with reference paths of the same
// class on other RNICs that met their baseline in the sweep, so both sides
// share a time window, and decides the group on those re-reads. The plan and
// the decision live here, next to ClassifyPaths, so the engine only probes.

// BWRecheckRounds is how many rounds a group is re-read in; each round reads
// the group's representative path once and then each reference once.
const BWRecheckRounds = 3

// BWRecheckMaxRefs caps the reference paths read in each round.
const BWRecheckMaxRefs = 2

// BWRecheckMaxGroups caps the groups rechecked in one run. A slow window over
// one RNIC's batch flags its local and its remote memory paths, two groups (a
// learned sub-class adds one), so the cap covers such a window over a stretch
// of several consecutive RNICs. A group costs BWRecheckRounds ×
// (1 + BWRecheckMaxRefs) probes at most, so a run spends at most
// BWRecheckRounds × (1 + BWRecheckMaxRefs) × BWRecheckMaxGroups extra probes
// on it, and only when the sweep flagged a path. Groups past the cap keep
// their sweep verdict.
const BWRecheckMaxGroups = 12

// BWRecheckPlan is one group to recheck: its representative path, the
// references read after it in every round, and the baseline the group is
// judged against.
type BWRecheckPlan struct {
	// Group is the key analyzer.Input.BWRecheck takes the result under.
	Group       string
	Path        PathRef
	Refs        []PathRef
	BaselineBPS uint64
}

// bwClass is the distance class a reference must share with a group: the
// distance, the learned sub-class, and the baseline value itself, so a path
// on an RNIC that derives another Fig.4 table is never read as a reference.
type bwClass struct {
	dist  model.Distance
	class string
	base  uint64
}

// bwGroupKey names the group of RNIC rnic's paths of distance d and learned
// sub-class class ("" for the distance itself).
func bwGroupKey(rnic string, d model.Distance, class string) string {
	if class == "" {
		return rnic + "/" + d.String()
	}
	return rnic + "/" + class
}

// idleJudged reports whether m's RNIC is judged against the idle baseline in
// this run: every RNIC of an idle run, and the unloaded ones of a busy run.
func idleJudged(m model.PathMeasurement, mode model.RunMode, busy map[string]bool) bool {
	return mode != model.ModeBusy || !busy[m.RNIC]
}

// PlanBWRecheck returns the groups to recheck: the paths ClassifyPaths, on the
// same arguments, judges abnormal against the idle baseline, grouped per RNIC
// and distance class (learned sub-class included), in measurement order of
// their first path. Paths of a loaded RNIC in a busy run are judged against
// their siblings instead (PlanBusyRecheck) and are never planned here.
//
// Each group's representative is its lowest sweep reading, the path most
// likely to hold a fault of its own. Its references are paths of the same
// class and baseline on other RNICs judged against the idle baseline that met
// it in the sweep (normal, or uncertain only because a baseline alone does not
// prove health), at most BWRecheckMaxRefs of them, each on a different RNIC
// where possible: those to the representative's endpoint first, then the
// others, in measurement order. A group with no reference is not planned: a
// class that dropped on every RNIC has nothing to read against.
//
// At most BWRecheckMaxGroups groups are planned.
func PlanBWRecheck(g topology.Graph, ms []model.PathMeasurement, mode model.RunMode, busy map[string]bool, cfg config.Config, bases baseline.Overlay) []BWRecheckPlan {
	judged := ClassifyPaths(g, ms, mode, busy, cfg, bases)
	var order []string
	members := map[string][]int{}
	classOf := map[string]bwClass{}
	refs := map[bwClass][]int{}
	for i, m := range judged {
		if !idleJudged(m, mode, busy) {
			continue
		}
		p, ok := g.Path(m.RNIC, m.Endpoint)
		if !ok {
			continue
		}
		base, class := pathBaseline(g, bases, p)
		c := bwClass{dist: p.Distance, class: class, base: base.BandwidthBPS}
		if m.Status != model.PathAbnormal {
			refs[c] = append(refs[c], i)
			continue
		}
		k := bwGroupKey(m.RNIC, p.Distance, class)
		if _, ok := members[k]; !ok {
			order = append(order, k)
			classOf[k] = c
		}
		members[k] = append(members[k], i)
	}
	var out []BWRecheckPlan
	for _, k := range order {
		if len(out) == BWRecheckMaxGroups {
			break
		}
		rep := members[k][0]
		for _, i := range members[k][1:] {
			if judged[i].BandwidthBPS < judged[rep].BandwidthBPS {
				rep = i
			}
		}
		m := judged[rep]
		c := classOf[k]
		rs := bwRecheckRefs(judged, refs[c], m.RNIC, m.Endpoint)
		if len(rs) == 0 {
			continue
		}
		out = append(out, BWRecheckPlan{Group: k, Path: PathRef{m.RNIC, m.Endpoint}, Refs: rs, BaselineBPS: c.base})
	}
	return out
}

// bwRecheckRefs picks up to BWRecheckMaxRefs references among the candidate
// measurements cand for a path rnic->ep: never on rnic itself, and one per
// RNIC while other RNICs remain, those to ep first.
func bwRecheckRefs(ms []model.PathMeasurement, cand []int, rnic, ep string) []PathRef {
	var out []PathRef
	used := map[int]bool{}
	usedRNIC := map[string]bool{}
	pick := func(ok func(model.PathMeasurement) bool) {
		for _, i := range cand {
			m := ms[i]
			if len(out) == BWRecheckMaxRefs || used[i] || m.RNIC == rnic || !ok(m) {
				continue
			}
			used[i] = true
			usedRNIC[m.RNIC] = true
			out = append(out, PathRef{m.RNIC, m.Endpoint})
		}
	}
	pick(func(m model.PathMeasurement) bool { return m.Endpoint == ep && !usedRNIC[m.RNIC] })
	pick(func(m model.PathMeasurement) bool { return !usedRNIC[m.RNIC] })
	pick(func(model.PathMeasurement) bool { return true })
	return out
}

// JudgeBWRecheck decides a group from its representative's re-read
// bandwidths, the reference re-reads of the same rounds and the group's
// baseline:
//
//   - The representative is still more than th below the references' median:
//     the drop is its own, and the group stays abnormal (confirmed).
//   - Otherwise it reads with its references, which is no evidence of a fault
//     of its own. If it also meets the baseline, the group is cleared.
//   - If not, the group is host_shift: the representative misses the
//     baseline while reading level with paths of its class on other RNICs, so
//     the host as a whole reads lower than when the baseline was measured.
//
// Only the paired comparison can confirm a verdict. Whether the references
// themselves clear the baseline does not: in a slow state they sit near the
// representative, and a reference median just above the baseline threshold
// with a representative just below it is the same state on both sides of the
// line, not a fault of the representative's own.
//
// As in JudgeBusyRecheck, the sweep's readings are left out on both sides:
// they flagged the group and were not taken next to each other. Path, Refs and
// Rounds are left for the caller.
func JudgeBWRecheck(path, refs []uint64, base uint64, th float64) model.BandwidthRecheck {
	p, r := medianU64(path), medianU64(refs)
	rc := model.BandwidthRecheck{PathBPS: p, RefBPS: r, BaselineBPS: base}
	switch {
	case degraded(p, r, th):
		rc.Outcome = model.BWRecheckConfirmed
	case !degraded(p, base, th):
		rc.Outcome = model.BWRecheckCleared
	default:
		rc.Outcome = model.BWRecheckHostShift
	}
	return rc
}

// applyBWRecheck attaches each group's recheck, keyed as BWRecheckPlan.Group,
// to every path of the group that is abnormal against the idle baseline, as
// planned, and applies its outcome to all of them: a confirmed group stays
// abnormal, a cleared one is judged as a path that met its baseline at the
// representative's re-read median (normal, or uncertain where a baseline
// alone does not prove health), and a host shift is uncertain with
// host_shift. Nothing else changes.
func applyBWRecheck(g topology.Graph, paths []model.PathMeasurement, mode model.RunMode, busy map[string]bool, bases baseline.Overlay, th float64, rc map[string]model.BandwidthRecheck) []model.PathMeasurement {
	if len(rc) == 0 {
		return paths
	}
	for i := range paths {
		m := &paths[i]
		if m.Status != model.PathAbnormal || !idleJudged(*m, mode, busy) {
			continue
		}
		p, ok := g.Path(m.RNIC, m.Endpoint)
		if !ok {
			continue
		}
		_, class := pathBaseline(g, bases, p)
		r, ok := rc[bwGroupKey(m.RNIC, p.Distance, class)]
		if !ok {
			continue
		}
		m.BWRecheck = &r
		switch r.Outcome {
		case model.BWRecheckCleared:
			m.Status, m.UncertainReason = metBaseline(g, mustRNIC(g, m.RNIC), p, r.PathBPS, th)
		case model.BWRecheckHostShift:
			markUncertain(m, model.UncertainHostShift)
		}
	}
	return paths
}

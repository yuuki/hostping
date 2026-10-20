package analyzer

import (
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// A busy verdict holds a loaded RNIC's path to the median of its siblings
// (busyRef), each side the median of --busy-samples readings. Under heavy
// service traffic a single reading scatters by several percent with a heavy
// low tail, and the classes are small, so a healthy path's median still lands
// --abnormal-th below its siblings now and then: a busy run reports a
// different path abnormal each time. Before such a verdict is reported, the
// engine re-reads the path in rounds with the siblings of its class (path,
// sibling A, sibling B, ..., repeated) on the same loaded RNIC, and keeps the
// verdict only if the path still reads low against them. The plan and the
// decision live here, next to ClassifyPaths, so the engine only probes.

// BusyRecheckRounds is how many rounds a rechecked path is re-read in; each
// round reads the path once and then each of its siblings once.
const BusyRecheckRounds = 3

// BusyRecheckMaxSiblings caps the siblings read in each round. The first ones
// of the class, in measurement order, are read.
const BusyRecheckMaxSiblings = 4

// BusyRecheckMaxPaths caps the paths rechecked in one busy run. It covers a
// whole class at BusyRecheckMaxSiblings + 1 paths with room to spare, so a
// small class flagged in full is rechecked in full. Every probe lands on an
// RNIC that is carrying service traffic, so a run spends at most
// BusyRecheckRounds × (1 + BusyRecheckMaxSiblings) × BusyRecheckMaxPaths extra
// probes on it, and one flagged path in a class of three costs
// BusyRecheckRounds × 3. Candidates past the cap keep their sweep verdict.
const BusyRecheckMaxPaths = 8

// BusyRecheckPlan is one path to recheck and the siblings read after it in
// every round.
type BusyRecheckPlan struct {
	Path     PathRef
	Siblings []PathRef
}

// PlanBusyRecheck returns the paths ClassifyPaths, on the same arguments,
// judges abnormal against their busy siblings (a loaded RNIC in a busy run),
// each with up to BusyRecheckMaxSiblings siblings: the other paths of its
// busyRefKey class, in measurement order, flagged or not.
//
// A flagged sibling is kept on purpose. In a small class one sibling that
// happens to read high in the sweep flags the other two at once, since each is
// judged against the median of that high reading and the other low one; each
// of the two is then the other's healthy reference. Nor is a class skipped
// when most of it is flagged: a genuine class-wide drop is still confirmed as
// long as a healthy sibling reads high in the same rounds, and a path whose
// every sibling reads as low as it does next to it is not degraded against
// them, which is the answer on a loaded RNIC. Only a class with no other path
// has nothing to read against.
//
// At most BusyRecheckMaxPaths paths are planned, in measurement order. Idle
// runs, and RNICs judged against the idle baseline in a busy run, plan
// nothing.
func PlanBusyRecheck(g topology.Graph, ms []model.PathMeasurement, mode model.RunMode, busy map[string]bool, cfg config.Config, bases baseline.Overlay) []BusyRecheckPlan {
	if mode != model.ModeBusy {
		return nil
	}
	r := newRun(g, mode, busy, cfg, bases)
	classes := r.pathClasses(ms)
	judged := r.classifyPaths(ms, classes)
	byClass := map[busyRefKey][]PathRef{}
	classOf := map[int]busyRefKey{}
	for i, m := range judged {
		if !busy[m.RNIC] {
			continue
		}
		k, ok := busyClassOf(classes, m)
		if !ok {
			continue
		}
		byClass[k] = append(byClass[k], PathRef{m.RNIC, m.Endpoint})
		classOf[i] = k
	}
	var out []BusyRecheckPlan
	for i, m := range judged {
		if m.Status != model.PathAbnormal {
			continue
		}
		k, ok := classOf[i]
		if !ok {
			continue
		}
		self := PathRef{m.RNIC, m.Endpoint}
		var sibs []PathRef
		for _, r := range byClass[k] {
			if r != self && len(sibs) < BusyRecheckMaxSiblings {
				sibs = append(sibs, r)
			}
		}
		if len(sibs) == 0 {
			continue
		}
		if len(out) == BusyRecheckMaxPaths {
			break
		}
		out = append(out, BusyRecheckPlan{Path: self, Siblings: sibs})
	}
	return out
}

// JudgeBusyRecheck decides a recheck from the path's re-read bandwidths and
// the sibling re-reads of the same rounds: the verdict holds when the median
// of the path's re-reads is still more than th below the median of every
// sibling re-read, the rule the sweep applied against busyRef.
//
// The sweep's own readings of the path are left out on purpose. They are the
// readings that put the path on the plan, picked for reading low, so pooling
// them with the re-reads would pull the path's median toward the very tail the
// recheck is there to look past; and they were not taken next to the sibling
// re-reads. Both sides are re-reads of the same rounds, so a time window that
// slows the RNIC slows both. Siblings and Rounds are left for the caller.
func JudgeBusyRecheck(path, siblings []uint64, th float64) model.BusyRecheck {
	p, s := medianU64(path), medianU64(siblings)
	return model.BusyRecheck{PathBPS: p, SiblingBPS: s, Confirmed: degraded(p, s, th)}
}

// applyBusyRecheck attaches each recheck, keyed by "rnic->endpoint", to its
// path when the path is abnormal on a loaded RNIC of a busy run, as it was
// planned. A path the recheck did not confirm becomes uncertain with
// UncertainRNICBusy, as any loaded path that is not degraded against its
// siblings; a confirmed one stays abnormal. Nothing else changes.
func (r run) applyBusyRecheck(paths []model.PathMeasurement, rc map[string]model.BusyRecheck) []model.PathMeasurement {
	if len(rc) == 0 || r.mode != model.ModeBusy {
		return paths
	}
	for i := range paths {
		m := &paths[i]
		if m.Status != model.PathAbnormal || !r.busy[m.RNIC] {
			continue
		}
		b, ok := rc[pathKey(m.RNIC, m.Endpoint)]
		if !ok {
			continue
		}
		m.BusyRecheck = &b
		if !b.Confirmed {
			markUncertain(m, model.UncertainRNICBusy)
		}
	}
	return paths
}

package analyzer

import (
	"sort"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// The paired recheck removes the sibling verdicts that came from when a path
// was measured. A second source survives it: on some platforms the
// small-message latency of every RNIC attached to one NUMA node rises
// together, by about the latency floor, and stays there for minutes — power
// management of the die the RNICs hang off is the likely cause. The recheck
// then confirms the rise, because the siblings it reads alongside sit on other
// nodes. A per-RNIC fault (an extra translation hop, an ACS/ATS detour)
// affects one RNIC, not every RNIC of a node equally, so a rise the node's
// other RNICs share is a platform state rather than a path fault.

// applyLocalityShift clears, after applyLatencyRecheck, each path still
// latency-abnormal against its siblings whose rise the other RNICs on its
// RNIC's NUMA node share. Its locality siblings are the judged paths of its
// latency class (latencyClassOf) from those other RNICs. When the path's sweep
// SmallLatNsP50 is not high against their median (siblingLatencyHigh, the
// sweep's own rule), the path becomes normal with LatencyLocalityShift and
// LatencyLocality records the reference.
//
// The verdict is kept when:
//   - the RNIC's NUMA node is unknown (negative), or no other RNIC on it has a
//     judged path of the class, so a host with one RNIC per node is unchanged;
//   - the endpoint reads slow from most other NUMA nodes too
//     (endpointSlowElsewhere). That is the endpoint's rise (a slow GPU or
//     memory node), which every RNIC sees, and the node's RNICs often reach
//     the endpoint at the same distance — one memory node per NUMA node.
//
// User-baseline verdicts are never touched: the operator's reference already
// is the host's normal level. Every decision reads the sweep readings and the
// verdicts as applyLatencyRecheck left them, so the order of the paths does
// not matter and a path the recheck cleared is not judged again.
// PlanLatencyRecheck runs the same rule (localityShifts) on the sweep
// verdicts, so the paths it would clear spend none of the recheck's cap.
func applyLocalityShift(g topology.Graph, paths []model.PathMeasurement, bases baseline.Overlay, th float64, floorNs uint64) []model.PathMeasurement {
	for i, loc := range localityShifts(g, paths, bases, th, floorNs) {
		m := &paths[i]
		m.LatencyStatus, m.LatencyReason, m.LatencyLocality = model.LatencyNormal, model.LatencyLocalityShift, &loc
	}
	return paths
}

// localityShifts returns, keyed by index into paths, the locality reference
// of each path applyLocalityShift clears, without changing paths.
func localityShifts(g topology.Graph, paths []model.PathMeasurement, bases baseline.Overlay, th float64, floorNs uint64) map[int]model.LatencyLocality {
	numaOf := map[string]int{}
	for _, n := range g.RNICs {
		numaOf[n.ID] = n.NUMA
	}
	known := func(rnic string) (int, bool) {
		n, ok := numaOf[rnic]
		return n, ok && n >= 0
	}
	flagged := func(m model.PathMeasurement) bool {
		return m.LatencyStatus == model.LatencyAbnormal && m.LatencyReason == model.LatencyBySiblings
	}

	type nodeClass struct {
		numa int
		c    latClass
	}
	// judged is, per (NUMA node, class), the indices of the judged paths;
	// reach is, per endpoint, the NUMA nodes with a judged path to it, true
	// when one of those paths is flagged.
	judged := map[nodeClass][]int{}
	classOf := map[int]latClass{}
	reach := map[string]map[int]bool{}
	for i, m := range paths {
		if m.LatencyStatus == model.LatencyUnknown {
			continue
		}
		numa, ok := known(m.RNIC)
		if !ok {
			continue
		}
		c, _, why := latencyClassOf(g, m, bases)
		if why != model.LatencyReasonNone {
			continue
		}
		classOf[i] = c
		judged[nodeClass{numa, c}] = append(judged[nodeClass{numa, c}], i)
		if reach[m.Endpoint] == nil {
			reach[m.Endpoint] = map[int]bool{}
		}
		reach[m.Endpoint][numa] = reach[m.Endpoint][numa] || flagged(m)
	}

	shifts := map[int]model.LatencyLocality{}
	for i, m := range paths {
		if !flagged(m) {
			continue
		}
		numa, ok := known(m.RNIC)
		c, judgedOK := classOf[i]
		if !ok || !judgedOK {
			continue
		}
		if endpointSlowElsewhere(reach[m.Endpoint], numa) {
			continue
		}
		var lats []uint64
		rnics := map[string]bool{}
		for _, j := range judged[nodeClass{numa, c}] {
			if paths[j].RNIC == m.RNIC {
				continue
			}
			lats = append(lats, paths[j].SmallLatNsP50)
			rnics[paths[j].RNIC] = true
		}
		if len(lats) == 0 {
			continue
		}
		ref := medianU64(lats)
		if siblingLatencyHigh(m.SmallLatNsP50, ref, th, floorNs) {
			continue
		}
		loc := model.LatencyLocality{NUMA: numa, RefNs: ref}
		for r := range rnics {
			loc.RNICs = append(loc.RNICs, r)
		}
		sort.Strings(loc.RNICs)
		shifts[i] = loc
	}
	return shifts
}

// endpointSlowElsewhere reports whether an endpoint reads slow from the NUMA
// nodes other than numa: among the other nodes with a judged path to it
// (reach, true when one of a node's paths is flagged against its siblings), a
// strict majority have a flagged path. A slow endpoint is read slow from
// (nearly) every node, while a platform shift can span several nodes at once
// — every node of one package, say — and still leave the rest reading the
// endpoint normally. With no other node there is nothing to judge by.
func endpointSlowElsewhere(reach map[int]bool, numa int) bool {
	other, slow := 0, 0
	for n, f := range reach {
		if n == numa {
			continue
		}
		other++
		if f {
			slow++
		}
	}
	return other > 0 && 2*slow > other
}

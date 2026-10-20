package analyzer

import (
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// ClassifyPaths marks each measurement normal/abnormal/uncertain (NSDI'23 §4.3).
//
// Idle: any path ≥AbnormalTh below its distance baseline (or the baseline of
// the learned sub-class that lists its root-port pair) is abnormal. Affinitive
// paths that still reach what the hardware can carry — the RNIC line rate, or
// the narrowest hop when the bus is the tighter of the two — are normal. Meeting
// a reduced cross-root / cross-socket baseline does not prove the path is clean
// — those stay uncertain (Fig.8 gray). Busy: a loaded RNIC's path is judged
// only against the median of its other siblings at the same communication
// distance and sub-class (busyRef); the rest stay uncertain.
//
// Uncertain is reached several ways, and they are not interchangeable to whoever
// reads the report, so each one records its model.PathUncertainReason.
func ClassifyPaths(g topology.Graph, ms []model.PathMeasurement, mode model.RunMode, busy map[string]bool, cfg config.Config, bases baseline.Overlay) []model.PathMeasurement {
	out := make([]model.PathMeasurement, len(ms))
	copy(out, ms)

	// The readings "the other endpoints" of §4.3 took, per RNIC and per
	// distance, keyed by the index of the measurement that took each. Only the
	// reduced busy probe set contributes, because those are the paths a loaded
	// RNIC measures at all. See busyRef for how a path's reference is drawn
	// from them.
	busyPeers := map[busyRefKey][]busyPeer{}
	for i, m := range out {
		k, ok := busyClassOf(g, bases, m)
		if !ok {
			continue
		}
		busyPeers[k] = append(busyPeers[k], busyPeer{idx: i, bw: m.BandwidthBPS})
	}

	for i := range out {
		m := &out[i]
		// Every branch below decides this path afresh, so a reason left over
		// from a previous classification must not outlive it.
		m.UncertainReason = model.UncertainNone
		m.BaselineClass = ""
		m.BusyRecheck = nil
		m.BWRecheck = nil
		p, ok := g.Path(m.RNIC, m.Endpoint)
		if !ok {
			markUncertain(m, model.UncertainNoPath)
			continue
		}
		n := mustRNIC(g, m.RNIC)
		base, class := pathBaseline(g, bases, p)
		m.BaselineClass = class

		if mode == model.ModeBusy && busy[m.RNIC] {
			if !g.BusyProbed(p) {
				markUncertain(m, model.UncertainRNICBusy)
				continue
			}
			ref := busyRef(busyPeers[busyRefKey{rnic: m.RNIC, dist: p.Distance, class: class}], i)
			if ref == 0 {
				markUncertain(m, model.UncertainRNICBusy)
				continue
			}
			if degraded(m.BandwidthBPS, ref, cfg.AbnormalTh) {
				m.Status = model.PathAbnormal
			} else {
				// Loaded RNIC cannot declare a path healthy vs the idle baseline.
				markUncertain(m, model.UncertainRNICBusy)
			}
			continue
		}

		if degraded(m.BandwidthBPS, base.BandwidthBPS, cfg.AbnormalTh) {
			m.Status = model.PathAbnormal
			continue
		}
		m.Status, m.UncertainReason = metBaseline(g, n, p, m.BandwidthBPS, cfg.AbnormalTh)
	}
	return out
}

// metBaseline is the idle verdict on path p of RNIC n when it read bw and met
// its distance baseline: normal when an affinitive path also reaches its
// ceiling, or for memory, which meeting the baseline proves usable for
// Algorithm 1; otherwise uncertain, with the reason why the baseline alone
// was not evidence of health.
func metBaseline(g topology.Graph, n model.RNIC, p model.Path, bw uint64, th float64) (model.PathStatus, model.PathUncertainReason) {
	e, _ := g.Endpoint(p.Endpoint)
	ceil := affinitiveCeiling(g, n, p)
	if ceil > 0 && !degraded(bw, ceil, th) {
		return model.PathNormal, model.UncertainNone
	}
	if e.Kind == model.EndpointMemory {
		// Local/remote memory that meets baseline is usable for Algorithm 1.
		return model.PathNormal, model.UncertainNone
	}
	switch {
	case !p.Affinitive:
		// Non-affinitive GPU: meeting a reduced ❸/❹ baseline ≠ bottleneck-free.
		return model.PathUncertain, model.UncertainNonAffinitive
	case ceil == 0:
		// Affinitive, but the RNIC reported no line rate, so there is no
		// ceiling to hold the path to — not a verdict on the path itself.
		return model.PathUncertain, model.UncertainNoCeiling
	default:
		// Affinitive and over its baseline, yet short of what the hardware
		// carries: a baseline alone must not promote a path to normal.
		return model.PathUncertain, model.UncertainBelowCeiling
	}
}

// busyRefKey scopes the busy-mode sibling comparison to one RNIC and one
// communication distance.
//
// NSDI'23 §4.3 lets a loaded RNIC judge a path by comparing it against its other
// endpoints instead of the idle baseline, and rests that on a premise it
// states outright: "the measured bandwidth between the RNIC and its affinitive
// endpoints is usually identical if no bottleneck occurs, no matter how much
// influenced by service traffic on the RNIC". Identical readings are what make
// one number stand for all of them. The paper's own footnote already limits
// that to the server it measured — "for some types of servers, the RNIC cannot
// reach the line rate when communicating with the memory in remote NUMA
// nodes".
//
// Where the premise does not hold the endpoints are not interchangeable, and
// the distances hostping already separates are exactly where it breaks: local
// memory, remote memory across the inter-socket bus, and a GPUDirect RDMA write
// that a bus-limited slot holds below line rate carry different baselines for
// that reason. Comparing them against one per-RNIC maximum asks the lowest of
// them to match the highest, so a healthy loaded host reports abnormal paths. Keying
// the reference by distance keeps the paper's comparison and drops only the
// assumption that every affinitive endpoint sits at one level.
//
// A learned sub-class splits a distance the same way, for the same reason:
// its paths read at a level of their own, so a path of the
// sub-class is compared only with siblings of that sub-class, and a path of
// the distance only with siblings outside every sub-class. A class left with
// no other reading cannot be judged (busyRef returns 0).
type busyRefKey struct {
	rnic string
	dist model.Distance
	// class is the learned sub-class name, "" for the distance itself.
	class string
}

// busyClassOf is the busyRefKey class of m's path, and false when the path is
// not routed or not in the reduced busy probe set, so no busy sibling
// comparison draws on it.
func busyClassOf(g topology.Graph, bases baseline.Overlay, m model.PathMeasurement) (busyRefKey, bool) {
	p, ok := g.Path(m.RNIC, m.Endpoint)
	if !ok || !g.BusyProbed(p) {
		return busyRefKey{}, false
	}
	_, class := pathBaseline(g, bases, p)
	return busyRefKey{rnic: m.RNIC, dist: p.Distance, class: class}, true
}

// pathBaseline is the idle baseline p is judged against — the Fig.4 default
// scaled to its RNIC with the operator's overrides applied, narrowed to the
// learned sub-class that lists p's root-port pair — and the full name of
// that sub-class ("cross_root/fast"), or "" when the distance's own value
// applies.
func pathBaseline(g topology.Graph, bases baseline.Overlay, p model.Path) (baseline.PathBaseline, string) {
	n := mustRNIC(g, p.RNIC)
	pair, _ := g.RootPortPair(p)
	b, sub := baseline.ForPath(bases.Effective(n), p.Distance).ForPair(pair)
	if sub == "" {
		return b, ""
	}
	return b, baseline.ClassName(p.Distance, sub)
}

// busyPeer is one busy-probed reading in a busyRefKey class, with the index of
// the measurement that took it so busyRef can leave that path out of its own
// reference.
type busyPeer struct {
	idx int
	bw  uint64
}

// busyRef is the bandwidth the busy-probed measurement at index self is judged
// against: the median of the other readings in its (RNIC, distance) class,
// averaging the two middle ones when there is an even number of them. Zero
// means the class holds no other reading, and the path cannot be judged.
//
// §4.3 does not say which level of "the other endpoints" a path is held to.
// The maximum is the obvious choice when the premise holds and the readings
// are identical, but on a loaded RNIC they are not: healthy siblings scatter
// by several percent per reading even after the engine takes the median of
// repeated probes, and the maximum of several such readings sits well above
// their typical level. A healthy path then lands AbnormalTh below that
// maximum at a steady rate, and a busy run reports an abnormal path with no
// fault behind it. The median of the others does not have that bias:
//
//   - It is unbiased under noise — it sits at the siblings' typical level,
//     not at the top of their spread.
//   - One outlier sibling, high or low, cannot move it.
//   - The path itself is left out, so it cannot raise its own bar, and a
//     degraded path is measured against the healthy level it fell from.
//
// With exactly one other sibling this is the comparison the maximum made: a
// path above its sibling was never degraded against a maximum that was its
// own reading, and a path below it compared against the sibling. Only classes
// of three or more paths change.
//
// The trade-off is a fault that degrades most of a class at once. When two of
// three siblings drop together, each of them is judged against the median of
// the other two, one of which is itself degraded, so the busy comparison needs
// a larger deficit — about a third rather than a fifth at the default
// AbnormalTh — before it flags them. The busy comparison is a stopgap for
// paths a loaded RNIC cannot measure cleanly; the idle full mesh still judges
// every path against its baseline and catches such a fault there.
func busyRef(peers []busyPeer, self int) uint64 {
	others := make([]uint64, 0, len(peers))
	for _, p := range peers {
		if p.idx != self {
			others = append(others, p.bw)
		}
	}
	if len(others) == 0 {
		return 0
	}
	return medianU64(others)
}

// markUncertain sets both halves of an undecided path: the status Algorithm 1
// and the report filter on, and the reason a consumer needs to tell a path
// hostping declined to judge from one it could not judge. PathUncertain is the
// zero PathStatus, so the two are set together to keep a reason from drifting
// away from the status it explains.
func markUncertain(m *model.PathMeasurement, r model.PathUncertainReason) {
	m.Status = model.PathUncertain
	m.UncertainReason = r
}

// affinitiveCeiling is the bandwidth an affinitive path reaches with nothing
// broken, and 0 when that cannot be decided.
//
// The paper compares against the RNIC line rate alone, which holds only while
// the line rate sits below the PCIe bus feeding it (200 Gb/s NIC on Gen4 x16).
// Invert that — a 400 Gb/s RNIC whose GPUDirect RDMA writes top out near the
// bus capacity — and line rate × (1-AbnormalTh) is unreachable by construction,
// so no GPU path could ever be normal and every GPU link stayed uncertain.
// Discovery already records each hop's healthy capacity, so take whichever of
// the two ceilings is lower. Non-affinitive paths keep their ceiling at 0:
// meeting a reduced ❸/❹ baseline is still not proof of health.
//
// A path capacity above the Fig.4 Gen4 x16 bus is held to it, as the default
// baseline is (baseline.DerivedRate): a Gen5 x16 hop carries ~504 Gb/s, but
// GPUDirect RDMA and host-DRAM loopback from a healthy 400G RNIC land far under
// 400 × (1-AbnormalTh), and every GPU path would go back to uncertain.
func affinitiveCeiling(g topology.Graph, n model.RNIC, p model.Path) uint64 {
	if !p.Affinitive || n.LineRate == 0 {
		return 0
	}
	c := g.PathCapacity(p)
	if c > baseline.Fig4BusBPS {
		c = baseline.Fig4BusBPS
	}
	if c > 0 && c < n.LineRate {
		return c
	}
	return n.LineRate
}

func degraded(got, ref uint64, th float64) bool {
	if ref == 0 {
		return false
	}
	return float64(got) < float64(ref)*(1-th)
}

func latencyHigh(got, ref uint64, th float64) bool {
	if ref == 0 {
		return got > 0
	}
	return float64(got) > float64(ref)*(1+th)
}

// DetectAbnormalLinks is NSDI'23 Algorithm 1. mode and busy decide which
// abnormal paths may mark a hop abnormal (see pathJudgesAbnormal): an idle run
// lets every path judge every hop it crosses, as the paper's full-mesh test does.
//
// It also marks which hops this run tested (LinkState.Tested): every hop of a
// normal path, and every hop an abnormal path is allowed to judge
// (pathJudgesAbnormal). Those are exactly the hops a decided path can move off
// LinkUncertain, so a gray hop is always tested. An uncertain path decides
// nothing and tests no hop.
func DetectAbnormalLinks(g topology.Graph, ms []model.PathMeasurement, mode model.RunMode, busy map[string]bool) []model.LinkState {
	states := newLinkStates(g)
	normal, abnormal := decidedPaths(g, ms)
	markNormalHops(states, normal)
	judged := judgedHops(g, states, abnormal, busyRootPorts(g, mode, busy))
	isolateAbnormalHops(states, judged)
	countAbnormalRNICs(states, abnormal, judged)
	grayFlappingHops(states, judged)

	out := make([]model.LinkState, 0, len(g.Links))
	for _, l := range g.Links {
		out = append(out, *states[l.ID])
	}
	return out
}

// newLinkStates starts every link of g uncertain and untested.
func newLinkStates(g topology.Graph) map[string]*model.LinkState {
	states := make(map[string]*model.LinkState, len(g.Links))
	for _, l := range g.Links {
		states[l.ID] = &model.LinkState{
			ID:            l.ID,
			Status:        model.LinkUncertain,
			AbnormalRNICs: map[string]struct{}{},
		}
	}
	return states
}

// decidedPaths is the routed paths of the normal and of the abnormal
// measurements, in measurement order. An uncertain path decides nothing.
func decidedPaths(g topology.Graph, ms []model.PathMeasurement) (normal, abnormal []model.Path) {
	for _, m := range ms {
		p, ok := g.Path(m.RNIC, m.Endpoint)
		if !ok {
			continue
		}
		switch m.Status {
		case model.PathNormal:
			normal = append(normal, p)
		case model.PathAbnormal:
			abnormal = append(abnormal, p)
		}
	}
	return normal, abnormal
}

// markNormalHops marks every hop of a normal path normal and tested.
func markNormalHops(states map[string]*model.LinkState, normal []model.Path) {
	for _, p := range normal {
		for _, id := range p.LinkIDs {
			if s := states[id]; s != nil {
				s.Status = model.LinkNormal
				s.Tested = true
			}
		}
	}
}

// judgedHops resolves, once, the hops each abnormal path is allowed to judge
// (pathJudgesAbnormal, with guarded the busy root ports), so every pass over
// the abnormal paths sees the same hop sets, and marks those hops tested.
func judgedHops(g topology.Graph, states map[string]*model.LinkState, abnormal []model.Path, guarded map[string]bool) [][]string {
	judged := make([][]string, len(abnormal))
	for i, p := range abnormal {
		for _, id := range p.LinkIDs {
			if !pathJudgesAbnormal(g, guarded, p.RNIC, id) {
				continue
			}
			if s := states[id]; s != nil {
				judged[i] = append(judged[i], id)
				s.Tested = true
			}
		}
	}
	return judged
}

// isolateAbnormalHops is NSDI'23 Algorithm 1: mark a hop abnormal only when
// it is the unique remaining uncertain hop (|U|=1) of an abnormal path.
// Marking every U>1 hop paints GPU and switch links abnormal on an RNIC-wide
// cap (#1/#9/#10). A path that already crosses a known-abnormal hop is
// explained by that hop and cannot isolate a second one. Repeat until no
// status changes so the result does not depend on the order measurements
// arrive in.
func isolateAbnormalHops(states map[string]*model.LinkState, judged [][]string) {
	for {
		changed := false
		for i := range judged {
			uncertain, seenAbnormal := "", false
			n := 0
			for _, id := range judged[i] {
				switch states[id].Status {
				case model.LinkUncertain:
					uncertain = id
					n++
				case model.LinkAbnormal:
					seenAbnormal = true
				}
			}
			if n == 1 && !seenAbnormal {
				states[uncertain].Status = model.LinkAbnormal
				changed = true
			}
		}
		if !changed {
			break
		}
	}
}

// countAbnormalRNICs sets abnormal_cnt: how many distinct RNICs judge a hop
// abnormal (Algorithm 1 lines 9-14). Counted once the statuses have settled.
func countAbnormalRNICs(states map[string]*model.LinkState, abnormal []model.Path, judged [][]string) {
	for i, p := range abnormal {
		for _, id := range judged[i] {
			s := states[id]
			if s.Status != model.LinkAbnormal {
				continue
			}
			if _, seen := s.AbnormalRNICs[p.RNIC]; !seen {
				s.AbnormalRNICs[p.RNIC] = struct{}{}
				s.AbnormalCount++
			}
		}
	}
}

// grayFlappingHops grays the hops of an abnormal path whose hops are all
// normal: such a path cannot explain itself, so some hop may be flapping
// (Algorithm 1 line 15). Every such path is decided against the settled
// statuses, so graying one path does not hide the next.
func grayFlappingHops(states map[string]*model.LinkState, judged [][]string) {
	var flapping []int
	for i := range judged {
		allNormal := true
		for _, id := range judged[i] {
			if states[id].Status != model.LinkNormal {
				allNormal = false
				break
			}
		}
		if allNormal {
			flapping = append(flapping, i)
		}
	}
	for _, i := range flapping {
		for _, id := range judged[i] {
			states[id].Status = model.LinkGray
		}
	}
}

// busyRootPorts is the set of root ports whose links only their own busy
// RNIC may mark abnormal. It is empty outside busy mode.
func busyRootPorts(g topology.Graph, mode model.RunMode, busy map[string]bool) map[string]bool {
	out := map[string]bool{}
	if mode != model.ModeBusy {
		return out
	}
	for _, n := range g.RNICs {
		if busy[n.ID] && n.RootPort != "" {
			out[n.RootPort] = true
		}
	}
	return out
}

// pathJudgesAbnormal reports whether an abnormal path from rnicID may count
// linkID toward Algorithm 1's unique uncertain hop. Any path may still mark a
// hop normal by traversing it (needed for unique-hop isolation).
//
// An RNIC PCIe link is judged only by its own RNIC.
//
// The root-port restriction is busy-mode only (NSDI'23 §4.3, "when triggered
// by abnormal metrics"): service traffic on the abnormal RNIC can depress what
// other RNICs measure to its affinitive GPUs, so "for the links under the same
// root port as the abnormal RNIC, we only use this abnormal RNIC to judge
// their status". guarded holds those root ports. An idle run has no such
// traffic and does the full-mesh test "to improve the accuracy of bottleneck
// inference", so there every path judges every hop it crosses.
//
// A cross-root path has no hop for the root-complex fabric between its two
// root ports, so a slowdown there lands on whichever of its hops no normal
// path covers. That needs both a remote-side hop left uncovered — the remote
// RNIC's affinitive paths already cross that root port and its switch and GPU
// links — and a drop below the reduced ❸/❹ baseline. Keeping remote RNICs out
// instead left such a path with only normal hops to judge, which grays them
// every run and reads a steady fault as flapping.
func pathJudgesAbnormal(g topology.Graph, guarded map[string]bool, rnicID, linkID string) bool {
	n, okN := g.RNIC(rnicID)
	l, okL := g.Link(linkID)
	if !okN || !okL {
		return true
	}
	switch l.Kind {
	case model.LinkKindPCIeRNIC:
		return l.Device == n.ID || l.ID == model.PCIeLinkID(n.ID)
	case model.LinkKindPCIeGPU, model.LinkKindPCIeSwitch, model.LinkKindRootPort:
		if l.RootPort == "" || !guarded[l.RootPort] {
			return true
		}
		return l.RootPort == n.RootPort
	default:
		return true
	}
}

func linkMap(states []model.LinkState) map[string]model.LinkState {
	m := make(map[string]model.LinkState, len(states))
	for _, s := range states {
		m[s.ID] = s
	}
	return m
}

// pathKey is the "rnic->endpoint" key of one path, as pathMap, latencyKey and
// PathRef.String spell it.
func pathKey(rnic, ep string) string { return rnic + "->" + ep }

func pathMap(ms []model.PathMeasurement) map[string]model.PathMeasurement {
	m := make(map[string]model.PathMeasurement, len(ms))
	for _, x := range ms {
		m[pathKey(x.RNIC, x.Endpoint)] = x
	}
	return m
}

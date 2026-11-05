package analyzer

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// pairTimeout is one mutual pair with a known timeout ratio.
type pairTimeout struct {
	m     model.MutualMeasurement
	ratio float64
}

func (p pairTimeout) peer(id string) string {
	if p.m.Src == id {
		return p.m.Dst
	}
	return p.m.Src
}

func (p pairTimeout) evidence() string {
	if !p.m.Connected {
		return fmt.Sprintf("%s ↔ %s not connected", p.m.Src, p.m.Dst)
	}
	return fmt.Sprintf("%s ↔ %s %d/%d timed out", p.m.Src, p.m.Dst, p.m.Sent-p.m.Recv, p.m.Sent)
}

// diagnoseTimeouts turns per-pair probe timeout ratios into ToN'24 §IV-C-2
// RNIC causes. Each RNIC is judged over all of its pairs, so the cause names
// the RNIC rather than every pair it appears in:
//
//   - every pair of the RNIC times out completely → the RNIC is down or
//     misrouted (ToN #13), rnic_connectivity_failure;
//   - its timeout ratio exceeds Timeout_th without being total → sudden,
//     partial loss of reachability such as a flapping RNIC (#15/#16),
//     rnic_intermittent_timeout;
//   - every RNIC of the host (that has a pair) times out completely → one
//     shared component, the ToR switch, is the likelier cause than as many
//     independent RNIC failures, tor_switch_anomaly at medium confidence.
//
// A pair with no probes sent has no ratio and takes no part.
//
// Cross-rail pairs (--mutual-scope=host) leave the rail, and whether two rails
// route to each other is a property of the fabric the host cannot see. Their
// failures therefore never make an RNIC down or satisfy the ToR rule on their
// own: an RNIC is down only when it has a same-rail pair and every one of its
// pairs times out, and the ToR rule needs at least two same-rail pairs. A
// cross-rail pair that answers still proves both RNICs are up. What the dead
// cross-rail pairs show beyond that is judged by crossRailTimeouts.
func diagnoseTimeouts(in Input) []model.RootCause {
	pairs, of := timedPairs(in.Mutual)
	if len(pairs) == 0 {
		return nil
	}
	ids := slices.Sorted(maps.Keys(of))
	allDead, down := rnicTimeoutStates(pairs, of, ids)
	if tor := torTimeout(pairs, ids, allDead); tor != nil {
		return tor
	}
	out := downRNICCauses(pairs, of, ids, down)
	out = append(out, deadPairCauses(pairs, down)...)
	out = append(out, crossRailTimeouts(pairs, ids, down)...)
	return append(out, intermittentTimeouts(in, pairs, of, ids, down)...)
}

// timedPairs is the pairs of ms that have a timeout ratio, in measurement
// order, and for each RNIC the indices of its pairs among them.
func timedPairs(ms []model.MutualMeasurement) ([]pairTimeout, map[string][]int) {
	var pairs []pairTimeout
	of := map[string][]int{}
	for _, m := range ms {
		r, ok := m.TimeoutRatio()
		if !ok {
			continue
		}
		i := len(pairs)
		pairs = append(pairs, pairTimeout{m: m, ratio: r})
		of[m.Src] = append(of[m.Src], i)
		if m.Dst != m.Src {
			of[m.Dst] = append(of[m.Dst], i)
		}
	}
	return pairs, of
}

// rnicTimeoutStates says per RNIC whether every one of its pairs times out
// completely (allDead) and whether it is down: all dead, with a same-rail
// pair among them.
func rnicTimeoutStates(pairs []pairTimeout, of map[string][]int, ids []string) (allDead, down map[string]bool) {
	allDead, down = map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		all, same := deadAndSameRail(pairs, of[id])
		allDead[id] = all
		down[id] = all && same
	}
	return allDead, down
}

// deadAndSameRail reports whether every pair at the indices idx times out
// completely, and whether any of them is same-rail.
func deadAndSameRail(pairs []pairTimeout, idx []int) (all, same bool) {
	all = true
	for _, i := range idx {
		if pairs[i].ratio < 1 {
			all = false
		}
		if !pairs[i].m.CrossRail {
			same = true
		}
	}
	return all, same
}

// torTimeout is the host-wide total timeout: every RNIC times out on all of
// its pairs, and at least two of the host's pairs are same-rail. It is nil
// otherwise. Cross-rail pairs alone never make it, and neither does a single
// same-rail pair: with one pair either RNIC alone explains it, so it stays a
// per-RNIC verdict.
func torTimeout(pairs []pairTimeout, ids []string, allDead map[string]bool) []model.RootCause {
	if sameRailPairs(pairs) < 2 {
		return nil
	}
	for _, id := range ids {
		if !allDead[id] {
			return nil
		}
	}
	return []model.RootCause{{
		Code: model.CauseToRSwitchAnomaly, Summary: "every RNIC on the host times out on its mutual probes (ToR switch anomaly suspected)",
		RNICs: ids, Confidence: model.ConfidenceMedium,
		Evidence: fmt.Sprintf("all %d RNICs time out on all %d pairs", len(ids), len(pairs)),
	}}
}

func sameRailPairs(pairs []pairTimeout) int {
	n := 0
	for _, p := range pairs {
		if !p.m.CrossRail {
			n++
		}
	}
	return n
}

// downRNICCauses names each down RNIC once, with the peers of all its pairs.
func downRNICCauses(pairs []pairTimeout, of map[string][]int, ids []string, down map[string]bool) []model.RootCause {
	var out []model.RootCause
	for _, id := range ids {
		if !down[id] {
			continue
		}
		peers := make([]string, 0, len(of[id]))
		for _, i := range of[id] {
			peers = append(peers, pairs[i].peer(id))
		}
		out = append(out, model.RootCause{
			Code: model.CauseRNICConnectFailure, Summary: "RNIC unreachable: every mutual probe timed out",
			RNICs: []string{id}, Confidence: model.ConfidenceHigh,
			Evidence: fmt.Sprintf("%s timeout_ratio=1 on every pair (%s)", id, strings.Join(peers, ", ")),
		})
	}
	return out
}

// deadPairCauses names each dead same-rail pair between two RNICs that are not
// down: both reach someone else, so it is that pair's routing or QP, not
// either RNIC as a whole.
func deadPairCauses(pairs []pairTimeout, down map[string]bool) []model.RootCause {
	var out []model.RootCause
	for _, p := range pairs {
		if p.ratio == 1 && !p.m.CrossRail && !down[p.m.Src] && !down[p.m.Dst] {
			out = append(out, model.RootCause{
				Code: model.CauseRNICConnectFailure, Summary: "RNIC mutual probe failed to connect",
				RNICs: []string{p.m.Src, p.m.Dst}, Confidence: model.ConfidenceHigh,
				Evidence: p.evidence(),
			})
		}
	}
	return out
}

// crossRailTimeouts judges the cross-rail pairs that time out completely
// between RNICs that are not down. It groups the RNICs by reachability: two
// RNICs share a group when a chain of pairs that answered (at least in part,
// same-rail or cross-rail) links them, so every measured pair between two
// groups is dead by construction. Three patterns follow:
//
//   - no cross-rail pair answers at all: the rails do not reach each other,
//     uniformly. That may be the design (isolated rails, where host scope has
//     nothing to measure) as much as a ToR or fabric routing fault, so it is
//     one low-confidence cause naming every RNIC involved;
//   - some cross-rail pairs answer and the RNICs still split into groups: a
//     subset of RNICs fails to every RNIC outside it, the pattern of RNICs
//     cabled to the wrong switch port and left on an isolated subnet
//     (ToN #14). Each group other than the unique largest one is named, at
//     medium confidence: which side is cut off is a majority guess, and a
//     dead RNIC without a same-rail pair looks the same;
//   - a dead cross-rail pair inside one group: both RNICs reach others across
//     the host, so it is the route between their two subnets, named on the
//     pair at medium confidence.
//
// Pairs with a down RNIC are already explained and take no part.
func crossRailTimeouts(pairs []pairTimeout, ids []string, down map[string]bool) []model.RootCause {
	reach, dead, crossAnswered := crossRailReach(pairs, down)
	if len(dead) == 0 {
		return nil
	}
	if !crossAnswered {
		return []model.RootCause{railsApartCause(dead)}
	}
	groups := reachGroups(reach, ids, down)
	out := crossRailRouteCauses(reach, dead)
	return append(out, cutGroupCauses(reach, dead, groups)...)
}

// crossRailReach links the RNICs that are not down by the pairs that answered
// (unionFind; an RNIC no such pair links is a group of its own). It returns
// that grouping, the cross-rail pairs between such RNICs that time out
// completely, in pair order, and whether any cross-rail pair answered.
func crossRailReach(pairs []pairTimeout, down map[string]bool) (reach unionFind, dead []pairTimeout, crossAnswered bool) {
	reach = unionFind{}
	for _, p := range pairs {
		if down[p.m.Src] || down[p.m.Dst] {
			continue
		}
		if p.ratio < 1 {
			reach.union(p.m.Src, p.m.Dst)
			crossAnswered = crossAnswered || p.m.CrossRail
			continue
		}
		if p.m.CrossRail {
			dead = append(dead, p)
		}
	}
	return reach, dead, crossAnswered
}

// railsApartCause is the uniform pattern: no cross-rail pair answers, so no
// rail reaches another.
func railsApartCause(dead []pairTimeout) model.RootCause {
	involved := map[string]bool{}
	for _, p := range dead {
		involved[p.m.Src], involved[p.m.Dst] = true, true
	}
	return model.RootCause{
		Code:       model.CauseRNICCrossRailUnreachable,
		Summary:    "no RNIC of this host reaches another across subnets (isolated rails by design, or a ToR/fabric routing fault)",
		RNICs:      sortedKeys(involved),
		Confidence: model.ConfidenceLow,
		Evidence:   fmt.Sprintf("all %d cross-rail pairs time out: %s", len(dead), joinLimited(pairEvidence(dead), 8)),
	}
}

// reachGroups lists the RNICs of ids that are not down by the root of their
// group. ids is sorted, so each group lists its RNICs in sorted order.
func reachGroups(reach unionFind, ids []string, down map[string]bool) map[string][]string {
	groups := map[string][]string{}
	for _, id := range ids {
		if !down[id] {
			r := reach.find(id)
			groups[r] = append(groups[r], id)
		}
	}
	return groups
}

// crossRailRouteCauses names each dead pair whose RNICs share a group.
func crossRailRouteCauses(reach unionFind, dead []pairTimeout) []model.RootCause {
	var out []model.RootCause
	for _, p := range dead {
		if reach.find(p.m.Src) != reach.find(p.m.Dst) {
			continue
		}
		out = append(out, model.RootCause{
			Code:    model.CauseRNICCrossRailUnreachable,
			Summary: "RNIC pair cannot reach each other across subnets while both reach other RNICs (route between their subnets suspected)",
			RNICs:   []string{p.m.Src, p.m.Dst}, Confidence: model.ConfidenceMedium,
			Evidence: p.evidence(),
		})
	}
	return out
}

// cutGroupCauses names each group that a dead pair between two groups cuts
// off, other than the unique largest group, with the dead pairs it has.
func cutGroupCauses(reach unionFind, dead []pairTimeout, groups map[string][]string) []model.RootCause {
	cut, order := cutPairs(reach, dead, groups)
	var out []model.RootCause
	for _, r := range order {
		g := groups[r]
		out = append(out, model.RootCause{
			Code:       model.CauseRNICCrossRailUnreachable,
			Summary:    "RNICs reach no RNIC outside their group across subnets while other RNICs do (cabled onto an isolated subnet suspected)",
			RNICs:      g,
			Confidence: model.ConfidenceMedium,
			Evidence:   fmt.Sprintf("group [%s] times out to every RNIC outside it: %s", strings.Join(g, " "), joinLimited(pairEvidence(cut[r]), 8)),
		})
	}
	return out
}

// cutPairs files each dead pair between two groups under both of its groups,
// except the unique largest one, and returns the filed groups (by root)
// ordered by their first RNIC.
func cutPairs(reach unionFind, dead []pairTimeout, groups map[string][]string) (map[string][]pairTimeout, []string) {
	largest, unique := largestGroup(groups)
	cut := map[string][]pairTimeout{}
	var order []string
	for _, p := range dead {
		ra, rb := reach.find(p.m.Src), reach.find(p.m.Dst)
		if ra == rb {
			continue
		}
		for _, r := range []string{ra, rb} {
			if unique && r == largest {
				continue
			}
			if _, ok := cut[r]; !ok {
				order = append(order, r)
			}
			cut[r] = append(cut[r], p)
		}
	}
	// Groups list their RNICs in sorted order, so the first one orders them.
	sort.Slice(order, func(i, j int) bool { return groups[order[i]][0] < groups[order[j]][0] })
	return cut, order
}

// largestGroup is the root of the largest group, and whether no other group
// is as large.
func largestGroup(groups map[string][]string) (largest string, unique bool) {
	for r, g := range groups {
		switch n := len(groups[largest]); {
		case largest == "" || len(g) > n:
			largest, unique = r, true
		case len(g) == n:
			unique = false
		}
	}
	return largest, unique
}

func pairEvidence(ps []pairTimeout) []string {
	ev := make([]string, 0, len(ps))
	for _, p := range ps {
		ev = append(ev, p.evidence())
	}
	return ev
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// intermittentTimeouts flags RNICs whose mean timeout ratio over their partial
// pairs exceeds Timeout_th. Pairs whose peer is down and dead pairs are
// already explained above and take no part.
//
// A flapping RNIC also raises the ratio of each healthy peer, by its share of
// that peer's pairs. A flagged RNIC is therefore cleared when every pair it
// loses probes on is with a flagged peer whose own ratio is higher: that peer
// explains it. Two RNICs with only each other to probe tie and are both kept,
// because the probe cannot tell which side lost the replies.
//
// Cross-rail pairs take part: a pair that answers in part does route, so its
// losses are not a matter of rail design. They cross more of the fabric than
// a same-rail pair, though, so an RNIC whose lossy pairs are all cross-rail is
// flagged at medium rather than high confidence.
func intermittentTimeouts(in Input, pairs []pairTimeout, of map[string][]int, ids []string, down map[string]bool) []model.RootCause {
	th := in.Cfg.TimeoutTh
	partial := func(id string) []pairTimeout {
		var ps []pairTimeout
		for _, i := range of[id] {
			p := pairs[i]
			if p.ratio < 1 && !down[p.peer(id)] {
				ps = append(ps, p)
			}
		}
		return ps
	}
	agg := map[string]float64{}
	flagged := map[string]bool{}
	for _, id := range ids {
		ps := partial(id)
		if down[id] || len(ps) == 0 {
			continue
		}
		var sum float64
		for _, p := range ps {
			sum += p.ratio
		}
		agg[id] = sum / float64(len(ps))
		flagged[id] = agg[id] > th
	}

	var out []model.RootCause
	for _, id := range ids {
		if !flagged[id] {
			continue
		}
		ps := partial(id)
		explained, crossOnly := true, true
		var ev []string
		for _, p := range ps {
			if p.ratio == 0 {
				continue
			}
			ev = append(ev, p.evidence())
			crossOnly = crossOnly && p.m.CrossRail
			if q := p.peer(id); !flagged[q] || agg[q] <= agg[id] {
				explained = false
			}
		}
		if explained {
			continue
		}
		conf := model.ConfidenceHigh
		if crossOnly {
			conf = model.ConfidenceMedium
		}
		out = append(out, model.RootCause{
			Code: model.CauseRNICIntermittent, Summary: "RNIC mutual probes time out intermittently (flapping suspected)",
			RNICs: []string{id}, Confidence: conf,
			Evidence: fmt.Sprintf("%s timeout_ratio=%.3f over %d pairs > %.2f: %s", id, agg[id], len(ps), th, strings.Join(ev, ", ")),
		})
	}
	return out
}

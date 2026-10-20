package analyzer

import (
	"fmt"
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
	var pairs []pairTimeout
	of := map[string][]int{}
	for _, m := range in.Mutual {
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
	if len(pairs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(of))
	for id := range of {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	samePairs := 0
	for _, p := range pairs {
		if !p.m.CrossRail {
			samePairs++
		}
	}
	allDead := map[string]bool{}
	down := map[string]bool{}
	for _, id := range ids {
		all, same := true, false
		for _, i := range of[id] {
			if pairs[i].ratio < 1 {
				all = false
			}
			if !pairs[i].m.CrossRail {
				same = true
			}
		}
		allDead[id] = all
		down[id] = all && same
	}

	// Host-wide total timeout. A single dead pair is left out: with one pair
	// either RNIC alone explains it, so it stays a per-RNIC verdict.
	if samePairs >= 2 {
		all := true
		for _, id := range ids {
			all = all && allDead[id]
		}
		if all {
			return []model.RootCause{{
				Code: model.CauseToRSwitchAnomaly, Summary: "every RNIC on the host times out on its mutual probes (ToR switch anomaly suspected)",
				RNICs: ids, Confidence: "medium",
				Evidence: fmt.Sprintf("all %d RNICs time out on all %d pairs", len(ids), len(pairs)),
			}}
		}
	}

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
			RNICs: []string{id}, Confidence: "high",
			Evidence: fmt.Sprintf("%s timeout_ratio=1 on every pair (%s)", id, strings.Join(peers, ", ")),
		})
	}
	// A dead same-rail pair between two RNICs that both reach someone else is
	// that pair's routing or QP, not either RNIC as a whole.
	for _, p := range pairs {
		if p.ratio == 1 && !p.m.CrossRail && !down[p.m.Src] && !down[p.m.Dst] {
			out = append(out, model.RootCause{
				Code: model.CauseRNICConnectFailure, Summary: "RNIC mutual probe failed to connect",
				RNICs: []string{p.m.Src, p.m.Dst}, Confidence: "high",
				Evidence: p.evidence(),
			})
		}
	}
	out = append(out, crossRailTimeouts(pairs, ids, down)...)
	return append(out, intermittentTimeouts(in, pairs, of, ids, down)...)
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
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p := parent[x]; p != x {
			parent[x] = find(p)
		}
		return parent[x]
	}
	for _, id := range ids {
		if !down[id] {
			parent[id] = id
		}
	}
	var dead []pairTimeout
	crossAnswered := false
	for _, p := range pairs {
		if down[p.m.Src] || down[p.m.Dst] {
			continue
		}
		if p.ratio < 1 {
			if a, b := find(p.m.Src), find(p.m.Dst); a != b {
				parent[a] = b
			}
			crossAnswered = crossAnswered || p.m.CrossRail
			continue
		}
		if p.m.CrossRail {
			dead = append(dead, p)
		}
	}
	if len(dead) == 0 {
		return nil
	}

	if !crossAnswered {
		involved := map[string]bool{}
		ev := make([]string, 0, len(dead))
		for _, p := range dead {
			involved[p.m.Src], involved[p.m.Dst] = true, true
			ev = append(ev, p.evidence())
		}
		return []model.RootCause{{
			Code:       model.CauseRNICCrossRailUnreachable,
			Summary:    "no RNIC of this host reaches another across subnets (isolated rails by design, or a ToR/fabric routing fault)",
			RNICs:      sortedKeys(involved),
			Confidence: "low",
			Evidence:   fmt.Sprintf("all %d cross-rail pairs time out: %s", len(dead), joinLimited(ev, 8)),
		}}
	}

	groups := map[string][]string{}
	for _, id := range ids {
		if !down[id] {
			r := find(id)
			groups[r] = append(groups[r], id)
		}
	}
	largest, unique := "", false
	for r, g := range groups {
		switch n := len(groups[largest]); {
		case largest == "" || len(g) > n:
			largest, unique = r, true
		case len(g) == n:
			unique = false
		}
	}

	var out []model.RootCause
	cut := map[string][]pairTimeout{}
	var order []string
	for _, p := range dead {
		ra, rb := find(p.m.Src), find(p.m.Dst)
		if ra == rb {
			out = append(out, model.RootCause{
				Code:    model.CauseRNICCrossRailUnreachable,
				Summary: "RNIC pair cannot reach each other across subnets while both reach other RNICs (route between their subnets suspected)",
				RNICs:   []string{p.m.Src, p.m.Dst}, Confidence: "medium",
				Evidence: p.evidence(),
			})
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
	for _, r := range order {
		g := groups[r]
		ev := make([]string, 0, len(cut[r]))
		for _, p := range cut[r] {
			ev = append(ev, p.evidence())
		}
		out = append(out, model.RootCause{
			Code:       model.CauseRNICCrossRailUnreachable,
			Summary:    "RNICs reach no RNIC outside their group across subnets while other RNICs do (cabled onto an isolated subnet suspected)",
			RNICs:      g,
			Confidence: "medium",
			Evidence:   fmt.Sprintf("group [%s] times out to every RNIC outside it: %s", strings.Join(g, " "), joinLimited(ev, 8)),
		})
	}
	return out
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

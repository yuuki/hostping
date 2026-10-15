package analyzer

import (
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// NSDI'23 §3.2 lists latency increase next to bandwidth degradation as a
// symptom of an intra-host bottleneck, §4.1.1 reads it from the small-message
// latency of Eq.2, and §5 applies Abnormal_th to "latency or bandwidth". A path
// whose latency rises while its bandwidth holds — an extra translation hop on a
// path that is not bus-bound, say — is invisible to the bandwidth status.
//
// The paper judges a path abnormal on either signal. hostping keeps the
// latency verdict separate (model.LatencyStatus) and out of Algorithm 1, which
// is the conservative half of that: the bandwidth baseline is scaled to each
// RNIC's line rate and PCIe hop, but the Fig.4 latencies are one server's
// numbers and nothing scales them, so a default-on check against them would
// fire on every host whose normal latency differs. A path's latency is
// therefore judged only against a latency baseline the operator measured and
// supplied, or else against its siblings on the same host, and a verdict
// resting on that must not paint hops abnormal.
//
// Only the small-message p50 is judged. LargeLatNsP50 carries no independent
// signal: Eq.1 derives bandwidth from the difference between the large- and
// small-message latencies, so a rise in the large latency alone is a bandwidth
// drop, which the bandwidth status already judges, and a rise in both cancels
// out of bandwidth and shows up in the small latency judged here.

// latencyMinSiblings is how many other paths at the same distance must have a
// latency before a path with no latency baseline is judged against their
// median.
const latencyMinSiblings = 3

// The latency floor (config.Config.LatencyFloor, --latency-floor; 500 ns by
// default) is the least excess over the sibling median a path must show, on
// top of Abnormal_th, to be judged abnormal against its siblings.
//
// A small-message p50 is one or two µs, so Abnormal_th alone is a few hundred
// ns — within what separates healthy paths of one distance on a real host:
// different RNICs, slot positions and switch ports, and run-to-run jitter. A
// relative threshold on a sibling reference is exactly what made the busy-mode
// comparison report a different healthy path abnormal on every run. The
// latency increases §3.2 and §4.1.1 are after are larger: in Fig.4 a GPUDirect
// detour through the root complex adds about 1.2 µs (❶ 1.0 µs → ❸ 2.2 µs), and
// an extra translation or bridge hop is of that order. Half of that detour
// still catches it, while sitting well above sub-µs jitter.
//
// That jitter is a platform's, though. On some hosts a healthy path's p50
// moves between two levels about a µs apart from run to run, and a path on the
// upper level clears both tests against siblings on the lower one; there the
// floor has to be raised, and rises smaller than it go unseen.
//
// A user baseline gets no floor: the operator measured that reference for
// this host, so Abnormal_th against it means what it says.

// LatencyRuns is how many consecutive idle runs must read a path abnormal
// against its siblings before diagnoseLatency reports it.
//
// The paired recheck and the locality shift clear the sibling verdicts that
// come from when a path was measured and from node-wide platform states. What
// is left now and then is a single path that reads high in one run, holds up
// in its recheck, and is fine in the next run, while some other path, on
// another RNIC and endpoint, does the same. A per-path latency fault (an extra
// translation hop, an ACS/ATS detour) does not come and go between runs, so
// requiring the same path in consecutive idle runs keeps the fault and drops
// the one-off readings, as slowMRRuns does for slow registration. It takes
// three, not two: a per-RNIC platform latency state can outlast one run and
// pass its recheck twice in a row, but rarely outlasts a few runs, and a real
// fault is still reported, one idle interval later.
const LatencyRuns = 3

// ClassifyLatency sets each path's LatencyStatus, LatencyReason and
// LatencyRefNs. It reads SmallLatNsP50 only, never Status, and ClassifyPaths
// and DetectAbnormalLinks never read what it writes.
//
// bases is the user overlay (Input.Bases), never the Fig.4 defaults: a path is
// judged against the user's latency for its distance (from the "tables"
// section for its RNIC's Fig.4 table, when the file has one, whose paths then
// form classes of their own) when one is set, and
// otherwise against the median of the host's other paths at that distance —
// both narrowed to the learned sub-class that lists the path's root-port
// pair, when one does (a sub-class with no latency of its own takes the
// distance's) —
// across every RNIC, so one RNIC or one endpoint whose every path is slow
// cannot raise its own reference. A path compares against the others only, and
// the median keeps a minority of slow siblings from moving the reference. A
// sibling verdict must also clear the latency floor. It is the sweep's verdict:
// the engine re-reads a path flagged against its siblings next to them and
// may clear it (PlanLatencyRecheck, applied by Analyze), and Analyze then
// clears a rise the other RNICs on the path's NUMA node share
// (applyLocalityShift).
//
// Busy-mode runs judge nothing: service traffic inflates a loaded RNIC's Tproc,
// and a busy run's probe set is too sparse for a sibling median anyway.
func ClassifyLatency(g topology.Graph, ms []model.PathMeasurement, mode model.RunMode, cfg config.Config, bases baseline.Overlay) []model.PathMeasurement {
	out := make([]model.PathMeasurement, len(ms))
	copy(out, ms)

	byClass := map[latClass][]int{}
	refOf := map[latClass]uint64{}
	for i := range out {
		m := &out[i]
		m.LatencyStatus, m.LatencyReason, m.LatencyRefNs, m.LatencyRecheck, m.LatencyLocality, m.LatencyStreak, m.LatencyRecheckSkipped = model.LatencyUnknown, model.LatencyReasonNone, 0, nil, nil, 0, ""
		if mode == model.ModeBusy {
			m.LatencyReason = model.LatencyBusy
			continue
		}
		c, user, why := latencyClassOf(g, *m, bases)
		if why != model.LatencyReasonNone {
			m.LatencyReason = why
			continue
		}
		byClass[c] = append(byClass[c], i)
		refOf[c] = user
	}

	for c, idx := range byClass {
		user := refOf[c]
		for _, i := range idx {
			m := &out[i]
			ref, reason := user, model.LatencyByBaseline
			if ref == 0 {
				sib := make([]uint64, 0, len(idx)-1)
				for _, j := range idx {
					if j != i {
						sib = append(sib, out[j].SmallLatNsP50)
					}
				}
				if len(sib) < latencyMinSiblings {
					m.LatencyReason = model.LatencyFewSiblings
					continue
				}
				ref, reason = medianU64(sib), model.LatencyBySiblings
			}
			m.LatencyRefNs, m.LatencyReason = ref, reason
			m.LatencyStatus = model.LatencyNormal
			abn := latencyHigh(m.SmallLatNsP50, ref, cfg.AbnormalTh)
			if reason == model.LatencyBySiblings {
				abn = siblingLatencyHigh(m.SmallLatNsP50, ref, cfg)
			}
			if abn {
				m.LatencyStatus = model.LatencyAbnormal
			}
		}
	}
	return out
}

// latClass is a path's latency class: its distance, narrowed to the learned
// sub-class that lists its root-port pair. The sub-class's latency is the
// reference when set, and a path's siblings are the class's other paths.
type latClass struct {
	dist model.Distance
	sub  string
	// section is the baseline-file "tables" section the RNIC takes its
	// values from, "" for the top level. A section exists because its
	// table reads differently, so its paths are not siblings of the rest.
	section string
}

// latencyClassOf returns m's latency class and the user latency set for it (0
// when none), or, when m cannot be judged at all, the reason why.
func latencyClassOf(g topology.Graph, m model.PathMeasurement, bases baseline.Overlay) (latClass, uint64, model.LatencyReason) {
	p, ok := g.Path(m.RNIC, m.Endpoint)
	if !ok {
		return latClass{}, 0, model.LatencyNoPath
	}
	if m.SmallLatNsP50 == 0 {
		return latClass{}, 0, model.LatencyNoReading
	}
	pair, _ := g.RootPortPair(p)
	n, _ := g.RNIC(m.RNIC)
	ov, sec := bases.Of(baseline.RNICTable(n))
	user, sub := ov[p.Distance].ForPair(pair)
	return latClass{dist: p.Distance, sub: sub, section: sec}, user.LatencyNs, model.LatencyReasonNone
}

// siblingLatencyHigh is the sibling verdict: lat exceeds the sibling
// reference ref by more than cfg.AbnormalTh and by at least cfg.LatencyFloor.
func siblingLatencyHigh(lat, ref uint64, cfg config.Config) bool {
	return latencyHigh(lat, ref, cfg.AbnormalTh) && lat >= ref+uint64(max(cfg.LatencyFloor, 0).Nanoseconds())
}

// latencyKey keys a path in the latency streaks, as "rnic->endpoint".
func latencyKey(m model.PathMeasurement) string { return m.RNIC + "->" + m.Endpoint }

// latencyStreaks updates Input.LatencyHits from this run's verdicts, as left
// by the recheck and the locality shift, and sets each path's LatencyStreak.
// In an idle run a path read abnormal against its siblings extends its streak;
// every other path's streak ends — one judged normal, one that could not be
// judged (too few siblings, no reading), one judged against a user baseline
// (which needs no streak), and one not measured at all, since the run did not
// show the rise again. Streaks of paths no longer in the graph end with it.
// Busy runs judge no latency and leave every streak as it was.
//
// A sibling verdict the recheck skipped for its cap (LatencyRecheckSkipped)
// holds its streak instead: the run read the path high but gave it no chance
// to be cleared, so the reading neither counts as a repeat nor ends one. A
// streak therefore grows only in idle runs where the path was rechecked and
// confirmed, or where no recheck was planned for its class at all (a
// class-wide rise, or an engine that does not recheck), or the recheck was
// abandoned — never on readings the cap left unchecked. PlanLatencyRecheck
// rotates the cap over runs, so a persistent fault is rechecked, and its
// streak grows, within a few runs.
func latencyStreaks(in Input, paths []model.PathMeasurement) map[string]int {
	if in.Mode == model.ModeBusy {
		out := maps.Clone(in.LatencyHits)
		if out == nil {
			out = map[string]int{}
		}
		return out
	}
	hits := map[string]int{}
	for i := range paths {
		m := &paths[i]
		if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings {
			continue
		}
		k := latencyKey(*m)
		if m.LatencyRecheckSkipped != "" {
			if n := in.LatencyHits[k]; n > 0 {
				hits[k] = n
			}
			m.LatencyStreak = hits[k]
			continue
		}
		hits[k] = in.LatencyHits[k] + 1
		m.LatencyStreak = hits[k]
	}
	return hits
}

// latencyReportable reports whether a latency-abnormal path may become a
// cause. A user-baseline verdict always may: the operator's reference does not
// move from run to run. A sibling verdict the recheck skipped for its cap never
// may, whatever streak it holds: this run's reading of it is unconfirmed. Any
// other sibling verdict needs LatencyRuns consecutive idle runs
// (latencyStreaks). A one-shot run carries no streaks, so there a sibling
// verdict is reported when the paired recheck confirmed it — a second,
// interleaved reading of the path in the same run — and not when the recheck
// did not run on it (a class-wide rise, past the cap, or abandoned).
func latencyReportable(m model.PathMeasurement, oneShot bool) bool {
	if m.LatencyReason != model.LatencyBySiblings {
		return true
	}
	if m.LatencyRecheckSkipped != "" {
		return false
	}
	if m.LatencyStreak >= LatencyRuns {
		return true
	}
	return oneShot && m.LatencyRecheck != nil && m.LatencyRecheck.Confirmed
}

// diagnoseLatency reports paths whose latency is abnormal while nothing else
// explains it: the path's bandwidth is not abnormal (a bandwidth cause then
// owns the path), and no ACS/ATS cause already names the GDR detour that adds
// the latency. It is advisory and changes no status. A sibling verdict is
// reported only once it has persisted (latencyReportable); a path below that
// keeps its abnormal latency_status and its latency_streak, but no cause
// names it. The grouping below still counts it as abnormal, since that is
// what the run read.
//
// One problem usually raises many paths at once — every path to a slow
// endpoint, every path from a slow RNIC, or, against a user baseline, every
// path of a distance — so the paths are grouped rather than reported one by
// one. A distance, then an endpoint, whose every judged path is
// latency-abnormal (at least two of them) becomes one cause; what is left is
// grouped per RNIC. Confidence is medium when every path in the cause was
// judged against a user baseline, and low when any rests on the sibling median.
func diagnoseLatency(g topology.Graph, paths []model.PathMeasurement, prior []model.RootCause, th float64, oneShot bool) []model.RootCause {
	acsEps := map[string]bool{}
	atsRNICs := map[string]bool{}
	for _, c := range prior {
		switch c.Code {
		case model.CauseACSEnabled:
			for _, id := range c.Endpoints {
				acsEps[id] = true
			}
		case model.CauseATSDisabled:
			for _, id := range c.RNICs {
				atsRNICs[id] = true
			}
		}
	}
	// ACS and ATS reroute GPUDirect RDMA through the root complex; they explain
	// latency on GPU paths only.
	explained := func(m model.PathMeasurement) bool {
		e, ok := g.Endpoint(m.Endpoint)
		if !ok || e.Kind != model.EndpointGPU {
			return false
		}
		return acsEps[m.Endpoint] || atsRNICs[m.RNIC]
	}

	distOf := func(m model.PathMeasurement) model.Distance {
		p, _ := g.Path(m.RNIC, m.Endpoint)
		return p.Distance
	}
	distTally := map[model.Distance]*latencyTally{}
	epTally := map[string]*latencyTally{}
	var flagged []model.PathMeasurement
	for _, m := range paths {
		if m.LatencyStatus == model.LatencyUnknown {
			continue
		}
		abn := m.LatencyStatus == model.LatencyAbnormal
		tallyAdd(distTally, distOf(m), abn)
		tallyAdd(epTally, m.Endpoint, abn)
		if !abn || m.Status == model.PathAbnormal || explained(m) || !latencyReportable(m, oneShot) {
			continue
		}
		flagged = append(flagged, m)
	}
	if len(flagged) == 0 {
		return nil
	}

	byDist := map[model.Distance][]model.PathMeasurement{}
	byEP := map[string][]model.PathMeasurement{}
	byRNIC := map[string][]model.PathMeasurement{}
	for _, m := range flagged {
		switch d := distOf(m); {
		case distTally[d].whole():
			byDist[d] = append(byDist[d], m)
		case epTally[m.Endpoint].whole():
			byEP[m.Endpoint] = append(byEP[m.Endpoint], m)
		default:
			byRNIC[m.RNIC] = append(byRNIC[m.RNIC], m)
		}
	}

	var out []model.RootCause
	for _, d := range model.Distances() {
		if ms := byDist[d]; len(ms) > 0 {
			out = append(out, latencyCause(ms, fmt.Sprintf("every judged %s path", d), th))
		}
	}
	for _, e := range g.Endpoints {
		if ms := byEP[e.ID]; len(ms) > 0 {
			out = append(out, latencyCause(ms, fmt.Sprintf("every judged path to %s", e.ID), th))
		}
	}
	for _, n := range g.RNICs {
		if ms := byRNIC[n.ID]; len(ms) > 0 {
			out = append(out, latencyCause(ms, fmt.Sprintf("%d path(s) from %s", len(ms), n.ID), th))
		}
	}
	return out
}

// latencyTally counts the judged and the latency-abnormal paths of one group.
type latencyTally struct{ judged, abnormal int }

// whole reports a group of at least two judged paths that are all abnormal.
func (t *latencyTally) whole() bool {
	return t != nil && t.judged >= 2 && t.abnormal == t.judged
}

func tallyAdd[K comparable](m map[K]*latencyTally, k K, abn bool) {
	t, ok := m[k]
	if !ok {
		t = &latencyTally{}
		m[k] = t
	}
	t.judged++
	if abn {
		t.abnormal++
	}
}

func latencyCause(ms []model.PathMeasurement, scope string, th float64) model.RootCause {
	conf := "medium"
	ev := make([]string, 0, len(ms))
	var rnics, eps []string
	for _, m := range ms {
		rnics = append(rnics, m.RNIC)
		eps = append(eps, m.Endpoint)
		if m.LatencyReason != model.LatencyByBaseline {
			conf = "low"
		}
		notes := []string{m.LatencyReason.String()}
		if r := m.LatencyRecheck; r != nil && r.Confirmed {
			notes = append(notes, fmt.Sprintf("recheck %dns vs %dns", r.PathNsP50, r.SiblingNsP50))
		}
		if m.LatencyReason == model.LatencyBySiblings {
			if m.LatencyStreak >= LatencyRuns {
				notes = append(notes, fmt.Sprintf("%d consecutive idle runs", m.LatencyStreak))
			} else {
				notes = append(notes, "one-shot run")
			}
		}
		ev = append(ev, fmt.Sprintf("%s->%s %dns vs %dns (%s)", m.RNIC, m.Endpoint, m.SmallLatNsP50, m.LatencyRefNs, strings.Join(notes, ", ")))
	}
	sort.Strings(rnics)
	sort.Strings(eps)
	return model.RootCause{
		Code:       model.CausePathLatencyAbnormal,
		Summary:    "intra-host path latency high while bandwidth holds",
		RNICs:      unique(rnics),
		Endpoints:  unique(eps),
		Confidence: conf,
		Evidence: fmt.Sprintf("small-message p50 more than %.0f%% above reference on %s: %s",
			th*100, scope, joinLimited(ev, 4)),
	}
}

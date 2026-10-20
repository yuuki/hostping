package analyzer

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/topology"
)

// Input is one analyzer pass.
type Input struct {
	Graph        topology.Graph
	Cfg          config.Config
	Mode         model.RunMode
	BusyRNICs    map[string]bool
	Measurements []model.PathMeasurement
	Mutual       []model.MutualMeasurement
	Util         map[string]float64
	PCIe         map[string]model.PCIeConfig // keyed by RNIC or GPU id
	DropDelta    map[string]uint64           // hardware discard/OOB deltas; not RC loss
	// RNICHints maps rnic id → slow_start | tx_window from device config.
	RNICHints map[string]string
	// GrayHits counts consecutive gray markings per link (flapping, §4.3).
	GrayHits map[string]int
	// SlowMRHits counts, per GPU endpoint, the consecutive idle runs whose
	// registration time crossed the gpu_mr_registration_slow threshold.
	SlowMRHits map[string]int
	// LatencyHits counts, per path keyed "rnic->endpoint", the consecutive
	// idle runs that read the path latency-abnormal against its siblings
	// (latencyStreaks).
	LatencyHits map[string]int
	// OneShot marks a run with no streaks carried from earlier runs and none
	// kept for later ones (hostpingctl diagnose, hostping --once). A sibling
	// latency verdict cannot persist across runs there, so the paired
	// recheck's confirmation stands in for the second run (diagnoseLatency).
	OneShot bool
	// Bases is a user overlay only. Zero BandwidthBPS / LatencyNs means unset.
	// Each RNIC takes the section for its Fig.4 table, or the top level.
	// Do not pass baseline.Defaults; ForRNIC scales Fig.4 then Apply.
	Bases baseline.Overlay
	// LatencyRecheck is the engine's paired re-measurement of the paths
	// PlanLatencyRecheck returned, keyed by "rnic->endpoint" and decided by
	// JudgeLatencyRecheck. A path in it whose sibling verdict was not
	// confirmed is latency-normal (LatencyRecheckCleared); it changes nothing
	// else. Nil means no recheck ran.
	LatencyRecheck map[string]model.LatencyRecheck
	// LatencyRecheckSkipped names, keyed by "rnic->endpoint", the candidates
	// PlanLatencyRecheck left out (LatencyRecheckSelection.Capped) and why. A
	// path in it still abnormal against its siblings after the recheck and
	// the locality shift is marked LatencyRecheckSkipped: it holds its
	// latency streak and is not reported in this run.
	LatencyRecheckSkipped map[string]model.LatencyRecheckSkip
	// BusyRecheck is the engine's paired re-read of the paths PlanBusyRecheck
	// returned, keyed by "rnic->endpoint" and decided by JudgeBusyRecheck. A
	// path in it whose busy sibling verdict was not confirmed is uncertain
	// with rnic_busy before Algorithm 1 and the diagnoses read it. Nil means
	// no recheck ran.
	BusyRecheck map[string]model.BusyRecheck
	// BWRecheck is the engine's paired re-read of the groups PlanBWRecheck
	// returned, keyed by BWRecheckPlan.Group and decided by JudgeBWRecheck.
	// Every path of a group that the recheck did not confirm is normal (or
	// uncertain where a baseline alone does not prove health) or uncertain
	// with host_shift before Algorithm 1 and the diagnoses read it. Nil means
	// no recheck ran.
	BWRecheck map[string]model.BandwidthRecheck
	// CPUBusySockets is the set of CPU packages whose CPU load was at or
	// above --cpu-busy-high when the run started (model.CPUBusySockets). An
	// abnormal path through one of them is uncertain with cpu_busy before
	// Algorithm 1 and the diagnoses read it. Nil means no package was loaded,
	// or the load was not read.
	CPUBusySockets map[int]bool
}

// Result is tomography + root-cause output.
type Result struct {
	Paths    []model.PathMeasurement
	Links    []model.LinkState
	Causes   []model.RootCause
	GrayHits map[string]int
	// SlowMRHits is Input.SlowMRHits updated by this run.
	SlowMRHits map[string]int
	// LatencyHits is Input.LatencyHits updated by this run.
	LatencyHits map[string]int
}

func Analyze(in Input) Result {
	r := in.run()
	// Every pass below judges the same paths, so their classes are resolved
	// once.
	classes := r.pathClasses(in.Measurements)
	paths := r.classifyPaths(in.Measurements, classes)
	// Applied before anything reads the bandwidth verdicts, so the links,
	// gray streaks and causes all see the confirmed ones only. The two touch
	// disjoint paths: idle-judged ones, and those of a loaded RNIC.
	paths = r.applyBWRecheck(paths, classes, in.BWRecheck)
	paths = r.applyBusyRecheck(paths, in.BusyRecheck)
	// After both rechecks, which confirm a drop that a constant CPU load
	// causes, and before anything reads the bandwidth verdicts.
	paths = applyCPUBusy(in.Graph, paths, in.CPUBusySockets)
	// Latency is judged on its own and never reaches Algorithm 1 (latency.go).
	// The order is the sweep verdict, the paired recheck, the locality shift,
	// the cap marks, then the streaks. PlanLatencyRecheck pre-applied the
	// locality shift to the sweep verdicts only to keep paths it clears off
	// the cap; it clears them again here, after the recheck.
	paths = r.classifyLatency(paths, classes)
	paths = applyLatencyRecheck(paths, in.LatencyRecheck)
	paths = r.applyLocalityShift(paths, classes)
	paths = markLatencyRecheckSkipped(paths, in.LatencyRecheckSkipped)
	paths = applyCPUBusyLatency(in.Graph, paths, in.CPUBusySockets)
	latHits := latencyStreaks(in, paths)
	links := DetectAbnormalLinks(in.Graph, paths, in.Mode, in.BusyRNICs)
	for i := range links {
		if u, ok := in.Util[links[i].ID]; ok {
			links[i].Utilization = &u
		}
	}

	// NSDI'23 §4.3: a link gray in FlapWindow consecutive loopback tests is
	// flapping. A tested link that is not gray this run — normal, abnormal, or
	// crossed by an abnormal path that could not isolate it — breaks the streak.
	// A link no decided path reached was not tested at all (for example, it sits
	// outside a loaded RNIC's reduced busy probe set), so its streak carries over.
	gray := map[string]int{}
	for k, v := range in.GrayHits {
		gray[k] = v
	}
	for _, l := range links {
		switch {
		case l.Status == model.LinkGray:
			gray[l.ID]++
		case l.Tested:
			delete(gray, l.ID)
		}
	}

	causes := diagnose(in, paths, links, gray)
	causes = append(causes, diagnoseMutual(in)...)
	mrCauses, mrHits := diagnoseSlowMR(in)
	causes = append(causes, mrCauses...)
	causes = suppressOverlaps(causes, in.Graph)
	causes = append(causes, diagnosePCIeDownTrain(in, paths, causes)...)
	causes = append(causes, diagnoseLatency(in.Graph, paths, causes, in.Cfg.AbnormalTh, in.OneShot)...)
	slices.SortStableFunc(causes, compareCauses)

	return Result{Paths: paths, Links: links, Causes: causes, GrayHits: gray, SlowMRHits: mrHits, LatencyHits: latHits}
}

// confidenceRank orders confidences high, medium, low for the report.
var confidenceRank = map[model.Confidence]int{model.ConfidenceHigh: 0, model.ConfidenceMedium: 1, model.ConfidenceLow: 2}

// causeKey is a cause's identity: its code and the RNICs, links and endpoints
// it names, in that order. Two causes with one key say the same thing about
// the same subject (dedupeCauses keeps the first), and the key breaks ties in
// the report order so that never depends on map iteration.
func causeKey(c model.RootCause) string {
	return c.Code + "\x00" +
		strings.Join(c.RNICs, ",") + "\x00" +
		strings.Join(c.Links, ",") + "\x00" +
		strings.Join(c.Endpoints, ",")
}

// compareCauses is the report order: confidence rank, then code, then the
// cause's identity (causeKey).
func compareCauses(a, b model.RootCause) int {
	if ra, rb := confidenceRank[a.Confidence], confidenceRank[b.Confidence]; ra != rb {
		return cmp.Compare(ra, rb)
	}
	if a.Code != b.Code {
		return cmp.Compare(a.Code, b.Code)
	}
	return cmp.Compare(causeKey(a), causeKey(b))
}

func diagnoseMisconfig(in Input, paths []model.PathMeasurement) []model.RootCause {
	pm := pathMap(paths)
	g := in.Graph
	var out []model.RootCause
	acsHits := 0
	var acsEps []string
	atsByRNIC := map[string][]string{}
	for _, n := range g.RNICs {
		for _, e := range g.AffinitiveEndpoints(n.ID) {
			if e.Kind != model.EndpointGPU {
				continue
			}
			m, ok := pm[pathKey(n.ID, e.ID)]
			if !ok || m.Status != model.PathAbnormal {
				continue
			}
			if !affinitiveLatencyHigh(in, n, m) {
				continue
			}
			rpc := in.PCIe[n.ID]
			if acsEnabledOnPath(in.PCIe, n, e) {
				acsHits++
				acsEps = append(acsEps, e.ID)
				continue
			}
			if rpc.ATSKnown && !rpc.ATSEnabled {
				atsByRNIC[n.ID] = append(atsByRNIC[n.ID], e.ID)
			}
		}
	}
	if acsHits > 0 {
		out = append(out, model.RootCause{
			Code: model.CauseACSEnabled, Summary: "ACS redirects GDR through the CPU root complex",
			Endpoints: unique(acsEps), Confidence: model.ConfidenceHigh,
			Evidence: fmt.Sprintf("%d affinitive GPU paths have high latency with ACS enabled", acsHits),
		})
	}
	for rnic, eps := range atsByRNIC {
		out = append(out, model.RootCause{
			Code: model.CauseATSDisabled, Summary: "ATS disabled; GDR address translation hairpins to the CPU",
			RNICs: []string{rnic}, Endpoints: unique(eps), Confidence: model.ConfidenceHigh,
			Evidence: "same-root-port GPU latency high while ATS is off",
		})
	}
	return out
}

func unique(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func suppressOverlaps(causes []model.RootCause, g topology.Graph) []model.RootCause {
	rpSockets := map[int]bool{}
	hasACS := false
	rnicWide := map[string]bool{}
	for _, c := range causes {
		if c.Code == model.CauseCPURootPortFailure {
			for _, id := range c.Links {
				if l, ok := g.Link(id); ok {
					rpSockets[l.Socket] = true
				}
			}
		}
		if c.Code == model.CauseACSEnabled || c.Code == model.CauseATSDisabled {
			hasACS = true
		}
		switch c.Code {
		case model.CauseRNICPCIeFailure, model.CauseRNICSlowStart, model.CauseRNICTxWindow, model.CauseOverloadedRNICPCIe:
			for _, id := range c.RNICs {
				rnicWide[id] = true
			}
		}
	}
	var out []model.RootCause
	for _, c := range causes {
		if c.Code == model.CauseMemoryChannelFailure {
			skip := false
			for _, id := range c.Links {
				if l, ok := g.Link(id); ok && rpSockets[l.Socket] {
					skip = true
				}
			}
			if skip {
				continue
			}
		}
		if c.Code == model.CauseGPUPCIeFailure && hasACS {
			continue
		}
		if c.Code == model.CauseGPUPCIeFailure && gpuUnderWideRNIC(g, c, rnicWide) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func gpuUnderWideRNIC(g topology.Graph, c model.RootCause, wide map[string]bool) bool {
	if len(wide) == 0 {
		return false
	}
	for _, id := range c.Endpoints {
		e, ok := g.Endpoint(id)
		if !ok {
			continue
		}
		if wide[affinitiveRNIC(g, e)] {
			return true
		}
	}
	for _, id := range c.RNICs {
		if wide[id] {
			return true
		}
	}
	return false
}

// flapping reports NSDI'23 §4.3 flapping: the link is gray in this run and has
// been gray in at least window consecutive loopback tests. A streak carried
// over from earlier runs is not enough on its own; a run that did not see the
// link gray has no evidence that it is flapping now.
func flapping(ls model.LinkState, gray map[string]int, window int) bool {
	return ls.Status == model.LinkGray && gray[ls.ID] >= window
}

// utilAtLeast reports whether the link's utilization was read and is at
// least th. An unknown utilization never explains a link as overloaded.
func utilAtLeast(ls model.LinkState, th float64) bool {
	return ls.Utilization != nil && *ls.Utilization >= th
}

// utilEvidence renders the utilization for diagnosis evidence: "util=0.42",
// or "util=unknown" when no live source read it.
func utilEvidence(ls model.LinkState) string {
	if ls.Utilization == nil {
		return "util=unknown"
	}
	return fmt.Sprintf("util=%.2f", *ls.Utilization)
}

func diagnose(in Input, paths []model.PathMeasurement, links []model.LinkState, gray map[string]int) []model.RootCause {
	st := linkMap(links)
	pm := pathMap(paths)
	g := in.Graph
	var causes []model.RootCause
	causes = append(causes, diagnoseMisconfig(in, paths)...)

	for _, n := range g.RNICs {
		// Every measured affinitive path of the RNIC is abnormal.
		eps, abnormal := measuredAffinitive(g.AffinitiveEndpoints(n.ID), n.ID, pm)
		if len(eps) == 0 || abnormal != len(eps) {
			continue
		}
		pcieID := model.PCIeLinkID(n.ID)
		ls := st[pcieID]
		cfg := in.PCIe[n.ID]
		switch {
		case utilAtLeast(ls, in.Cfg.UtilHigh):
			causes = append(causes, model.RootCause{
				Code: model.CauseOverloadedRNICPCIe, Summary: "RNIC PCIe link overloaded (loopback contention)",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: model.ConfidenceHigh,
				Evidence: fmt.Sprintf("all affinitive paths abnormal; %s", utilEvidence(ls)),
			})
		case rnicPCIeDegraded(cfg, n.LineRate):
			causes = append(causes, model.RootCause{
				Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link failed or down-trained",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: model.ConfidenceHigh,
				Evidence: fmt.Sprintf("PCIe %s trained below its maximum; all affinitive endpoints abnormal", pcieEvidence(cfg)),
			})
		case in.RNICHints[n.ID] == model.HintTxWindow:
			causes = append(causes, model.RootCause{
				Code: model.CauseRNICTxWindow, Summary: "RNIC Tx window too small",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Endpoints: eps, Confidence: model.ConfidenceHigh,
				Evidence: "RNIC-wide bandwidth cap with Tx window configuration",
			})
		case in.RNICHints[n.ID] == model.HintSlowStart:
			causes = append(causes, model.RootCause{
				Code: model.CauseRNICSlowStart, Summary: "RNIC-wide bandwidth cap (slow start or small Tx window)",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Endpoints: eps, Confidence: model.ConfidenceMedium,
				Evidence: "device config shows RNIC-wide throttle (slow start); not inferred from equal BWs alone",
			})
		default:
			// The negotiated link is healthy, unknown, at its maximum, or
			// below the line rate with unknown maxima here, so nothing but
			// the equal bandwidth cap points at PCIe. NSDI'23 #9 (slow start)
			// and #10 (Tx window) cap every affinitive path the same way and
			// were first misdiagnosed as this cause; live hosts have no
			// vendor-neutral source for that device config, so the hint cases
			// above never fire there and the throttle cannot be excluded.
			causes = append(causes, model.RootCause{
				Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link likely failed",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Endpoints: eps, Confidence: model.ConfidenceMedium,
				Evidence: "all affinitive paths from this RNIC are abnormal; " + noPCIeEvidence(cfg, n.LineRate) +
					"; an RNIC-wide throttle (slow start / Tx window, NSDI'23 #9/#10) was not ruled out",
			})
		}
	}

	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointGPU {
			continue
		}
		id := model.PCIeLinkID(e.ID)
		ls := st[id]
		if ls.Status != model.LinkAbnormal {
			continue
		}
		affRNIC := affinitiveRNIC(g, e)
		latAbn := false
		if affRNIC != "" {
			if m, ok := pm[pathKey(affRNIC, e.ID)]; ok {
				latAbn = affinitiveLatencyHigh(in, mustRNIC(g, affRNIC), m)
			}
		}
		if latAbn {
			continue // ACS/ATS handled by diagnoseMisconfig
		}
		switch {
		case utilAtLeast(ls, in.Cfg.UtilHigh):
			causes = append(causes, model.RootCause{
				Code: model.CauseGPUPCIeFailure, Summary: "GPU PCIe link overloaded",
				Links: []string{id}, Endpoints: []string{e.ID}, Confidence: model.ConfidenceMedium,
				Evidence: utilEvidence(ls),
			})
		default:
			causes = append(causes, model.RootCause{
				Code: model.CauseGPUPCIeFailure, Summary: "GPU PCIe link failed",
				Links: []string{id}, Endpoints: []string{e.ID}, Confidence: model.ConfidenceHigh,
				Evidence: "GPU PCIe hop abnormal without a matching latency increase",
			})
		}
	}
	causes = append(causes, diagnoseGPUFromSiblings(g, pm, st, in)...)

	causes = append(causes, diagnoseBusLinks(g, st, gray, in.Cfg)...)

	causes = append(causes, diagnoseRootPorts(g, paths, st)...)
	causes = append(causes, diagnoseBusyEqualRNIC(in, paths, st, causes)...)

	if len(causes) == 0 {
		if anyAbnormalPath(paths) {
			causes = append(causes, model.RootCause{
				Code: model.CauseUnknown, Summary: "abnormal paths without a unique hop",
				Confidence: model.ConfidenceLow, Evidence: "tomography left only gray/uncertain links",
			})
		}
	}
	return dedupeCauses(causes)
}

// busLinkRule is how one kind of bus link between NUMA nodes or memory
// channels is diagnosed: flapping (NSDI'23 §4.3) names its own cause; an
// abnormal link whose utilization is at or above UtilHigh is overloaded,
// where the kind has an overload cause; any other abnormal link failed.
type busLinkRule struct {
	kind string
	// flapCode / flapSummary name the flapping cause.
	flapCode, flapSummary string
	// overloadCode / overloadSummary name the overload cause. An empty
	// overloadCode means no source reads this kind's utilization, so an
	// abnormal link is never explained as load.
	overloadCode, overloadSummary string
	// failCode / failSummary name the failure cause; failEvidence renders
	// its evidence from the link state.
	failCode, failSummary string
	failEvidence          func(ls model.LinkState) string
}

// busLinkRules are applied in this order: memory channels, then the
// inter-socket bus, then the package mesh. Only upi.* links ever carry the
// UPI causes; a fault localized to the package mesh of a sub-NUMA-clustered
// host is reported as snc_mesh_failure instead, judged by the upi_failure
// rules.
var busLinkRules = []busLinkRule{
	{
		kind:            model.LinkKindMemory,
		flapCode:        model.CauseMemoryChannelFlapping,
		flapSummary:     "memory channel flapping",
		overloadCode:    model.CauseOverloadedMemory,
		overloadSummary: "memory channel overloaded",
		failCode:        model.CauseMemoryChannelFailure,
		failSummary:     "memory channel failed",
		failEvidence: func(ls model.LinkState) string {
			return fmt.Sprintf("abnormal_cnt=%d %s", ls.AbnormalCount, utilEvidence(ls))
		},
	},
	{
		kind:            model.LinkKindUPI,
		flapCode:        model.CauseUPIFailure,
		flapSummary:     "inter-socket bus flapping",
		overloadCode:    model.CauseOverloadedUPI,
		overloadSummary: "inter-socket bus overloaded",
		failCode:        model.CauseUPIFailure,
		failSummary:     "inter-socket bus (UPI/xGMI) failed",
		failEvidence: func(ls model.LinkState) string {
			return fmt.Sprintf("status=%s abnormal_cnt=%d", ls.Status, ls.AbnormalCount)
		},
	},
	{
		// The fabric between two NUMA nodes of one package (sub-NUMA
		// clustering). No source reads its utilization.
		kind:        model.LinkKindSNCMesh,
		flapCode:    model.CauseSNCMeshFailure,
		flapSummary: "intra-package mesh between NUMA nodes flapping",
		failCode:    model.CauseSNCMeshFailure,
		failSummary: "intra-package mesh between NUMA nodes (sub-NUMA clustering) failed",
		failEvidence: func(ls model.LinkState) string {
			return fmt.Sprintf("status=%s abnormal_cnt=%d %s", ls.Status, ls.AbnormalCount, utilEvidence(ls))
		},
	},
}

// diagnoseBusLinks applies busLinkRules, one kind at a time and each kind's
// links in g.Links order.
func diagnoseBusLinks(g topology.Graph, st map[string]model.LinkState, gray map[string]int, cfg config.Config) []model.RootCause {
	var out []model.RootCause
	for _, r := range busLinkRules {
		for _, l := range g.LinksByKind(r.kind) {
			ls := st[l.ID]
			if flapping(ls, gray, cfg.FlapWindow) {
				out = append(out, model.RootCause{
					Code: r.flapCode, Summary: r.flapSummary,
					Links: []string{l.ID}, Confidence: model.ConfidenceHigh,
					Evidence: fmt.Sprintf("gray in %d consecutive loopback tests", gray[l.ID]),
				})
				continue
			}
			// NSDI'23 §4.3 / ToN'24 §IV-C: utilization only explains a link
			// that tomography already found abnormal; a busy but healthy bus
			// is no cause.
			if ls.Status != model.LinkAbnormal {
				continue
			}
			if r.overloadCode != "" && utilAtLeast(ls, cfg.UtilHigh) {
				out = append(out, model.RootCause{
					Code: r.overloadCode, Summary: r.overloadSummary,
					Links: []string{l.ID}, Confidence: model.ConfidenceHigh,
					Evidence: fmt.Sprintf("abnormal_cnt=%d %s", ls.AbnormalCount, utilEvidence(ls)),
				})
				continue
			}
			out = append(out, model.RootCause{
				Code: r.failCode, Summary: r.failSummary,
				Links: []string{l.ID}, Confidence: model.ConfidenceHigh,
				Evidence: r.failEvidence(ls),
			})
		}
	}
	return out
}

// diagnoseGPUFromSiblings catches a GPU PCIe fault when Algorithm 1 cannot
// isolate the hop: a busy RNIC never paints its own PCIe Normal, so |U|>1.
// Sibling affinitive paths that are not Abnormal still name the GPU.
func diagnoseGPUFromSiblings(g topology.Graph, pm map[string]model.PathMeasurement, st map[string]model.LinkState, in Input) []model.RootCause {
	var out []model.RootCause
	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointGPU {
			continue
		}
		id := model.PCIeLinkID(e.ID)
		if st[id].Status == model.LinkAbnormal {
			continue
		}
		aff := affinitiveRNIC(g, e)
		if aff == "" {
			continue
		}
		m, ok := pm[pathKey(aff, e.ID)]
		if !ok || m.Status != model.PathAbnormal {
			continue
		}
		if affinitiveLatencyHigh(in, mustRNIC(g, aff), m) {
			continue
		}
		siblingOK := false
		for _, sib := range g.AffinitiveEndpoints(aff) {
			if sib.ID == e.ID {
				continue
			}
			sm, ok := pm[pathKey(aff, sib.ID)]
			if ok && sm.Status != model.PathAbnormal {
				siblingOK = true
				break
			}
		}
		if !siblingOK {
			continue
		}
		out = append(out, model.RootCause{
			Code: model.CauseGPUPCIeFailure, Summary: "GPU PCIe link failed",
			Links: []string{id}, Endpoints: []string{e.ID}, Confidence: model.ConfidenceHigh,
			Evidence: "affinitive GPU path abnormal while sibling affinitive paths are not",
		})
	}
	return out
}

func mustRNIC(g topology.Graph, id string) model.RNIC {
	n, _ := g.RNIC(id)
	return n
}

// affinitiveLatencyHigh reports whether m, an affinitive path of RNIC n, has
// a small-message latency above the same_switch baseline: the ACS/ATS
// symptom diagnoseMisconfig reads, which the GPU PCIe rules leave to it.
func affinitiveLatencyHigh(in Input, n model.RNIC, m model.PathMeasurement) bool {
	base := baseline.ForPath(in.Bases.Effective(n), model.DistanceSameSwitch)
	return latencyHigh(m.SmallLatNsP50, base.LatencyNs, in.Cfg.AbnormalTh)
}

func diagnoseRootPorts(g topology.Graph, paths []model.PathMeasurement, st map[string]model.LinkState) []model.RootCause {
	pm := pathMap(paths)
	localHealthy := localGPUsHealthyByRNIC(g, pm)
	var out []model.RootCause
	for _, l := range g.LinksByKind(model.LinkKindRootPort) {
		// Gray sibling RPs are Algorithm 1 conflicts, not isolated #5 faults.
		if st[l.ID].Status != model.LinkAbnormal {
			continue
		}
		localOK := true
		crossGPUAbn := false
		var trans []string
		for _, n := range g.RNICs {
			if n.RootPort == l.ID && !localHealthy[n.ID] {
				localOK = false
			}
		}
		memAbn := false
		for _, ml := range g.LinksByKind(model.LinkKindMemory) {
			if ml.Socket != l.Socket {
				continue
			}
			if st[ml.ID].Status == model.LinkAbnormal {
				memAbn = true
			}
		}
		for _, p := range g.Paths {
			if !p.ContainsLink(l.ID) {
				continue
			}
			n, okN := g.RNIC(p.RNIC)
			e, okE := g.Endpoint(p.Endpoint)
			if !okN || !okE || e.Kind != model.EndpointGPU {
				continue
			}
			if e.RootPort == n.RootPort || !localHealthy[p.RNIC] {
				continue
			}
			aff := affinitiveRNIC(g, e)
			am, okAff := pm[pathKey(aff, e.ID)]
			if aff == "" || !okAff || am.Status != model.PathNormal {
				continue
			}
			m, ok := pm[pathKey(p.RNIC, p.Endpoint)]
			if ok && m.Status == model.PathAbnormal {
				crossGPUAbn = true
				trans = append(trans, pathKey(p.RNIC, p.Endpoint))
			}
		}
		// #5: isolated RP, healthy P2P, and either a healthy dest GPU fails
		// in transit or memory on the socket is still normal.
		if !localOK || (!crossGPUAbn && memAbn) {
			continue
		}
		ev := fmt.Sprintf("P2P under %s is healthy; cross-root GPU paths fail (%s)", l.ID, joinLimited(trans, 4))
		if !crossGPUAbn {
			// Fired on the memory branch: no cross-root GPU path is the
			// evidence, the abnormal paths through the root port are.
			var failing []string
			for _, p := range g.Paths {
				if !p.ContainsLink(l.ID) {
					continue
				}
				if m, ok := pm[pathKey(p.RNIC, p.Endpoint)]; ok && m.Status == model.PathAbnormal {
					failing = append(failing, pathKey(p.RNIC, p.Endpoint))
				}
			}
			ev = fmt.Sprintf("P2P under %s is healthy and no memory link on socket %d is abnormal; paths through it fail (%s)", l.ID, l.Socket, joinLimited(failing, 4))
		}
		out = append(out, model.RootCause{
			Code: model.CauseCPURootPortFailure, Summary: "CPU root port bandwidth degraded for transit traffic",
			Links: []string{l.ID}, Confidence: model.ConfidenceHigh,
			Evidence: ev,
		})
	}
	return out
}

// localGPUsHealthyByRNIC is rnicLocalGPUsHealthy decided once per RNIC ID
// (the first RNIC of that ID, as g.RNIC resolves it): it reads only the RNIC
// and the paths, so diagnoseRootPorts need not decide it once per path.
func localGPUsHealthyByRNIC(g topology.Graph, pm map[string]model.PathMeasurement) map[string]bool {
	out := map[string]bool{}
	for _, n := range g.RNICs {
		if _, ok := out[n.ID]; !ok {
			out[n.ID] = rnicLocalGPUsHealthy(g, n, pm)
		}
	}
	return out
}

func rnicLocalGPUsHealthy(g topology.Graph, n model.RNIC, pm map[string]model.PathMeasurement) bool {
	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointGPU || e.RootPort != n.RootPort {
			continue
		}
		m, ok := pm[pathKey(n.ID, e.ID)]
		if !ok || m.Status != model.PathNormal {
			return false
		}
	}
	return true
}

func diagnoseMutual(in Input) []model.RootCause {
	out := diagnoseTimeouts(in)
	out = append(out, diagnoseDrops(in)...)
	return dedupeCauses(out)
}

// diagnoseDrops reports each dropping RNIC once. A hardware discard/OOB delta
// is already per RNIC, so it never repeats per mutual pair. Probe loss is only
// known per pair, so it is attributed with dropSuspects: one faulty RNIC shows
// up on every pair it is probed over, and reporting the pairs as evidence keeps
// that visible without emitting the same RNIC once per peer.
func diagnoseDrops(in Input) []model.RootCause {
	pairsOf := map[string][]string{}
	for _, m := range in.Mutual {
		if !m.Connected {
			continue
		}
		label := m.Src + " ↔ " + m.Dst
		pairsOf[m.Src] = append(pairsOf[m.Src], label)
		pairsOf[m.Dst] = append(pairsOf[m.Dst], label)
	}

	var out []model.RootCause
	counted := map[string]bool{}
	ids := make([]string, 0, len(in.DropDelta))
	for id := range in.DropDelta {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := in.DropDelta[id]
		if d == 0 {
			continue
		}
		counted[id] = true
		ev := fmt.Sprintf("%s discard/OOB delta=%d", id, d)
		if p := pairsOf[id]; len(p) > 0 {
			ev += fmt.Sprintf(" on %s (RC loss_ratio ignored)", strings.Join(p, ", "))
		}
		out = append(out, model.RootCause{
			Code: model.CauseRNICPacketDrops, Summary: "packet drops on RNIC hardware counters",
			RNICs: []string{id}, Confidence: model.ConfidenceHigh,
			Evidence: ev,
		})
	}

	// Pairs whose counter drops are not already explained by a per-RNIC delta,
	// and unreliable-probe loss. RC loss_ratio is never drop evidence.
	var counterPairs, lossPairs []model.MutualMeasurement
	for _, m := range in.Mutual {
		if !m.Connected || counted[m.Src] || counted[m.Dst] {
			continue
		}
		switch {
		case m.Reliable && m.CounterDrops > 0:
			counterPairs = append(counterPairs, m)
		case !m.Reliable && m.LossRatio > 0:
			lossPairs = append(lossPairs, m)
		}
	}
	out = append(out, dropCauses(counterPairs, "packet drops on RNIC hardware counters",
		func(m model.MutualMeasurement) string {
			return fmt.Sprintf("%s ↔ %s discard/OOB delta=%d", m.Src, m.Dst, m.CounterDrops)
		})...)
	out = append(out, dropCauses(lossPairs, "packet drops on RNIC mutual probe",
		func(m model.MutualMeasurement) string {
			return fmt.Sprintf("%s → %s loss=%.4f rtt_p50=%dns", m.Src, m.Dst, m.LossRatio, m.RTTnsP50)
		})...)
	return out
}

// dropCauses emits one cause per suspected RNIC, or one per pair when no RNIC
// is common to a group.
func dropCauses(pairs []model.MutualMeasurement, summary string, evidence func(model.MutualMeasurement) string) []model.RootCause {
	var out []model.RootCause
	for _, g := range dropSuspects(pairs) {
		ev := make([]string, 0, len(g.pairs))
		for _, m := range g.pairs {
			ev = append(ev, evidence(m))
		}
		if len(g.rnics) == 0 {
			// Nothing is common to the group (a lossy triangle, say), so no
			// single RNIC explains it: report the pairs as before.
			for i, m := range g.pairs {
				out = append(out, model.RootCause{
					Code: model.CauseRNICPacketDrops, Summary: summary,
					RNICs: []string{m.Src, m.Dst}, Confidence: model.ConfidenceHigh,
					Evidence: ev[i],
				})
			}
			continue
		}
		out = append(out, model.RootCause{
			Code: model.CauseRNICPacketDrops, Summary: summary,
			RNICs: g.rnics, Confidence: model.ConfidenceHigh,
			Evidence: strings.Join(ev, "; "),
		})
	}
	return out
}

// unionFind is a disjoint-set forest over ids, with path compression. An id
// it has not seen is a set of its own.
type unionFind map[string]string

// find is the root of x's set.
func (u unionFind) find(x string) string {
	p, ok := u[x]
	if !ok {
		u[x] = x
		return x
	}
	if p != x {
		u[x] = u.find(p)
	}
	return u[x]
}

// union joins the sets of a and b under b's root.
func (u unionFind) union(a, b string) {
	if ra, rb := u.find(a), u.find(b); ra != rb {
		u[ra] = rb
	}
}

// dropGroup is one connected group of dropping pairs and the RNICs that take
// part in every pair of it.
type dropGroup struct {
	rnics []string
	pairs []model.MutualMeasurement
}

// dropSuspects splits dropping pairs into connected groups and names, per
// group, the RNICs present in every pair of that group. A single faulty RNIC
// yields exactly one suspect; a single dropping pair names both of its RNICs,
// because the probe cannot tell which side dropped.
func dropSuspects(pairs []model.MutualMeasurement) []dropGroup {
	sets := unionFind{}
	for _, m := range pairs {
		sets.union(m.Src, m.Dst)
	}
	var order []string
	byRoot := map[string][]model.MutualMeasurement{}
	for _, m := range pairs {
		r := sets.find(m.Src)
		if _, ok := byRoot[r]; !ok {
			order = append(order, r)
		}
		byRoot[r] = append(byRoot[r], m)
	}
	out := make([]dropGroup, 0, len(order))
	for _, r := range order {
		ms := byRoot[r]
		seen := map[string]int{}
		for _, m := range ms {
			seen[m.Src]++
			if m.Dst != m.Src {
				seen[m.Dst]++
			}
		}
		var common []string
		for id, n := range seen {
			if n == len(ms) {
				common = append(common, id)
			}
		}
		sort.Strings(common)
		out = append(out, dropGroup{rnics: common, pairs: ms})
	}
	return out
}

func affinitiveRNIC(g topology.Graph, e model.Endpoint) string {
	for _, n := range g.RNICs {
		if n.RootPort == e.RootPort {
			return n.ID
		}
	}
	return ""
}

// assumedLineRateBPS stands in for an unknown RNIC line rate, matching the
// 200G default baseline.Defaults uses.
const assumedLineRateBPS = 200_000_000_000

// defaultPCIeWidth is what an unreadable current_link_width is judged at: a
// full-width slot, so an unknown width never invents a down-train.
const defaultPCIeWidth = 16

// healthyPCIeSpeed reports whether a negotiated PCIe link can still carry the
// RNIC line rate. Both halves of the link matter: a Gen4 x4 link is slower than
// a Gen2 x16 one, so speed and width become one effective bandwidth and are
// compared against the line rate. MaxSpeed/MaxWidth deliberately do not decide
// the verdict: sysfs reports the device's capability, not the slot's, so a Gen5
// x16 card in a Gen4 or physically x8 slot negotiates below its maximum without
// being faulty. Whether the link trained below its maximum is judged
// separately, by rnicPCIeTraining and diagnosePCIeDownTrain.
func healthyPCIeSpeed(c model.PCIeConfig, lineRate uint64) bool {
	gts := pci.GTs(c.Speed)
	if gts == 0 {
		return true // "Unknown" or unparsable: no evidence either way
	}
	width := c.Width
	if width <= 0 {
		width = defaultPCIeWidth
	}
	need := lineRate
	if need == 0 {
		need = assumedLineRateBPS
	}
	return pci.EffectiveBPS(gts, width) >= need
}

// pcieTraining is how a negotiated link compares with the maxima config space
// reports for it.
type pcieTraining int

const (
	// pcieTrainingUnknown: no half is below a known maximum, and at least one
	// half has no readable negotiated value or maximum.
	pcieTrainingUnknown pcieTraining = iota
	// pcieAtMax: both halves are readable and at their known maxima.
	pcieAtMax
	// pcieDownTrained: speed or width is below a known maximum.
	pcieDownTrained
)

// rnicPCIeTraining compares each half of an RNIC's negotiated link with the
// lower of the device and upstream port maxima, using whichever of the two is
// readable. It runs only where every affinitive path of the RNIC is already
// abnormal (or, busy, uncertain), so a link below the device's own maximum is
// taken as down-trained even when the port maximum is unread; the config-space
// rule in diagnosePCIeDownTrain, which has no symptom to lean on, needs both.
// "At max" is safe with one maximum only: a link never trains above either end.
func rnicPCIeTraining(c model.PCIeConfig) pcieTraining {
	speed, speedMax := pci.GTs(c.Speed), knownMin(pci.GTs(c.MaxSpeed), pci.GTs(c.PortMaxSpeed))
	width, widthMax := c.Width, knownMin(c.MaxWidth, c.PortMaxWidth)
	if (speed > 0 && speedMax > 0 && speed < speedMax) || (width > 0 && widthMax > 0 && width < widthMax) {
		return pcieDownTrained
	}
	if speed > 0 && speedMax > 0 && width > 0 && widthMax > 0 {
		return pcieAtMax
	}
	return pcieTrainingUnknown
}

// knownMin is the smaller of a and b, ignoring an unread (zero) value.
func knownMin[T int | float64](a, b T) T {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

// rnicPCIeDegraded is the PCIe evidence behind a high rnic_pcie_failure from
// the bandwidth rules: the link trained below its known maximum and what it
// negotiated no longer carries the line rate. A link at its maximum that is
// narrower than the line rate is a slot that is too small by design, reported
// once as the advisory rnic_pcie_undersized; a link whose maxima are unread
// shows no down-train, so the bandwidth cap alone stays medium.
func rnicPCIeDegraded(c model.PCIeConfig, lineRate uint64) bool {
	return c.Speed != "" && !healthyPCIeSpeed(c, lineRate) && rnicPCIeTraining(c) == pcieDownTrained
}

// noPCIeEvidence says why config space does not back a PCIe verdict: the
// negotiated link was unread, it still carries the line rate, it is at its
// maximum, or its maxima are unread.
func noPCIeEvidence(c model.PCIeConfig, lineRate uint64) string {
	if pci.GTs(c.Speed) == 0 {
		return "negotiated PCIe link unknown"
	}
	if !healthyPCIeSpeed(c, lineRate) {
		switch rnicPCIeTraining(c) {
		case pcieAtMax:
			return fmt.Sprintf("PCIe %s is at its maximum, below the line rate by design, not down-trained", pcieEvidence(c))
		case pcieTrainingUnknown:
			return fmt.Sprintf("PCIe %s cannot carry the line rate, but its maximum is unread, so no down-train is shown", pcieEvidence(c))
		}
	}
	return fmt.Sprintf("PCIe config space shows no degradation (%s)", pcieEvidence(c))
}

// pcieEvidence renders the negotiated link plus the device and upstream port
// maxima when known.
func pcieEvidence(c model.PCIeConfig) string {
	s := fmt.Sprintf("%s x%d", c.Speed, c.Width)
	var max []string
	if m := linkMax(c.MaxSpeed, c.MaxWidth); m != "" {
		max = append(max, "max "+m)
	}
	if m := linkMax(c.PortMaxSpeed, c.PortMaxWidth); m != "" {
		max = append(max, "port max "+m)
	}
	if len(max) > 0 {
		s += " (" + strings.Join(max, ", ") + ")"
	}
	return s
}

func linkMax(speed string, width int) string {
	switch {
	case speed != "" && width > 0:
		return fmt.Sprintf("%s x%d", speed, width)
	case width > 0:
		return fmt.Sprintf("x%d", width)
	default:
		return speed
	}
}

// diagnoseBusyEqualRNIC is NSDI'23 §4.3: on a PFC/drop-triggered host, sibling
// classification leaves an equally-throttled RNIC entirely uncertain. If
// tomography found no abnormal hop, the RNIC PCIe link may still be the bottleneck.
func diagnoseBusyEqualRNIC(in Input, paths []model.PathMeasurement, st map[string]model.LinkState, already []model.RootCause) []model.RootCause {
	if in.Mode != model.ModeBusy {
		return nil
	}
	if anyLinkAbnormal(st) {
		return nil
	}
	pm := pathMap(paths)
	var out []model.RootCause
	for _, n := range in.Graph.RNICs {
		if !in.BusyRNICs[n.ID] {
			continue
		}
		if hasCauseFor(already, "", n.ID, "") {
			continue
		}
		aff := in.Graph.AffinitiveEndpoints(n.ID)
		if len(aff) == 0 {
			continue
		}
		// Do not re-apply the idle bandwidth baseline (banned for loaded RNICs).
		// Only util, negotiated PCIe, or device-config hints may name a cause.
		if !allAffinitivePresentUncertain(aff, n.ID, pm) {
			continue
		}
		pcieID := model.PCIeLinkID(n.ID)
		ls := st[pcieID]
		cfg := in.PCIe[n.ID]
		switch {
		case utilAtLeast(ls, in.Cfg.UtilHigh):
			out = append(out, model.RootCause{
				Code: model.CauseOverloadedRNICPCIe, Summary: "RNIC PCIe link overloaded (loopback contention)",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: model.ConfidenceHigh,
				Evidence: fmt.Sprintf("busy equal-throttle fallback; %s", utilEvidence(ls)),
			})
		case in.RNICHints[n.ID] == model.HintTxWindow:
			out = append(out, model.RootCause{
				Code: model.CauseRNICTxWindow, Summary: "RNIC Tx window too small",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: model.ConfidenceHigh,
				Evidence: "busy equal-throttle fallback with Tx window configuration",
			})
		case in.RNICHints[n.ID] == model.HintSlowStart:
			out = append(out, model.RootCause{
				Code: model.CauseRNICSlowStart, Summary: "RNIC-wide bandwidth cap (slow start)",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: model.ConfidenceMedium,
				Evidence: "busy equal-throttle fallback with slow-start configuration",
			})
		case rnicPCIeDegraded(cfg, n.LineRate):
			out = append(out, model.RootCause{
				Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link failed or down-trained",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: model.ConfidenceHigh,
				Evidence: fmt.Sprintf("busy equal-throttle fallback; PCIe %s trained below its maximum", pcieEvidence(cfg)),
			})
		}
	}
	return out
}

// allAffinitivePresentUncertain reports whether the RNIC has a measured
// affinitive path and none of them is abnormal.
func allAffinitivePresentUncertain(aff []model.Endpoint, rnic string, pm map[string]model.PathMeasurement) bool {
	eps, abnormal := measuredAffinitive(aff, rnic, pm)
	return len(eps) > 0 && abnormal == 0
}

// measuredAffinitive lists, in aff order, the affinitive endpoints of rnic
// whose path was measured, and how many of those paths are abnormal. An
// endpoint with no measurement (skipped: NoGDR / no device) is left out,
// because it is not evidence of health.
func measuredAffinitive(aff []model.Endpoint, rnic string, pm map[string]model.PathMeasurement) (eps []string, abnormal int) {
	for _, e := range aff {
		m, ok := pm[pathKey(rnic, e.ID)]
		if !ok {
			continue
		}
		eps = append(eps, e.ID)
		if m.Status == model.PathAbnormal {
			abnormal++
		}
	}
	return eps, abnormal
}

func anyLinkAbnormal(st map[string]model.LinkState) bool {
	for _, ls := range st {
		if ls.Status == model.LinkAbnormal {
			return true
		}
	}
	return false
}

// hasCauseFor reports whether causes already name rnic (or endpoint) under
// code, or under any code when code is "".
func hasCauseFor(causes []model.RootCause, code, rnic, endpoint string) bool {
	for _, c := range causes {
		if code != "" && c.Code != code {
			continue
		}
		for _, id := range c.RNICs {
			if rnic != "" && id == rnic {
				return true
			}
		}
		for _, id := range c.Endpoints {
			if endpoint != "" && id == endpoint {
				return true
			}
		}
	}
	return false
}

func acsEnabledOnPath(pcie map[string]model.PCIeConfig, n model.RNIC, e model.Endpoint) bool {
	for _, key := range []string{e.ID, n.ID, e.RootPort, n.RootPort, e.Switch, n.Switch} {
		if key == "" {
			continue
		}
		if c, ok := pcie[key]; ok && c.ACSKnown && c.ACSEnabled {
			return true
		}
	}
	return false
}

func anyAbnormalPath(ms []model.PathMeasurement) bool {
	for _, m := range ms {
		if m.Status == model.PathAbnormal {
			return true
		}
	}
	return false
}

func joinLimited(s []string, n int) string {
	if len(s) <= n {
		return strings.Join(s, ", ")
	}
	return strings.Join(s[:n], ", ") + ", …"
}

func dedupeCauses(in []model.RootCause) []model.RootCause {
	seen := map[string]struct{}{}
	var out []model.RootCause
	for _, c := range in {
		key := causeKey(c)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}
	return out
}

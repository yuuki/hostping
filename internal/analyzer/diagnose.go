package analyzer

import (
	"fmt"
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
	if in.BusyRNICs == nil {
		in.BusyRNICs = map[string]bool{}
	}
	if in.Util == nil {
		in.Util = map[string]float64{}
	}
	if in.GrayHits == nil {
		in.GrayHits = map[string]int{}
	}
	if in.DropDelta == nil {
		in.DropDelta = map[string]uint64{}
	}

	if in.RNICHints == nil {
		in.RNICHints = map[string]string{}
	}

	paths := ClassifyPaths(in.Graph, in.Measurements, in.Mode, in.BusyRNICs, in.Cfg, in.Bases)
	// Applied before anything reads the bandwidth verdicts, so the links,
	// gray streaks and causes all see the confirmed ones only. The two touch
	// disjoint paths: idle-judged ones, and those of a loaded RNIC.
	paths = applyBWRecheck(in.Graph, paths, in.Mode, in.BusyRNICs, in.Bases, in.Cfg.AbnormalTh, in.BWRecheck)
	paths = applyBusyRecheck(paths, in.Mode, in.BusyRNICs, in.BusyRecheck)
	// After both rechecks, which confirm a drop that a constant CPU load
	// causes, and before anything reads the bandwidth verdicts.
	paths = applyCPUBusy(in.Graph, paths, in.CPUBusySockets)
	// Latency is judged on its own and never reaches Algorithm 1 (latency.go).
	// The order is the sweep verdict, the paired recheck, the locality shift,
	// the cap marks, then the streaks. PlanLatencyRecheck pre-applied the
	// locality shift to the sweep verdicts only to keep paths it clears off
	// the cap; it clears them again here, after the recheck.
	paths = ClassifyLatency(in.Graph, paths, in.Mode, in.Cfg, in.Bases)
	paths = applyLatencyRecheck(paths, in.LatencyRecheck)
	paths = applyLocalityShift(in.Graph, paths, in.Bases, in.Cfg)
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
	// high, medium, low — rank explicitly. Ties break on the cause's identity
	// fields so the order never depends on map iteration.
	rank := map[model.Confidence]int{model.ConfidenceHigh: 0, model.ConfidenceMedium: 1, model.ConfidenceLow: 2}
	subject := func(c model.RootCause) string {
		return strings.Join(c.RNICs, ",") + "\x00" +
			strings.Join(c.Links, ",") + "\x00" +
			strings.Join(c.Endpoints, ",")
	}
	sort.Slice(causes, func(i, j int) bool {
		ri, rj := rank[causes[i].Confidence], rank[causes[j].Confidence]
		if ri != rj {
			return ri < rj
		}
		if causes[i].Code != causes[j].Code {
			return causes[i].Code < causes[j].Code
		}
		return subject(causes[i]) < subject(causes[j])
	})

	return Result{Paths: paths, Links: links, Causes: causes, GrayHits: gray, SlowMRHits: mrHits, LatencyHits: latHits}
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
			m, ok := pm[n.ID+"->"+e.ID]
			if !ok || m.Status != model.PathAbnormal {
				continue
			}
			nb := baseline.ForPath(in.Bases.Effective(n), model.DistanceSameSwitch)
			if !latencyHigh(m.SmallLatNsP50, nb.LatencyNs, in.Cfg.AbnormalTh) {
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
			Endpoints: unique(acsEps), Confidence: "high",
			Evidence: fmt.Sprintf("%d affinitive GPU paths have high latency with ACS enabled", acsHits),
		})
	}
	for rnic, eps := range atsByRNIC {
		out = append(out, model.RootCause{
			Code: model.CauseATSDisabled, Summary: "ATS disabled; GDR address translation hairpins to the CPU",
			RNICs: []string{rnic}, Endpoints: unique(eps), Confidence: "high",
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

func diagnose(in Input, paths []model.PathMeasurement, links []model.LinkState, gray map[string]int) []model.RootCause {
	st := linkMap(links)
	pm := pathMap(paths)
	g := in.Graph
	var causes []model.RootCause
	causes = append(causes, diagnoseMisconfig(in, paths)...)

	for _, n := range g.RNICs {
		aff := g.AffinitiveEndpoints(n.ID)
		if len(aff) == 0 {
			continue
		}
		allAbn := true
		seen := 0
		var eps []string
		for _, e := range aff {
			m, ok := pm[n.ID+"->"+e.ID]
			if !ok {
				continue // skipped (NoGDR / no device); not evidence of health
			}
			seen++
			if m.Status != model.PathAbnormal {
				allAbn = false
				break
			}
			eps = append(eps, e.ID)
		}
		if !allAbn || seen == 0 {
			continue
		}
		pcieID := "pcie." + n.ID
		ls := st[pcieID]
		cfg := in.PCIe[n.ID]
		switch {
		case ls.UtilAtLeast(in.Cfg.UtilHigh):
			causes = append(causes, model.RootCause{
				Code: model.CauseOverloadedRNICPCIe, Summary: "RNIC PCIe link overloaded (loopback contention)",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("all affinitive paths abnormal; %s", ls.UtilEvidence()),
			})
		case rnicPCIeDegraded(cfg, n.LineRate):
			causes = append(causes, model.RootCause{
				Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link failed or down-trained",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("PCIe %s trained below its maximum; all affinitive endpoints abnormal", pcieEvidence(cfg)),
			})
		case in.RNICHints[n.ID] == "tx_window":
			causes = append(causes, model.RootCause{
				Code: model.CauseRNICTxWindow, Summary: "RNIC Tx window too small",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Endpoints: eps, Confidence: "high",
				Evidence: "RNIC-wide bandwidth cap with Tx window configuration",
			})
		case in.RNICHints[n.ID] == "slow_start":
			causes = append(causes, model.RootCause{
				Code: model.CauseRNICSlowStart, Summary: "RNIC-wide bandwidth cap (slow start or small Tx window)",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Endpoints: eps, Confidence: "medium",
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
				Links: []string{pcieID}, RNICs: []string{n.ID}, Endpoints: eps, Confidence: "medium",
				Evidence: "all affinitive paths from this RNIC are abnormal; " + noPCIeEvidence(cfg, n.LineRate) +
					"; an RNIC-wide throttle (slow start / Tx window, NSDI'23 #9/#10) was not ruled out",
			})
		}
	}

	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointGPU {
			continue
		}
		id := "pcie." + e.ID
		ls := st[id]
		if ls.Status != model.LinkAbnormal {
			continue
		}
		affRNIC := affinitiveRNIC(g, e)
		latAbn := false
		if affRNIC != "" {
			if m, ok := pm[affRNIC+"->"+e.ID]; ok {
				n, _ := g.RNIC(affRNIC)
				base := baseline.ForPath(in.Bases.Effective(n), model.DistanceSameSwitch)
				latAbn = latencyHigh(m.SmallLatNsP50, base.LatencyNs, in.Cfg.AbnormalTh)
			}
		}
		if latAbn {
			continue // ACS/ATS handled by diagnoseMisconfig
		}
		switch {
		case ls.UtilAtLeast(in.Cfg.UtilHigh):
			causes = append(causes, model.RootCause{
				Code: model.CauseGPUPCIeFailure, Summary: "GPU PCIe link overloaded",
				Links: []string{id}, Endpoints: []string{e.ID}, Confidence: "medium",
				Evidence: ls.UtilEvidence(),
			})
		default:
			causes = append(causes, model.RootCause{
				Code: model.CauseGPUPCIeFailure, Summary: "GPU PCIe link failed",
				Links: []string{id}, Endpoints: []string{e.ID}, Confidence: "high",
				Evidence: "GPU PCIe hop abnormal without a matching latency increase",
			})
		}
	}
	causes = append(causes, diagnoseGPUFromSiblings(g, pm, st, in)...)

	for _, l := range g.LinksByKind("memory") {
		ls := st[l.ID]
		if flapping(ls, gray, in.Cfg.FlapWindow) {
			causes = append(causes, model.RootCause{
				Code: model.CauseMemoryChannelFlapping, Summary: "memory channel flapping",
				Links: []string{l.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("gray in %d consecutive loopback tests", gray[l.ID]),
			})
			continue
		}
		// NSDI'23 §4.3 / ToN'24 §IV-C: utilization only explains a link that
		// tomography already found abnormal; a busy but healthy bus is no cause.
		if ls.Status != model.LinkAbnormal {
			continue
		}
		if ls.UtilAtLeast(in.Cfg.UtilHigh) {
			causes = append(causes, model.RootCause{
				Code: model.CauseOverloadedMemory, Summary: "memory channel overloaded",
				Links: []string{l.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("abnormal_cnt=%d %s", ls.AbnormalCount, ls.UtilEvidence()),
			})
			continue
		}
		causes = append(causes, model.RootCause{
			Code: model.CauseMemoryChannelFailure, Summary: "memory channel failed",
			Links: []string{l.ID}, Confidence: "high",
			Evidence: fmt.Sprintf("abnormal_cnt=%d %s", ls.AbnormalCount, ls.UtilEvidence()),
		})
	}

	// Only upi.* links ever carry the UPI causes; a fault localized to the
	// package mesh of a sub-NUMA-clustered host is reported below instead.
	for _, l := range g.LinksByKind(model.LinkKindUPI) {
		ls := st[l.ID]
		if flapping(ls, gray, in.Cfg.FlapWindow) {
			causes = append(causes, model.RootCause{
				Code: model.CauseUPIFailure, Summary: "inter-socket bus flapping",
				Links: []string{l.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("gray in %d consecutive loopback tests", gray[l.ID]),
			})
			continue
		}
		// NSDI'23 §4.3 / ToN'24 §IV-C: utilization only explains a link that
		// tomography already found abnormal; a busy but healthy bus is no cause.
		if ls.Status != model.LinkAbnormal {
			continue
		}
		if ls.UtilAtLeast(in.Cfg.UtilHigh) {
			causes = append(causes, model.RootCause{
				Code: model.CauseOverloadedUPI, Summary: "inter-socket bus overloaded",
				Links: []string{l.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("abnormal_cnt=%d %s", ls.AbnormalCount, ls.UtilEvidence()),
			})
			continue
		}
		causes = append(causes, model.RootCause{
			Code: model.CauseUPIFailure, Summary: "inter-socket bus (UPI/xGMI) failed",
			Links: []string{l.ID}, Confidence: "high",
			Evidence: fmt.Sprintf("status=%s abnormal_cnt=%d", ls.Status, ls.AbnormalCount),
		})
	}

	// The fabric between two NUMA nodes of one package (sub-NUMA
	// clustering), judged by the upi_failure rules. No source reads its
	// utilization, so an abnormal mesh link is never explained as load.
	for _, l := range g.LinksByKind(model.LinkKindSNCMesh) {
		ls := st[l.ID]
		if flapping(ls, gray, in.Cfg.FlapWindow) {
			causes = append(causes, model.RootCause{
				Code: model.CauseSNCMeshFailure, Summary: "intra-package mesh between NUMA nodes flapping",
				Links: []string{l.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("gray in %d consecutive loopback tests", gray[l.ID]),
			})
			continue
		}
		if ls.Status != model.LinkAbnormal {
			continue
		}
		causes = append(causes, model.RootCause{
			Code: model.CauseSNCMeshFailure, Summary: "intra-package mesh between NUMA nodes (sub-NUMA clustering) failed",
			Links: []string{l.ID}, Confidence: "high",
			Evidence: fmt.Sprintf("status=%s abnormal_cnt=%d %s", ls.Status, ls.AbnormalCount, ls.UtilEvidence()),
		})
	}

	causes = append(causes, diagnoseRootPorts(g, paths, st, in)...)
	causes = append(causes, diagnoseBusyEqualRNIC(in, paths, st, causes)...)

	if len(causes) == 0 {
		if anyAbnormalPath(paths) {
			causes = append(causes, model.RootCause{
				Code: model.CauseUnknown, Summary: "abnormal paths without a unique hop",
				Confidence: "low", Evidence: "tomography left only gray/uncertain links",
			})
		}
	}
	return dedupeCauses(causes)
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
		id := "pcie." + e.ID
		if st[id].Status == model.LinkAbnormal {
			continue
		}
		aff := affinitiveRNIC(g, e)
		if aff == "" {
			continue
		}
		m, ok := pm[aff+"->"+e.ID]
		if !ok || m.Status != model.PathAbnormal {
			continue
		}
		base := baseline.ForPath(in.Bases.Effective(mustRNIC(g, aff)), model.DistanceSameSwitch)
		if latencyHigh(m.SmallLatNsP50, base.LatencyNs, in.Cfg.AbnormalTh) {
			continue
		}
		siblingOK := false
		for _, sib := range g.AffinitiveEndpoints(aff) {
			if sib.ID == e.ID {
				continue
			}
			sm, ok := pm[aff+"->"+sib.ID]
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
			Links: []string{id}, Endpoints: []string{e.ID}, Confidence: "high",
			Evidence: "affinitive GPU path abnormal while sibling affinitive paths are not",
		})
	}
	return out
}

func mustRNIC(g topology.Graph, id string) model.RNIC {
	n, _ := g.RNIC(id)
	return n
}

func diagnoseRootPorts(g topology.Graph, paths []model.PathMeasurement, st map[string]model.LinkState, in Input) []model.RootCause {
	pm := pathMap(paths)
	var out []model.RootCause
	for _, l := range g.LinksByKind("root_port") {
		// Gray sibling RPs are Algorithm 1 conflicts, not isolated #5 faults.
		if st[l.ID].Status != model.LinkAbnormal {
			continue
		}
		localOK := true
		crossGPUAbn := false
		var trans []string
		for _, n := range g.RNICs {
			if n.RootPort != l.ID {
				continue
			}
			if !rnicLocalGPUsHealthy(g, n, pm) {
				localOK = false
			}
		}
		memAbn := false
		for _, ml := range g.LinksByKind("memory") {
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
			if e.RootPort == n.RootPort {
				continue
			}
			if !rnicLocalGPUsHealthy(g, n, pm) {
				continue
			}
			aff := affinitiveRNIC(g, e)
			am, okAff := pm[aff+"->"+e.ID]
			if aff == "" || !okAff || am.Status != model.PathNormal {
				continue
			}
			m, ok := pm[p.RNIC+"->"+p.Endpoint]
			if ok && m.Status == model.PathAbnormal {
				crossGPUAbn = true
				trans = append(trans, p.RNIC+"->"+p.Endpoint)
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
				if m, ok := pm[p.RNIC+"->"+p.Endpoint]; ok && m.Status == model.PathAbnormal {
					failing = append(failing, p.RNIC+"->"+p.Endpoint)
				}
			}
			ev = fmt.Sprintf("P2P under %s is healthy and no memory link on socket %d is abnormal; paths through it fail (%s)", l.ID, l.Socket, joinLimited(failing, 4))
		}
		out = append(out, model.RootCause{
			Code: model.CauseCPURootPortFailure, Summary: "CPU root port bandwidth degraded for transit traffic",
			Links: []string{l.ID}, Confidence: "high",
			Evidence: ev,
		})
	}
	return out
}

func rnicLocalGPUsHealthy(g topology.Graph, n model.RNIC, pm map[string]model.PathMeasurement) bool {
	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointGPU || e.RootPort != n.RootPort {
			continue
		}
		m, ok := pm[n.ID+"->"+e.ID]
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
			RNICs: []string{id}, Confidence: "high",
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
					RNICs: []string{m.Src, m.Dst}, Confidence: "high",
					Evidence: ev[i],
				})
			}
			continue
		}
		out = append(out, model.RootCause{
			Code: model.CauseRNICPacketDrops, Summary: summary,
			RNICs: g.rnics, Confidence: "high",
			Evidence: strings.Join(ev, "; "),
		})
	}
	return out
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
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		p, ok := parent[x]
		if !ok {
			parent[x] = x
			return x
		}
		if p != x {
			parent[x] = find(p)
		}
		return parent[x]
	}
	for _, m := range pairs {
		a, b := find(m.Src), find(m.Dst)
		if a != b {
			parent[a] = b
		}
	}
	var order []string
	byRoot := map[string][]model.MutualMeasurement{}
	for _, m := range pairs {
		r := find(m.Src)
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
		if hasRNICCause(already, n.ID) {
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
		pcieID := "pcie." + n.ID
		ls := st[pcieID]
		cfg := in.PCIe[n.ID]
		switch {
		case ls.UtilAtLeast(in.Cfg.UtilHigh):
			out = append(out, model.RootCause{
				Code: model.CauseOverloadedRNICPCIe, Summary: "RNIC PCIe link overloaded (loopback contention)",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("busy equal-throttle fallback; %s", ls.UtilEvidence()),
			})
		case in.RNICHints[n.ID] == "tx_window":
			out = append(out, model.RootCause{
				Code: model.CauseRNICTxWindow, Summary: "RNIC Tx window too small",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: "high",
				Evidence: "busy equal-throttle fallback with Tx window configuration",
			})
		case in.RNICHints[n.ID] == "slow_start":
			out = append(out, model.RootCause{
				Code: model.CauseRNICSlowStart, Summary: "RNIC-wide bandwidth cap (slow start)",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: "medium",
				Evidence: "busy equal-throttle fallback with slow-start configuration",
			})
		case rnicPCIeDegraded(cfg, n.LineRate):
			out = append(out, model.RootCause{
				Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link failed or down-trained",
				Links: []string{pcieID}, RNICs: []string{n.ID}, Confidence: "high",
				Evidence: fmt.Sprintf("busy equal-throttle fallback; PCIe %s trained below its maximum", pcieEvidence(cfg)),
			})
		}
	}
	return out
}

func allAffinitivePresentUncertain(aff []model.Endpoint, rnic string, pm map[string]model.PathMeasurement) bool {
	if len(aff) == 0 {
		return false
	}
	seen := 0
	for _, e := range aff {
		m, ok := pm[rnic+"->"+e.ID]
		if !ok {
			continue
		}
		seen++
		if m.Status == model.PathAbnormal {
			return false
		}
	}
	return seen > 0
}

func anyLinkAbnormal(st map[string]model.LinkState) bool {
	for _, ls := range st {
		if ls.Status == model.LinkAbnormal {
			return true
		}
	}
	return false
}

func hasRNICCause(causes []model.RootCause, rnic string) bool {
	for _, c := range causes {
		for _, id := range c.RNICs {
			if id == rnic {
				return true
			}
		}
	}
	return false
}

func acsEnabledOnPath(pcie map[string]model.PCIeConfig, n model.RNIC, e model.Endpoint) bool {
	if pcie == nil {
		return false
	}
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
		key := c.Code + "|" + strings.Join(c.Links, ",") + "|" + strings.Join(c.RNICs, ",") + "|" + strings.Join(c.Endpoints, ",")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}
	return out
}

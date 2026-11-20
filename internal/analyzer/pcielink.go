package analyzer

import (
	"slices"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// Passive evidence against a PCIe link: what the hardware monitor read
// without sending a probe (--pcie-interval, monitor.PCIeWatch).
//
// It comes in two kinds, and each stays what it is. A link that trained
// below what both of its ends can do is judged by the down-train rules of
// diagnosePCIeDownTrain, the same ones an engine run applies; PassiveCauses
// only applies them between runs. Link errors the AER counters of the two
// ends count are a cause of their own, pcie_link_errors: an error rate over
// the link's bit-error budget shows the link is marginal, not that it has
// lost bandwidth, so it never becomes rnic_pcie_failure, gpu_pcie_failure or
// cpu_root_port_failure, never raises their confidence and never changes a
// path or link status. Whether the link is also a bottleneck is what the
// probes say.

// passiveOnlyEvidence replaces the symptom half of a down-train cause's
// evidence when no engine run is behind it.
const passiveOnlyEvidence = "; read without a probe on two consecutive samples (config space only)"

// diagnoseLinkErrors reports each proven passive link-error finding of the
// run as pcie_link_errors. A suspect finding is a hint and names no cause.
//
// The cause is medium whatever the probes measured: the counters are one line
// of evidence, and they show a link that errs, not one that has lost
// bandwidth. When the probes of the same run name the same link, that is
// another cause of the report, with its own confidence; this one only says
// so in its evidence, so the two are read together.
func diagnoseLinkErrors(in Input, already []model.RootCause) []model.RootCause {
	var out []model.RootCause
	for _, f := range in.PCIeLinks {
		if !f.State.Proven() {
			continue
		}
		l, ok := in.Graph.Link(f.Link)
		if !ok {
			continue
		}
		c := linkErrorsCause(l, f)
		if codes := causesNaming(already, l.ID); len(codes) > 0 {
			c.Evidence += "; the same run also names the link (" + strings.Join(codes, ", ") + ")"
		} else {
			c.Evidence += "; the same run names no other cause at the link (counters only)"
		}
		out = append(out, c)
	}
	return out
}

// causesNaming lists the codes of the causes that name link, each once, in
// the order given.
func causesNaming(causes []model.RootCause, link string) []string {
	var codes []string
	for _, c := range causes {
		if slices.Contains(c.Links, link) && !slices.Contains(codes, c.Code) {
			codes = append(codes, c.Code)
		}
	}
	return codes
}

// linkErrorsCause is the pcie_link_errors cause of finding f on link l,
// without the half of its evidence that says what else is known of the
// link.
func linkErrorsCause(l model.Link, f model.PCIeLinkFinding) model.RootCause {
	c := model.RootCause{
		Code:       model.CausePCIeLinkErrors,
		Summary:    "PCIe link counts link errors beyond its bit-error budget",
		Links:      []string{l.ID},
		Confidence: model.ConfidenceMedium,
		Evidence:   "AER: " + f.Evidence(),
	}
	if f.State == model.PCIeLinkUncorrectable {
		c.Summary = "PCIe link reported an uncorrectable link error"
	}
	switch l.Kind {
	case model.LinkKindPCIeRNIC:
		c.RNICs = []string{l.Device}
	case model.LinkKindPCIeGPU:
		c.Endpoints = []string{l.Device}
	}
	return c
}

// PassiveCauses is what the passive reading p proves on its own, with no
// engine run behind it:
//
//   - rnic_pcie_failure and gpu_pcie_failure for an RNIC or GPU link whose
//     negotiated speed and width read the same on two samples and that
//     trained below its capability, by the rules and at the confidence
//     diagnosePCIeDownTrain gives config space alone (medium);
//   - pcie_link_errors for every proven link-error finding (medium).
//
// The advisory rnic_pcie_undersized is left to the engine runs: it describes
// the slot, and nothing about it changes between them. The causes are in
// report order.
func PassiveCauses(g topology.Graph, p model.PCIePassive) []model.RootCause {
	var out []model.RootCause
	none := map[string]model.PathMeasurement{}
	for _, n := range g.RNICs {
		cfg, ok := p.Configs[n.ID]
		if !ok {
			continue
		}
		if c, ok := rnicPCIeDownTrain(g, none, n, cfg, nil); ok {
			out = append(out, passiveOnly(c))
		}
	}
	for _, e := range g.Endpoints {
		cfg, ok := p.Configs[e.ID]
		if !ok || e.Kind != model.EndpointGPU {
			continue
		}
		if c, ok := gpuPCIeDownTrain(g, none, e, cfg, nil, false); ok {
			out = append(out, passiveOnly(c))
		}
	}
	for _, f := range p.Findings {
		if !f.State.Proven() {
			continue
		}
		if l, ok := g.Link(f.Link); ok {
			c := linkErrorsCause(l, f)
			c.Evidence += "; read without a probe (counters only)"
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, compareCauses)
	return out
}

// passiveOnly rewrites the evidence of a config-space-only down-train cause
// for a report without an engine run: no path was measured, so "no affinitive
// path abnormal yet" would claim a measurement that did not happen.
func passiveOnly(c model.RootCause) model.RootCause {
	c.Evidence = strings.TrimSuffix(c.Evidence, symptomEvidence(0)) + passiveOnlyEvidence
	return c
}

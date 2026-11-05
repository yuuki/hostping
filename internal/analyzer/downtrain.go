package analyzer

import (
	"fmt"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/topology"
)

// diagnosePCIeDownTrain reads NSDI'23 failures #1 and #2 (RNIC / GPU PCIe
// link degradation) straight from config space: a link that trained below
// what both of its ends can train to.
//
// The bandwidth rules cannot see every down-train. The default baseline is
// held to the Gen4 x16 bus Fig.4 was measured on (baseline.DerivedRate), so a
// Gen5 x16 slot that trained to Gen4 x16 still carries what the default asks
// for, and the "every affinitive path abnormal" rule never fires. The
// negotiated link is direct evidence that does not depend on those thresholds.
//
// Capability is min(device, upstream port) max_link_*: a Gen5 card in a Gen4
// slot, or an x16 card in a port configured x8, is below its own maximum
// without being faulty. A link whose port maximum is unknown is not judged.
//
// Confidence follows the symptom. Config space alone is medium: sysfs can be
// read in a transient state, and nothing has shown the host is slower for it.
// With an affinitive path from the same device already abnormal the two agree
// and the cause is high.
//
// RNIC and GPU links are read differently because they idle differently:
//
//   - An RNIC link that trained below its capability is a cause only when what
//     it negotiated no longer carries the line rate. A 200G RNIC whose Gen5 slot
//     trained to Gen4 x16 is still not the bottleneck, which is the same line
//     healthyPCIeSpeed draws.
//   - A GPU's link speed is not evidence at all. GPUs drop their link to a low
//     speed when idle to save power and retrain on load, and hostping reads
//     config space on an idle host by design, so a low GPU link speed is the
//     healthy steady state. Link width is not renegotiated that way in
//     ordinary operation, so a GPU link narrower than its capability is
//     reported. Some GPUs can also power down lanes dynamically; that is why
//     width alone stays medium until a path agrees.
//
// It runs after suppressOverlaps and skips a device that already carries the
// matching cause: config-space evidence neither duplicates a bandwidth verdict
// nor suppresses another device's cause. It also reports the advisory
// rnic_pcie_undersized for an RNIC link at its maximum that cannot carry the
// line rate (rnicPCIeUndersized).
func diagnosePCIeDownTrain(in Input, paths []model.PathMeasurement, already []model.RootCause) []model.RootCause {
	if len(in.PCIe) == 0 {
		return nil
	}
	pm := pathMap(paths)
	g := in.Graph
	// ACS/ATS already explains an abnormal GPU path; it is not a symptom of
	// the GPU link then.
	misconfig := hasMisconfigCause(already)
	var out []model.RootCause
	for _, n := range g.RNICs {
		cfg, ok := in.PCIe[n.ID]
		if !ok {
			continue
		}
		if c, ok := rnicPCIeUndersized(n, cfg); ok {
			out = append(out, c)
		}
		if c, ok := rnicPCIeDownTrain(g, pm, n, cfg, already); ok {
			out = append(out, c)
		}
	}
	for _, e := range g.Endpoints {
		cfg, ok := in.PCIe[e.ID]
		if !ok || e.Kind != model.EndpointGPU {
			continue
		}
		if c, ok := gpuPCIeDownTrain(g, pm, e, cfg, already, misconfig); ok {
			out = append(out, c)
		}
	}
	return out
}

// hasMisconfigCause reports whether causes name ACS or ATS misconfiguration.
func hasMisconfigCause(causes []model.RootCause) bool {
	for _, c := range causes {
		if c.Code == model.CauseACSEnabled || c.Code == model.CauseATSDisabled {
			return true
		}
	}
	return false
}

// rnicPCIeDownTrain is diagnosePCIeDownTrain's rnic_pcie_failure for RNIC n,
// whose link reads cfg: it trained below its capability and no longer
// carries the line rate, and no rnic_pcie_failure names n already.
func rnicPCIeDownTrain(g topology.Graph, pm map[string]model.PathMeasurement, n model.RNIC, cfg model.PCIeConfig, already []model.RootCause) (model.RootCause, bool) {
	if hasCauseFor(already, model.CauseRNICPCIeFailure, n.ID, "") {
		return model.RootCause{}, false
	}
	speed, width := pci.DownTrain(cfg)
	if (!speed && !width) || healthyPCIeSpeed(cfg, n.LineRate) {
		return model.RootCause{}, false
	}
	abn := abnormalAffinitiveEndpoints(g, pm, n.ID)
	lineRate := n.LineRate
	if lineRate == 0 {
		lineRate = assumedLineRateBPS
	}
	ev := fmt.Sprintf("PCIe %s trained below its %s capability; %s cannot carry the %s line rate",
		pcieEvidence(cfg), downTrainHalves(speed, width), gbpsString(pci.NegotiatedBPS(cfg)), gbpsString(lineRate))
	return model.RootCause{
		Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link down-trained",
		Links: []string{model.PCIeLinkID(n.ID)}, RNICs: []string{n.ID}, Endpoints: abn,
		Confidence: downTrainConfidence(len(abn) > 0),
		Evidence:   ev + symptomEvidence(len(abn)),
	}, true
}

// abnormalAffinitiveEndpoints lists the affinitive endpoints of RNIC rnic
// whose path from it is abnormal.
func abnormalAffinitiveEndpoints(g topology.Graph, pm map[string]model.PathMeasurement, rnic string) []string {
	var abn []string
	for _, e := range g.AffinitiveEndpoints(rnic) {
		if m, ok := pm[pathKey(rnic, e.ID)]; ok && m.Status == model.PathAbnormal {
			abn = append(abn, e.ID)
		}
	}
	return abn
}

// gpuPCIeDownTrain is diagnosePCIeDownTrain's gpu_pcie_failure for GPU e,
// whose link reads cfg: it trained narrower than its width capability, and no
// gpu_pcie_failure names e already. Its affinitive path is a symptom unless
// misconfig (ACS/ATS) already explains it.
func gpuPCIeDownTrain(g topology.Graph, pm map[string]model.PathMeasurement, e model.Endpoint, cfg model.PCIeConfig, already []model.RootCause, misconfig bool) (model.RootCause, bool) {
	if hasCauseFor(already, model.CauseGPUPCIeFailure, "", e.ID) {
		return model.RootCause{}, false
	}
	if _, width := pci.DownTrain(cfg); !width {
		return model.RootCause{}, false
	}
	abn := 0
	if aff := affinitiveRNIC(g, e); aff != "" && !misconfig {
		if m, ok := pm[pathKey(aff, e.ID)]; ok && m.Status == model.PathAbnormal {
			abn = 1
		}
	}
	return model.RootCause{
		Code: model.CauseGPUPCIeFailure, Summary: "GPU PCIe link down-trained",
		Links: []string{model.PCIeLinkID(e.ID)}, Endpoints: []string{e.ID},
		Confidence: downTrainConfidence(abn > 0),
		Evidence:   fmt.Sprintf("PCIe %s trained below its width capability", pcieEvidence(cfg)) + symptomEvidence(abn),
	}, true
}

// rnicPCIeUndersized is the advisory for an RNIC whose PCIe link trained at
// its maximum, yet that maximum cannot carry the line rate: a slot narrower or
// slower than the RNIC by design. Nothing is broken and reseating changes
// nothing, so it is never rnic_pcie_failure. It is reported once per RNIC per
// run, from config space alone, at low confidence, and like the other advisory
// codes it changes no path or link status.
func rnicPCIeUndersized(n model.RNIC, cfg model.PCIeConfig) (model.RootCause, bool) {
	if healthyPCIeSpeed(cfg, n.LineRate) || rnicPCIeTraining(cfg) != pcieAtMax {
		return model.RootCause{}, false
	}
	lineRate := n.LineRate
	if lineRate == 0 {
		lineRate = assumedLineRateBPS
	}
	return model.RootCause{
		Code: model.CauseRNICPCIeUndersized, Summary: "RNIC PCIe link narrower than its line rate by design",
		Links: []string{model.PCIeLinkID(n.ID)}, RNICs: []string{n.ID}, Confidence: model.ConfidenceLow,
		Evidence: fmt.Sprintf("PCIe %s trained at its maximum; %s cannot carry the %s line rate; not a down-train",
			pcieEvidence(cfg), gbpsString(pci.NegotiatedBPS(cfg)), gbpsString(lineRate)),
	}, true
}

func downTrainConfidence(symptom bool) model.Confidence {
	if symptom {
		return "high"
	}
	return "medium"
}

func symptomEvidence(abnormal int) string {
	if abnormal == 0 {
		return "; no affinitive path abnormal yet (config space only)"
	}
	return fmt.Sprintf("; %d affinitive path(s) abnormal", abnormal)
}

func downTrainHalves(speed, width bool) string {
	switch {
	case speed && width:
		return "speed and width"
	case speed:
		return "speed"
	default:
		return "width"
	}
}

func gbpsString(bps uint64) string {
	return fmt.Sprintf("%.0f Gb/s", float64(bps)/1e9)
}

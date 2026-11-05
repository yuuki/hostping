package analyzer

import (
	"reflect"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// gen4Link is a Gen4 device trained to speed x width under a Gen4 x16 port,
// with a device maximum of Gen4 x maxWidth.
func gen4Link(speed string, width, maxWidth int) model.PCIeConfig {
	const gen4 = "16.0 GT/s PCIe"
	return model.PCIeConfig{
		Speed: speed, Width: width,
		MaxSpeed: gen4, MaxWidth: maxWidth,
		PortMaxSpeed: gen4, PortMaxWidth: 16,
	}
}

// Which config-space causes diagnosePCIeDownTrain reports on Fig.2 (200G
// RNICs, Gen4 x16 hops), in what order, and the evidence each one carries.
func TestDiagnosePCIeDownTrainCauses(t *testing.T) {
	const (
		gen1 = "2.5 GT/s PCIe"
		gen3 = "8.0 GT/s PCIe"
		gen4 = "16.0 GT/s PCIe"
		gen5 = "32.0 GT/s PCIe"
		abn  = model.PathAbnormal
	)
	gpuCause := func(id string, conf model.Confidence, ev string) model.RootCause {
		return model.RootCause{
			Code: model.CauseGPUPCIeFailure, Summary: "GPU PCIe link down-trained",
			Links: []string{"pcie." + id}, Endpoints: []string{id}, Confidence: conf, Evidence: ev,
		}
	}
	const gpuNarrow = "PCIe 16.0 GT/s PCIe x8 (max 16.0 GT/s PCIe x16, port max 16.0 GT/s PCIe x16) trained below its width capability"
	cases := []struct {
		name    string
		graph   func(g *topology.Graph)
		pcie    map[string]model.PCIeConfig
		paths   map[string]model.PathStatus
		already []model.RootCause
		want    []model.RootCause
	}{
		{
			name:    "no config space read",
			paths:   map[string]model.PathStatus{"rnic0->gpu0": abn},
			already: []model.RootCause{{Code: model.CauseACSEnabled}},
		},
		{
			// rnic0's uncertain path to gpu1 is no symptom; rnic1 is
			// undersized, not down-trained, and an rnic_pcie_failure it
			// already carries does not skip that advisory; rnic2's line rate
			// is unknown and judged at 200G; rnic3 is down-trained but
			// already carries rnic_pcie_failure, which skips it; mem0's entry
			// is not an RNIC or GPU link.
			name: "RNIC links in graph order",
			graph: func(g *topology.Graph) {
				g.RNICs[2].LineRate = 0
			},
			pcie: map[string]model.PCIeConfig{
				"rnic0": gen4Link(gen4, 8, 16),
				"rnic1": gen4Link(gen4, 8, 8),
				"rnic2": gen4Link(gen3, 8, 16),
				"rnic3": gen4Link(gen4, 8, 16),
				"mem0":  gen4Link(gen4, 8, 16),
			},
			paths: map[string]model.PathStatus{
				"rnic0->gpu0": abn, "rnic0->gpu1": model.PathUncertain, "rnic0->mem0": abn, "rnic3->gpu6": abn,
			},
			already: []model.RootCause{
				{Code: model.CauseRNICPCIeFailure, RNICs: []string{"rnic1"}},
				{Code: model.CauseRNICPCIeFailure, RNICs: []string{"rnic3"}},
			},
			want: []model.RootCause{
				{
					Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link down-trained",
					Links: []string{"pcie.rnic0"}, RNICs: []string{"rnic0"}, Endpoints: []string{"gpu0", "mem0"},
					Confidence: model.ConfidenceHigh,
					Evidence: "PCIe 16.0 GT/s PCIe x8 (max 16.0 GT/s PCIe x16, port max 16.0 GT/s PCIe x16) trained below its width capability; " +
						"126 Gb/s cannot carry the 200 Gb/s line rate; 2 affinitive path(s) abnormal",
				},
				{
					Code: model.CauseRNICPCIeUndersized, Summary: "RNIC PCIe link narrower than its line rate by design",
					Links: []string{"pcie.rnic1"}, RNICs: []string{"rnic1"}, Confidence: model.ConfidenceLow,
					Evidence: "PCIe 16.0 GT/s PCIe x8 (max 16.0 GT/s PCIe x8, port max 16.0 GT/s PCIe x16) trained at its maximum; " +
						"126 Gb/s cannot carry the 200 Gb/s line rate; not a down-train",
				},
				{
					Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link down-trained",
					Links: []string{"pcie.rnic2"}, RNICs: []string{"rnic2"}, Confidence: model.ConfidenceMedium,
					Evidence: "PCIe 8.0 GT/s PCIe x8 (max 16.0 GT/s PCIe x16, port max 16.0 GT/s PCIe x16) trained below its speed and width capability; " +
						"63 Gb/s cannot carry the 200 Gb/s line rate; no affinitive path abnormal yet (config space only)",
				},
			},
		},
		{
			// rnic0 trained to Gen4 x16 of a Gen5 x16 capability still
			// carries 200G; rnic1 is at its maximum and carries it too.
			name: "RNIC links that still carry the line rate",
			pcie: map[string]model.PCIeConfig{
				"rnic0": {Speed: gen4, Width: 16, MaxSpeed: gen5, MaxWidth: 16, PortMaxSpeed: gen5, PortMaxWidth: 16},
				"rnic1": gen4Link(gen4, 16, 16),
				"rnic2": gen4Link(gen3, 16, 16),
			},
			want: []model.RootCause{{
				Code: model.CauseRNICPCIeFailure, Summary: "RNIC PCIe link down-trained",
				Links: []string{"pcie.rnic2"}, RNICs: []string{"rnic2"}, Confidence: model.ConfidenceMedium,
				Evidence: "PCIe 8.0 GT/s PCIe x16 (max 16.0 GT/s PCIe x16, port max 16.0 GT/s PCIe x16) trained below its speed capability; " +
					"126 Gb/s cannot carry the 200 Gb/s line rate; no affinitive path abnormal yet (config space only)",
			}},
		},
		{
			// gpu1's low speed is an idle GPU; gpu3 already carries
			// gpu_pcie_failure; gpu8 has no affinitive RNIC.
			name: "GPU links are judged by width",
			graph: func(g *topology.Graph) {
				g.Endpoints = append(g.Endpoints, model.Endpoint{ID: "gpu8", Kind: model.EndpointGPU, Index: 8, RootPort: "rp.9"})
			},
			pcie: map[string]model.PCIeConfig{
				"gpu0": gen4Link(gen4, 8, 16),
				"gpu1": gen4Link(gen1, 16, 16),
				"gpu2": gen4Link(gen4, 8, 16),
				"gpu3": gen4Link(gen4, 8, 16),
				"gpu8": gen4Link(gen4, 8, 16),
			},
			paths:   map[string]model.PathStatus{"rnic0->gpu0": abn, "rnic1->gpu1": abn, "rnic1->gpu3": abn},
			already: []model.RootCause{{Code: model.CauseGPUPCIeFailure, Endpoints: []string{"gpu3"}}},
			want: []model.RootCause{
				gpuCause("gpu0", model.ConfidenceHigh, gpuNarrow+"; 1 affinitive path(s) abnormal"),
				gpuCause("gpu2", model.ConfidenceMedium, gpuNarrow+"; no affinitive path abnormal yet (config space only)"),
				gpuCause("gpu8", model.ConfidenceMedium, gpuNarrow+"; no affinitive path abnormal yet (config space only)"),
			},
		},
		{
			name:    "ACS already explains the abnormal GPU path",
			pcie:    map[string]model.PCIeConfig{"gpu0": gen4Link(gen4, 8, 16)},
			paths:   map[string]model.PathStatus{"rnic0->gpu0": abn},
			already: []model.RootCause{{Code: model.CauseACSEnabled, Endpoints: []string{"gpu0"}}},
			want:    []model.RootCause{gpuCause("gpu0", model.ConfidenceMedium, gpuNarrow+"; no affinitive path abnormal yet (config space only)")},
		},
		{
			name:    "ATS already explains the abnormal GPU path",
			pcie:    map[string]model.PCIeConfig{"gpu0": gen4Link(gen4, 8, 16)},
			paths:   map[string]model.PathStatus{"rnic0->gpu0": abn},
			already: []model.RootCause{{Code: model.CauseATSDisabled, RNICs: []string{"rnic0"}}},
			want:    []model.RootCause{gpuCause("gpu0", model.ConfidenceMedium, gpuNarrow+"; no affinitive path abnormal yet (config space only)")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := topology.Fig2()
			if tc.graph != nil {
				tc.graph(&g)
			}
			in := Input{Graph: g, PCIe: tc.pcie}
			got := diagnosePCIeDownTrain(in, fig2PathStatuses(g, tc.paths), tc.already)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diagnosePCIeDownTrain:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

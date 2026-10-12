package analyzer_test

import (
	"fmt"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

const gbps = 1_000_000_000

// fig2WithLineRate re-rates Fig.2 to one RNIC line rate. Every PCIe hop keeps
// the 252 Gb/s capacity discovery records for an x16 slot, so a 400 Gb/s RNIC
// reproduces a host whose line rate outruns the bus feeding it.
func fig2WithLineRate(rate uint64) topology.Graph {
	g := topology.Fig2()
	for i := range g.RNICs {
		g.RNICs[i].LineRate = rate
	}
	return g
}

// flatBaseline is the override shape an operator needs once the scaled Fig.4
// defaults no longer fit the hardware: one floor on every distance, set below
// what the host actually reaches.
func flatBaseline(bps uint64) map[model.Distance]baseline.PathBaseline {
	out := map[model.Distance]baseline.PathBaseline{}
	for _, d := range model.Distances() {
		out[d] = baseline.PathBaseline{BandwidthBPS: bps}
	}
	return out
}

// An affinitive path is normal once it reaches what its hardware can carry.
// The paper reads that as the RNIC line rate, which holds only while the line
// rate sits below the bus: GPUDirect RDMA on a 400 Gb/s RNIC behind a ~252 Gb/s
// x16 slot cannot reach 400 × (1-AbnormalTh) = 320 Gb/s, so every GPU path
// stayed uncertain no matter how healthy the host was. The ceiling is
// min(line rate, narrowest hop); the abnormal rule is untouched.
func TestClassifyAffinitiveGPUAgainstBusCapacity(t *testing.T) {
	cases := []struct {
		name     string
		lineRate uint64
		endpoint string
		bw       uint64
		bases    map[model.Distance]baseline.PathBaseline
		want     model.PathStatus
	}{
		// A healthy GDR write over Gen5 x16 lands near the bus capacity, far
		// under the 320 Gb/s the line rate alone would demand.
		{"400G GDR at a typical rate", 400 * gbps, "gpu0", 273 * gbps, flatBaseline(240 * gbps), model.PathNormal},
		{"400G GDR near the low end", 400 * gbps, "gpu0", 245 * gbps, flatBaseline(240 * gbps), model.PathNormal},
		// A four-lane down-train is still a fault, not a narrow bus.
		{"400G x4 down-trained GPU", 400 * gbps, "gpu0", 67 * gbps, flatBaseline(240 * gbps), model.PathAbnormal},
		// Clears the operator's 240 Gb/s floor but not 252 × 0.8 = 201.6 Gb/s:
		// a loose baseline must not promote a path to normal on its own.
		{"400G GPU under the bus ceiling", 400 * gbps, "gpu0", 195 * gbps, flatBaseline(240 * gbps), model.PathUncertain},
		// Fig.8 gray: a reduced cross-root baseline is not proof of health.
		{"400G non-affinitive GPU", 400 * gbps, "gpu2", 273 * gbps, flatBaseline(240 * gbps), model.PathUncertain},
		// The paper's host: 200G NIC under a wider bus, so the line rate stays
		// the binding ceiling (min picks 200G, not 252G).
		{"200G paper host at 170G", 200 * gbps, "gpu0", 170 * gbps, nil, model.PathNormal},
		{"200G paper host at 120G", 200 * gbps, "gpu0", 120 * gbps, nil, model.PathAbnormal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := fig2WithLineRate(tc.lineRate)
			ms := []model.PathMeasurement{{
				RNIC: "rnic0", Endpoint: tc.endpoint, BandwidthBPS: tc.bw, SmallLatNsP50: 3000,
			}}
			out := analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), baseline.Uniform(tc.bases))
			if len(out) != 1 {
				t.Fatalf("paths %d", len(out))
			}
			if out[0].Status != tc.want {
				t.Fatalf("rnic0->%s at %d bps on a %d bps RNIC: got %s, want %s",
					tc.endpoint, tc.bw, tc.lineRate, out[0].Status, tc.want)
			}
		})
	}
}

// The symptom this fixes: with no GPU path ever normal, Algorithm 1 gets no
// signal for any GPU hop and pcie.gpu0..7 report uncertain forever.
func TestGPULinksResolveOn400GHost(t *testing.T) {
	g := fig2WithLineRate(400 * gbps)
	var ms []model.PathMeasurement
	for _, p := range g.Paths {
		e, _ := g.Endpoint(p.Endpoint)
		bw := uint64(270 * gbps) // host DRAM
		if e.Kind == model.EndpointGPU {
			bw = 273 * gbps
			if !p.Affinitive {
				bw = 245 * gbps
			}
		}
		ms = append(ms, model.PathMeasurement{RNIC: p.RNIC, Endpoint: p.Endpoint, BandwidthBPS: bw, SmallLatNsP50: 3000})
	}

	classified := analyzer.ClassifyPaths(g, ms, model.ModeIdle, nil, config.Defaults(), baseline.Uniform(flatBaseline(240*gbps)))
	for _, m := range classified {
		if m.Status == model.PathAbnormal {
			t.Fatalf("healthy 400G host has an abnormal path: %+v", m)
		}
	}
	states := linkStatuses(analyzer.DetectAbnormalLinks(g, classified, model.ModeIdle, nil))
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("pcie.gpu%d", i)
		if states[id] != model.LinkNormal {
			t.Errorf("%s: want normal, got %s", id, states[id])
		}
	}
}

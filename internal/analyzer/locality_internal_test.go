package analyzer

import (
	"fmt"
	"testing"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// fourNodes is a synthetic host of four NUMA nodes with two RNICs and one
// memory node each: rnicN sits on node N/2, memK on node K, and an RNIC
// reaches its own node's memory as local_mem and the others as remote_mem.
func fourNodes() topology.Graph {
	var g topology.Graph
	for k := range 4 {
		g.Endpoints = append(g.Endpoints, model.Endpoint{ID: fmt.Sprintf("mem%d", k), Kind: model.EndpointMemory, Index: k, NUMA: k})
	}
	for i := range 8 {
		n := model.RNIC{ID: fmt.Sprintf("rnic%d", i), NUMA: i / 2}
		g.RNICs = append(g.RNICs, n)
		for _, e := range g.Endpoints {
			d := model.DistanceRemoteMem
			if e.NUMA == n.NUMA {
				d = model.DistanceLocalMem
			}
			g.Paths = append(g.Paths, model.Path{RNIC: n.ID, Endpoint: e.ID, Distance: d})
		}
	}
	return g
}

// judgeFourNodes reads every path at 1000 ns, the paths of the given RNICs
// and to the given endpoints at 2000 ns, and runs the sweep verdict and then
// the locality check.
func judgeFourNodes(t *testing.T, g topology.Graph, slow ...string) map[string]model.PathMeasurement {
	t.Helper()
	var ms []model.PathMeasurement
	for _, p := range g.Paths {
		m := model.PathMeasurement{RNIC: p.RNIC, Endpoint: p.Endpoint, SmallLatNsP50: 1000}
		for _, s := range slow {
			if p.RNIC == s || p.Endpoint == s {
				m.SmallLatNsP50 = 2000
			}
		}
		ms = append(ms, m)
	}
	cfg := config.Defaults()
	judged := ClassifyLatency(g, ms, model.ModeIdle, cfg, baseline.Overlay{})
	for _, m := range judged {
		if m.SmallLatNsP50 == 2000 && m.LatencyStatus != model.LatencyAbnormal {
			t.Fatalf("precondition: sweep judged %s->%s %s/%s, want abnormal", m.RNIC, m.Endpoint, m.LatencyStatus, m.LatencyReason)
		}
	}
	out := map[string]model.PathMeasurement{}
	for _, m := range applyLocalityShift(g, judged, baseline.Overlay{}, cfg.AbnormalTh) {
		out[m.RNIC+"->"+m.Endpoint] = m
	}
	return out
}

// A shift over two of four nodes at once flags each endpoint from both of
// them. That is a minority of the other nodes, so the endpoint is not blamed
// and every shifted path is cleared.
func TestLocalityShiftSpanningNodes(t *testing.T) {
	g := fourNodes()
	got := judgeFourNodes(t, g, "rnic0", "rnic1", "rnic2", "rnic3")
	for k, m := range got {
		shifted := m.RNIC == "rnic0" || m.RNIC == "rnic1" || m.RNIC == "rnic2" || m.RNIC == "rnic3"
		if shifted && m.LatencyReason != model.LatencyLocalityShift {
			t.Errorf("%s = %s/%s, want normal/locality_shift", k, m.LatencyStatus, m.LatencyReason)
		}
		if m.LatencyStatus == model.LatencyAbnormal {
			t.Errorf("%s still abnormal", k)
		}
	}
}

// A slow endpoint is read slow from every node, so its local paths keep their
// verdict even though the node's two RNICs agree with each other.
func TestLocalityShiftSlowEndpointFromAllNodes(t *testing.T) {
	g := fourNodes()
	got := judgeFourNodes(t, g, "mem0")
	for k, m := range got {
		if m.Endpoint != "mem0" {
			continue
		}
		if m.LatencyStatus != model.LatencyAbnormal || m.LatencyReason != model.LatencyBySiblings || m.LatencyLocality != nil {
			t.Errorf("%s = %s/%s, want abnormal/siblings", k, m.LatencyStatus, m.LatencyReason)
		}
	}
}

// The guard counts other nodes with a judged path to the endpoint and needs a
// strict majority of them flagged; with no other node it does not apply.
func TestEndpointSlowElsewhere(t *testing.T) {
	for _, tc := range []struct {
		reach map[int]bool
		want  bool
	}{
		{map[int]bool{0: true}, false},
		{map[int]bool{0: true, 1: true}, true},
		{map[int]bool{0: true, 1: false}, false},
		{map[int]bool{0: true, 1: true, 2: false, 3: false}, false},
		{map[int]bool{0: true, 1: true, 2: true, 3: false}, true},
		{map[int]bool{0: false, 1: true, 2: true}, true},
	} {
		if got := endpointSlowElsewhere(tc.reach, 0); got != tc.want {
			t.Errorf("reach %v from node 0: %v, want %v", tc.reach, got, tc.want)
		}
	}
}

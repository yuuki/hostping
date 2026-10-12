package analyzer

import (
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// applyCPUBusy turns every abnormal path that transits a loaded CPU package
// into an uncertain one with cpu_busy. It runs after the paired
// rechecks, because a constant CPU load survives them: the recheck re-reads
// the path while the load still runs, against references that sit on
// unloaded packages, and confirms the drop.
//
// A loopback path stays within the packages of its two ends — the RNIC's,
// and the endpoint's when the path crosses the inter-socket bus — and crosses
// the on-die fabric of each, so a path transits exactly those packages. A
// normal or otherwise uncertain path keeps its verdict: a path that reaches
// its baseline under load is no less healthy for it, and a path through an
// unloaded package is judged as before.
func applyCPUBusy(g topology.Graph, paths []model.PathMeasurement, loaded map[int]bool) []model.PathMeasurement {
	if len(loaded) == 0 {
		return paths
	}
	for i := range paths {
		m := &paths[i]
		if m.Status != model.PathAbnormal {
			continue
		}
		if transitsLoadedSocket(g, m.RNIC, m.Endpoint, loaded) {
			markUncertain(m, model.UncertainCPUBusy)
		}
	}
	return paths
}

// applyCPUBusyLatency unjudges every latency-abnormal path that transits a
// loaded CPU package, for the same reason as applyCPUBusy: the latency is
// unknown with cpu_busy, so it neither extends a latency streak nor becomes a
// cause. It runs after the latency recheck and the locality shift, which a
// constant load passes for the same reason as the bandwidth recheck.
func applyCPUBusyLatency(g topology.Graph, paths []model.PathMeasurement, loaded map[int]bool) []model.PathMeasurement {
	if len(loaded) == 0 {
		return paths
	}
	for i := range paths {
		m := &paths[i]
		if m.LatencyStatus != model.LatencyAbnormal {
			continue
		}
		if transitsLoadedSocket(g, m.RNIC, m.Endpoint, loaded) {
			m.LatencyStatus = model.LatencyUnknown
			m.LatencyReason = model.LatencyCPUBusy
			m.LatencyRecheckSkipped = ""
		}
	}
	return paths
}

// transitsLoadedSocket reports whether the path from rnic to endpoint has an
// end on a loaded package. An end the graph does not know is not on one.
func transitsLoadedSocket(g topology.Graph, rnic, endpoint string, loaded map[int]bool) bool {
	if n, ok := g.RNIC(rnic); ok && loaded[n.Socket] {
		return true
	}
	if e, ok := g.Endpoint(endpoint); ok && loaded[e.Socket] {
		return true
	}
	return false
}

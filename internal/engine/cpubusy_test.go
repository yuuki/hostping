package engine

import (
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// socketContention is the drop reproduced on hardware with every
// core of one package streaming memory reads: the memory paths of that
// package's RNICs read a quarter of their level, to every NUMA node, and the
// GPU paths that cross into the package from the other one about 70%. The
// load is constant, so the paired re-reads read the same.
func socketContention(g topology.Graph, socket int) func(rnic, ep string, n int) float64 {
	return func(rnic, ep string, _ int) float64 {
		r, _ := g.RNIC(rnic)
		e, _ := g.Endpoint(ep)
		switch {
		case strings.HasPrefix(ep, "mem") && r.Socket == socket:
			return 0.25
		case strings.HasPrefix(ep, "gpu") && r.Socket != e.Socket && (r.Socket == socket || e.Socket == socket):
			return 0.7
		}
		return 1
	}
}

func cpuLoad(g topology.Graph, highSocket int) []model.CPULoad {
	var out []model.CPULoad
	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointMemory {
			continue
		}
		l := model.CPULoad{NUMA: e.NUMA, Socket: e.Socket, Busy: 0.02}
		if e.Socket == highSocket {
			l.Busy, l.High = 1, true
		}
		out = append(out, l)
	}
	return out
}

// CPU load on one package drops every path through it, the paired
// re-read confirms the drops, and the run reported cpu_root_port_failure on a
// healthy host. With the package's CPU load read on the poll that started the
// run, those paths are uncertain with cpu_busy and no cause is left; the paths
// that do not transit the package are judged as before.
func TestCPUBusySocketIsNotARootPortFailure(t *testing.T) {
	g := topology.Fig2()
	scale := socketContention(g, 1)

	// Without the CPU load the drops are confirmed and paged.
	rep, _ := runBW(t, g, newBWWindowProber(sim.New(g), scale), RunRequest{Mode: model.ModeIdle})
	if !reportHas(rep, model.CauseCPURootPortFailure) {
		t.Fatalf("the reproduction raised no %s without CPU load: %+v", model.CauseCPURootPortFailure, rep.Causes)
	}

	req := RunRequest{Mode: model.ModeIdle, CPULoad: cpuLoad(g, 1)}
	rep, _ = runBW(t, g, newBWWindowProber(sim.New(g), scale), req)
	if len(rep.Causes) != 0 {
		t.Errorf("a CPU-loaded package left causes %+v", rep.Causes)
	}
	for _, p := range rep.Paths {
		r, _ := g.RNIC(p.RNIC)
		e, _ := g.Endpoint(p.Endpoint)
		through := r.Socket == 1 || e.Socket == 1
		if p.Status == model.PathAbnormal {
			t.Errorf("%s->%s abnormal under CPU load", p.RNIC, p.Endpoint)
		}
		if p.UncertainReason == model.UncertainCPUBusy && !through {
			t.Errorf("%s->%s does not transit socket 1 yet is cpu_busy", p.RNIC, p.Endpoint)
		}
		if p.LatencyStatus == model.LatencyAbnormal && through {
			t.Errorf("%s->%s latency abnormal through a loaded package", p.RNIC, p.Endpoint)
		}
	}
	if got := pathStatus(t, rep, "rnic2", "mem1"); got.Status != model.PathUncertain || got.UncertainReason != model.UncertainCPUBusy {
		t.Errorf("rnic2->mem1 = %+v, want uncertain/cpu_busy", got)
	}
	if got := pathStatus(t, rep, "rnic0", "mem0"); got.Status != model.PathNormal {
		t.Errorf("rnic0->mem0 does not transit the loaded package: %+v, want normal", got)
	}
	if len(rep.CPULoad) != 2 || !rep.CPULoad[1].High {
		t.Errorf("report CPU load = %+v", rep.CPULoad)
	}
}

// A real root-port drop on a package whose CPUs are not loaded is still
// reported while another package is loaded.
func TestCPUBusyOtherSocketKeepsRootPortFailure(t *testing.T) {
	g := topology.Fig2()
	p := newBWWindowProber(sim.New(g), memWindow(func(int) bool { return true }, "rnic0"))
	rep, _ := runBW(t, g, p, RunRequest{Mode: model.ModeIdle, CPULoad: cpuLoad(g, 1)})
	if got := pathStatus(t, rep, "rnic0", "mem0"); got.Status != model.PathAbnormal {
		t.Fatalf("rnic0->mem0 = %+v, want abnormal", got)
	}
	if got := pathStatus(t, rep, "rnic0", "mem1"); got.Status != model.PathUncertain || got.UncertainReason != model.UncertainCPUBusy {
		t.Errorf("rnic0->mem1 crosses into the loaded package: %+v, want uncertain/cpu_busy", got)
	}
	if !reportHas(rep, model.CauseCPURootPortFailure) {
		t.Fatalf("a real drop on an unloaded package raised no %s: %+v", model.CauseCPURootPortFailure, rep.Causes)
	}
}

func reportHas(rep model.Report, code string) bool {
	for _, c := range rep.Causes {
		if c.Code == code {
			return true
		}
	}
	return false
}

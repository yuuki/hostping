package monitor

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// Bus utilization (NSDI'23 §4.1.2 / ToN'24 §IV-A-3) tells an overloaded hop
// from a failed one. Live sources, each optional:
//
//   - RNIC PCIe (pcie.<rnic>): the monitor's own counter delta, see rnicUtil.
//   - GPU PCIe (pcie.<gpu>): NVML's PCIe throughput via nvidia-smi dmon.
//   - Memory (mem.<n>): Intel uncore IMC CAS counters, opt-in, see Uncore.
//   - Inter-socket (upi.<a>-<b>): Intel uncore UPI data flits, opt-in, see
//     upi.go.
//
// Memory and inter-socket capacities are nominal constants unless the
// operator supplies them (model.Link.CapacityAssumed), and a measured rate
// divided by a guess is no utilization, so an assumed link stays unknown. A
// hop without a source is absent from every utilization map — unknown, never
// 0.

// utilCapacity is the capacity utilization is computed against: the link's
// CapacityBPS, unless the link is missing or its capacity was assumed rather
// than read.
func utilCapacity(g topology.Graph, linkID string) (uint64, bool) {
	l, ok := g.Link(linkID)
	if !ok || l.CapacityBPS == 0 || l.CapacityAssumed {
		return 0, false
	}
	return l.CapacityBPS, true
}

// rnicUtil is the utilization of the PCIe hop attaching an RNIC, from the
// port byte rate the monitor already derives (max of rx and tx, since PCIe is
// full duplex and each direction has the whole capacity).
//
// It is a lower bound on how busy that hop is. The port counters see only
// traffic that reaches the wire, so they miss:
//   - loopback the RNIC turns around internally (hostping's own probes are
//     one such stream, as is any intra-host RDMA a workload runs through the
//     RNIC), which crosses the hop twice without touching the port;
//   - PCIe protocol overhead — TLP headers, work-request and completion
//     DMA, doorbells — which exceeds the network headers the counters do
//     include;
//   - any other PCI function sharing the same physical link (a second port
//     of the same card) whose traffic is counted on its own port;
//   - bursts shorter than the poll interval, which the average flattens.
//
// No vendor counter gives the exact rate: the RNIC driver exposes no PCIe
// byte counter in sysfs or ethtool, its stall percentages stayed at 0 under
// host contention, and its PCIe performance register (root, vendor tools)
// counted no data under a known loopback load.
//
// A lower bound can miss an overload but never invents one, which is the safe
// direction for a rule that turns a failure into "overloaded".
func rnicUtil(g topology.Graph, rnicID string, throughputBPS uint64) (float64, bool) {
	c, ok := utilCapacity(g, model.PCIeLinkID(rnicID))
	if !ok {
		return 0, false
	}
	return float64(throughputBPS) / float64(c), true
}

// gpuPCIeUtil maps each GPU PCIe hop to its utilization from byte rates keyed
// by normalized BDF. A GPU without a rate or a read capacity is left out.
func gpuPCIeUtil(g topology.Graph, rate map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointGPU || e.PCIeBDF == "" {
			continue
		}
		bps, ok := rate[normalizeBDF(e.PCIeBDF)]
		if !ok {
			continue
		}
		id := model.PCIeLinkID(e.ID)
		c, ok := utilCapacity(g, id)
		if !ok {
			continue
		}
		out[id] = bps * 8 / float64(c)
	}
	return out
}

// dmonTimeout bounds nvidia-smi dmon, which waits for one sampling period
// before printing its only row.
const dmonTimeout = 5 * time.Second

// nvidiaPCIeByBDF reads the PCIe throughput of every NVIDIA GPU as
// max(rx, tx) bytes per second keyed by normalized BDF. dmon reports
// NVML's PCIe throughput counter, which covers every transfer on the GPU's
// link (GPUDirect RDMA from any RNIC, host copies, peer traffic) but is
// sampled over a short window, so it is a snapshot rather than an average.
// dmon names GPUs by index, so the index → BDF map is read in the same pass.
// Any failure yields nil: unknown, never 0.
func nvidiaPCIeByBDF(ctx context.Context, run runner) map[string]float64 {
	idx, err := run(ctx, "nvidia-smi", "--query-gpu=index,pci.bus_id", "--format=csv,noheader")
	if err != nil {
		return nil
	}
	bdf := map[string]string{}
	for _, line := range strings.Split(string(idx), "\n") {
		i, b, ok := strings.Cut(strings.TrimSpace(line), ",")
		if !ok {
			continue
		}
		bdf[strings.TrimSpace(i)] = normalizeBDF(b)
	}
	if len(bdf) == 0 {
		return nil
	}
	b, err := run(ctx, "nvidia-smi", "dmon", "-s", "t", "-c", "1")
	if err != nil {
		return nil
	}
	return parseDmonPCIe(string(b), bdf)
}

// parseDmonPCIe reads `nvidia-smi dmon -s t` output: a "# gpu rxpci txpci"
// header naming the columns, a "# Idx MB/s MB/s" unit line, then one row per
// GPU. Columns are located by name so extra ones are tolerated; a GPU whose
// value is "-" (unsupported) or unparsable is left out.
func parseDmonPCIe(out string, bdfByIndex map[string]string) map[string]float64 {
	gpuCol, rxCol, txCol := -1, -1, -1
	res := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			f := strings.Fields(strings.TrimPrefix(line, "#"))
			for i, name := range f {
				switch strings.ToLower(name) {
				case "gpu":
					gpuCol = i
				case "rxpci":
					rxCol = i
				case "txpci":
					txCol = i
				}
			}
			continue
		}
		if gpuCol < 0 || rxCol < 0 || txCol < 0 {
			continue
		}
		f := strings.Fields(line)
		if len(f) <= max(gpuCol, rxCol, txCol) {
			continue
		}
		b, ok := bdfByIndex[f[gpuCol]]
		if !ok {
			continue
		}
		rx, err1 := strconv.ParseFloat(f[rxCol], 64)
		tx, err2 := strconv.ParseFloat(f[txCol], 64)
		if err1 != nil || err2 != nil || rx < 0 || tx < 0 {
			continue
		}
		// dmon prints MB/s.
		res[b] = max(rx, tx) * 1e6
	}
	if len(res) == 0 {
		return nil
	}
	return res
}

// gpuPCIeCache rate-limits the nvidia-smi dmon pass like gpuUtilCache. It is
// only consulted right before an engine run (Sampler.Bus), so a cached value
// always predates the probes that follow it.
type gpuPCIeCache = rateLimited[map[string]float64, nvidiaPCIeQuery]

var gpuPCIe = &gpuPCIeCache{}

// nvidiaPCIeQuery is the GPU PCIe throughput query by normalized BDF
// (nvidiaPCIeByBDF).
type nvidiaPCIeQuery struct{}

func (nvidiaPCIeQuery) defaultRunner() runner { return runSMI }
func (nvidiaPCIeQuery) fetch(ctx context.Context, run runner) map[string]float64 {
	return nvidiaPCIeByBDF(ctx, run)
}

// runSMI bounds an nvidia-smi command like any sampler command, except dmon,
// which carries its own, longer deadline.
func runSMI(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "dmon" {
		return runWithTimeout(ctx, dmonTimeout, name, args...)
	}
	return runBounded(ctx, name, args...)
}

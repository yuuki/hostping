package sim

import (
	"fmt"
	"strings"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// pcieGen4 is the sysfs current_link_speed spelling of a healthy Gen4 link.
const pcieGen4 = "16.0 GT/s PCIe"

// World is an intra-host network with optional faults for loopback/mutual simulation.
type World struct {
	Graph       topology.Graph
	Bases       map[model.Distance]baseline.PathBaseline
	LinkBW      map[string]uint64 // remaining hop bandwidth
	LinkLatNs   map[string]uint64 // extra hop latency
	LinkUtil    map[string]float64
	ACS         bool
	ATSOff      map[string]bool // RNIC id
	SlowStart   map[string]bool // RNIC id: cap all paths equally
	TxWindow    map[string]bool
	RNICCap     map[string]uint64 // extra RNIC-wide cap (slow start / tx window)
	ConnectFail map[string]bool
	// Isolated RNICs sit on a subnet of their own, as if cabled to the wrong
	// switch port (ToN #14): they reach each other but no other RNIC.
	Isolated    map[string]bool
	DropPPM     map[string]uint32
	PCIe        map[string]model.PCIeConfig
	Busy        map[string]bool
	ServiceLoad float64 // fraction of RNIC loopback BW stolen on busy RNICs
	// SlowMR multiplies an endpoint's simulated MR registration time
	// (ToN'24 §VI-C, e.g. a failing GPU). Keyed by endpoint id.
	SlowMR map[string]uint64
	// ComputeMode is a GPU's compute mode as the monitor would read it from
	// nvidia-smi, keyed by endpoint id. A GPU absent here reads as unknown.
	ComputeMode map[string]string
	// FastPairs are root-port pairs whose P2P route costs nothing: a path
	// across one reads at the same_switch level instead of its distance's,
	// as some platforms do for some host-bridge pairs. It is
	// healthy hardware, not a fault, but lets --sim reproduce a distance
	// that reads at two levels for hostpingctl calibrate to learn.
	FastPairs map[model.RootPortPair]bool
}

// Simulated setup verb times. Eq.1 says nothing about them, so these are
// representative round values, identical for every healthy endpoint of a kind.
const (
	simGPUAllocNs  uint64 = 400_000
	simGPUMRRegNs  uint64 = 150_000
	simHostMRRegNs uint64 = 40_000 // two host MRs
	simQPSetupNs   uint64 = 250_000
	// simSlowMRFactor is what the slow_mr fault multiplies registration by.
	simSlowMRFactor uint64 = 40
	// simExtraLatNs is what the lat fault adds to one hop: an extra
	// translation step that costs latency but, since Eq.1 cancels host
	// latency out of bandwidth, leaves bandwidth where it was.
	simExtraLatNs uint64 = 1000
)

func New(g topology.Graph) *World {
	w := &World{
		Graph:       g,
		Bases:       baseline.Defaults(0),
		LinkBW:      map[string]uint64{},
		LinkLatNs:   map[string]uint64{},
		LinkUtil:    map[string]float64{},
		ATSOff:      map[string]bool{},
		SlowStart:   map[string]bool{},
		TxWindow:    map[string]bool{},
		RNICCap:     map[string]uint64{},
		ConnectFail: map[string]bool{},
		Isolated:    map[string]bool{},
		DropPPM:     map[string]uint32{},
		PCIe:        map[string]model.PCIeConfig{},
		Busy:        map[string]bool{},
		ServiceLoad: 0.35,
		SlowMR:      map[string]uint64{},
		ComputeMode: map[string]string{},
		FastPairs:   map[model.RootPortPair]bool{},
	}
	for _, l := range g.Links {
		w.LinkBW[l.ID] = l.CapacityBPS
	}
	// Speed strings use the sysfs spelling of current_link_speed.
	for _, n := range g.RNICs {
		w.PCIe[n.ID] = model.PCIeConfig{BDF: n.PCIeBDF, Speed: pcieGen4, Width: 16, MaxSpeed: pcieGen4, MaxWidth: 16, ATSEnabled: true, ATSKnown: true, ACSKnown: true, NUMA: n.Socket}
	}
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			w.PCIe[e.ID] = model.PCIeConfig{BDF: e.PCIeBDF, Speed: pcieGen4, Width: 16, MaxSpeed: pcieGen4, MaxWidth: 16, NUMA: e.NUMA, ACSKnown: true, ATSKnown: true}
		}
	}
	return w
}

// ApplyFault parses CLI/test fault specs:
//
//	rnic_pcie:rnic0
//	gpu_pcie:gpu0
//	mem:mem0
//	mem_flap:mem0
//	upi (every upi link)
//	mesh:0-1 (snc_mesh link mesh.0-1; --topology fig2-snc)
//	rp:rp.0
//	acs
//	ats:rnic0
//	slow_start:rnic0
//	tx_window:rnic0
//	overload_upi
//	overload_mem:mem.0
//	busy:rnic0,rnic2
//	connect_fail:rnic0
//	drops:rnic0
//	slow_mr:gpu0
//	flap:rnic0
//	tor
//	lat:pcie.gpu0
//	isolate:rnic3 (or isolate:rnic2,rnic3)
//	compute_mode:gpu0 (Exclusive_Process; or compute_mode:gpu0=Prohibited)
//	fast_rp:rp.0,rp.1 (paths across this root-port pair read at the same_switch level)
func (w *World) ApplyFault(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, arg, _ := strings.Cut(part, ":")
		switch kind {
		case "rnic_pcie":
			// Full Gen4 speed, down-trained to four lanes: ~63 Gb/s, which the
			// speed string alone cannot tell from a healthy x16 link.
			id := model.PCIeLinkID(arg)
			w.LinkBW[id] = 67_000_000_000
			c := w.PCIe[arg]
			c.Width = 4
			w.PCIe[arg] = c
		case "gpu_pcie":
			w.LinkBW[model.PCIeLinkID(arg)] = 67_000_000_000
			c := w.PCIe[arg]
			c.Width = 4
			w.PCIe[arg] = c
		case "mem":
			w.LinkBW[memLink(arg)] = 80_000_000_000
		case "mem_flap":
			w.LinkBW[memLink(arg)] = 80_000_000_000
		case "upi":
			for _, l := range w.Graph.LinksByKind(model.LinkKindUPI) {
				w.LinkBW[l.ID] = 70_000_000_000
			}
		case "mesh":
			id := arg
			if !strings.HasPrefix(id, "mesh.") {
				id = "mesh." + id
			}
			if l, ok := w.Graph.Link(id); !ok || l.Kind != model.LinkKindSNCMesh {
				return fmt.Errorf("mesh: %q is not an snc_mesh link of topology %s (try --topology fig2-snc --fault mesh:0-1)", arg, w.Graph.Name)
			}
			w.LinkBW[id] = 70_000_000_000
		case "rp":
			w.LinkBW[arg] = 70_000_000_000
		case "acs":
			w.ACS = true
			for k, c := range w.PCIe {
				c.ACSEnabled = true
				w.PCIe[k] = c
			}
		case "ats":
			w.ATSOff[arg] = true
			c := w.PCIe[arg]
			c.ATSEnabled = false
			w.PCIe[arg] = c
		case "slow_start":
			w.SlowStart[arg] = true
			w.RNICCap[arg] = 90_000_000_000
		case "tx_window":
			w.TxWindow[arg] = true
			w.RNICCap[arg] = 90_000_000_000
		case "overload_upi":
			for _, l := range w.Graph.LinksByKind(model.LinkKindUPI) {
				w.LinkBW[l.ID] = 80_000_000_000
				w.LinkUtil[l.ID] = 0.95
			}
		case "overload_mem":
			w.LinkBW[memLink(arg)] = 80_000_000_000
			w.LinkUtil[memLink(arg)] = 0.95
		case "busy":
			for _, id := range strings.Split(arg, ",") {
				id = strings.TrimSpace(id)
				if id != "" {
					w.Busy[id] = true
				}
			}
		case "connect_fail":
			w.ConnectFail[arg] = true
		case "drops":
			w.DropPPM[arg] = 50_000
		case "slow_mr":
			w.SlowMR[arg] = simSlowMRFactor
		case "flap":
			// A quarter of the probes over the RNIC go unanswered: above
			// ToN'24 Timeout_th, far from total. The simulated probe is not
			// RC, so the same lost probes also read as loss.
			w.DropPPM[arg] = 250_000
		case "tor":
			// The ToR switch every RNIC hangs off: no pair connects.
			for _, n := range w.Graph.RNICs {
				w.ConnectFail[n.ID] = true
			}
		case "lat":
			w.LinkLatNs[arg] += simExtraLatNs
		case "isolate":
			for _, id := range strings.Split(arg, ",") {
				id = strings.TrimSpace(id)
				if id != "" {
					w.Isolated[id] = true
				}
			}
		case "compute_mode":
			// Not a link fault: the GPU is healthy, but its compute mode
			// tells the engine to leave it alone, so its paths are skipped.
			id, mode, ok := strings.Cut(arg, "=")
			if !ok {
				mode = model.ComputeModeExclusiveProcess
			}
			if !gpuEndpoint(w, id) {
				return fmt.Errorf("compute_mode: %q is not a GPU endpoint", id)
			}
			if mode != model.ComputeModeDefault && !model.ComputeModeBlocksProbe(mode) {
				return fmt.Errorf("compute_mode: unknown mode %q", mode)
			}
			w.ComputeMode[id] = mode
		case "fast_rp":
			// Not a fault either: the platform routes P2P between these two
			// root ports at full rate.
			a, b, ok := strings.Cut(arg, ",")
			a, b = strings.TrimSpace(a), strings.TrimSpace(b)
			if !ok || a == b || !rootPortLink(w, a) || !rootPortLink(w, b) {
				return fmt.Errorf("fast_rp: want two distinct root-port link IDs (fast_rp:rp.0,rp.1), got %q", arg)
			}
			w.FastPairs[model.NewRootPortPair(a, b)] = true
		default:
			return fmt.Errorf("unknown fault %q", kind)
		}
	}
	return nil
}

func memLink(arg string) string {
	if strings.HasPrefix(arg, "mem.") {
		return arg
	}
	if strings.HasPrefix(arg, "mem") && len(arg) > 3 {
		return "mem." + strings.TrimPrefix(arg, "mem")
	}
	return arg
}

func (w *World) pathBW(p model.Path) uint64 {
	var min uint64
	for i, id := range p.LinkIDs {
		bw := w.LinkBW[id]
		if i == 0 || bw < min {
			min = bw
		}
	}
	if cap, ok := w.RNICCap[p.RNIC]; ok && (min == 0 || cap < min) {
		min = cap
	}
	if (w.ACS || w.ATSOff[p.RNIC]) && gpuEndpoint(w, p.Endpoint) {
		hairpin := w.Bases[model.DistanceCrossRoot].BandwidthBPS
		if min == 0 || hairpin < min {
			min = hairpin
		}
	}
	n, _ := w.Graph.RNIC(p.RNIC)
	if min == 0 || n.LineRate < min {
		if n.LineRate > 0 {
			min = n.LineRate
		}
	}
	// P2P / baseline also caps achievable BW (outstanding read limit).
	base := baseline.ForPath(w.Bases, w.levelOf(p))
	if base.BandwidthBPS > 0 && base.BandwidthBPS < min {
		min = base.BandwidthBPS
	}
	if w.Busy[p.RNIC] && w.ServiceLoad > 0 {
		stolen := uint64(float64(n.LineRate) * w.ServiceLoad)
		if stolen < min {
			min -= stolen
		} else {
			min = min / 4
		}
	}
	return min
}

// levelOf is the distance whose Fig.4 level p reads at: a GPU path forced
// through the root complex (ACS, ATS off) reads at cross_root, and a path
// across a fast root-port pair at same_switch. Otherwise its own distance.
func (w *World) levelOf(p model.Path) model.Distance {
	if (w.ACS || w.ATSOff[p.RNIC]) && gpuEndpoint(w, p.Endpoint) {
		return model.DistanceCrossRoot
	}
	if len(w.FastPairs) > 0 {
		if pair, ok := w.Graph.RootPortPair(p); ok && w.FastPairs[pair] {
			return model.DistanceSameSwitch
		}
	}
	return p.Distance
}

func rootPortLink(w *World, id string) bool {
	l, ok := w.Graph.Link(id)
	return ok && l.Kind == model.LinkKindRootPort
}

func (w *World) pathLatNs(p model.Path) uint64 {
	lat := baseline.ForPath(w.Bases, w.levelOf(p)).LatencyNs
	for _, id := range p.LinkIDs {
		lat += w.LinkLatNs[id]
	}
	return lat
}

func (w *World) PathBW(p model.Path) uint64    { return w.pathBW(p) }
func (w *World) PathLatNs(p model.Path) uint64 { return w.pathLatNs(p) }

func gpuEndpoint(w *World, id string) bool {
	e, ok := w.Graph.Endpoint(id)
	return ok && e.Kind == model.EndpointGPU
}

// Loopback implements paper Eq.1 for one RNIC→endpoint pair.
func (w *World) Loopback(rnic, endpoint string, small, large uint32) (model.PathMeasurement, error) {
	p, ok := w.Graph.Path(rnic, endpoint)
	if !ok {
		return model.PathMeasurement{}, fmt.Errorf("no path %s -> %s", rnic, endpoint)
	}
	bw := w.pathBW(p)
	host := w.pathLatNs(p)
	tproc := baseline.TprocNs
	if host >= tproc {
		host -= tproc
	}
	smallLat := LatencyNs(tproc, host, uint64(small), bw)
	largeLat := LatencyNs(tproc, host, uint64(large), bw)
	return model.PathMeasurement{
		RNIC:          rnic,
		Endpoint:      endpoint,
		SmallLatNsP50: smallLat,
		SmallLatNsP99: P99FromP50(smallLat),
		LargeLatNsP50: largeLat,
		BandwidthBPS:  BandwidthBPS(uint64(large), largeLat, smallLat),
		Verbs:         w.Verbs(endpoint),
	}, nil
}

// Verbs is the setup verb time a loopback probe to endpoint reports, from
// any RNIC: registration time belongs to the endpoint's memory. The Zig
// simulator echoes these back through the C ABI rather than modelling them,
// so this is the single source for both simulated probers.
func (w *World) Verbs(endpoint string) model.VerbNs {
	v := model.VerbNs{MRReg: simHostMRRegNs, QPSetup: simQPSetupNs}
	if gpuEndpoint(w, endpoint) {
		v.GPUAlloc = simGPUAllocNs
		v.MRReg = simGPUMRRegNs
	}
	if f := w.SlowMR[endpoint]; f > 0 {
		v.MRReg *= f
	}
	return v
}

// simMutualRTTnsP50 is the idle intra-host RoCE RTT the simulator reports.
const simMutualRTTnsP50 uint64 = 4000

func (w *World) Mutual(src, dst string) model.MutualMeasurement {
	m := model.MutualMeasurement{
		Src: src, Dst: dst, Connected: true,
		RTTnsP50: simMutualRTTnsP50, RTTnsP99: MutualP99FromP50(simMutualRTTnsP50),
		Sent: 64, Recv: 64,
	}
	if w.ConnectFail[src] || w.ConnectFail[dst] || w.Isolated[src] != w.Isolated[dst] {
		// A QP that never connected has no RTT and sent nothing; reporting a
		// latency for it would be inventing a measurement that did not happen.
		return model.MutualMeasurement{Src: src, Dst: dst, Connected: false, LossRatio: 1}
	}
	ppm := w.DropPPM[src]
	if w.DropPPM[dst] > ppm {
		ppm = w.DropPPM[dst]
	}
	if ppm > 0 {
		lost := uint32(uint64(m.Sent) * uint64(ppm) / 1_000_000)
		if lost >= m.Sent {
			lost = m.Sent
		}
		m.Recv = m.Sent - lost
		m.LossRatio = float64(lost) / float64(m.Sent)
	}
	return m
}

// P99FromP50 is the synthetic loopback tail. Eq.1 is deterministic, so there
// is no measured distribution to take a percentile of; the simulator reports
// p50 plus 5%. sim.zig's p99FromP50 must stay identical — the same world is
// simulated in Zig when --sim runs through the C ABI, and
// TestZigSimMatchesGoSim in internal/zigprobe locks the two together.
func P99FromP50(p50 uint64) uint64 { return p50 + p50/20 }

// MutualP99FromP50 is the same synthetic tail for mutual RTT, matching
// sim.zig's mutualP99FromP50.
func MutualP99FromP50(p50 uint64) uint64 { return p50 + p50/4 }

// LatencyNs is NSDI'23 Eq.1: Tproc + Lathost + Size/BWhost.
func LatencyNs(tproc, lathost, sizeBytes, bwBPS uint64) uint64 {
	return tproc + lathost + TransferNs(sizeBytes, bwBPS)
}

func TransferNs(sizeBytes, bwBPS uint64) uint64 {
	if bwBPS == 0 {
		return 1 << 32
	}
	return (sizeBytes * 8 * 1_000_000_000) / bwBPS
}

func BandwidthBPS(sizeBytes, latLargeNs, latSmallNs uint64) uint64 {
	if latLargeNs <= latSmallNs {
		return 0
	}
	dt := latLargeNs - latSmallNs
	return (sizeBytes * 8 * 1_000_000_000) / dt
}

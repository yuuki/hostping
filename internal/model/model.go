// Package model holds Hostping domain types shared by the engine, analyzer, and telemetry.
package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoGDR is returned when a GPU endpoint is requested but GPUDirect RDMA
// registration is not available. The engine skips that path instead of aborting.
var ErrNoGDR = errors.New("gpu endpoint requires GPUDirect RDMA")

// Live verbs failures that must not abort the rest of the host mesh.
var (
	ErrNoDevice = errors.New("no rdma device")
	ErrNoGID    = errors.New("no active rocev2 gid")
	ErrProbe    = errors.New("probe failed")
	// ErrNoLID is an ACTIVE InfiniBand port with LID 0: no subnet manager
	// has configured it, so it has no address to loop back through.
	ErrNoLID = errors.New("infiniband port has no lid")
	// ErrPortNotActive is an InfiniBand RNIC none of whose ports is ACTIVE.
	ErrPortNotActive = errors.New("infiniband port not active")
)

// ErrPortLoopback marks a loopback that failed at an InfiniBand port: its
// WRITEs completed with a port-level transport error (retry counter exceeded,
// a remote error, a response timeout) in every addressing mode the probe
// tried. Every other probe of that port would wait out the same hardware
// retry budget, so the engine stops probing the RNIC for the rest of the run.
// Wrap an error with PortLoopbackError rather than matching this directly.
var ErrPortLoopback = errors.New("loopback failed at the port")

// PortLoopbackError marks err as an ErrPortLoopback failure. The text is
// err's own, so skip reasons read as before; errors.Is matches both err's
// sentinels and ErrPortLoopback.
func PortLoopbackError(err error) error {
	if err == nil {
		return nil
	}
	return portLoopbackErr{err: err}
}

type portLoopbackErr struct{ err error }

func (e portLoopbackErr) Error() string   { return e.err.Error() }
func (e portLoopbackErr) Unwrap() []error { return []error{e.err, ErrPortLoopback} }

// Port addressing modes a live probe reports (PortAddressing.Mode).
const (
	// AddrRoCEv2GID addresses a RoCE port by a RoCEv2 GID, with a GRH.
	AddrRoCEv2GID = "rocev2_gid"
	// AddrIBLID addresses an InfiniBand port by its LID, with no GRH.
	AddrIBLID = "ib_lid"
	// AddrIBLIDGRH addresses an InfiniBand port by its LID plus a GRH over
	// GID index 0; tried only when the LID-only connect is refused.
	AddrIBLIDGRH = "ib_lid_grh"
)

// Link layers of an RNIC port, as sysfs ports/N/link_layer spells them.
const (
	LinkLayerEthernet   = "Ethernet"
	LinkLayerInfiniBand = "InfiniBand"
)

// PortAddressing is how a live loopback probe addressed its RNIC port. Mode
// is empty when no live probe ran (the simulator).
type PortAddressing struct {
	Mode string
	Port int
	// LID is the port's LID on InfiniBand, 0 on RoCE.
	LID int
}

// ErrComputeMode is the engine's reason for not probing a GPU whose compute
// mode is not Default: on an EXCLUSIVE_PROCESS GPU hostping's own context
// would either fail or lock out the job that owns the GPU, and a PROHIBITED
// GPU admits no context at all. The skip is reported like any other.
var ErrComputeMode = errors.New("compute_mode")

// GPU compute modes, spelled as nvidia-smi --query-gpu=compute_mode prints
// them. Exclusive_Thread is the pre-CUDA 8 mode, kept so an old driver is not
// read as unknown.
const (
	ComputeModeDefault          = "Default"
	ComputeModeExclusiveProcess = "Exclusive_Process"
	ComputeModeExclusiveThread  = "Exclusive_Thread"
	ComputeModeProhibited       = "Prohibited"
)

// ComputeModeBlocksProbe reports whether a GPU in mode must not be probed. An
// empty or unrecognised mode is unknown and does not block: a host without
// nvidia-smi, or a GPU without the notion (AMD), keeps being probed as before.
func ComputeModeBlocksProbe(mode string) bool {
	switch mode {
	case ComputeModeExclusiveProcess, ComputeModeExclusiveThread, ComputeModeProhibited:
		return true
	}
	return false
}

// LinkStatus is the binary-tomography state of an intra-host link (NSDI'23 Algorithm 1).
type LinkStatus int

const (
	LinkUncertain LinkStatus = iota
	LinkNormal
	LinkAbnormal
	LinkGray
)

func (s LinkStatus) String() string {
	switch s {
	case LinkUncertain:
		return "uncertain"
	case LinkNormal:
		return "normal"
	case LinkAbnormal:
		return "abnormal"
	case LinkGray:
		return "gray"
	default:
		return "unknown"
	}
}

// PathStatus is the measured status of an RNIC→endpoint path.
type PathStatus int

const (
	PathUncertain PathStatus = iota
	PathNormal
	PathAbnormal
)

func (s PathStatus) String() string {
	switch s {
	case PathUncertain:
		return "uncertain"
	case PathNormal:
		return "normal"
	case PathAbnormal:
		return "abnormal"
	default:
		return "unknown"
	}
}

// PathUncertainReason is why a path ended up PathUncertain.
//
// "uncertain" alone conflates two very different answers: a path hostping
// declined to judge because the evidence it had could not prove health, and a
// path it could not measure against anything at all. Every GPU is affinitive to
// exactly one RNIC, so on a multi-GPU host most of the RNIC→GPU mesh sits in the
// first group on every healthy run; without a reason an operator cannot tell
// that steady state from a real gap.
type PathUncertainReason int

const (
	// UncertainNone is a decided path (normal or abnormal).
	UncertainNone PathUncertainReason = iota
	// UncertainNoPath: the topology graph has no RNIC→endpoint route for this
	// measurement, so there is no distance, baseline or hop list to judge it by.
	UncertainNoPath
	// UncertainRNICBusy: the RNIC was carrying traffic, so the idle baseline
	// does not apply. The path either sits outside the reduced busy probe set,
	// or matched its affinitive siblings — which rules out a fault that only
	// this path sees, but cannot show the RNIC-wide load is not hiding one.
	UncertainRNICBusy
	// UncertainNonAffinitive: an idle-mode GPU that is not under the RNIC's
	// root port and cleared its reduced ❸/❹ baseline. That baseline is a floor
	// for a cross-root/cross-socket hop, not evidence of a clean path (NSDI'23
	// Fig.8 gray), so the path is deliberately left unjudged.
	UncertainNonAffinitive
	// UncertainBelowCeiling: an idle-mode affinitive GPU that cleared its
	// distance baseline but not min(line rate, narrowest hop) × (1-AbnormalTh).
	// Nothing is wrong enough to call abnormal, yet a baseline — which an
	// operator may have set loose — is not the evidence of health the ceiling
	// is, so the path is left unjudged. Unlike UncertainNonAffinitive this is
	// not the steady state of a healthy host: an affinitive path reaches its
	// ceiling when nothing is broken, so a path that keeps landing here is
	// running short of what its hardware carries.
	UncertainBelowCeiling
	// UncertainNoCeiling: an idle-mode affinitive GPU that cleared its distance
	// baseline on an RNIC whose line rate is unknown (0), so there is no ceiling
	// to hold it to. The path may well be healthy; hostping cannot tell, and the
	// fix is in discovery rather than on the path.
	UncertainNoCeiling
	// UncertainHostShift: an idle-judged path the sweep read below its
	// baseline whose paired bandwidth re-read (BandwidthRecheck) read level
	// with paths of the same distance class on other RNICs, yet still below
	// the baseline. The host as a whole read lower than when the baseline was
	// measured, so the path cannot be judged against it.
	UncertainHostShift
	// UncertainCPUBusy: a path the sweep read abnormal that transits a CPU
	// package whose CPUs were busy above --cpu-busy-high on the poll that
	// started the run (CPULoad). CPU load on a package contends for its
	// on-die fabric, which every path through the package crosses (RNIC to
	// memory, local or remote, and RNIC to GPU across the package), and no
	// RNIC counter or bus utilization shows it. A drop on such a path is not
	// evidence of a hardware fault, so it cannot feed Algorithm 1 or a cause.
	UncertainCPUBusy
)

func (r PathUncertainReason) String() string {
	switch r {
	case UncertainNone:
		return "none"
	case UncertainNoPath:
		return "no_path"
	case UncertainRNICBusy:
		return "rnic_busy"
	case UncertainNonAffinitive:
		return "non_affinitive"
	case UncertainBelowCeiling:
		return "below_ceiling"
	case UncertainNoCeiling:
		return "no_ceiling"
	case UncertainHostShift:
		return "host_shift"
	case UncertainCPUBusy:
		return "cpu_busy"
	default:
		return "unknown"
	}
}

func (s LinkStatus) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

func (s PathStatus) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

func (r PathUncertainReason) MarshalJSON() ([]byte, error) {
	return []byte(`"` + r.String() + `"`), nil
}

// LatencyStatus is a path's small-message latency verdict, kept apart from the
// bandwidth-derived PathStatus that Algorithm 1 reads.
//
// NSDI'23 §3.2 names latency increase as a symptom of its own, and §5 applies
// Abnormal_th to "latency or bandwidth". hostping judges the two separately
// because only bandwidth has a default baseline that scales to the hardware:
// the Fig.4 latencies were measured on one server and do not carry over to
// another, so a path is judged on latency only against a latency baseline the
// operator supplied, or against its siblings. A verdict that weak must not let
// the bandwidth tomography paint hops abnormal, so it stays advisory.
type LatencyStatus int

const (
	LatencyUnknown LatencyStatus = iota
	LatencyNormal
	LatencyAbnormal
)

func (s LatencyStatus) String() string {
	switch s {
	case LatencyUnknown:
		return "unknown"
	case LatencyNormal:
		return "normal"
	case LatencyAbnormal:
		return "abnormal"
	default:
		return "invalid"
	}
}

// LatencyReason says what a latency verdict was judged against or, for
// LatencyUnknown, why the latency could not be judged.
type LatencyReason int

const (
	// LatencyReasonNone is a path that was never classified.
	LatencyReasonNone LatencyReason = iota
	// LatencyByBaseline: judged against the latency the operator set for the
	// path's distance (--baseline KEY=BW,LAT or --baseline-file).
	LatencyByBaseline
	// LatencyBySiblings: no latency baseline was set, so judged against the
	// median of the host's other paths at the same distance.
	LatencyBySiblings
	// LatencyBusy: a busy-mode run. Service traffic inflates a loaded RNIC's
	// posting overhead (Tproc, Eq.1), so its latency says nothing about the path.
	LatencyBusy
	// LatencyFewSiblings: no latency baseline, and too few other paths at the
	// same distance to take a median from.
	LatencyFewSiblings
	// LatencyNoPath: no RNIC→endpoint route in the topology graph, so no distance.
	LatencyNoPath
	// LatencyNoReading: the probe reported no small-message latency.
	LatencyNoReading
	// LatencyRecheckCleared: the sweep read the path abnormal against its
	// siblings, but a paired re-measurement interleaved with siblings of its
	// class read it within --abnormal-th and the floor of them, so the first
	// reading was the time it was taken in, not the path. LatencyRecheck
	// carries the re-reads.
	LatencyRecheckCleared
	// LatencyLocalityShift: the path is still abnormal against its siblings
	// after the recheck, but it reads within --abnormal-th and the floor of
	// the same-class paths of the other RNICs on its RNIC's NUMA node, so the
	// rise is the node's platform state, not the path. LatencyLocality carries
	// that reference.
	LatencyLocalityShift
	// LatencyCPUBusy: the path read abnormal, but it transits a CPU package
	// whose CPUs were busy above --cpu-busy-high when the run started
	// (UncertainCPUBusy), so the rise says nothing about the path.
	LatencyCPUBusy
)

func (r LatencyReason) String() string {
	switch r {
	case LatencyReasonNone:
		return "none"
	case LatencyByBaseline:
		return "user_baseline"
	case LatencyBySiblings:
		return "siblings"
	case LatencyBusy:
		return "busy"
	case LatencyFewSiblings:
		return "few_siblings"
	case LatencyNoPath:
		return "no_path"
	case LatencyNoReading:
		return "no_reading"
	case LatencyRecheckCleared:
		return "recheck_cleared"
	case LatencyLocalityShift:
		return "locality_shift"
	case LatencyCPUBusy:
		return "cpu_busy"
	default:
		return "unknown"
	}
}

func (s LatencyStatus) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

func (r LatencyReason) MarshalJSON() ([]byte, error) {
	return []byte(`"` + r.String() + `"`), nil
}

// EndpointKind is a Hostping loopback target: GPU video memory or a NUMA memory node.
type EndpointKind string

const (
	EndpointGPU    EndpointKind = "gpu"
	EndpointMemory EndpointKind = "memory"
)

// Trigger is why the Hostping engine ran (hardware monitor, §4.2).
type Trigger string

const (
	TriggerIdle     Trigger = "idle"
	TriggerPFC      Trigger = "pfc"
	TriggerDrops    Trigger = "packet_drops"
	TriggerManual   Trigger = "manual"
	TriggerPeriodic Trigger = "periodic"
	// TriggerMutualTimeout is the scheduled mutual probe's window showing a
	// pair above Timeout_th (ToN'24 §IV-A-2).
	TriggerMutualTimeout Trigger = "mutual_timeout"
)

// Distance is the GDR/communication distance from NSDI'23 Fig.2 / Fig.4.
type Distance int

const (
	DistanceSameSwitch  Distance = 1
	DistanceMultiSwitch Distance = 2
	DistanceCrossRoot   Distance = 3
	DistanceCrossSocket Distance = 4
	DistanceLocalMem    Distance = 10
	DistanceRemoteMem   Distance = 11
)

// Distances is the Fig.4 / memory order used by flags, files, and diagnose output.
func Distances() []Distance {
	return []Distance{
		DistanceSameSwitch,
		DistanceMultiSwitch,
		DistanceCrossRoot,
		DistanceCrossSocket,
		DistanceLocalMem,
		DistanceRemoteMem,
	}
}

// Key is the operator-facing distance name (same_switch, local_mem, …).
func (d Distance) Key() string {
	switch d {
	case DistanceSameSwitch:
		return "same_switch"
	case DistanceMultiSwitch:
		return "multi_switch"
	case DistanceCrossRoot:
		return "cross_root"
	case DistanceCrossSocket:
		return "cross_socket"
	case DistanceLocalMem:
		return "local_mem"
	case DistanceRemoteMem:
		return "remote_mem"
	default:
		return ""
	}
}

func (d Distance) String() string {
	if k := d.Key(); k != "" {
		return k
	}
	return "unknown"
}

// ParseDistance accepts the --baseline / file keys.
func ParseDistance(s string) (Distance, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "same_switch":
		return DistanceSameSwitch, nil
	case "multi_switch":
		return DistanceMultiSwitch, nil
	case "cross_root":
		return DistanceCrossRoot, nil
	case "cross_socket":
		return DistanceCrossSocket, nil
	case "local_mem":
		return DistanceLocalMem, nil
	case "remote_mem":
		return DistanceRemoteMem, nil
	default:
		return 0, fmt.Errorf("unknown distance %q (want same_switch, multi_switch, cross_root, cross_socket, local_mem, remote_mem)", s)
	}
}

// Link is one intra-host hop (PCIe, root port, memory channel, UPI/xGMI).
type Link struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"` // pcie_rnic, pcie_gpu, pcie_switch, root_port, memory, upi, snc_mesh
	Device   string `json:"device,omitempty"`
	Socket   int    `json:"socket"`
	RootPort string `json:"root_port,omitempty"`
	// CapacityBPS is the healthy bandwidth of this hop. Discovery fills device
	// (RNIC, GPU) and root-port hops from what the link can train to (min of
	// device and upstream port max_link_*), not from what it trained to, so a
	// down-trained link is not its own baseline. Switch hops get a nominal
	// constant: sysfs pairs a downstream port with the wrong link partner and
	// may report fictitious speeds for in-package links between switch levels.
	CapacityBPS uint64 `json:"capacity_bps"`
	// NegotiatedBPS is the effective bandwidth the PCIe link had trained to
	// when the graph was discovered: evidence only, never a threshold. The
	// analyzer re-reads the live link every run. 0 means unknown, not PCIe,
	// or a switch hop (whose sysfs link speed is not trusted).
	NegotiatedBPS uint64 `json:"negotiated_bps,omitempty"`
	// CapacityAssumed marks a CapacityBPS that discovery could not read and
	// filled with a nominal constant: a PCIe hop whose sysfs states no
	// capability, every switch hop, and every memory channel and inter-socket
	// bus; an snc_mesh link is assumed with no constant at all (0, unknown).
	// An assumed capacity still bounds baselines, but bus utilization is never
	// computed against it: dividing a measured rate by a guess could call a
	// busy link overloaded, or an overloaded one fine.
	CapacityAssumed bool `json:"capacity_assumed,omitempty"`
	// Upstream is the node at the CPU-side end of a PCIe device or switch hop:
	// the switch (its sw.* link ID) or root port (rp.* link ID) the hop hangs
	// off. Discovery fills it from the sysfs parent chain so a cascaded switch
	// sits under the switch above it. Empty means unknown (a hand-written
	// topology file); the topology diagram then falls back to the device's
	// Switch, then its root port. It is never read by the analyzer.
	Upstream string `json:"upstream,omitempty"`
}

// Kinds of a node of the host topology diagram (TopologyNode.Kind).
const (
	TopoNodeSocket   = "socket"
	TopoNodeNUMA     = "numa"
	TopoNodeMemory   = "memory"
	TopoNodeRootPort = "root_port"
	TopoNodeSwitch   = "switch"
	TopoNodeRNIC     = "rnic"
	TopoNodeGPU      = "gpu"
)

// TopoEdgeNoLink is the TopologyEdge.Link of a structural edge, one that is no
// measured hop (a NUMA node's membership of its package).
const TopoEdgeNoLink = "none"

// TopoEdgeKindPackage is the TopologyEdge.Kind of the structural edge from a
// CPU package to one of its NUMA nodes under sub-NUMA clustering.
const TopoEdgeKindPackage = "package"

// TopologyNode is a vertex of the host topology diagram: a CPU package, a NUMA
// node of a package under sub-NUMA clustering, a memory node, a root port, a
// PCIe switch, an RNIC or a GPU. NUMA and PCIeBDF are -1 and "" where they do
// not apply.
type TopologyNode struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Socket  int    `json:"socket"`
	NUMA    int    `json:"numa"`
	PCIeBDF string `json:"pcie_bdf,omitempty"`
}

// TopologyEdge is an edge of the host topology diagram, directed upstream
// (CPU side) to downstream (device side), and lower to higher socket or NUMA
// number between two CPU nodes. Link is the Link.ID it draws, or
// TopoEdgeNoLink for a structural edge.
type TopologyEdge struct {
	ID              string `json:"id"`
	Link            string `json:"link"`
	Source          string `json:"source"`
	Target          string `json:"target"`
	Kind            string `json:"kind"`
	CapacityBPS     uint64 `json:"capacity_bps"`
	NegotiatedBPS   uint64 `json:"negotiated_bps"`
	CapacityAssumed bool   `json:"capacity_assumed"`
}

// Endpoint is a GPU or memory node that can be registered via ibv_reg_mr.
type Endpoint struct {
	ID       string       `json:"id"`
	Kind     EndpointKind `json:"kind"`
	Index    int          `json:"index"`
	Socket   int          `json:"socket"`
	RootPort string       `json:"root_port,omitempty"`
	NUMA     int          `json:"numa"`
	PCIeBDF  string       `json:"pcie_bdf,omitempty"`
	Switch   string       `json:"switch,omitempty"`
}

// RNIC is an RDMA-capable NIC.
type RNIC struct {
	ID       string `json:"id"`
	Device   string `json:"device"` // mlx5_0
	Netdev   string `json:"netdev"`
	Socket   int    `json:"socket"` // CPU package; the NUMA node unless sub-NUMA clustering is on
	NUMA     int    `json:"numa"`
	RootPort string `json:"root_port"`
	PCIeBDF  string `json:"pcie_bdf,omitempty"`
	LineRate uint64 `json:"line_rate_bps"`
	// PCIeCapBPS is the healthy bandwidth of the PCIe hop that attaches this
	// RNIC. Every intra-host loopback from it crosses that hop, so it bounds
	// what any path can reach no matter how fast the wire is. Topology
	// discovery fills it from the pcie_rnic link; 0 means unknown.
	PCIeCapBPS uint64 `json:"pcie_capacity_bps,omitempty"`
	GIDIndex   int    `json:"gid_index,omitempty"`
	// LinkLayer is the probed port's link layer (LinkLayerEthernet or
	// LinkLayerInfiniBand). Empty means unknown.
	LinkLayer string `json:"link_layer,omitempty"`
	// Fabric is a same-rail key: the IPv4 subnet of the RoCEv2 GID's netdev
	// (v4:a.b.c.0 for a /24, v4:<network>/<len> otherwise), a GID prefix, or
	// an InfiniBand subnet prefix ("ib:"). Empty means unknown.
	Fabric string `json:"fabric,omitempty"`
	Switch string `json:"switch,omitempty"`
}

// ExcludedRNIC is an RDMA device discovery dropped on the operator's request
// (--exclude-rnic): it is never probed, paired, linked or exported, and is
// listed only so the decision stays visible.
type ExcludedRNIC struct {
	Device  string `json:"device"`
	PCIeBDF string `json:"pcie_bdf,omitempty"`
	// Reason is ExcludeReasonOperator.
	Reason string `json:"reason"`
}

// ExcludeReasonOperator marks an RNIC excluded by --exclude-rnic.
const ExcludeReasonOperator = "operator"

// Path is the ordered intra-host hop list from an RNIC to an endpoint.
type Path struct {
	RNIC     string   `json:"rnic"`
	Endpoint string   `json:"endpoint"`
	LinkIDs  []string `json:"link_ids"`
	Distance Distance `json:"distance"`
	// Affinitive is true for memory nodes and GPUs under the same CPU root port as the RNIC.
	Affinitive bool `json:"affinitive"`
}

// ContainsLink reports whether the path traverses id.
func (p Path) ContainsLink(id string) bool {
	for _, l := range p.LinkIDs {
		if l == id {
			return true
		}
	}
	return false
}

// LinkKindRootPort is the Link.Kind of a CPU root-port hop.
const LinkKindRootPort = "root_port"

// Link kinds of the buses between NUMA nodes. LinkKindUPI joins two CPU
// packages (UPI, xGMI). LinkKindSNCMesh joins two NUMA nodes of one package
// under sub-NUMA clustering (Intel SNC, AMD NPS > 1): traffic between them
// stays on the package's own fabric and crosses no inter-socket bus.
const (
	LinkKindUPI     = "upi"
	LinkKindSNCMesh = "snc_mesh"
)

// RootPortPair is the unordered pair of root-port link IDs a path crosses,
// held sorted so that {a, b} and {b, a} are one key. It keys the learned
// sub-classes of a distance (see the baseline package). The zero value is no
// pair.
type RootPortPair [2]string

// NewRootPortPair returns the pair {a, b} in its canonical (sorted) order.
func NewRootPortPair(a, b string) RootPortPair {
	if b < a {
		a, b = b, a
	}
	return RootPortPair{a, b}
}

// IsZero reports the absent pair.
func (p RootPortPair) IsZero() bool { return p == RootPortPair{} }

func (p RootPortPair) String() string { return p[0] + "+" + p[1] }

// PathMeasurement is one loopback result (small-message latency + derived bandwidth).
type PathMeasurement struct {
	RNIC     string `json:"rnic"`
	Endpoint string `json:"endpoint"`
	// EndpointKind is set by the engine from the topology graph.
	EndpointKind  EndpointKind `json:"endpoint_kind,omitempty"`
	SmallLatNsP50 uint64       `json:"small_lat_ns_p50"`
	SmallLatNsP99 uint64       `json:"small_lat_ns_p99"`
	LargeLatNsP50 uint64       `json:"large_lat_ns_p50"`
	BandwidthBPS  uint64       `json:"bandwidth_bps"`
	Status        PathStatus   `json:"status"`
	// UncertainReason narrows Status == PathUncertain. It is omitted on a
	// decided path, where it carries no meaning.
	UncertainReason PathUncertainReason `json:"uncertain_reason,omitempty"`
	// BusyRecheck is the paired re-read of a loaded RNIC's path the busy
	// sweep read abnormal against its siblings; nil when the path was not
	// rechecked. A path it did not confirm is uncertain with rnic_busy.
	BusyRecheck *BusyRecheck `json:"busy_recheck,omitempty"`
	// BWRecheck is the paired re-read of the group (RNIC, distance class) of
	// a path the sweep read abnormal against its idle baseline; nil when the
	// group was not rechecked. Every path of a rechecked group carries the
	// same record, and its Outcome decided the path's Status.
	BWRecheck *BandwidthRecheck `json:"bw_recheck,omitempty"`
	// LatencyStatus is the verdict on SmallLatNsP50, and LatencyReason what it
	// was judged against or why it was not. It is advisory: it never feeds
	// Status or Algorithm 1.
	LatencyStatus LatencyStatus `json:"latency_status"`
	LatencyReason LatencyReason `json:"latency_reason,omitempty"`
	// LatencyRefNs is the latency the path was judged against; 0 when unjudged.
	// A path cleared by the recheck keeps the sweep's sibling median here.
	LatencyRefNs uint64 `json:"latency_ref_ns,omitempty"`
	// LatencyRecheck is the paired re-measurement of a path the sweep read
	// abnormal against its siblings; nil when the path was not rechecked.
	LatencyRecheck *LatencyRecheck `json:"latency_recheck,omitempty"`
	// LatencyRecheckSkipped says why a path still abnormal against its
	// siblings was not rechecked although its class was: "cap" when the run
	// reached the recheck's per-run path cap. Such a verdict was never
	// confirmed, so it neither extends LatencyStreak (which holds its value)
	// nor becomes a cause in this run. Empty otherwise.
	LatencyRecheckSkipped LatencyRecheckSkip `json:"latency_recheck_skipped,omitempty"`
	// LatencyLocality is the reference that cleared a sibling verdict as a
	// rise shared by the RNICs of one NUMA node (LatencyLocalityShift); nil
	// otherwise.
	LatencyLocality *LatencyLocality `json:"latency_locality,omitempty"`
	// LatencyStreak is how many consecutive idle runs, this one included,
	// have read the path abnormal against its siblings (after the recheck and
	// the locality shift); 0 when this run did not, and the held count when
	// the recheck's cap left the path out (LatencyRecheckSkipped). path_latency_abnormal
	// reports a sibling verdict only once the streak reaches three (or, in a
	// one-shot run, when the paired recheck confirmed it), so a path below
	// that is abnormal here but named by no cause.
	LatencyStreak int `json:"latency_streak,omitempty"`
	// BaselineClass names the learned sub-class whose baseline judges the
	// path, as "distance/name" (e.g. "cross_root/fast"): the baseline file
	// lists the path's root-port pair in that sub-class of its distance.
	// Empty means the distance's own baseline. Busy mode compares the path
	// only with siblings of the same class.
	BaselineClass string `json:"baseline_class,omitempty"`
	// Verbs is how long the probe's setup verbs took. It is advisory: it
	// never feeds Status or Algorithm 1. Omitted when the probe reported none.
	Verbs VerbNs `json:"verb_ns,omitzero"`
	// Addressing is how the live probe addressed the RNIC port. The engine
	// logs it; it is not part of the report.
	Addressing PortAddressing `json:"-"`
}

// LatencyRecheckSkip is why a path flagged against its siblings, in a class
// the recheck covered, was not rechecked.
type LatencyRecheckSkip string

// LatencyRecheckSkipCap: the run had more candidates than the recheck's
// per-run path cap, and this path was not among those selected.
const LatencyRecheckSkipCap LatencyRecheckSkip = "cap"

// LatencyRecheck is the paired re-measurement of one path whose sweep reading
// was latency-abnormal against its siblings: the path re-read in turn with
// siblings of its class, preferably on other RNICs, so both sides are read in
// the same time window. It changes only the latency verdict; the sweep's
// readings (SmallLatNsP50, BandwidthBPS, ...) stay as they were.
type LatencyRecheck struct {
	// PathNsP50 is the median of the path's re-read small-message p50s.
	PathNsP50 uint64 `json:"path_ns_p50"`
	// SiblingNsP50 is the median of the interleaved sibling re-reads.
	SiblingNsP50 uint64 `json:"sibling_ns_p50"`
	// Siblings names the sibling paths read, as "rnic->endpoint", in order.
	Siblings []string `json:"siblings"`
	// Confirmed is true when the re-reads still exceed the siblings by
	// --abnormal-th and the sibling floor, keeping the abnormal verdict.
	Confirmed bool `json:"confirmed"`
}

// BusyRecheck is the paired re-read of one busy-probed path whose busy sweep
// reading was abnormal against the median of its siblings (same loaded RNIC,
// distance and sub-class): the path re-read in rounds, each round reading the
// path and then every sibling once, so both sides share a time window. It
// changes only the bandwidth verdict; the sweep's readings (BandwidthBPS, ...)
// stay as they were.
type BusyRecheck struct {
	// PathBPS is the median of the path's re-read bandwidths.
	PathBPS uint64 `json:"path_bps"`
	// SiblingBPS is the median of every sibling re-read of the same rounds.
	SiblingBPS uint64 `json:"sibling_bps"`
	// Siblings names the sibling paths read in each round, as
	// "rnic->endpoint", in order.
	Siblings []string `json:"siblings"`
	// Rounds is how many rounds were read.
	Rounds int `json:"rounds"`
	// Confirmed is true when the re-reads still sit --abnormal-th below the
	// siblings, keeping the abnormal verdict.
	Confirmed bool `json:"confirmed"`
}

// BWRecheckOutcome is what a BandwidthRecheck decided for its group.
type BWRecheckOutcome string

const (
	// BWRecheckConfirmed: the representative still read --abnormal-th below
	// the references. The group stays abnormal.
	BWRecheckConfirmed BWRecheckOutcome = "confirmed"
	// BWRecheckCleared: the representative read level with the references
	// and met its baseline. The group is judged as a path that met it.
	BWRecheckCleared BWRecheckOutcome = "cleared"
	// BWRecheckHostShift: the representative read level with the references
	// and below its baseline, whether or not the references cleared it. The
	// group is uncertain with host_shift.
	BWRecheckHostShift BWRecheckOutcome = "host_shift"
)

// BandwidthRecheck is the paired re-read of one group of idle-judged paths —
// one RNIC's paths of one distance class (sub-class included) that the sweep
// read abnormal against their baseline. A representative path of the group is
// re-read in rounds, interleaved with reference paths of the same class on
// other RNICs that met their baseline in the sweep, so both sides share a
// time window. It changes only the bandwidth verdict; the sweep's readings
// stay as they were.
type BandwidthRecheck struct {
	// Path names the representative re-read for the group, as
	// "rnic->endpoint".
	Path string `json:"path"`
	// PathBPS is the median of the representative's re-read bandwidths.
	PathBPS uint64 `json:"path_bps"`
	// RefBPS is the median of every reference re-read of the same rounds.
	RefBPS uint64 `json:"ref_bps"`
	// BaselineBPS is the idle baseline the group is judged against.
	BaselineBPS uint64 `json:"baseline_bps"`
	// Refs names the reference paths read in each round, as
	// "rnic->endpoint", in order.
	Refs []string `json:"refs"`
	// Rounds is how many rounds were read.
	Rounds int `json:"rounds"`
	// Outcome is what the re-reads decided for the group.
	Outcome BWRecheckOutcome `json:"outcome"`
}

// LatencyLocality is the locality reference of a path whose sibling verdict
// was cleared as a NUMA-node-wide rise: the same-class paths of the other
// RNICs on its RNIC's NUMA node, read in the sweep. The path's SmallLatNsP50
// did not exceed RefNs by --abnormal-th and the sibling floor.
type LatencyLocality struct {
	// NUMA is the NUMA node of the path's RNIC.
	NUMA int `json:"numa"`
	// RefNs is the median sweep small-message p50 of the locality siblings.
	RefNs uint64 `json:"ref_ns"`
	// RNICs names the other RNICs on the node whose paths formed RefNs.
	RNICs []string `json:"rnics"`
}

// VerbNs is the execution time of the RDMA verbs one loopback probe called
// before it could measure anything, in nanoseconds (ToN'24 §VI-C: slow verbs
// — e.g. GPU memory registration on a failing GPU — are a bottleneck of their
// own that latency and bandwidth never show). Zero means not measured.
type VerbNs struct {
	// GPUAlloc is the device lookup and allocation of the GPU buffer; always
	// 0 for a memory endpoint.
	GPUAlloc uint64 `json:"gpu_alloc,omitempty"`
	// MRReg is memory registration, summed over every MR the probe
	// registered (one over the GPU buffer, two over host buffers).
	MRReg uint64 `json:"mr_reg,omitempty"`
	// QPSetup is creating both loopback QPs and moving them to RTS,
	// including retries over candidate GIDs.
	QPSetup uint64 `json:"qp_setup,omitempty"`
	// MemWarmup is the --mem-warmup fill that ran before the timed
	// iterations of a memory endpoint's probe. It is not a verb; it is here
	// so the warm-up's cost is reported next to the setup it follows.
	MemWarmup uint64 `json:"mem_warmup,omitempty"`
}

// LinkState is Algorithm 1 output for one hop.
type LinkState struct {
	ID            string              `json:"id"`
	Status        LinkStatus          `json:"status"`
	AbnormalCount int                 `json:"abnormal_count"`
	AbnormalRNICs map[string]struct{} `json:"-"`
	// Utilization is the bus utilization (0–1) sampled before the run, or nil
	// when no live source could read it. Unknown is not 0: an unread bus is
	// evidence neither that it was quiet nor that it was overloaded.
	Utilization *float64 `json:"utilization,omitempty"`
	// Tested reports that this run put the hop through a loopback test: some
	// measured path decided normal or abnormal spoke about it. Status alone
	// cannot say so, because LinkUncertain covers both a hop no decided path
	// reached and one an abnormal path crossed without isolating it.
	Tested bool `json:"-"`
}

// Util returns the link's utilization and whether it was read.
func (l LinkState) Util() (float64, bool) {
	if l.Utilization == nil {
		return 0, false
	}
	return *l.Utilization, true
}

// UtilAtLeast reports whether the utilization was read and is at least th.
// An unknown utilization never explains a link as overloaded.
func (l LinkState) UtilAtLeast(th float64) bool {
	u, ok := l.Util()
	return ok && u >= th
}

// UtilEvidence renders the utilization for diagnosis evidence: "util=0.42",
// or "util=unknown" when no live source read it.
func (l LinkState) UtilEvidence() string {
	u, ok := l.Util()
	if !ok {
		return "util=unknown"
	}
	return fmt.Sprintf("util=%.2f", u)
}

// RootCause is a diagnosed intra-host / RNIC problem.
type RootCause struct {
	Code       string   `json:"code"`
	Summary    string   `json:"summary"`
	Links      []string `json:"links,omitempty"`
	RNICs      []string `json:"rnics,omitempty"`
	Endpoints  []string `json:"endpoints,omitempty"`
	Confidence string   `json:"confidence"` // high, medium, low
	Evidence   string   `json:"evidence"`
}

// Known bottleneck codes from NSDI'23 §6 and ToN'24 RNIC mutual probing.
const (
	CauseRNICPCIeFailure       = "rnic_pcie_failure"
	CauseGPUPCIeFailure        = "gpu_pcie_failure"
	CauseMemoryChannelFailure  = "memory_channel_failure"
	CauseUPIFailure            = "upi_failure"
	CauseCPURootPortFailure    = "cpu_root_port_failure"
	CauseMemoryChannelFlapping = "memory_channel_flapping"
	CauseACSEnabled            = "acs_enabled"
	CauseATSDisabled           = "ats_disabled"
	CauseRNICSlowStart         = "rnic_slow_start"
	CauseRNICTxWindow          = "rnic_tx_window"
	CauseOverloadedUPI         = "overloaded_intersocket_bus"
	CauseOverloadedMemory      = "overloaded_memory_channel"
	CauseRNICConnectFailure    = "rnic_connectivity_failure"
	CauseRNICIntermittent      = "rnic_intermittent_timeout"
	CauseToRSwitchAnomaly      = "tor_switch_anomaly"
	CauseRNICPacketDrops       = "rnic_packet_drops"
	CauseOverloadedRNICPCIe    = "overloaded_rnic_pcie"
	// CauseGPUMRRegistrationSlow is advisory (ToN'24 §VI-C): a GPU whose
	// memory registration takes far longer than its siblings'. It never
	// changes a path or link status.
	CauseGPUMRRegistrationSlow = "gpu_mr_registration_slow"
	// CausePathLatencyAbnormal is advisory (NSDI'23 §3.2, §4.1.1): paths whose
	// small-message latency rose past Abnormal_th while their bandwidth did
	// not degrade. It never changes a path or link status.
	CausePathLatencyAbnormal = "path_latency_abnormal"
	// CauseRNICPCIeUndersized is advisory: an RNIC whose PCIe link trained at
	// its maximum, but whose maximum cannot carry the line rate (a slot that
	// is too narrow or slow by design). It is not a fault and never changes a
	// path or link status.
	CauseRNICPCIeUndersized = "rnic_pcie_undersized"
	CauseUnknown            = "unknown"
)

// CauseRNICCrossRailUnreachable is reported from cross-rail mutual pairs only
// (--mutual-scope=host): RNICs of the host that cannot reach each other across
// subnets. Whether they should is a property of the fabric the host cannot
// see, so it is never high confidence.
const CauseRNICCrossRailUnreachable = "rnic_cross_rail_unreachable"

// CauseSNCMeshFailure is a fault Algorithm 1 localizes to an snc_mesh link:
// the fabric between two NUMA nodes of one package under sub-NUMA clustering.
// No inter-socket bus is involved, so it is never upi_failure. It is judged by
// the upi_failure rules.
const CauseSNCMeshFailure = "snc_mesh_failure"

// MutualMeasurement is ToN RNIC-to-RNIC connectivity probe output.
type MutualMeasurement struct {
	Src       string  `json:"src"`
	Dst       string  `json:"dst"`
	Connected bool    `json:"connected"`
	RTTnsP50  uint64  `json:"rtt_ns_p50"`
	RTTnsP99  uint64  `json:"rtt_ns_p99"`
	LossRatio float64 `json:"loss_ratio"`
	Sent      uint32  `json:"sent"`
	Recv      uint32  `json:"recv"`
	// Reliable is true for the live verbs probe: a lost probe is a timeout
	// (Recv < Sent), and LossRatio is not Ethernet drop evidence — drops come
	// from the RNIC counters. It is false only in the simulator, whose lost
	// probes are its injected drops.
	Reliable     bool   `json:"reliable,omitempty"`
	CounterDrops uint64 `json:"counter_drops,omitempty"`
	// CrossRail marks a pair whose RNICs sit on different rails (fabric
	// keys), probed only under --mutual-scope=host. Its traffic leaves the
	// rail, so its failures are judged apart from same-rail ones.
	CrossRail bool `json:"cross_rail,omitempty"`
}

// MutualScope selects which RNIC pairs the mutual probe measures.
type MutualScope string

const (
	// MutualScopeRail pairs RNICs that share a rail (IPv4 subnet or typed
	// GID prefix). It is the default.
	MutualScopeRail MutualScope = "rail"
	// MutualScopeHost pairs every RNIC of the host with a RoCEv2 GID, across
	// rails too. It assumes the rails route to each other.
	MutualScopeHost MutualScope = "host"
)

// MutualTransport is the verbs transport the live mutual probe uses.
type MutualTransport string

const (
	// MutualTransportRC is an RC SEND ping-pong, the default. RC retransmits a
	// lost packet for about half a second, so an isolated loss never shows.
	MutualTransportRC MutualTransport = "rc"
	// MutualTransportUD is a UD request/reply: one lost packet is one timed-out
	// probe (ToN'24 §IV-A-2). The scheduled probe requires it.
	MutualTransportUD MutualTransport = "ud"
)

// TimeoutRatio is the fraction of probes that got no reply, 1 − Recv/Sent
// (ToN'24 §IV-A-2). A pair that never connected timed out on every probe, so it
// is 1. ok is false when the pair connected but reports no probes sent: there
// is no rate to speak of, and 0 would read as a healthy pair.
func (m MutualMeasurement) TimeoutRatio() (ratio float64, ok bool) {
	if !m.Connected {
		return 1, true
	}
	if m.Sent == 0 {
		return 0, false
	}
	if m.Recv >= m.Sent {
		return 0, true
	}
	return 1 - float64(m.Recv)/float64(m.Sent), true
}

// BusSample is vendor-neutral bus utilization for a named link.
type BusSample struct {
	LinkID      string
	Utilization float64
	Timestamp   time.Time
}

// RNICCounters is one poll of throughput / PFC / drop counters.
type RNICCounters struct {
	Device            string
	Netdev            string
	RxBytes           uint64
	TxBytes           uint64
	RxDrops           uint64
	TxDrops           uint64
	TxPauseDurationUs uint64
	RxPauseDurationUs uint64
	// PauseSources and DropSources are the sampler's bitmask of the sources
	// the pause-duration and drop counters were read from on this poll. 0
	// means none was readable, so the value is not 0 but unread. The monitor
	// only compares them: a set that shrinks is a counter that can no longer
	// be read, and one that changes cannot be differenced against the last.
	PauseSources uint8
	DropSources  uint8
	// PauseGap and DropGap say why a family has no source, for the log.
	PauseGap  string
	DropGap   string
	Timestamp time.Time
}

// RNICRates is derived from consecutive counter snapshots.
type RNICRates struct {
	Device             string
	ThroughputBPS      uint64
	PauseDurationRatio float64
	DropDelta          uint64
	// PauseKnown and DropKnown are set when PauseDurationRatio and DropDelta
	// were measured on this poll. Otherwise the value is 0 but means nothing:
	// the counter has no source, could not be read, or is being re-baselined.
	PauseKnown bool
	DropKnown  bool
	Idle       bool
}

// GPUStatus is idle vs active for the hardware monitor.
type GPUStatus struct {
	ID      string
	Idle    bool
	Util    float64
	Unknown bool // true when utilization could not be read; unknown ≠ idle
	// ComputeMode is the GPU's compute mode (ComputeModeDefault, ...), or ""
	// when it could not be read or the GPU has no such setting.
	ComputeMode string
}

// PCIeConfig captures ACS/ATS and negotiated link speed used in root-cause rules.
// Speed/Width are the negotiated (current) values; MaxSpeed/MaxWidth are what
// the device advertises, and PortMaxSpeed/PortMaxWidth what the upstream port
// it hangs off advertises. The lower of the two maxima is what the link can
// train to (the port stands in for the slot), so a down-train is visible even
// when the negotiated link still carries the line rate. Max* and PortMax* are
// empty / 0 when unknown.
type PCIeConfig struct {
	BDF          string
	Speed        string
	Width        int
	MaxSpeed     string
	MaxWidth     int
	PortMaxSpeed string
	PortMaxWidth int
	ACSEnabled   bool
	ATSEnabled   bool
	ACSKnown     bool
	ATSKnown     bool
	NUMA         int
}

// EndpointSkip is one group of loopback probes the engine skipped because the
// endpoint could not be registered (ErrNoGDR, ErrNoDevice, ErrNoGID, ErrNoLID,
// ErrPortNotActive, ErrProbe)
// or, for a GPU, was not probed because of its compute mode (ErrComputeMode).
// Reason is the probe's own error text, which carries the detail string that
// names the cause ("NoGDR: no cuda"); it is the only place that reason is
// stated, so the report keeps it verbatim.
type EndpointSkip struct {
	Kind EndpointKind `json:"kind"`
	// Reason is unbounded probe text: fine in JSON and logs, not as a metric attribute.
	Reason    string   `json:"reason"`
	Endpoints []string `json:"endpoints"`
	// Probes counts skipped RNIC→endpoint probes, which is larger than
	// len(Endpoints) whenever several RNICs reach the same endpoint.
	Probes int `json:"probes"`
}

// RunMode selects full-mesh (idle) vs affinitive-only (busy) probing.
type RunMode string

const (
	ModeIdle RunMode = "idle"
	ModeBusy RunMode = "busy"
)

// Report is one Hostping engine+analyzer pass, uploaded as OTel attributes and JSON.
type Report struct {
	Trigger   Trigger           `json:"trigger"`
	Mode      RunMode           `json:"mode"`
	Host      string            `json:"host"`
	Started   time.Time         `json:"started"`
	Elapsed   time.Duration     `json:"-"`
	ElapsedNs int64             `json:"elapsed_ns"`
	Paths     []PathMeasurement `json:"paths"`
	// SkippedEndpoints names the endpoints missing from Paths, so a report with
	// only memory rows says why the GPU half was never measured.
	SkippedEndpoints []EndpointSkip      `json:"skipped_endpoints,omitempty"`
	Links            []LinkState         `json:"links"`
	Causes           []RootCause         `json:"causes"`
	Mutual           []MutualMeasurement `json:"mutual,omitempty"`
	HostIdle         bool                `json:"host_idle"`
	AbnormalRNIC     []string            `json:"abnormal_rnics,omitempty"`
	Baselines        []BaselineTable     `json:"baselines,omitempty"`
	// CPULoad is each NUMA node's CPU busy fraction on the monitor poll that
	// started the run, in NUMA order; empty when the run had no CPU reading.
	CPULoad []CPULoad `json:"cpu_load,omitempty"`
}

// CPULoad is the busy fraction of one NUMA node's CPUs over a monitor poll
// interval, from /proc/stat.
type CPULoad struct {
	NUMA   int `json:"numa"`
	Socket int `json:"socket"`
	// Busy is the fraction of the node's CPU time not spent idle or in
	// iowait, 0–1.
	Busy float64 `json:"busy"`
	// High is set when Busy reached --cpu-busy-high. Every path that
	// transits Socket is then judged cpu_busy rather than abnormal.
	High bool `json:"high,omitempty"`
}

// CPUBusySockets is the set of CPU packages with a NUMA node whose load is
// High; nil when there is none.
func CPUBusySockets(loads []CPULoad) map[int]bool {
	var out map[int]bool
	for _, l := range loads {
		if !l.High {
			continue
		}
		if out == nil {
			out = map[int]bool{}
		}
		out[l.Socket] = true
	}
	return out
}

// SkippedEndpointCount counts the distinct endpoints this run skipped. An
// empty kind counts every kind, and an endpoint skipped for two reasons (one
// per RNIC) counts once.
func (r Report) SkippedEndpointCount(kind EndpointKind) int {
	seen := map[string]struct{}{}
	for _, s := range r.SkippedEndpoints {
		if kind != "" && s.Kind != kind {
			continue
		}
		for _, ep := range s.Endpoints {
			seen[ep] = struct{}{}
		}
	}
	return len(seen)
}

const (
	BaselineSourceDefault = "default"
	BaselineSourceUser    = "user"
)

// BaselineTable is the effective idle baseline for one RNIC line rate and the
// PCIe hop feeding it.
type BaselineTable struct {
	LineRateBPS uint64 `json:"line_rate_bps"`
	// PCIeCapBPS is the RNIC PCIe hop capacity the table was derived with;
	// 0 when unknown.
	PCIeCapBPS uint64 `json:"pcie_capacity_bps,omitempty"`
	// DerivedFromBPS is the rate the Fig.4 ratios were scaled from:
	// min(LineRateBPS, PCIeCapBPS, the Fig.4 Gen4 x16 bus). It is below
	// LineRateBPS when the wire outruns the bus feeding it, which is why a 400G
	// RNIC is not judged against 0.985×400G.
	DerivedFromBPS uint64 `json:"derived_from_bps"`
	// Section is the key of the baseline-file "tables" section the
	// operator's values came from (see baseline.TableKey); "" when they came
	// from the file's top level, or nothing was set.
	Section string          `json:"section,omitempty"`
	Entries []BaselineEntry `json:"entries"`
}

// BaselineEntry is one distance row, noting whether BW / latency came from the user.
type BaselineEntry struct {
	Distance        string `json:"distance"`
	BandwidthBPS    uint64 `json:"bandwidth_bps"`
	LatencyNs       uint64 `json:"latency_ns"`
	BandwidthSource string `json:"bandwidth_source"`
	LatencySource   string `json:"latency_source"`
	// SubClasses are the distance's learned sub-classes from the baseline
	// file, with effective values: a value the sub-class leaves unset is the
	// distance's own.
	SubClasses []BaselineSubClass `json:"sub_classes,omitempty"`
}

// BaselineSubClass is one learned sub-class of a distance: paths across one
// of its root-port pairs are judged against its values instead of the
// distance's.
type BaselineSubClass struct {
	Name          string         `json:"name"`
	BandwidthBPS  uint64         `json:"bandwidth_bps"`
	LatencyNs     uint64         `json:"latency_ns"`
	RootPortPairs []RootPortPair `json:"root_port_pairs"`
}

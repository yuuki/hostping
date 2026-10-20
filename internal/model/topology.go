package model

// EndpointKind is a Hostping loopback target: GPU video memory or a NUMA memory node.
type EndpointKind string

const (
	EndpointGPU    EndpointKind = "gpu"
	EndpointMemory EndpointKind = "memory"
)

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

// Link kinds of the PCIe hops and the memory channels. LinkKindPCIeRNIC and
// LinkKindPCIeGPU are the device's own PCIe link (ID "pcie.<device>", see
// PCIeLinkID), LinkKindPCIeSwitch an upstream switch port, LinkKindMemory a
// NUMA node's memory channel.
const (
	LinkKindPCIeRNIC   = "pcie_rnic"
	LinkKindPCIeGPU    = "pcie_gpu"
	LinkKindPCIeSwitch = "pcie_switch"
	LinkKindMemory     = "memory"
)

// PCIeLinkID is the link ID of a device's own PCIe hop: "pcie." followed by
// the device (RNIC or GPU) ID.
func PCIeLinkID(device string) string { return "pcie." + device }

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

package topology

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
)

// Diagram returns the host topology as a graph of nodes and edges for a
// dashboard to draw: CPU packages (and, under sub-NUMA clustering, their NUMA
// nodes), memory nodes, root ports, PCIe switches, RNICs and GPUs as nodes,
// and one edge per link of the graph between the two nodes it joins, directed
// from the CPU side to the device side. Root-port and switch nodes take the ID
// of their link (rp.*, sw.*), RNICs, GPUs and memory nodes their own ID, a
// package socketN and a NUMA node numaN.
//
// Two consecutive links of a path share a node, so a path's LinkIDs walk the
// edges. The exception is sub-NUMA clustering: a root port and a memory node
// hang off their NUMA node, and a UPI link joins packages, so a path that
// leaves the package also crosses the structural package→NUMA edge (link
// "none") the path does not list.
//
// A link whose ends cannot be placed (an unknown kind, an inter-socket link on
// a host with more than two packages whose ID names no pair) gets no edge.
func (g Graph) Diagram() ([]model.TopologyNode, []model.TopologyEdge) {
	d := newDiagram(g)
	d.build()
	return d.nodes, d.edges
}

type diagram struct {
	g     Graph
	nodes []model.TopologyNode
	edges []model.TopologyEdge
	have  map[string]bool
	// numas holds the CPU NUMA nodes of every package.
	numas map[int][]int
	// rpNUMA is the NUMA node of a root port, from a device beneath it.
	rpNUMA map[string]int
	// links indexes the graph's links by ID.
	links map[string]model.Link
}

func newDiagram(g Graph) *diagram {
	d := &diagram{
		g:      g,
		have:   map[string]bool{},
		numas:  map[int][]int{},
		rpNUMA: map[string]int{},
		links:  map[string]model.Link{},
	}
	note := func(sock, numa int) {
		for _, n := range d.numas[sock] {
			if n == numa {
				return
			}
		}
		d.numas[sock] = append(d.numas[sock], numa)
	}
	for _, n := range g.RNICs {
		note(n.Socket, n.NUMA)
		if _, ok := d.rpNUMA[n.RootPort]; !ok && n.RootPort != "" {
			d.rpNUMA[n.RootPort] = n.NUMA
		}
	}
	for _, e := range g.Endpoints {
		note(e.Socket, e.NUMA)
		if _, ok := d.rpNUMA[e.RootPort]; !ok && e.RootPort != "" {
			d.rpNUMA[e.RootPort] = e.NUMA
		}
	}
	for _, l := range g.Links {
		if _, ok := d.numas[l.Socket]; !ok && (l.Kind == model.LinkKindRootPort || l.Kind == "memory") {
			d.numas[l.Socket] = nil
		}
		d.links[l.ID] = l
	}
	for s := range d.numas {
		sort.Ints(d.numas[s])
	}
	return d
}

func (d *diagram) sockets() []int {
	out := make([]int, 0, len(d.numas))
	for s := range d.numas {
		out = append(out, s)
	}
	sort.Ints(out)
	return out
}

// snc reports a package split into more than one NUMA node.
func (d *diagram) snc(sock int) bool { return len(d.numas[sock]) > 1 }

// cpuNode is the node a root port or memory node on (sock, numa) hangs off:
// the NUMA node under sub-NUMA clustering, the package otherwise.
func (d *diagram) cpuNode(sock, numa int) string {
	if d.snc(sock) {
		return numaNodeID(numa)
	}
	return socketNodeID(sock)
}

func socketNodeID(s int) string { return fmt.Sprintf("socket%d", s) }
func numaNodeID(n int) string   { return fmt.Sprintf("numa%d", n) }

func (d *diagram) addNode(n model.TopologyNode) {
	if d.have[n.ID] {
		return
	}
	d.have[n.ID] = true
	d.nodes = append(d.nodes, n)
}

func (d *diagram) addEdge(l model.Link, source, target string) {
	if source == "" || target == "" || !d.have[source] || !d.have[target] {
		return
	}
	d.edges = append(d.edges, model.TopologyEdge{
		ID: l.ID, Link: l.ID, Source: source, Target: target, Kind: l.Kind,
		CapacityBPS: l.CapacityBPS, NegotiatedBPS: l.NegotiatedBPS, CapacityAssumed: l.CapacityAssumed,
	})
}

// busBDF is the PCI address in an rp.* or sw.* ID, "" when the ID carries none
// (the simulator's rp.0).
func busBDF(id string) string {
	if i := strings.IndexByte(id, '.'); i >= 0 && pci.IsBDF(id[i+1:]) {
		return id[i+1:]
	}
	return ""
}

func titleOr(prefix, bdf, id string) string {
	if bdf != "" {
		return prefix + " " + bdf
	}
	return prefix + " " + id
}

func (d *diagram) build() {
	g := d.g
	// CPU packages and, under SNC, their NUMA nodes.
	for _, s := range d.sockets() {
		numa := -1
		if len(d.numas[s]) == 1 {
			numa = d.numas[s][0]
		}
		d.addNode(model.TopologyNode{ID: socketNodeID(s), Kind: model.TopoNodeSocket, Title: fmt.Sprintf("socket %d", s), Socket: s, NUMA: numa})
	}
	for _, s := range d.sockets() {
		if !d.snc(s) {
			continue
		}
		for _, n := range d.numas[s] {
			d.addNode(model.TopologyNode{ID: numaNodeID(n), Kind: model.TopoNodeNUMA, Title: fmt.Sprintf("NUMA %d", n), Socket: s, NUMA: n})
		}
	}
	memOf := map[string]model.Endpoint{}
	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointMemory {
			continue
		}
		memOf[e.ID] = e
		memOf[fmt.Sprintf("mem.%d", e.Index)] = e
		d.addNode(model.TopologyNode{ID: e.ID, Kind: model.TopoNodeMemory, Title: fmt.Sprintf("%s (NUMA %d)", e.ID, e.NUMA), Socket: e.Socket, NUMA: e.NUMA})
	}
	// Root ports and switches, in link order.
	for _, l := range g.Links {
		switch l.Kind {
		case model.LinkKindRootPort:
			d.addRootPort(l.ID, l.Socket)
		case "pcie_switch":
			bdf := busBDF(l.ID)
			d.addNode(model.TopologyNode{ID: l.ID, Kind: model.TopoNodeSwitch, Title: titleOr("switch", bdf, l.ID), Socket: l.Socket, NUMA: d.numaOfRootPort(l.RootPort, l.Socket), PCIeBDF: bdf})
		}
	}
	devOf := map[string]string{} // pcie.* link device → its Switch, RootPort
	rpOf := map[string]string{}
	for _, n := range g.RNICs {
		d.addRootPort(n.RootPort, n.Socket)
		title := n.Device
		if title == "" {
			title = n.ID
		}
		d.addNode(model.TopologyNode{ID: n.ID, Kind: model.TopoNodeRNIC, Title: title, Socket: n.Socket, NUMA: n.NUMA, PCIeBDF: n.PCIeBDF})
		devOf[n.ID], rpOf[n.ID] = n.Switch, n.RootPort
	}
	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointGPU {
			continue
		}
		d.addRootPort(e.RootPort, e.Socket)
		title := e.ID
		if e.PCIeBDF != "" {
			title += " " + e.PCIeBDF
		}
		d.addNode(model.TopologyNode{ID: e.ID, Kind: model.TopoNodeGPU, Title: title, Socket: e.Socket, NUMA: e.NUMA, PCIeBDF: e.PCIeBDF})
		devOf[e.ID], rpOf[e.ID] = e.Switch, e.RootPort
	}

	for _, l := range g.Links {
		switch l.Kind {
		case model.LinkKindRootPort:
			d.addEdge(l, d.cpuNode(l.Socket, d.numaOfRootPort(l.ID, l.Socket)), l.ID)
		case "pcie_switch":
			d.addEdge(l, d.upstream(l, "", l.RootPort), l.ID)
		case "pcie_rnic", "pcie_gpu":
			dev := l.Device
			if dev == "" {
				dev = strings.TrimPrefix(l.ID, "pcie.")
			}
			rp := rpOf[dev]
			if rp == "" {
				rp = l.RootPort
			}
			d.addEdge(l, d.upstream(l, devOf[dev], rp), dev)
		case "memory":
			e, ok := memOf[l.Device]
			if !ok {
				e, ok = memOf[l.ID]
			}
			if ok {
				d.addEdge(l, d.cpuNode(e.Socket, e.NUMA), e.ID)
			}
		case model.LinkKindUPI:
			if a, b, ok := d.upiEnds(l.ID); ok {
				d.addEdge(l, socketNodeID(a), socketNodeID(b))
			}
		case model.LinkKindSNCMesh:
			if a, b, ok := pairOf(l.ID, "mesh."); ok {
				d.addEdge(l, numaNodeID(a), numaNodeID(b))
			}
		}
	}
	for _, s := range d.sockets() {
		if !d.snc(s) {
			continue
		}
		for _, n := range d.numas[s] {
			d.edges = append(d.edges, model.TopologyEdge{
				ID: socketNodeID(s) + "-" + numaNodeID(n), Link: model.TopoEdgeNoLink,
				Source: socketNodeID(s), Target: numaNodeID(n), Kind: model.TopoEdgeKindPackage,
			})
		}
	}
}

func (d *diagram) addRootPort(id string, sock int) {
	if id == "" || d.have[id] {
		return
	}
	if l, ok := d.links[id]; ok {
		sock = l.Socket
	}
	bdf := busBDF(id)
	d.addNode(model.TopologyNode{ID: id, Kind: model.TopoNodeRootPort, Title: titleOr("root port", bdf, id), Socket: sock, NUMA: d.numaOfRootPort(id, sock), PCIeBDF: bdf})
}

// numaOfRootPort is the NUMA node of the devices under root port rp, or the
// package's only NUMA node when no device names it; -1 when unknown.
func (d *diagram) numaOfRootPort(rp string, sock int) int {
	if n, ok := d.rpNUMA[rp]; ok {
		return n
	}
	if len(d.numas[sock]) == 1 {
		return d.numas[sock][0]
	}
	return -1
}

// upstream is the node the PCIe hop l hangs off: the Upstream discovery
// recorded, else the device's switch, else its root port, provided the node
// exists.
func (d *diagram) upstream(l model.Link, sw, rp string) string {
	for _, id := range []string{l.Upstream, sw, rp} {
		if id != "" && id != l.ID && d.have[id] {
			return id
		}
	}
	return ""
}

// upiEnds is the pair of packages an inter-socket link joins: the pair its ID
// names (upi.0-1), or the only two packages of the host.
func (d *diagram) upiEnds(id string) (int, int, bool) {
	if a, b, ok := pairOf(id, "upi."); ok {
		return a, b, true
	}
	if s := d.sockets(); len(s) == 2 {
		return s[0], s[1], true
	}
	return 0, 0, false
}

// pairOf parses prefix + "A-B" into its two numbers, lower first.
func pairOf(id, prefix string) (int, int, bool) {
	var a, b int
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok {
		return 0, 0, false
	}
	if n, err := fmt.Sscanf(rest, "%d-%d", &a, &b); err != nil || n != 2 || fmt.Sprintf("%d-%d", a, b) != rest {
		return 0, 0, false
	}
	if a > b {
		a, b = b, a
	}
	return a, b, true
}

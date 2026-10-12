package topology

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
)

const (
	// discoverPCIeCap is the Gen4 x16 capacity a PCIe hop falls back to when
	// sysfs does not say what its link can train to, and the capacity every
	// switch hop carries (see pcieLink).
	discoverPCIeCap = uint64(252_000_000_000)
	discoverMemCap  = uint64(400_000_000_000)
	discoverUPICap  = uint64(300_000_000_000)
)

// DiscoverOptions adjusts what sysfs discovery keeps.
type DiscoverOptions struct {
	// Exclude names RDMA devices to drop, each by IB device name (mlx5_3) or
	// PCI address (0000:3c:00.1), exactly. An entry that matches no device is
	// an error, so a typo cannot leave a device probed.
	Exclude []string
}

// Discover builds a graph from live sysfs. It fails closed if RNICs are
// missing or their PCI ancestry cannot be walked to a root port.
func Discover(opts DiscoverOptions) (Graph, error) {
	return DiscoverWith("/sys", opts)
}

// DiscoverFrom walks sysroot (normally /sys) so tests can inject a tree.
func DiscoverFrom(sysroot string) (Graph, error) {
	return DiscoverWith(sysroot, DiscoverOptions{})
}

// DiscoverWith is DiscoverFrom with options.
func DiscoverWith(sysroot string, opts DiscoverOptions) (Graph, error) {
	socks := discoverSockets(sysroot)
	rnics, chains, excluded, err := discoverRNICs(sysroot, socks, opts.Exclude)
	if err != nil {
		return Graph{}, err
	}
	if len(rnics) == 0 && len(excluded) > 0 {
		return Graph{}, fmt.Errorf("--exclude-rnic excludes every InfiniBand device under %s/class/infiniband", sysroot)
	}
	if len(rnics) == 0 {
		return Graph{}, fmt.Errorf("no InfiniBand devices under %s/class/infiniband; pass --sim for the Fig.2 world", sysroot)
	}
	for _, n := range rnics {
		if n.PCIeBDF == "" {
			return Graph{}, fmt.Errorf("RNIC %s has no PCI BDF", n.Device)
		}
		if n.RootPort == "" {
			return Graph{}, fmt.Errorf("RNIC %s (%s): PCI walk did not find a root port", n.Device, n.PCIeBDF)
		}
	}
	eps := discoverMemory(sysroot, socks)
	gpus, gpuChains := discoverGPUs(sysroot, socks)
	eps = append(eps, gpus...)

	g := Graph{Name: "sysfs", RNICs: rnics, Endpoints: eps, Excluded: excluded}
	g.Links = synthesizeDiscoveredLinks(sysroot, g, chains, gpuChains)
	g.Paths = synthesizeDiscoveredPaths(g, chains, gpuChains)
	g.setRNICPCIeCaps()
	return g, nil
}

// pciChain is one device's PCI ancestry, reduced to the hops Hostping models.
//
// A PCIe switch appears in sysfs as an upstream port (PCIe type 5) plus one
// downstream port (type 6) per slot. All of those ports are one switch, so a
// switch is identified by its upstream port BDF: every device behind any of
// its downstream ports carries the same sw.{upstream} ID. Switches therefore
// holds one entry per physical switch traversed, host-near → device-near, and
// Switch is the device-nearest one. A bridge whose PCIe type is unreadable
// falls back to being its own switch.
//
// RootLinkBDF is the function directly beneath the root port, whose sysfs link
// attributes describe the root-port link: reading a link from its lower end
// also yields the upper end's max_link_* through pci.ReadLink. Switch hops
// have no such BDF; see pcieLink for why their links are not read.
type pciChain struct {
	Hops        []string // host-near → device, including the endpoint BDF
	RootPort    string   // rp.{bdf}
	RootLinkBDF string   // BDF at the lower end of the root-port link
	Switch      string   // sw.{upstream port bdf} of the device-nearest switch, if any
	Switches    []string // host-near → device-near switch IDs
}

// ancestors returns the hops above the device, host-near → device-near:
// the root port followed by every switch. Two devices share everything up to
// the common prefix of their ancestor lists; the last common entry is the
// lowest common ancestor their peer-to-peer traffic turns around at.
func (c pciChain) ancestors() []string {
	out := make([]string, 0, len(c.Switches)+1)
	if c.RootPort != "" {
		out = append(out, c.RootPort)
	}
	return append(out, c.Switches...)
}

func commonPrefix(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func isSwitchID(id string) bool { return strings.HasPrefix(id, "sw.") }

func discoverRNICs(sysroot string, socks sockets, exclude []string) ([]model.RNIC, map[string]pciChain, []model.ExcludedRNIC, error) {
	ents, err := os.ReadDir(filepath.Join(sysroot, "class/infiniband"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("infiniband sysfs: %w", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	matched := make([]bool, len(exclude))
	var out []model.RNIC
	var excluded []model.ExcludedRNIC
	chains := map[string]pciChain{}
	localLIDs := localIBLIDs(sysroot, names)
	idx := 0
	for _, dev := range names {
		bdf := pciBDF(filepath.Join(sysroot, "class/infiniband", dev, "device"))
		if excludedRNIC(exclude, matched, dev, bdf) {
			logf("hostping: topology: %s (%s) excluded by --exclude-rnic: not probed, paired or exported", dev, bdf)
			excluded = append(excluded, model.ExcludedRNIC{Device: dev, PCIeBDF: bdf, Reason: model.ExcludeReasonOperator})
			continue
		}
		if skipIBDevice(sysroot, dev) {
			continue
		}
		numa := readInt(filepath.Join(sysroot, "class/infiniband", dev, "device", "numa_node"), 0)
		ch, err := walkPCI(sysroot, bdf)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("RNIC %s: %w", dev, err)
		}
		n := model.RNIC{
			ID:       fmt.Sprintf("rnic%d", idx),
			Device:   dev,
			Netdev:   firstNetdev(sysroot, dev),
			Socket:   socks.of(numa),
			NUMA:     numa,
			RootPort: ch.RootPort,
			Switch:   ch.Switch,
			PCIeBDF:  bdf,
			LineRate: readRate(sysroot, dev),
			Fabric:   fabricKey(sysroot, dev),
		}
		n.LinkLayer = rnicLinkLayer(sysroot, dev)
		if n.LinkLayer == model.LinkLayerInfiniBand {
			if why := managementHCAHints(sysroot, dev, n.Netdev, localLIDs); len(why) > 0 {
				logf("hostping: topology: warning: %s (%s) looks like a management HCA (%s); if it carries no data traffic, pass --exclude-rnic %s", dev, bdf, strings.Join(why, "; "), dev)
			}
		}
		out = append(out, n)
		chains[n.ID] = ch
		idx++
	}
	for i, e := range exclude {
		if !matched[i] {
			return nil, nil, nil, fmt.Errorf("--exclude-rnic %q matches no device under %s/class/infiniband (want an IB device name or a PCI address)", e, sysroot)
		}
	}
	return out, chains, excluded, nil
}

// excludedRNIC reports whether dev (at PCI address bdf) is named in exclude,
// marking every entry that names it.
func excludedRNIC(exclude []string, matched []bool, dev, bdf string) bool {
	hit := false
	for i, e := range exclude {
		if e == dev || (bdf != "" && strings.EqualFold(e, bdf)) {
			matched[i] = true
			hit = true
		}
	}
	return hit
}

func skipIBDevice(sysroot, dev string) bool {
	if strings.Contains(strings.ToLower(dev), "bond") {
		return true
	}
	netdev := firstNetdev(sysroot, dev)
	if netdev == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(sysroot, "class/net", netdev, "bonding"))
	return err == nil
}

func discoverMemory(sysroot string, socks sockets) []model.Endpoint {
	if eps := discoverNUMANodes(sysroot, socks); len(eps) > 0 {
		return eps
	}
	if eps := discoverEDAC(sysroot, socks); len(eps) > 0 {
		return eps
	}
	return []model.Endpoint{{ID: "mem0", Kind: model.EndpointMemory}}
}

func discoverNUMANodes(sysroot string, socks sockets) []model.Endpoint {
	ents, err := os.ReadDir(filepath.Join(sysroot, "devices/system/node"))
	if err != nil {
		return nil
	}
	var out []model.Endpoint
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "node") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "node"))
		if err != nil {
			continue
		}
		if memoryOnlyNUMA(sysroot, e.Name()) {
			continue
		}
		out = append(out, model.Endpoint{
			ID: fmt.Sprintf("mem%d", n), Kind: model.EndpointMemory, Index: n, Socket: socks.of(n), NUMA: n,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NUMA < out[j].NUMA })
	return out
}

// memoryOnlyNUMA reports GPU/HBM nodes: cpulist exists and is empty.
// Missing cpulist (test fixtures, odd sysfs) is treated as a CPU node.
func memoryOnlyNUMA(sysroot, node string) bool {
	p := filepath.Join(sysroot, "devices/system/node", node, "cpulist")
	if _, err := os.Stat(p); err != nil {
		return false
	}
	return strings.TrimSpace(readFile(p)) == ""
}

func discoverEDAC(sysroot string, socks sockets) []model.Endpoint {
	ents, err := os.ReadDir(filepath.Join(sysroot, "devices/system/edac/mc"))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "mc") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []model.Endpoint
	idx := 0
	for _, name := range names {
		node := readInt(filepath.Join(sysroot, "devices/system/edac/mc", name, "csrow0", "node"), -1)
		if node < 0 {
			node = readInt(filepath.Join(sysroot, "devices/system/edac/mc", name, "node"), -1)
		}
		if node < 0 {
			continue
		}
		out = append(out, model.Endpoint{
			ID: fmt.Sprintf("mem%d", idx), Kind: model.EndpointMemory, Index: idx, Socket: socks.of(node), NUMA: node,
		})
		idx++
	}
	return out
}

func discoverGPUs(sysroot string, socks sockets) ([]model.Endpoint, map[string]pciChain) {
	ents, err := os.ReadDir(filepath.Join(sysroot, "bus/pci/devices"))
	if err != nil {
		return nil, nil
	}
	var bdfs []string
	for _, e := range ents {
		bdfs = append(bdfs, e.Name())
	}
	sort.Strings(bdfs)
	idx := 0
	var out []model.Endpoint
	chains := map[string]pciChain{}
	for _, bdf := range bdfs {
		class := strings.TrimSpace(readFile(filepath.Join(sysroot, "bus/pci/devices", bdf, "class")))
		if class != "0x030200" && class != "0x030000" {
			continue
		}
		vendor := strings.TrimSpace(readFile(filepath.Join(sysroot, "bus/pci/devices", bdf, "vendor")))
		if vendor != "0x10de" && vendor != "0x1002" {
			continue
		}
		ch, err := walkPCI(sysroot, bdf)
		if err != nil || ch.RootPort == "" {
			// Omit rather than pretend this GPU shares an RNIC root port.
			continue
		}
		numa := readInt(filepath.Join(sysroot, "bus/pci/devices", bdf, "numa_node"), 0)
		ep := model.Endpoint{
			ID: fmt.Sprintf("gpu%d", idx), Kind: model.EndpointGPU, Index: idx,
			Socket: socks.of(numa), RootPort: ch.RootPort, Switch: ch.Switch, NUMA: numa, PCIeBDF: bdf,
		}
		out = append(out, ep)
		chains[ep.ID] = ch
		idx++
	}
	return out, chains
}

func walkPCI(sysroot, bdf string) (pciChain, error) {
	if bdf == "" || !pci.IsBDF(bdf) {
		return pciChain{}, fmt.Errorf("invalid PCI BDF %q", bdf)
	}
	resolved, err := pci.Resolve(sysroot, bdf)
	if err != nil {
		return pciChain{}, fmt.Errorf("resolve %s: %w", bdf, err)
	}
	hops := pci.AncestryBDFs(resolved)
	if len(hops) == 0 {
		return pciChain{}, fmt.Errorf("no PCI ancestry for %s", bdf)
	}
	ch := pciChain{Hops: hops}
	inSwitch := false // already inside a switch whose ID we recorded
	for i, hop := range hops {
		if hop == bdf {
			continue
		}
		switch pci.Role(sysroot, hop, i) {
		case "root_port":
			if ch.RootPort == "" {
				ch.RootPort = "rp." + hop
				// hop != bdf, so something sits below it on the chain.
				ch.RootLinkBDF = hops[i+1]
			}
			inSwitch = false
		case "switch":
			t := pci.PCIeType(sysroot, hop)
			if t == 6 && inSwitch {
				// Downstream port of the switch we just recorded.
				continue
			}
			id := "sw." + hop
			ch.Switches = append(ch.Switches, id)
			ch.Switch = id
			inSwitch = t == 5 || t == 6
		}
	}
	if ch.RootPort == "" {
		return ch, fmt.Errorf("no root port above %s (ancestry %v)", bdf, hops)
	}
	return ch, nil
}

// pcieLink reads what the PCIe link whose lower end is bdf can train to and
// what it trained to. The capacity is the capability (min of the device and
// upstream port max_link_*), never the negotiated value: a down-trained link
// must not become its own healthy baseline. It falls back to discoverPCIeCap
// when sysfs does not state both maxima, and reports that capacity as assumed
// so no utilization is computed against it.
//
// Only links whose two ends hostping can attribute are read this way: the
// link attaching an RNIC or GPU (the device and the port it hangs off) and the
// link below a root port. A switch hop keeps discoverPCIeCap, assumed, with no
// negotiated value (switchLink), because sysfs does not describe its link
// reliably:
//
//   - Without a pcie_type attribute, upstream and downstream ports of a switch
//     look alike and each becomes its own sw.* hop. A downstream port's link
//     attributes describe the link below it, yet pci.ReadLink pairs them with
//     the port's parent — the same switch's upstream port, which is no link
//     partner — so the capability can come out below what the link negotiated.
//   - A link between cascaded switch levels may be an in-package virtual link
//     whose reported speed is fictitious: it can report 5 GT/s as both current
//     and maximum while carrying several times that bandwidth.
//
// Either number would become the path's healthy ceiling (Graph.PathCapacity),
// hold a healthy 400G RNIC to a fraction of what it carries, and let a
// degraded path pass as normal.
func pcieLink(sysroot, bdf string) (capBPS, negBPS uint64, assumed bool) {
	if bdf == "" {
		return discoverPCIeCap, 0, true
	}
	c, err := pci.ReadLink(sysroot, bdf)
	if err != nil {
		return discoverPCIeCap, 0, true
	}
	capBPS = pci.CapacityBPS(c)
	if capBPS == 0 {
		return discoverPCIeCap, pci.NegotiatedBPS(c), true
	}
	return capBPS, pci.NegotiatedBPS(c), false
}

// switchLink is a switch hop at the nominal discoverPCIeCap. Its sysfs link is
// not read (see pcieLink), so the capacity is assumed and no negotiated value
// is recorded.
func switchLink(id string, socket int, rootPort, upstream string) model.Link {
	return model.Link{ID: id, Kind: "pcie_switch", Device: id, Socket: socket, RootPort: rootPort, Upstream: upstream, CapacityBPS: discoverPCIeCap, CapacityAssumed: true}
}

// upstream is the node a device or switch hangs off: the switch above it in
// the chain, or the root port when nothing is between them. A tree has one
// parent per bridge, so every device sharing a switch records the same one.
func (c pciChain) upstreamOf(i int) string {
	if i > 0 {
		return c.Switches[i-1]
	}
	return c.RootPort
}

// deviceUpstream is the node the chain's device hangs off.
func (c pciChain) deviceUpstream() string {
	return c.upstreamOf(len(c.Switches))
}

// addSwitchLinks adds the switch hops of ch, each pointing at its parent.
func addSwitchLinks(add func(model.Link), ch pciChain, socket int) {
	for i, sw := range ch.Switches {
		add(switchLink(sw, socket, ch.RootPort, ch.upstreamOf(i)))
	}
}

func synthesizeDiscoveredLinks(sysroot string, g Graph, rnicCh, gpuCh map[string]pciChain) []model.Link {
	var links []model.Link
	seen := map[string]bool{}
	add := func(l model.Link) {
		if seen[l.ID] {
			return
		}
		seen[l.ID] = true
		links = append(links, l)
	}
	// pcie adds one PCIe hop, reading sysfs only for a hop not seen yet.
	pcie := func(l model.Link, bdf string) {
		if seen[l.ID] {
			return
		}
		l.CapacityBPS, l.NegotiatedBPS, l.CapacityAssumed = pcieLink(sysroot, bdf)
		add(l)
	}
	for _, n := range g.RNICs {
		ch := rnicCh[n.ID]
		pcie(model.Link{ID: "pcie." + n.ID, Kind: "pcie_rnic", Device: n.ID, Socket: n.Socket, RootPort: n.RootPort, Upstream: ch.deviceUpstream()}, n.PCIeBDF)
		pcie(model.Link{ID: n.RootPort, Kind: "root_port", Socket: n.Socket, RootPort: n.RootPort}, ch.RootLinkBDF)
		addSwitchLinks(add, ch, n.Socket)
	}
	for _, e := range g.Endpoints {
		switch e.Kind {
		case model.EndpointGPU:
			ch := gpuCh[e.ID]
			pcie(model.Link{ID: "pcie." + e.ID, Kind: "pcie_gpu", Device: e.ID, Socket: e.Socket, RootPort: e.RootPort, Upstream: ch.deviceUpstream()}, e.PCIeBDF)
			pcie(model.Link{ID: e.RootPort, Kind: "root_port", Socket: e.Socket, RootPort: e.RootPort}, ch.RootLinkBDF)
			addSwitchLinks(add, ch, e.Socket)
		case model.EndpointMemory:
			add(model.Link{ID: fmt.Sprintf("mem.%d", e.Index), Kind: "memory", Device: e.ID, Socket: e.Socket, CapacityBPS: discoverMemCap, CapacityAssumed: true})
		}
	}
	sockets := map[int]bool{}
	for _, n := range g.RNICs {
		sockets[n.Socket] = true
	}
	for _, e := range g.Endpoints {
		sockets[e.Socket] = true
	}
	var socks []int
	for s := range sockets {
		socks = append(socks, s)
	}
	sort.Ints(socks)
	for i := 0; i < len(socks); i++ {
		for j := i + 1; j < len(socks); j++ {
			add(model.Link{ID: upiID(socks[i], socks[j]), Kind: model.LinkKindUPI, CapacityBPS: discoverUPICap, CapacityAssumed: true})
		}
	}
	// Under sub-NUMA clustering the NUMA nodes of one package are joined by
	// its own fabric, not by UPI. Without SNC every socket holds one NUMA
	// node and this adds nothing.
	for _, l := range meshLinks(g) {
		add(l)
	}
	return links
}

func synthesizeDiscoveredPaths(g Graph, rnicCh, gpuCh map[string]pciChain) []model.Path {
	var paths []model.Path
	for _, n := range g.RNICs {
		for _, e := range g.Endpoints {
			paths = append(paths, pathRNICToEndpoint(n, e, rnicCh[n.ID], gpuCh[e.ID]))
		}
	}
	return paths
}

func pathRNICToEndpoint(n model.RNIC, e model.Endpoint, rc, ec pciChain) model.Path {
	p := model.Path{RNIC: n.ID, Endpoint: e.ID, LinkIDs: []string{"pcie." + n.ID}}
	// Climbing away from the RNIC visits its switches device-near first.
	up := reverse(rc.Switches)
	switch e.Kind {
	case model.EndpointGPU:
		ra, ea := rc.ancestors(), ec.ancestors()
		k := commonPrefix(ra, ea)
		switch {
		case k > 0:
			// Same root complex: climb from the RNIC to the lowest common
			// ancestor ra[k-1] and descend to the GPU. Hops above the LCA
			// (a root port under a shared switch) are never traversed.
			p.Affinitive = true
			if isSwitchID(ra[k-1]) {
				p.Distance = model.DistanceSameSwitch
			} else if len(ra) > k || len(ea) > k {
				// Turned around at the root port with a switch on the way.
				p.Distance = model.DistanceMultiSwitch
			} else {
				// No switch at all on either side.
				p.Distance = model.DistanceSameSwitch
			}
			p.LinkIDs = append(p.LinkIDs, reverse(ra[k:])...)
			p.LinkIDs = append(p.LinkIDs, ra[k-1])
			p.LinkIDs = append(p.LinkIDs, ea[k:]...)
			p.LinkIDs = append(p.LinkIDs, "pcie."+e.ID)
		case e.Socket == n.Socket:
			// Within one package; under sub-NUMA clustering the root ports
			// may sit on different NUMA nodes, joined by the package mesh.
			p.Distance = model.DistanceCrossRoot
			p.LinkIDs = append(p.LinkIDs, up...)
			p.LinkIDs = append(p.LinkIDs, n.RootPort)
			if e.NUMA != n.NUMA {
				p.LinkIDs = append(p.LinkIDs, meshID(n.NUMA, e.NUMA))
			}
			p.LinkIDs = append(p.LinkIDs, e.RootPort)
			p.LinkIDs = append(p.LinkIDs, ec.Switches...)
			p.LinkIDs = append(p.LinkIDs, "pcie."+e.ID)
		default:
			p.Distance = model.DistanceCrossSocket
			p.LinkIDs = append(p.LinkIDs, up...)
			p.LinkIDs = append(p.LinkIDs, n.RootPort, upiID(n.Socket, e.Socket), e.RootPort)
			p.LinkIDs = append(p.LinkIDs, ec.Switches...)
			p.LinkIDs = append(p.LinkIDs, "pcie."+e.ID)
		}
	case model.EndpointMemory:
		p.LinkIDs = append(p.LinkIDs, up...)
		mem := fmt.Sprintf("mem.%d", e.Index)
		switch {
		case e.NUMA == n.NUMA && e.Socket == n.Socket:
			p.Distance = model.DistanceLocalMem
			p.Affinitive = true
			p.LinkIDs = append(p.LinkIDs, n.RootPort, mem)
		case e.Socket == n.Socket:
			// Another NUMA node of the same package (sub-NUMA clustering).
			p.Distance = model.DistanceRemoteMem
			p.LinkIDs = append(p.LinkIDs, n.RootPort, meshID(n.NUMA, e.NUMA), mem)
		default:
			p.Distance = model.DistanceRemoteMem
			p.LinkIDs = append(p.LinkIDs, n.RootPort, upiID(n.Socket, e.Socket), mem)
		}
	}
	return p
}

func reverse(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

func upiID(a, b int) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("upi.%d-%d", a, b)
}

func fabricKey(sysroot, ibdev string) string {
	port := firstActiveIBPort(sysroot, ibdev)
	if port == "" {
		return "unknown"
	}
	base := filepath.Join(sysroot, "class/infiniband", ibdev, "ports", port)
	if portLinkLayer(base) == model.LinkLayerInfiniBand {
		return ibFabricKey(base)
	}
	var rocev2 []string
	var rocev2Idx []int
	for i := 0; i < 16; i++ {
		raw := strings.TrimSpace(readFile(filepath.Join(base, "gids", strconv.Itoa(i))))
		if raw == "" || gidAllZero(raw) {
			continue
		}
		if !gidRoCEv2(base, i) {
			continue
		}
		rocev2 = append(rocev2, raw)
		rocev2Idx = append(rocev2Idx, i)
	}
	for k, g := range rocev2 {
		if ip := rocev4(gidHex(g)); ip != nil {
			netdev := strings.TrimSpace(readFile(filepath.Join(base, "gid_attrs", "ndevs", strconv.Itoa(rocev2Idx[k]))))
			if netdev == "" {
				netdev = firstNetdev(sysroot, ibdev)
			}
			return v4RailKey(ibdev, netdev, ip)
		}
	}
	for _, g := range rocev2 {
		if !gidLinkLocal(g) {
			hex := gidHex(g)
			if len(hex) >= 16 {
				return "gid:" + strings.ToLower(hex[:16])
			}
			return "gid:" + strings.ToLower(hex)
		}
	}
	return "unknown"
}

// netdevPrefixLen returns the prefix length of ip as configured on netdev. It
// is a variable so tests can stand in for the host's interfaces.
var netdevPrefixLen = interfacePrefixLen

// interfacePrefixLen reads netdev's addresses and returns the prefix length of
// the one equal to ip: the address the kernel embedded in the RoCEv2 GID.
func interfacePrefixLen(netdev string, ip net.IP) (int, error) {
	if netdev == "" {
		return 0, fmt.Errorf("no netdev")
	}
	ifi, err := net.InterfaceByName(netdev)
	if err != nil {
		return 0, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return 0, err
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || !n.IP.Equal(ip) {
			continue
		}
		ones, bits := n.Mask.Size()
		if bits != 32 && bits != 128 {
			break
		}
		if bits == 128 {
			ones -= 96
		}
		if ones < 0 || ones > 32 {
			break
		}
		return ones, nil
	}
	return 0, fmt.Errorf("%s has no address %s", netdev, ip)
}

// v4RailKey is the rail key of an RNIC whose RoCEv2 GID maps the IPv4 address
// ip: the IPv4 subnet configured on netdev, the interface backing the GID.
// Two RNICs on one subnet reach each other without a gateway; RNICs on
// different subnets are routed. A /24 keeps the historical form v4:a.b.c.0;
// any other prefix length is spelled v4:<network>/<len>. When the prefix
// cannot be read the key falls back to the /24, and the fallback is logged.
func v4RailKey(ibdev, netdev string, ip net.IP) string {
	ones, err := netdevPrefixLen(netdev, ip)
	if err != nil {
		logf("topology: %s: IPv4 prefix length of %s unreadable (%v); assuming /24 for its rail", ibdev, ip, err)
		ones = 24
	}
	network := ip.Mask(net.CIDRMask(ones, 32))
	if ones == 24 {
		return "v4:" + network.String()
	}
	return "v4:" + network.String() + "/" + strconv.Itoa(ones)
}

func firstActiveIBPort(sysroot, ibdev string) string {
	ents, err := os.ReadDir(filepath.Join(sysroot, "class/infiniband", ibdev, "ports"))
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Slice(names, func(i, j int) bool {
		a, ea := strconv.Atoi(names[i])
		b, eb := strconv.Atoi(names[j])
		if ea == nil && eb == nil {
			return a < b
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		st := strings.ToUpper(readFile(filepath.Join(sysroot, "class/infiniband", ibdev, "ports", name, "state")))
		if strings.Contains(st, "ACTIVE") || strings.HasPrefix(strings.TrimSpace(st), "4:") {
			return name
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return "1"
}

func gidRoCEv2(portDir string, idx int) bool {
	raw := strings.ToLower(readFile(filepath.Join(portDir, "gid_attrs", "types", strconv.Itoa(idx))))
	return strings.Contains(raw, "roce v2")
}

func gidHex(gid string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(gid), ":", ""))
}

func gidAllZero(gid string) bool {
	hex := gidHex(gid)
	return hex == "" || strings.Trim(hex, "0") == ""
}

func gidLinkLocal(gid string) bool {
	return strings.HasPrefix(gidHex(gid), "fe80")
}

func rocev4(hex string) net.IP {
	if len(hex) < 32 {
		return nil
	}
	// Real mlx5 RoCEv2 IPv4-mapped: 0000:0000:0000:0000:0000:ffff:aabb:ccdd
	// Do not treat fe80::…:ffff:x.x.x.x (RoCEv1 GID0) as IPv4-mapped.
	if hex[:20] != "00000000000000000000" || hex[20:24] != "ffff" {
		return nil
	}
	b := make([]byte, 4)
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseUint(hex[24+i*2:26+i*2], 16, 8)
		if err != nil {
			return nil
		}
		b[i] = byte(v)
	}
	return net.IP(b)
}

func pciBDF(deviceLink string) string {
	target, err := filepath.EvalSymlinks(deviceLink)
	if err != nil {
		target, err = os.Readlink(deviceLink)
		if err != nil {
			return ""
		}
	}
	return filepath.Base(target)
}

func firstNetdev(sysroot, ibdev string) string {
	ents, err := os.ReadDir(filepath.Join(sysroot, "class/infiniband", ibdev, "device", "net"))
	if err != nil {
		return ""
	}
	if len(ents) == 0 {
		return ""
	}
	return ents[0].Name()
}

func readRate(sysroot, ibdev string) uint64 {
	port := firstActiveIBPort(sysroot, ibdev)
	if port == "" {
		port = "1"
	}
	v := strings.TrimSpace(readFile(filepath.Join(sysroot, "class/infiniband", ibdev, "ports", port, "rate")))
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return uint64(f * 1e9)
}

func readInt(path string, def int) int {
	s := strings.TrimSpace(readFile(path))
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

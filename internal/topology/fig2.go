package topology

import (
	"strconv"

	"github.com/yuuki/hostping/internal/model"
)

const lineRate200G = 200_000_000_000

// Fig2 returns the training-machine topology from NSDI'23 Fig.2:
// two Xeon sockets connected by UPI; each socket has two CPU root ports,
// each root port a PCIe switch with one 200 Gb/s RNIC and two A100 GPUs,
// plus a local memory node per socket.
func Fig2() Graph {
	const lr = lineRate200G
	rnics := []model.RNIC{
		{ID: "rnic0", Device: "mlx5_0", Netdev: "eth0", Socket: 0, NUMA: 0, RootPort: "rp.0", PCIeBDF: "0000:17:00.0", LineRate: lr},
		{ID: "rnic1", Device: "mlx5_1", Netdev: "eth1", Socket: 0, NUMA: 0, RootPort: "rp.1", PCIeBDF: "0000:37:00.0", LineRate: lr},
		{ID: "rnic2", Device: "mlx5_2", Netdev: "eth2", Socket: 1, NUMA: 1, RootPort: "rp.2", PCIeBDF: "0000:57:00.0", LineRate: lr},
		{ID: "rnic3", Device: "mlx5_3", Netdev: "eth3", Socket: 1, NUMA: 1, RootPort: "rp.3", PCIeBDF: "0000:77:00.0", LineRate: lr},
	}
	eps := []model.Endpoint{
		{ID: "gpu0", Kind: model.EndpointGPU, Index: 0, Socket: 0, RootPort: "rp.0", NUMA: 0, PCIeBDF: "0000:18:00.0"},
		{ID: "gpu1", Kind: model.EndpointGPU, Index: 1, Socket: 0, RootPort: "rp.0", NUMA: 0, PCIeBDF: "0000:19:00.0"},
		{ID: "gpu2", Kind: model.EndpointGPU, Index: 2, Socket: 0, RootPort: "rp.1", NUMA: 0, PCIeBDF: "0000:38:00.0"},
		{ID: "gpu3", Kind: model.EndpointGPU, Index: 3, Socket: 0, RootPort: "rp.1", NUMA: 0, PCIeBDF: "0000:39:00.0"},
		{ID: "gpu4", Kind: model.EndpointGPU, Index: 4, Socket: 1, RootPort: "rp.2", NUMA: 1, PCIeBDF: "0000:58:00.0"},
		{ID: "gpu5", Kind: model.EndpointGPU, Index: 5, Socket: 1, RootPort: "rp.2", NUMA: 1, PCIeBDF: "0000:59:00.0"},
		{ID: "gpu6", Kind: model.EndpointGPU, Index: 6, Socket: 1, RootPort: "rp.3", NUMA: 1, PCIeBDF: "0000:78:00.0"},
		{ID: "gpu7", Kind: model.EndpointGPU, Index: 7, Socket: 1, RootPort: "rp.3", NUMA: 1, PCIeBDF: "0000:79:00.0"},
		{ID: "mem0", Kind: model.EndpointMemory, Index: 0, Socket: 0, NUMA: 0},
		{ID: "mem1", Kind: model.EndpointMemory, Index: 1, Socket: 1, NUMA: 1},
	}

	link := func(id, kind, dev, rp string, sock int, cap uint64) model.Link {
		return model.Link{ID: id, Kind: kind, Device: dev, Socket: sock, RootPort: rp, CapacityBPS: cap}
	}
	links := []model.Link{
		link("pcie.rnic0", model.LinkKindPCIeRNIC, "rnic0", "rp.0", 0, pcieGen4x16),
		link("pcie.rnic1", model.LinkKindPCIeRNIC, "rnic1", "rp.1", 0, pcieGen4x16),
		link("pcie.rnic2", model.LinkKindPCIeRNIC, "rnic2", "rp.2", 1, pcieGen4x16),
		link("pcie.rnic3", model.LinkKindPCIeRNIC, "rnic3", "rp.3", 1, pcieGen4x16),
		link("pcie.gpu0", model.LinkKindPCIeGPU, "gpu0", "rp.0", 0, pcieGen4x16),
		link("pcie.gpu1", model.LinkKindPCIeGPU, "gpu1", "rp.0", 0, pcieGen4x16),
		link("pcie.gpu2", model.LinkKindPCIeGPU, "gpu2", "rp.1", 0, pcieGen4x16),
		link("pcie.gpu3", model.LinkKindPCIeGPU, "gpu3", "rp.1", 0, pcieGen4x16),
		link("pcie.gpu4", model.LinkKindPCIeGPU, "gpu4", "rp.2", 1, pcieGen4x16),
		link("pcie.gpu5", model.LinkKindPCIeGPU, "gpu5", "rp.2", 1, pcieGen4x16),
		link("pcie.gpu6", model.LinkKindPCIeGPU, "gpu6", "rp.3", 1, pcieGen4x16),
		link("pcie.gpu7", model.LinkKindPCIeGPU, "gpu7", "rp.3", 1, pcieGen4x16),
		link("sw.0", model.LinkKindPCIeSwitch, "sw0", "rp.0", 0, pcieGen4x16),
		link("sw.1", model.LinkKindPCIeSwitch, "sw1", "rp.1", 0, pcieGen4x16),
		link("sw.2", model.LinkKindPCIeSwitch, "sw2", "rp.2", 1, pcieGen4x16),
		link("sw.3", model.LinkKindPCIeSwitch, "sw3", "rp.3", 1, pcieGen4x16),
		link("rp.0", model.LinkKindRootPort, "rp0", "rp.0", 0, pcieGen4x16),
		link("rp.1", model.LinkKindRootPort, "rp1", "rp.1", 0, pcieGen4x16),
		link("rp.2", model.LinkKindRootPort, "rp2", "rp.2", 1, pcieGen4x16),
		link("rp.3", model.LinkKindRootPort, "rp3", "rp.3", 1, pcieGen4x16),
		link("mem.0", model.LinkKindMemory, "mem0", "", 0, memCap),
		link("mem.1", model.LinkKindMemory, "mem1", "", 1, memCap),
		link("upi.0", model.LinkKindUPI, "upi0", "", 0, upiCap),
	}

	// Every device hangs off its root port's switch, and the switch off the
	// root port.
	for i := range links {
		l := &links[i]
		switch l.Kind {
		case model.LinkKindPCIeRNIC, model.LinkKindPCIeGPU:
			l.Upstream = "sw." + l.RootPort[len("rp."):]
		case model.LinkKindPCIeSwitch:
			l.Upstream = l.RootPort
		}
	}

	g := Graph{Name: "fig2", RNICs: rnics, Endpoints: eps, Links: links}
	g.Paths = fig2Paths(g, func(int, int) string { return "upi.0" })
	g.setRNICPCIeCaps()
	return g
}

// Fig2SNC is Fig2 with sub-NUMA clustering on: each of the two packages is
// split into two NUMA nodes, one per root port, each with its own memory. The
// NUMA nodes of a package are joined by the package mesh (mesh.0-1, mesh.2-3)
// and the packages by one UPI link (upi.0-1), named as discovery names them.
// A GPU under the other root port of the same package is cross_root across the
// mesh; memory of the other NUMA node of the package is remote_mem across it.
func Fig2SNC() Graph {
	g := Fig2()
	g.Name = "fig2-snc"
	for i := range g.RNICs {
		g.RNICs[i].NUMA = i // rnicN sits on rp.N
	}
	var eps []model.Endpoint
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			e.NUMA = e.Index / 2 // two GPUs per root port
			eps = append(eps, e)
		}
	}
	for i := 0; i < 4; i++ {
		eps = append(eps, model.Endpoint{ID: "mem" + strconv.Itoa(i), Kind: model.EndpointMemory, Index: i, Socket: i / 2, NUMA: i})
	}
	g.Endpoints = eps

	var links []model.Link
	for _, l := range g.Links {
		if l.Kind == model.LinkKindMemory || l.Kind == model.LinkKindUPI {
			continue
		}
		links = append(links, l)
	}
	for i := 0; i < 4; i++ {
		links = append(links, model.Link{ID: "mem." + strconv.Itoa(i), Kind: model.LinkKindMemory, Device: "mem" + strconv.Itoa(i), Socket: i / 2, CapacityBPS: memCap})
	}
	// A real host's mesh capacity is unknown; the simulator still needs a
	// healthy bandwidth for it, so it gets the memory channel's.
	links = append(links,
		model.Link{ID: "upi.0-1", Kind: model.LinkKindUPI, Device: "upi0", CapacityBPS: upiCap},
		model.Link{ID: "mesh.0-1", Kind: model.LinkKindSNCMesh, Socket: 0, CapacityBPS: memCap, CapacityAssumed: true},
		model.Link{ID: "mesh.2-3", Kind: model.LinkKindSNCMesh, Socket: 1, CapacityBPS: memCap, CapacityAssumed: true},
	)
	g.Links = links
	g.Paths = fig2Paths(g, upiID)
	return g
}

// Fig2Rails is Fig2 wired rail-optimized: every RNIC on a rail (subnet) of
// its own, the layout on which the default rail-scope mutual probe has no
// pair. It lets --sim exercise --mutual-scope=host and its cross-rail
// diagnoses; the intra-host graph is Fig.2 unchanged.
func Fig2Rails() Graph {
	g := Fig2()
	g.Name = "fig2-rails"
	for i := range g.RNICs {
		g.RNICs[i].Fabric = "rail" + strconv.Itoa(i)
	}
	return g
}

// fig2Paths enumerates the paths of a Fig.2 graph the way discovery would:
// every RNIC and GPU hangs off the one switch of its root port (rp.N → sw.N),
// so the live path builder, fed those chains, classifies and routes each pair.
// Only the UPI link's name differs between the figures, hence upi.
func fig2Paths(g Graph, upi upiNamer) []model.Path {
	chain := func(rootPort string) pciChain {
		return pciChain{RootPort: rootPort, Switches: []string{"sw." + rootPort[len("rp."):]}}
	}
	rnicCh := map[string]pciChain{}
	for _, n := range g.RNICs {
		rnicCh[n.ID] = chain(n.RootPort)
	}
	gpuCh := map[string]pciChain{}
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			gpuCh[e.ID] = chain(e.RootPort)
		}
	}
	return enumeratePaths(g, rnicCh, gpuCh, upi)
}

package topology

import "github.com/yuuki/hostping/internal/model"

const (
	lineRate200G = 200_000_000_000
	pcieGen4x16  = 252_000_000_000
	upiCap       = 300_000_000_000
	memCap       = 400_000_000_000
)

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
		link("pcie.rnic0", "pcie_rnic", "rnic0", "rp.0", 0, pcieGen4x16),
		link("pcie.rnic1", "pcie_rnic", "rnic1", "rp.1", 0, pcieGen4x16),
		link("pcie.rnic2", "pcie_rnic", "rnic2", "rp.2", 1, pcieGen4x16),
		link("pcie.rnic3", "pcie_rnic", "rnic3", "rp.3", 1, pcieGen4x16),
		link("pcie.gpu0", "pcie_gpu", "gpu0", "rp.0", 0, pcieGen4x16),
		link("pcie.gpu1", "pcie_gpu", "gpu1", "rp.0", 0, pcieGen4x16),
		link("pcie.gpu2", "pcie_gpu", "gpu2", "rp.1", 0, pcieGen4x16),
		link("pcie.gpu3", "pcie_gpu", "gpu3", "rp.1", 0, pcieGen4x16),
		link("pcie.gpu4", "pcie_gpu", "gpu4", "rp.2", 1, pcieGen4x16),
		link("pcie.gpu5", "pcie_gpu", "gpu5", "rp.2", 1, pcieGen4x16),
		link("pcie.gpu6", "pcie_gpu", "gpu6", "rp.3", 1, pcieGen4x16),
		link("pcie.gpu7", "pcie_gpu", "gpu7", "rp.3", 1, pcieGen4x16),
		link("sw.0", "pcie_switch", "sw0", "rp.0", 0, pcieGen4x16),
		link("sw.1", "pcie_switch", "sw1", "rp.1", 0, pcieGen4x16),
		link("sw.2", "pcie_switch", "sw2", "rp.2", 1, pcieGen4x16),
		link("sw.3", "pcie_switch", "sw3", "rp.3", 1, pcieGen4x16),
		link("rp.0", "root_port", "rp0", "rp.0", 0, pcieGen4x16),
		link("rp.1", "root_port", "rp1", "rp.1", 0, pcieGen4x16),
		link("rp.2", "root_port", "rp2", "rp.2", 1, pcieGen4x16),
		link("rp.3", "root_port", "rp3", "rp.3", 1, pcieGen4x16),
		link("mem.0", "memory", "mem0", "", 0, memCap),
		link("mem.1", "memory", "mem1", "", 1, memCap),
		link("upi.0", model.LinkKindUPI, "upi0", "", 0, upiCap),
	}

	g := Graph{Name: "fig2", RNICs: rnics, Endpoints: eps, Links: links}
	g.Paths = enumerateFig2(g, "upi.0")
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
		eps = append(eps, model.Endpoint{ID: "mem" + itoa(i), Kind: model.EndpointMemory, Index: i, Socket: i / 2, NUMA: i})
	}
	g.Endpoints = eps

	var links []model.Link
	for _, l := range g.Links {
		if l.Kind == "memory" || l.Kind == model.LinkKindUPI {
			continue
		}
		links = append(links, l)
	}
	for i := 0; i < 4; i++ {
		links = append(links, model.Link{ID: "mem." + itoa(i), Kind: "memory", Device: "mem" + itoa(i), Socket: i / 2, CapacityBPS: memCap})
	}
	// A real host's mesh capacity is unknown; the simulator still needs a
	// healthy bandwidth for it, so it gets the memory channel's.
	links = append(links,
		model.Link{ID: "upi.0-1", Kind: model.LinkKindUPI, Device: "upi0", CapacityBPS: upiCap},
		model.Link{ID: "mesh.0-1", Kind: model.LinkKindSNCMesh, Socket: 0, CapacityBPS: memCap, CapacityAssumed: true},
		model.Link{ID: "mesh.2-3", Kind: model.LinkKindSNCMesh, Socket: 1, CapacityBPS: memCap, CapacityAssumed: true},
	)
	g.Links = links
	g.Paths = enumerateFig2(g, "upi.0-1")
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
		g.RNICs[i].Fabric = "rail" + itoa(i)
	}
	return g
}

func enumerateFig2(g Graph, upi string) []model.Path {
	sw := map[string]string{"rp.0": "sw.0", "rp.1": "sw.1", "rp.2": "sw.2", "rp.3": "sw.3"}
	var paths []model.Path
	for _, n := range g.RNICs {
		for _, e := range g.Endpoints {
			p := model.Path{RNIC: n.ID, Endpoint: e.ID}
			p.LinkIDs = append(p.LinkIDs, "pcie."+n.ID, sw[n.RootPort])
			switch {
			case e.Kind == model.EndpointGPU && e.RootPort == n.RootPort:
				p.Distance = model.DistanceSameSwitch
				p.Affinitive = true
				p.LinkIDs = append(p.LinkIDs, "pcie."+e.ID)
			case e.Kind == model.EndpointGPU && e.Socket == n.Socket:
				p.Distance = model.DistanceCrossRoot
				p.LinkIDs = append(p.LinkIDs, n.RootPort)
				if e.NUMA != n.NUMA {
					p.LinkIDs = append(p.LinkIDs, meshID(n.NUMA, e.NUMA))
				}
				p.LinkIDs = append(p.LinkIDs, e.RootPort, sw[e.RootPort], "pcie."+e.ID)
			case e.Kind == model.EndpointGPU:
				p.Distance = model.DistanceCrossSocket
				p.LinkIDs = append(p.LinkIDs, n.RootPort, upi, e.RootPort, sw[e.RootPort], "pcie."+e.ID)
			case e.Kind == model.EndpointMemory && e.Socket == n.Socket && e.NUMA == n.NUMA:
				p.Distance = model.DistanceLocalMem
				p.Affinitive = true
				p.LinkIDs = append(p.LinkIDs, n.RootPort, "mem."+itoa(e.Index))
			case e.Kind == model.EndpointMemory && e.Socket == n.Socket:
				p.Distance = model.DistanceRemoteMem
				p.LinkIDs = append(p.LinkIDs, n.RootPort, meshID(n.NUMA, e.NUMA), "mem."+itoa(e.Index))
			default:
				p.Distance = model.DistanceRemoteMem
				p.LinkIDs = append(p.LinkIDs, n.RootPort, upi, "mem."+itoa(e.Index))
			}
			paths = append(paths, p)
		}
	}
	return paths
}

func itoa(i int) string {
	return string(rune('0' + i))
}

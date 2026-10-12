package topology

import (
	"path/filepath"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// checkDiagram checks the invariants every diagram must hold: unique node and
// edge IDs, edges between existing nodes, exactly one edge per link, one
// parent per node below the CPU, and every path walkable along the edges (two
// consecutive links share a node; under sub-NUMA clustering they may instead
// be joined through the structural package edges).
func checkDiagram(t *testing.T, g Graph) ([]model.TopologyNode, []model.TopologyEdge) {
	t.Helper()
	nodes, edges := g.Diagram()
	node := map[string]model.TopologyNode{}
	for _, n := range nodes {
		if _, dup := node[n.ID]; dup {
			t.Fatalf("duplicate node %q", n.ID)
		}
		node[n.ID] = n
	}
	byLink := map[string]model.TopologyEdge{}
	ids := map[string]bool{}
	parents := map[string]int{}
	pkg := map[string][]string{} // structural adjacency
	for _, e := range edges {
		if ids[e.ID] {
			t.Fatalf("duplicate edge %q", e.ID)
		}
		ids[e.ID] = true
		if _, ok := node[e.Source]; !ok {
			t.Fatalf("edge %s: unknown source %q", e.ID, e.Source)
		}
		if _, ok := node[e.Target]; !ok {
			t.Fatalf("edge %s: unknown target %q", e.ID, e.Target)
		}
		switch e.Kind {
		case model.LinkKindUPI, model.LinkKindSNCMesh:
			if e.Source >= e.Target {
				t.Errorf("edge %s: %s→%s, want lower→higher", e.ID, e.Source, e.Target)
			}
		default:
			parents[e.Target]++
		}
		if e.Link == model.TopoEdgeNoLink {
			pkg[e.Source] = append(pkg[e.Source], e.Target)
			pkg[e.Target] = append(pkg[e.Target], e.Source)
			continue
		}
		if e.Link != e.ID {
			t.Errorf("edge %s: link %q, want the link ID", e.ID, e.Link)
		}
		byLink[e.Link] = e
	}
	for _, l := range g.Links {
		e, ok := byLink[l.ID]
		if !ok {
			t.Fatalf("link %s (%s) has no edge", l.ID, l.Kind)
		}
		if e.Kind != l.Kind || e.CapacityBPS != l.CapacityBPS || e.NegotiatedBPS != l.NegotiatedBPS || e.CapacityAssumed != l.CapacityAssumed {
			t.Errorf("edge %+v does not carry link %+v", e, l)
		}
	}
	for _, n := range nodes {
		want := 1
		if n.Kind == model.TopoNodeSocket {
			want = 0
		}
		if parents[n.ID] != want {
			t.Errorf("node %s (%s): %d parents, want %d", n.ID, n.Kind, parents[n.ID], want)
		}
	}
	ends := func(id string) []string {
		e := byLink[id]
		return []string{e.Source, e.Target}
	}
	// reach is the set of nodes joined to x through structural edges.
	reach := func(x string) map[string]bool {
		out := map[string]bool{x: true}
		todo := []string{x}
		for len(todo) > 0 {
			v := todo[0]
			todo = todo[1:]
			for _, w := range pkg[v] {
				if !out[w] {
					out[w] = true
					todo = append(todo, w)
				}
			}
		}
		return out
	}
	for _, p := range g.Paths {
		first := ends(p.LinkIDs[0])
		if first[1] != p.RNIC {
			t.Errorf("%s→%s: first hop %s ends at %s, not the RNIC", p.RNIC, p.Endpoint, p.LinkIDs[0], first[1])
		}
		last := ends(p.LinkIDs[len(p.LinkIDs)-1])
		if last[1] != p.Endpoint {
			t.Errorf("%s→%s: last hop %s ends at %s, not the endpoint", p.RNIC, p.Endpoint, p.LinkIDs[len(p.LinkIDs)-1], last[1])
		}
		for i := 1; i < len(p.LinkIDs); i++ {
			a, b := ends(p.LinkIDs[i-1]), ends(p.LinkIDs[i])
			joined := false
			for _, x := range a {
				r := reach(x)
				for _, y := range b {
					if r[y] {
						joined = true
					}
				}
			}
			if !joined {
				t.Errorf("%s→%s: %s %v and %s %v share no node", p.RNIC, p.Endpoint, p.LinkIDs[i-1], a, p.LinkIDs[i], b)
			}
		}
	}
	if len(pkg) == 0 {
		// Without SNC the walk needs no structural edge at all.
		for _, p := range g.Paths {
			for i := 1; i < len(p.LinkIDs); i++ {
				a, b := ends(p.LinkIDs[i-1]), ends(p.LinkIDs[i])
				if a[0] != b[0] && a[0] != b[1] && a[1] != b[0] && a[1] != b[1] {
					t.Errorf("%s→%s: %s and %s share no node", p.RNIC, p.Endpoint, p.LinkIDs[i-1], p.LinkIDs[i])
				}
			}
		}
	}
	return nodes, edges
}

func edgeOf(t *testing.T, edges []model.TopologyEdge, link string) model.TopologyEdge {
	t.Helper()
	for _, e := range edges {
		if e.Link == link {
			return e
		}
	}
	t.Fatalf("no edge for link %s", link)
	return model.TopologyEdge{}
}

func assertEdge(t *testing.T, edges []model.TopologyEdge, link, source, target string) {
	t.Helper()
	e := edgeOf(t, edges, link)
	if e.Source != source || e.Target != target {
		t.Errorf("edge %s: %s→%s, want %s→%s", link, e.Source, e.Target, source, target)
	}
}

func nodeOf(t *testing.T, nodes []model.TopologyNode, id string) model.TopologyNode {
	t.Helper()
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no node %s", id)
	return model.TopologyNode{}
}

func TestDiagramFig2(t *testing.T) {
	for _, name := range []string{"fig2", "fig2-rails"} {
		t.Run(name, func(t *testing.T) {
			g, err := Named(name)
			if err != nil {
				t.Fatal(err)
			}
			nodes, edges := checkDiagram(t, g)
			// 2 sockets, 2 memory nodes, 4 root ports, 4 switches, 4 RNICs, 8 GPUs.
			if len(nodes) != 24 || len(edges) != len(g.Links) {
				t.Fatalf("%d nodes, %d edges; want 24 and %d", len(nodes), len(edges), len(g.Links))
			}
			assertEdge(t, edges, "pcie.rnic0", "sw.0", "rnic0")
			assertEdge(t, edges, "pcie.gpu1", "sw.0", "gpu1")
			assertEdge(t, edges, "sw.0", "rp.0", "sw.0")
			assertEdge(t, edges, "rp.0", "socket0", "rp.0")
			assertEdge(t, edges, "rp.3", "socket1", "rp.3")
			assertEdge(t, edges, "mem.1", "socket1", "mem1")
			assertEdge(t, edges, "upi.0", "socket0", "socket1")
			want := map[string]model.TopologyNode{
				"socket0": {ID: "socket0", Kind: "socket", Title: "socket 0", Socket: 0, NUMA: 0},
				"mem1":    {ID: "mem1", Kind: "memory", Title: "mem1 (NUMA 1)", Socket: 1, NUMA: 1},
				"rp.2":    {ID: "rp.2", Kind: "root_port", Title: "root port rp.2", Socket: 1, NUMA: 1},
				"sw.2":    {ID: "sw.2", Kind: "switch", Title: "switch sw.2", Socket: 1, NUMA: 1},
				"rnic2":   {ID: "rnic2", Kind: "rnic", Title: "mlx5_2", Socket: 1, NUMA: 1, PCIeBDF: "0000:57:00.0"},
				"gpu5":    {ID: "gpu5", Kind: "gpu", Title: "gpu5 0000:59:00.0", Socket: 1, NUMA: 1, PCIeBDF: "0000:59:00.0"},
			}
			for id, w := range want {
				if got := nodeOf(t, nodes, id); got != w {
					t.Errorf("node %s = %+v, want %+v", id, got, w)
				}
			}
		})
	}
}

// Under sub-NUMA clustering the root ports and memory hang off NUMA nodes,
// joined by the mesh within a package and to the package by structural edges.
func TestDiagramFig2SNC(t *testing.T) {
	g := Fig2SNC()
	nodes, edges := checkDiagram(t, g)
	assertEdge(t, edges, "rp.1", "numa1", "rp.1")
	assertEdge(t, edges, "mem.2", "numa2", "mem2")
	assertEdge(t, edges, "mesh.2-3", "numa2", "numa3")
	assertEdge(t, edges, "upi.0-1", "socket0", "socket1")
	var structural int
	for _, e := range edges {
		if e.Link == model.TopoEdgeNoLink {
			structural++
			if e.Kind != model.TopoEdgeKindPackage {
				t.Errorf("structural edge %+v", e)
			}
		}
	}
	if structural != 4 {
		t.Fatalf("%d package edges, want one per NUMA node", structural)
	}
	if n := nodeOf(t, nodes, "socket1"); n.NUMA != -1 {
		t.Fatalf("an SNC package has no single NUMA node: %+v", n)
	}
	if n := nodeOf(t, nodes, "numa3"); n.Kind != model.TopoNodeNUMA || n.Socket != 1 {
		t.Fatalf("numa3 = %+v", n)
	}
}

func TestDiagramDiscoverSNC(t *testing.T) {
	quietLog(t)
	root := mkRootPortTree(t, []int{0, 1, 2, 3})
	mkNUMANodes(t, root, []int{0, 1, 2, 3}, map[int]int{0: 0, 1: 0, 2: 1, 3: 1})
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	_, edges := checkDiagram(t, g)
	assertEdge(t, edges, "rp.0000:50:00.0", "numa2", "rp.0000:50:00.0")
	assertEdge(t, edges, "mesh.0-1", "numa0", "numa1")
}

func TestDiagramDiscoverFromPCIWalk(t *testing.T) {
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0", "0x060400", "6", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/0000:3c:00.1", "0x030200", "0", "0x10de", "0")
	mkPCI(t, root, "devices/pci0000:5a/0000:5a:00.0", "0x060400", "4", "", "1")
	mkPCI(t, root, "devices/pci0000:5a/0000:5a:00.0/0000:5c:00.0", "0x020000", "0", "0x15b3", "1")
	mkPCI(t, root, "devices/pci0000:5a/0000:5a:00.0/0000:5c:00.1", "0x030200", "0", "0x10de", "1")
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mkIB(t, root, "mlx5_1", "0000:5c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0102")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	mustWrite(t, filepath.Join(root, "devices/system/node/node1"), "")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	nodes, edges := checkDiagram(t, g)
	assertEdge(t, edges, "pcie.rnic0", "sw.0000:3b:00.0", "rnic0")
	assertEdge(t, edges, "pcie.gpu0", "sw.0000:3b:00.0", "gpu0")
	assertEdge(t, edges, "sw.0000:3b:00.0", "rp.0000:3a:00.0", "sw.0000:3b:00.0")
	// A device directly under the root port hangs off it.
	assertEdge(t, edges, "pcie.rnic1", "rp.0000:5a:00.0", "rnic1")
	assertEdge(t, edges, "pcie.gpu1", "rp.0000:5a:00.0", "gpu1")
	assertEdge(t, edges, "rp.0000:5a:00.0", "socket1", "rp.0000:5a:00.0")
	assertEdge(t, edges, "upi.0-1", "socket0", "socket1")
	if n := nodeOf(t, nodes, "sw.0000:3b:00.0"); n.PCIeBDF != "0000:3b:00.0" || n.Title != "switch 0000:3b:00.0" {
		t.Fatalf("switch node %+v", n)
	}
	if n := nodeOf(t, nodes, "rp.0000:3a:00.0"); n.PCIeBDF != "0000:3a:00.0" || n.Title != "root port 0000:3a:00.0" {
		t.Fatalf("root port node %+v", n)
	}
}

// Each cascaded switch level sits under the one above it, whether or not sysfs
// exposes pcie_type (without it every bridge port is its own switch node).
func TestDiagramCascadedSwitches(t *testing.T) {
	for _, withType := range []bool{true, false} {
		root, _, _ := mkCascadedSwitchTree(t, withType)
		g, err := DiscoverFrom(root)
		if err != nil {
			t.Fatal(err)
		}
		_, edges := checkDiagram(t, g)
		if withType {
			assertEdge(t, edges, "sw.0000:21:00.0", "rp.0000:20:00.0", "sw.0000:21:00.0")
			assertEdge(t, edges, "sw.0000:23:00.0", "sw.0000:21:00.0", "sw.0000:23:00.0")
			assertEdge(t, edges, "pcie.rnic0", "sw.0000:23:00.0", "rnic0")
			continue
		}
		chain := []string{"rp.0000:20:00.0", "sw.0000:21:00.0", "sw.0000:22:00.0", "sw.0000:23:00.0", "sw.0000:24:00.0"}
		for i := 1; i < len(chain); i++ {
			assertEdge(t, edges, chain[i], chain[i-1], chain[i])
		}
		assertEdge(t, edges, "pcie.rnic0", "sw.0000:24:00.0", "rnic0")
	}
}

// mkPEXTree builds the production layout behind one Gen5 x16 root port: a
// two-level Broadcom PEX890xx whose upper switch (us1) fans out to two lower
// switches. The first lower switch (usA) carries a GPU, a CX-7 and an NVMe
// drive on three downstream ports, sharing the root port; the second (usB)
// carries a second GPU.
//
//	rp ─ us1 ┬ ds1a ─ usA ┬ dsA0 ─ gpu0
//	         │            ├ dsA1 ─ mlx5_0
//	         │            └ dsA2 ─ nvme
//	         └ ds1b ─ usB ─ dsB0 ─ gpu1
func mkPEXTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	rp := "devices/pci0000:26/0000:26:01.0"
	us1 := rp + "/0000:27:00.0"
	ds1a := us1 + "/0000:28:00.0"
	ds1b := us1 + "/0000:28:01.0"
	usA := ds1a + "/0000:29:00.0"
	usB := ds1b + "/0000:2e:00.0"
	mkPCI(t, root, rp, "0x060400", "4", "", "0")
	mkPCI(t, root, us1, "0x060400", "5", "", "0")
	mkPCI(t, root, ds1a, "0x060400", "6", "", "0")
	mkPCI(t, root, ds1b, "0x060400", "6", "", "0")
	mkPCI(t, root, usA, "0x060400", "5", "", "0")
	mkPCI(t, root, usA+"/0000:2a:00.0", "0x060400", "6", "", "0")
	mkPCI(t, root, usA+"/0000:2a:00.0/0000:2b:00.0", "0x030200", "0", "0x10de", "0")
	mkPCI(t, root, usA+"/0000:2a:01.0", "0x060400", "6", "", "0")
	mkPCI(t, root, usA+"/0000:2a:01.0/0000:2c:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, usA+"/0000:2a:02.0", "0x060400", "6", "", "0")
	mkPCI(t, root, usA+"/0000:2a:02.0/0000:2d:00.0", "0x010802", "0", "0x144d", "0")
	mkPCI(t, root, usB, "0x060400", "5", "", "0")
	mkPCI(t, root, usB+"/0000:2f:00.0", "0x060400", "6", "", "0")
	mkPCI(t, root, usB+"/0000:2f:00.0/0000:30:00.0", "0x030200", "0", "0x10de", "0")
	mkIB(t, root, "mlx5_0", "0000:2c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	return root
}

func TestDiagramPEXSharedSwitch(t *testing.T) {
	g, err := DiscoverFrom(mkPEXTree(t))
	if err != nil {
		t.Fatal(err)
	}
	nodes, edges := checkDiagram(t, g)
	const (
		rp  = "rp.0000:26:01.0"
		us1 = "sw.0000:27:00.0"
		usA = "sw.0000:29:00.0"
		usB = "sw.0000:2e:00.0"
	)
	assertEdge(t, edges, rp, "socket0", rp)
	assertEdge(t, edges, us1, rp, us1)
	// The two lower switches are siblings under the upper one.
	assertEdge(t, edges, usA, us1, usA)
	assertEdge(t, edges, usB, us1, usB)
	// GPU and CX-7 share the lower switch; the NVMe drive is no node.
	assertEdge(t, edges, "pcie.gpu0", usA, "gpu0")
	assertEdge(t, edges, "pcie.rnic0", usA, "rnic0")
	assertEdge(t, edges, "pcie.gpu1", usB, "gpu1")
	for _, n := range nodes {
		if n.PCIeBDF == "0000:2d:00.0" {
			t.Fatalf("NVMe drive drawn as %+v", n)
		}
	}
	// 1 socket, 1 memory node, 1 root port, 3 switches, 1 RNIC, 2 GPUs.
	if len(nodes) != 9 {
		t.Fatalf("%d nodes: %+v", len(nodes), nodes)
	}
	p, _ := g.Path("rnic0", "gpu1")
	if p.Distance != model.DistanceSameSwitch {
		t.Fatalf("rnic0→gpu1 %+v", p)
	}
}

// A hand-written topology file carries no Upstream: devices hang off their
// Switch, then their root port, and a switch off its root port.
func TestDiagramWithoutUpstream(t *testing.T) {
	g := Fig2()
	for i := range g.Links {
		g.Links[i].Upstream = ""
	}
	g.RNICs[0].Switch = "sw.0"
	_, edges := g.Diagram()
	assertEdge(t, edges, "pcie.rnic0", "sw.0", "rnic0")
	assertEdge(t, edges, "pcie.rnic1", "rp.1", "rnic1")
	assertEdge(t, edges, "sw.1", "rp.1", "sw.1")
}

func TestDiagramUPIBetweenMoreThanTwoSockets(t *testing.T) {
	g := Graph{
		RNICs: []model.RNIC{{ID: "rnic0", Socket: 0, RootPort: "rp.0"}},
		Endpoints: []model.Endpoint{
			{ID: "mem0", Kind: model.EndpointMemory, Index: 0, Socket: 0, NUMA: 0},
			{ID: "mem1", Kind: model.EndpointMemory, Index: 1, Socket: 1, NUMA: 1},
			{ID: "mem2", Kind: model.EndpointMemory, Index: 2, Socket: 2, NUMA: 2},
		},
		Links: []model.Link{
			{ID: "upi.0-2", Kind: model.LinkKindUPI},
			{ID: "upi.x", Kind: model.LinkKindUPI},
		},
	}
	_, edges := g.Diagram()
	if len(edges) != 1 {
		t.Fatalf("edges %+v, want upi.0-2 only", edges)
	}
	assertEdge(t, edges, "upi.0-2", "socket0", "socket2")
}

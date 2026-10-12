package topology

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// mkRootPortTree builds a synthetic host with one root port per NUMA node in
// numas, an RNIC and a GPU directly under each, both on that node.
func mkRootPortTree(t *testing.T, numas []int) string {
	t.Helper()
	root := t.TempDir()
	for i, n := range numas {
		bus := fmt.Sprintf("%02x", 0x30+0x10*i)
		rp := fmt.Sprintf("devices/pci0000:%s/0000:%s:00.0", bus, bus)
		numa := strconv.Itoa(n)
		mkPCI(t, root, rp, "0x060400", "4", "", numa)
		mkPCI(t, root, rp+fmt.Sprintf("/0000:%02x:00.0", 0x31+0x10*i), "0x020000", "0", "0x15b3", numa)
		mkPCI(t, root, rp+fmt.Sprintf("/0000:%02x:00.1", 0x31+0x10*i), "0x030200", "0", "0x10de", numa)
		mkIB(t, root, "mlx5_"+strconv.Itoa(i), fmt.Sprintf("0000:%02x:00.0", 0x31+0x10*i), "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	}
	return root
}

// mkNUMANodes writes nodeN/cpulist for every node, two CPUs each, and, when
// pkgs is not nil, the physical_package_id of those CPUs.
func mkNUMANodes(t *testing.T, root string, nodes []int, pkgs map[int]int) {
	t.Helper()
	for _, n := range nodes {
		lo := 2 * n
		mustWrite(t, filepath.Join(root, "devices/system/node", "node"+strconv.Itoa(n), "cpulist"), fmt.Sprintf("%d-%d\n", lo, lo+1))
		if pkgs == nil {
			continue
		}
		for c := lo; c <= lo+1; c++ {
			mustWrite(t, filepath.Join(root, "devices/system/cpu", "cpu"+strconv.Itoa(c), "topology/physical_package_id"), strconv.Itoa(pkgs[n])+"\n")
		}
	}
}

func linkIDs(g Graph, kind string) []string {
	var out []string
	for _, l := range g.Links {
		if kind == "" || l.Kind == kind {
			out = append(out, l.ID)
		}
	}
	return out
}

func quietLog(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	old := logf
	logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logf = old })
	return &lines
}

// Sub-NUMA clustering: two packages of two NUMA nodes each. The socket is the
// package, NUMA nodes of one package are joined by snc_mesh links, and upi
// links exist only between packages.
func TestDiscoverSNCPackages(t *testing.T) {
	logs := quietLog(t)
	root := mkRootPortTree(t, []int{0, 1, 2, 3})
	mkNUMANodes(t, root, []int{0, 1, 2, 3}, map[int]int{0: 0, 1: 0, 2: 1, 3: 1})

	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(*logs) != 0 {
		t.Fatalf("readable packages must not log a fallback: %v", *logs)
	}
	for i, n := range g.RNICs {
		if n.NUMA != i || n.Socket != i/2 {
			t.Fatalf("%s: numa=%d socket=%d, want numa=%d socket=%d", n.ID, n.NUMA, n.Socket, i, i/2)
		}
	}
	for _, e := range g.Endpoints {
		if e.Socket != e.NUMA/2 {
			t.Fatalf("%s: socket %d for NUMA %d, want package %d", e.ID, e.Socket, e.NUMA, e.NUMA/2)
		}
	}
	if got := linkIDs(g, model.LinkKindUPI); !reflect.DeepEqual(got, []string{"upi.0-1"}) {
		t.Fatalf("upi links %v, want only upi.0-1 between the packages", got)
	}
	if got := linkIDs(g, model.LinkKindSNCMesh); !reflect.DeepEqual(got, []string{"mesh.0-1", "mesh.2-3"}) {
		t.Fatalf("mesh links %v", got)
	}
	for _, l := range g.LinksByKind(model.LinkKindSNCMesh) {
		if l.CapacityBPS != 0 || !l.CapacityAssumed {
			t.Fatalf("%s: capacity %d assumed=%v, want unknown and assumed", l.ID, l.CapacityBPS, l.CapacityAssumed)
		}
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		rnic, ep   string
		dist       model.Distance
		via, not   string
		affinitive bool
	}{
		{"rnic0", "gpu1", model.DistanceCrossRoot, "mesh.0-1", "upi.0-1", false},
		{"rnic0", "gpu2", model.DistanceCrossSocket, "upi.0-1", "mesh.0-1", false},
		{"rnic3", "gpu2", model.DistanceCrossRoot, "mesh.2-3", "upi.0-1", false},
		{"rnic0", "mem0", model.DistanceLocalMem, "mem.0", "mesh.0-1", true},
		{"rnic0", "mem1", model.DistanceRemoteMem, "mesh.0-1", "upi.0-1", false},
		{"rnic0", "mem3", model.DistanceRemoteMem, "upi.0-1", "mesh.2-3", false},
	}
	for _, c := range cases {
		p, ok := g.Path(c.rnic, c.ep)
		if !ok || p.Distance != c.dist || !p.ContainsLink(c.via) || p.ContainsLink(c.not) || p.Affinitive != c.affinitive {
			t.Fatalf("%s->%s: %+v, want %s via %s and not %s", c.rnic, c.ep, p, c.dist, c.via, c.not)
		}
	}
	var mems []string
	for _, e := range g.AffinitiveEndpoints("rnic0") {
		if e.Kind == model.EndpointMemory {
			mems = append(mems, e.ID)
		}
	}
	if !reflect.DeepEqual(mems, []string{"mem0"}) {
		t.Fatalf("affinitive memory of rnic0 %v, want only its own NUMA node", mems)
	}
}

// Fig2SNC is wired the way discovery wires an SNC host.
func TestFig2SNC(t *testing.T) {
	g := Fig2SNC()
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := linkIDs(g, model.LinkKindUPI); !reflect.DeepEqual(got, []string{"upi.0-1"}) {
		t.Fatalf("upi links %v", got)
	}
	if got := linkIDs(g, model.LinkKindSNCMesh); !reflect.DeepEqual(got, []string{"mesh.0-1", "mesh.2-3"}) {
		t.Fatalf("mesh links %v", got)
	}
	for _, c := range []struct {
		rnic, ep, via string
		dist          model.Distance
	}{
		{"rnic0", "gpu2", "mesh.0-1", model.DistanceCrossRoot},
		{"rnic0", "gpu4", "upi.0-1", model.DistanceCrossSocket},
		{"rnic2", "mem3", "mesh.2-3", model.DistanceRemoteMem},
		{"rnic2", "mem0", "upi.0-1", model.DistanceRemoteMem},
	} {
		p, ok := g.Path(c.rnic, c.ep)
		if !ok || p.Distance != c.dist || !p.ContainsLink(c.via) {
			t.Fatalf("%s->%s: %+v", c.rnic, c.ep, p)
		}
	}
	if p, _ := g.Path("rnic1", "mem1"); p.Distance != model.DistanceLocalMem || !p.Affinitive {
		t.Fatalf("rnic1->mem1: %+v", p)
	}
	if g2, err := Named("fig2-snc"); err != nil || g2.Name != "fig2-snc" {
		t.Fatalf("Named(fig2-snc): %v %v", g2.Name, err)
	}
}

// pathKeys renders every path as "rnic>endpoint distance link,link,...".
func pathKeys(g Graph) []string {
	var out []string
	for _, p := range g.Paths {
		out = append(out, fmt.Sprintf("%s>%s %s %s", p.RNIC, p.Endpoint, p.Distance.Key(), strings.Join(p.LinkIDs, ",")))
	}
	return out
}

// A host without sub-NUMA clustering (one NUMA node per package) keeps the
// graph hostping built before packages were read: same sockets, link IDs,
// distances and hop lists as a tree whose package information is missing,
// which takes the NUMA-as-socket fallback. Its package numbering does not
// matter, so even packages numbered against the NUMA nodes change nothing.
func TestDiscoverWithoutSNCUnchanged(t *testing.T) {
	logs := quietLog(t)
	numas := []int{0, 0, 1, 1}

	legacy := mkRootPortTree(t, numas)
	mkNUMANodes(t, legacy, []int{0, 1}, nil)
	want, err := DiscoverFrom(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "modeling each NUMA node as a socket") {
		t.Fatalf("unreadable packages should log the fallback once: %v", *logs)
	}

	for _, pkgs := range []map[int]int{{0: 0, 1: 1}, {0: 1, 1: 0}} {
		root := mkRootPortTree(t, numas)
		mkNUMANodes(t, root, []int{0, 1}, pkgs)
		g, err := DiscoverFrom(root)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(g.Links, want.Links) {
			t.Fatalf("packages %v: links\n%+v\nwant\n%+v", pkgs, g.Links, want.Links)
		}
		if !reflect.DeepEqual(pathKeys(g), pathKeys(want)) {
			t.Fatalf("packages %v: paths\n%v\nwant\n%v", pkgs, pathKeys(g), pathKeys(want))
		}
		if !reflect.DeepEqual(g.RNICs, want.RNICs) || !reflect.DeepEqual(g.Endpoints, want.Endpoints) {
			t.Fatalf("packages %v: devices changed\n%+v\n%+v", pkgs, g.RNICs, g.Endpoints)
		}
	}

	// The IDs and hop lists hostping produced before it read packages.
	wantLinks := []string{
		"pcie.rnic0", "rp.0000:30:00.0", "pcie.rnic1", "rp.0000:40:00.0",
		"pcie.rnic2", "rp.0000:50:00.0", "pcie.rnic3", "rp.0000:60:00.0",
		"mem.0", "mem.1", "pcie.gpu0", "pcie.gpu1", "pcie.gpu2", "pcie.gpu3",
		"upi.0-1",
	}
	if got := linkIDs(want, ""); !reflect.DeepEqual(got, wantLinks) {
		t.Fatalf("link IDs %v\nwant %v", got, wantLinks)
	}
	for _, k := range []string{
		"rnic0>gpu1 cross_root pcie.rnic0,rp.0000:30:00.0,rp.0000:40:00.0,pcie.gpu1",
		"rnic0>gpu2 cross_socket pcie.rnic0,rp.0000:30:00.0,upi.0-1,rp.0000:50:00.0,pcie.gpu2",
		"rnic0>mem0 local_mem pcie.rnic0,rp.0000:30:00.0,mem.0",
		"rnic0>mem1 remote_mem pcie.rnic0,rp.0000:30:00.0,upi.0-1,mem.1",
	} {
		found := false
		for _, got := range pathKeys(want) {
			found = found || got == k
		}
		if !found {
			t.Fatalf("missing path %q in %v", k, pathKeys(want))
		}
	}
}

// Memory-only NUMA nodes (empty cpulist) have no package: they neither turn
// SNC on nor stop it from being detected.
func TestNUMAPackagesSkipsMemoryOnlyNodes(t *testing.T) {
	root := t.TempDir()
	mkNUMANodes(t, root, []int{0, 1}, map[int]int{0: 0, 1: 0})
	mustWrite(t, filepath.Join(root, "devices/system/node/node2/cpulist"), "\n")
	pkgs, err := numaPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pkgs, map[int]int{0: 0, 1: 0}) || !snc(pkgs) {
		t.Fatalf("packages %v", pkgs)
	}
	s := discoverSockets(root)
	if s.of(1) != 0 || s.of(2) != 2 {
		t.Fatalf("socket of node1=%d node2=%d", s.of(1), s.of(2))
	}
}

func TestNUMAPackagesUnreadable(t *testing.T) {
	root := t.TempDir()
	mkNUMANodes(t, root, []int{0, 1}, map[int]int{0: 0, 1: 0})
	// cpu3 (node1) loses its package.
	mustWrite(t, filepath.Join(root, "devices/system/cpu/cpu3/topology/physical_package_id"), "\n")
	if _, err := numaPackages(root); err == nil {
		t.Fatal("want an error for a CPU without a package")
	}
	logs := quietLog(t)
	if s := discoverSockets(root); s.of(1) != 1 {
		t.Fatalf("fallback socket of node1 = %d, want the NUMA node", s.of(1))
	}
	if len(*logs) != 1 {
		t.Fatalf("want one fallback log line, got %v", *logs)
	}
}

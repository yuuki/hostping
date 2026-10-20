package topology

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
)

func TestDiscoverFromPCIWalk(t *testing.T) {
	root := t.TempDir()
	// Socket 0 RP0 + switch: RNIC + GPU share a switch.
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0", "0x060400", "6", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/0000:3c:00.1", "0x030200", "0", "0x10de", "0")
	// Socket 0 RP1 + switch: second rail on the same socket.
	mkPCI(t, root, "devices/pci0000:4a/0000:4a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:4a/0000:4a:00.0/0000:4b:00.0", "0x060400", "6", "", "0")
	mkPCI(t, root, "devices/pci0000:4a/0000:4a:00.0/0000:4b:00.0/0000:4c:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, "devices/pci0000:4a/0000:4a:00.0/0000:4b:00.0/0000:4c:00.1", "0x030200", "0", "0x10de", "0")
	// Socket 1 RP, device directly under the root port.
	mkPCI(t, root, "devices/pci0000:5a/0000:5a:00.0", "0x060400", "4", "", "1")
	mkPCI(t, root, "devices/pci0000:5a/0000:5a:00.0/0000:5c:00.0", "0x020000", "0", "0x15b3", "1")
	mkPCI(t, root, "devices/pci0000:5a/0000:5a:00.0/0000:5c:00.1", "0x030200", "0", "0x10de", "1")

	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mkIB(t, root, "mlx5_1", "0000:4c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0201")
	mkIB(t, root, "mlx5_2", "0000:5c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0102")

	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	mustWrite(t, filepath.Join(root, "devices/system/node/node1"), "")

	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.RNICs) != 3 {
		t.Fatalf("rnics=%d", len(g.RNICs))
	}
	if g.RNICs[0].RootPort != "rp.0000:3a:00.0" || g.RNICs[1].RootPort != "rp.0000:4a:00.0" {
		t.Fatalf("root ports %+v %+v", g.RNICs[0], g.RNICs[1])
	}
	if g.RNICs[0].RootPort == g.RNICs[1].RootPort {
		t.Fatal("distinct RNICs collapsed onto one fake root port")
	}
	if g.RNICs[0].Fabric == g.RNICs[1].Fabric {
		t.Fatalf("distinct RoCE /24s should not share fabric: %s", g.RNICs[0].Fabric)
	}
	if g.RNICs[0].Fabric != g.RNICs[2].Fabric {
		t.Fatalf("same /24 should share fabric: %s vs %s", g.RNICs[0].Fabric, g.RNICs[2].Fabric)
	}

	var gpus int
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			gpus++
		}
	}
	if gpus != 3 {
		t.Fatalf("gpus=%d", gpus)
	}

	p, ok := g.Path("rnic0", "gpu0")
	if !ok || p.Distance != model.DistanceSameSwitch || !p.Affinitive {
		t.Fatalf("same-switch %+v", p)
	}
	if !p.ContainsLink("sw.0000:3b:00.0") {
		t.Fatalf("same-switch path omitted the switch hop: %+v", p.LinkIDs)
	}
	p, _ = g.Path("rnic0", "gpu1")
	if p.Distance != model.DistanceCrossRoot {
		t.Fatalf("cross-root distance %d", p.Distance)
	}
	p, _ = g.Path("rnic0", "gpu2")
	if p.Distance != model.DistanceCrossSocket || !p.ContainsLink("upi.0-1") {
		t.Fatalf("cross-socket %+v", p)
	}
	if !hasLinkKind(g, "pcie_switch") {
		t.Fatal("expected switch hops")
	}
}

// A real PCIe switch exposes one upstream port (type 5) and several
// downstream ports (type 6). RNIC and GPU on different downstream ports are
// still Fig.4 ❶ and must not climb to the root port.
func TestDiscoverSameSwitchAcrossDownstreamPorts(t *testing.T) {
	root := t.TempDir()
	rp := "devices/pci0000:3a/0000:3a:00.0"
	up := rp + "/0000:3b:00.0"
	mkPCI(t, root, rp, "0x060400", "4", "", "0")
	mkPCI(t, root, up, "0x060400", "5", "", "0")
	mkPCI(t, root, up+"/0000:3c:00.0", "0x060400", "6", "", "0")
	mkPCI(t, root, up+"/0000:3c:00.0/0000:3d:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, up+"/0000:3c:01.0", "0x060400", "6", "", "0")
	mkPCI(t, root, up+"/0000:3c:01.0/0000:3e:00.0", "0x030200", "0", "0x10de", "0")
	mkIB(t, root, "mlx5_0", "0000:3d:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")

	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	const sw = "sw.0000:3b:00.0"
	if g.RNICs[0].Switch != sw {
		t.Fatalf("RNIC switch %q, want the upstream port %q", g.RNICs[0].Switch, sw)
	}
	gpu, ok := g.Endpoint("gpu0")
	if !ok || gpu.Switch != sw {
		t.Fatalf("GPU switch %q, want %q", gpu.Switch, sw)
	}
	p, ok := g.Path("rnic0", "gpu0")
	if !ok || p.Distance != model.DistanceSameSwitch || !p.Affinitive {
		t.Fatalf("want same_switch, got %+v", p)
	}
	if !p.ContainsLink(sw) {
		t.Fatalf("path omitted the switch: %v", p.LinkIDs)
	}
	if p.ContainsLink("rp.0000:3a:00.0") {
		t.Fatalf("same-switch P2P must not traverse the root port: %v", p.LinkIDs)
	}
	if dup := duplicateLink(p); dup != "" {
		t.Fatalf("hop %q repeated: %v", dup, p.LinkIDs)
	}
	if got := switchLinkIDs(g); len(got) != 1 || got[0] != sw {
		t.Fatalf("one switch expected, got %v", got)
	}
}

// Two distinct switches under one root port stay Fig.4 ❷.
func TestDiscoverMultiSwitchUnderOneRootPort(t *testing.T) {
	root := t.TempDir()
	rp := "devices/pci0000:3a/0000:3a:00.0"
	upA := rp + "/0000:3b:00.0"
	upB := rp + "/0000:4b:00.0"
	mkPCI(t, root, rp, "0x060400", "4", "", "0")
	mkPCI(t, root, upA, "0x060400", "5", "", "0")
	mkPCI(t, root, upA+"/0000:3c:00.0", "0x060400", "6", "", "0")
	mkPCI(t, root, upA+"/0000:3c:00.0/0000:3d:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, upB, "0x060400", "5", "", "0")
	mkPCI(t, root, upB+"/0000:4c:00.0", "0x060400", "6", "", "0")
	mkPCI(t, root, upB+"/0000:4c:00.0/0000:4d:00.0", "0x030200", "0", "0x10de", "0")
	mkIB(t, root, "mlx5_0", "0000:3d:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")

	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.Path("rnic0", "gpu0")
	if !ok || p.Distance != model.DistanceMultiSwitch || !p.Affinitive {
		t.Fatalf("want multi_switch, got %+v", p)
	}
	for _, id := range []string{"sw.0000:3b:00.0", "rp.0000:3a:00.0", "sw.0000:4b:00.0"} {
		if !p.ContainsLink(id) {
			t.Fatalf("multi-switch path missing %s: %v", id, p.LinkIDs)
		}
	}
	if dup := duplicateLink(p); dup != "" {
		t.Fatalf("hop %q repeated: %v", dup, p.LinkIDs)
	}
}

// RNIC and GPU behind the same downstream port are Fig.4 ❶ as well.
func TestDiscoverSameDownstreamPort(t *testing.T) {
	root := t.TempDir()
	down := "devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0/0000:3c:00.0"
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3b:00.0", "0x060400", "5", "", "0")
	mkPCI(t, root, down, "0x060400", "6", "", "0")
	mkPCI(t, root, down+"/0000:3d:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, down+"/0000:3e:00.0", "0x030200", "0", "0x10de", "0")
	mkIB(t, root, "mlx5_0", "0000:3d:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")

	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.Path("rnic0", "gpu0")
	if !ok || p.Distance != model.DistanceSameSwitch || !p.Affinitive {
		t.Fatalf("want same_switch, got %+v", p)
	}
	if p.ContainsLink("rp.0000:3a:00.0") {
		t.Fatalf("same-switch P2P must not traverse the root port: %v", p.LinkIDs)
	}
	if dup := duplicateLink(p); dup != "" {
		t.Fatalf("hop %q repeated: %v", dup, p.LinkIDs)
	}
}

func duplicateLink(p model.Path) string {
	seen := map[string]bool{}
	for _, id := range p.LinkIDs {
		if seen[id] {
			return id
		}
		seen[id] = true
	}
	return ""
}

func switchLinkIDs(g Graph) []string {
	var out []string
	for _, l := range g.Links {
		if l.Kind == "pcie_switch" {
			out = append(out, l.ID)
		}
	}
	return out
}

func TestMutualPairRequiresSameFabric(t *testing.T) {
	a := model.RNIC{ID: "a", Device: "mlx5_0", Fabric: "v4:192.168.1.0"}
	b := model.RNIC{ID: "b", Device: "mlx5_1", Fabric: "v4:192.168.2.0"}
	c := model.RNIC{ID: "c", Device: "mlx5_2", Fabric: "v4:192.168.1.0"}
	if MutualPair(a, b, model.MutualScopeRail) {
		t.Fatal("cross-rail pair")
	}
	if !MutualPair(a, c, model.MutualScopeRail) {
		t.Fatal("same-rail pair")
	}
	if !MutualPair(model.RNIC{ID: "x", Device: "mlx5_0"}, model.RNIC{ID: "y", Device: "mlx5_1"}, model.MutualScopeRail) {
		t.Fatal("empty fabric should pair (Fig.2)")
	}
	if MutualPair(model.RNIC{ID: "x", Device: "mlx5_0", Fabric: "unknown"}, model.RNIC{ID: "y", Device: "mlx5_1", Fabric: "unknown"}, model.MutualScopeRail) {
		t.Fatal("unknown RoCEv2 must not pair")
	}
}

func TestDiscoverRoCEv1OnlyIsUnknownFabric(t *testing.T) {
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkIBRoCEv1Only(t, root, "mlx5_0", "0000:3c:00.0", "fe80:0000:0000:0000:0200:5eff:fe00:0001")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if g.RNICs[0].Fabric != "unknown" {
		t.Fatalf("RoCEv1-only GID0 must not invent a rail: %s", g.RNICs[0].Fabric)
	}
}

func TestDiscoverUsesActivePortRate(t *testing.T) {
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/1/state"), "1: DOWN\n")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/1/rate"), "200 Gb/sec (4X HDR)\n")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/2/state"), "4: ACTIVE\n")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/2/rate"), "100 Gb/sec (4X EDR)\n")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/2/gids/0"), "fe80:0000:0000:0000:0200:5eff:fe00:0001\n")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/2/gid_attrs/types/0"), "IB/RoCE v1\n")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/2/gids/1"), "0000:0000:0000:0000:0000:ffff:c0a8:0a01\n")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/2/gid_attrs/types/1"), "RoCE v2\n")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if g.RNICs[0].LineRate != 100_000_000_000 {
		t.Fatalf("active-port rate %d", g.RNICs[0].LineRate)
	}
	if g.RNICs[0].Fabric != "v4:192.168.10.0" {
		t.Fatalf("fabric %s", g.RNICs[0].Fabric)
	}
}

func TestDiscoverFailsClosedWithoutIB(t *testing.T) {
	root := t.TempDir()
	_, err := DiscoverFrom(root)
	if err == nil {
		t.Fatal("expected error without infiniband class")
	}
}

func TestDiscoverMemoryPrefersNUMAOverEDACWithoutNodes(t *testing.T) {
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	mustWrite(t, filepath.Join(root, "devices/system/node/node1"), "")
	for i := 0; i < 8; i++ {
		mustWrite(t, filepath.Join(root, "devices/system/edac/mc", "mc"+strconv.Itoa(i), "ue_count"), "0\n")
	}
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	got := memoryEndpoints(g)
	if len(got) != 2 || got[0].NUMA != 0 || got[1].NUMA != 1 {
		t.Fatalf("want mem0/mem1 on NUMA 0/1, got %+v", got)
	}
}

func TestDiscoverEDACFallbackUsesExplicitNodes(t *testing.T) {
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/edac/mc/mc0/csrow0/node"), "0\n")
	mustWrite(t, filepath.Join(root, "devices/system/edac/mc/mc1/csrow0/node"), "1\n")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	got := memoryEndpoints(g)
	if len(got) != 2 || got[0].NUMA != 0 || got[1].NUMA != 1 {
		t.Fatalf("EDAC fallback %+v", got)
	}
}

func TestDiscoverSkipsMemoryOnlyNUMA(t *testing.T) {
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0/cpulist"), "0-31\n")
	mustWrite(t, filepath.Join(root, "devices/system/node/node1/cpulist"), "32-63\n")
	mustWrite(t, filepath.Join(root, "devices/system/node/node2/cpulist"), "\n")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	got := memoryEndpoints(g)
	if len(got) != 2 || got[0].ID != "mem0" || got[1].ID != "mem1" {
		t.Fatalf("GPU NUMA must not become a memory endpoint: %+v", got)
	}
}

func TestDiscoverSkipsBondIBDevice(t *testing.T) {
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, "devices/pci0000:4a/0000:4a:00.0", "0x060400", "4", "", "1")
	mkPCI(t, root, "devices/pci0000:4a/0000:4a:00.0/0000:4c:00.0", "0x020000", "0", "0x15b3", "1")
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mkIB(t, root, "mlx5_bond_0", "0000:4c:00.0", "0000:0000:0000:0000:0000:ffff:0aff:ff27")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.RNICs) != 1 || g.RNICs[0].Device != "mlx5_0" {
		t.Fatalf("bond IB device must be omitted: %+v", g.RNICs)
	}
}

func TestDiscoverSkipsBondMasterNetdev(t *testing.T) {
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, "devices/pci0000:4a/0000:4a:00.0", "0x060400", "4", "", "1")
	mkPCI(t, root, "devices/pci0000:4a/0000:4a:00.0/0000:4c:00.0", "0x020000", "0", "0x15b3", "1")
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mkIB(t, root, "mlx5_1", "0000:4c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0201")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/device/net/bond0/operstate"), "up\n")
	mustWrite(t, filepath.Join(root, "class/net/bond0/bonding/mode"), "active-backup\n")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.RNICs) != 1 || g.RNICs[0].Device != "mlx5_1" {
		t.Fatalf("bond-master netdev must be omitted: %+v", g.RNICs)
	}
}

func memoryEndpoints(g Graph) []model.Endpoint {
	var out []model.Endpoint
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointMemory {
			out = append(out, e)
		}
	}
	return out
}

func hasLinkKind(g Graph, kind string) bool {
	for _, l := range g.Links {
		if l.Kind == kind {
			return true
		}
	}
	return false
}

func mkPCI(t *testing.T, root, rel, class, pcieType, vendor, numa string) {
	t.Helper()
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "class"), class+"\n")
	if pcieType != "" {
		mustWrite(t, filepath.Join(dir, "pcie_type"), pcieType+"\n")
	}
	if vendor != "" {
		mustWrite(t, filepath.Join(dir, "vendor"), vendor+"\n")
	}
	mustWrite(t, filepath.Join(dir, "numa_node"), numa+"\n")
	bdf := filepath.Base(rel)
	devDir := filepath.Join(root, "bus/pci/devices")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(devDir, dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(devDir, bdf)
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func mkIB(t *testing.T, root, name, bdf, rocev2 string) {
	t.Helper()
	mkIBDevice(t, root, name, bdf)
	dev := filepath.Join(root, "class/infiniband", name)
	mustWrite(t, filepath.Join(dev, "ports/1/state"), "4: ACTIVE\n")
	mustWrite(t, filepath.Join(dev, "ports/1/rate"), "200 Gb/sec (4X HDR)\n")
	// Real mlx5: GID0 is fe80 RoCEv1; the IPv4-mapped RoCEv2 entry is later.
	mustWrite(t, filepath.Join(dev, "ports/1/gids/0"), "fe80:0000:0000:0000:0200:5eff:fe00:0001\n")
	mustWrite(t, filepath.Join(dev, "ports/1/gid_attrs/types/0"), "IB/RoCE v1\n")
	mustWrite(t, filepath.Join(dev, "ports/1/gids/1"), rocev2+"\n")
	mustWrite(t, filepath.Join(dev, "ports/1/gid_attrs/types/1"), "RoCE v2\n")
}

func mkIBRoCEv1Only(t *testing.T, root, name, bdf, gid string) {
	t.Helper()
	mkIBDevice(t, root, name, bdf)
	dev := filepath.Join(root, "class/infiniband", name)
	mustWrite(t, filepath.Join(dev, "ports/1/state"), "4: ACTIVE\n")
	mustWrite(t, filepath.Join(dev, "ports/1/rate"), "200 Gb/sec (4X HDR)\n")
	mustWrite(t, filepath.Join(dev, "ports/1/gids/0"), gid+"\n")
	mustWrite(t, filepath.Join(dev, "ports/1/gid_attrs/types/0"), "IB/RoCE v1\n")
}

func mkIBDevice(t *testing.T, root, name, bdf string) {
	t.Helper()
	dev := filepath.Join(root, "class/infiniband", name)
	if err := os.MkdirAll(filepath.Join(dev, "ports/1/gids"), 0o755); err != nil {
		t.Fatal(err)
	}
	pciDir := filepath.Join(root, "bus/pci/devices", bdf)
	target, err := filepath.Rel(dev, pciDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dev, "device")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dev, "device", "numa_node"), readIfExists(filepath.Join(pciDir, "numa_node")))
}

func readIfExists(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "0\n"
	}
	return string(b)
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkLinkAttrs writes the negotiated and maximum link attributes of the PCI
// function at rel.
func mkLinkAttrs(t *testing.T, root, rel, cur string, curW int, max string, maxW int) {
	t.Helper()
	dir := filepath.Join(root, rel)
	mustWrite(t, filepath.Join(dir, "current_link_speed"), cur+"\n")
	mustWrite(t, filepath.Join(dir, "current_link_width"), strconv.Itoa(curW)+"\n")
	mustWrite(t, filepath.Join(dir, "max_link_speed"), max+"\n")
	mustWrite(t, filepath.Join(dir, "max_link_width"), strconv.Itoa(maxW)+"\n")
}

// PCIe hop capacities come from what each link can train to — min(device,
// upstream port) max_link_* — and never from what it trained to, so a
// down-trained link does not become its own baseline. The negotiated value is
// kept on the link as evidence.
func TestDiscoverPCIeCapacityFromSysfs(t *testing.T) {
	const (
		gen4 = "16.0 GT/s PCIe"
		gen5 = "32.0 GT/s PCIe"
	)
	gen4x16 := pci.EffectiveBPS(16, 16)
	gen5x16 := pci.EffectiveBPS(32, 16)

	root := t.TempDir()
	rp := "devices/pci0000:3a/0000:3a:00.0"
	up := rp + "/0000:3b:00.0"
	dn0 := up + "/0000:3c:00.0"
	dn1 := up + "/0000:3c:01.0"
	nic := dn0 + "/0000:3d:00.0"
	gpu := dn1 + "/0000:3e:00.0"
	mkPCI(t, root, rp, "0x060400", "4", "", "0")
	mkPCI(t, root, up, "0x060400", "5", "", "0")
	mkPCI(t, root, dn0, "0x060400", "6", "", "0")
	mkPCI(t, root, dn1, "0x060400", "6", "", "0")
	mkPCI(t, root, nic, "0x020000", "0", "0x15b3", "0")
	mkPCI(t, root, gpu, "0x030200", "0", "0x10de", "0")
	// Gen5 x16 all the way down; the RNIC's slot trained to Gen4.
	mkLinkAttrs(t, root, rp, gen5, 16, gen5, 16)
	mkLinkAttrs(t, root, up, gen5, 16, gen5, 16)
	mkLinkAttrs(t, root, dn0, gen4, 16, gen5, 16)
	mkLinkAttrs(t, root, dn1, gen4, 16, gen4, 16)
	mkLinkAttrs(t, root, nic, gen4, 16, gen5, 16)
	// A Gen5 GPU on a Gen4 downstream port: the slot is the limit.
	mkLinkAttrs(t, root, gpu, gen4, 16, gen5, 16)
	mkIB(t, root, "mlx5_0", "0000:3d:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")

	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id       string
		capacity uint64
		neg      uint64
	}{
		{"pcie.rnic0", gen5x16, gen4x16},
		{"pcie.gpu0", gen4x16, gen4x16},
		{"rp.0000:3a:00.0", gen5x16, gen5x16},
	}
	for _, tc := range cases {
		l, ok := g.Link(tc.id)
		if !ok {
			t.Fatalf("no link %s", tc.id)
		}
		if l.CapacityBPS != tc.capacity || l.NegotiatedBPS != tc.neg {
			t.Errorf("%s: capacity %d negotiated %d, want %d / %d", tc.id, l.CapacityBPS, l.NegotiatedBPS, tc.capacity, tc.neg)
		}
		if l.CapacityAssumed {
			t.Errorf("%s: capacity read from sysfs marked assumed", tc.id)
		}
	}
	if g.RNICs[0].PCIeCapBPS != gen5x16 {
		t.Fatalf("RNIC must carry its hop's capability, got %d", g.RNICs[0].PCIeCapBPS)
	}
	// The switch link is not read from sysfs even when it looks plausible.
	assertSwitchHopsAssumed(t, g)
}

// assertSwitchHopsAssumed checks every switch hop carries the nominal
// capacity, marked assumed, with no negotiated value.
func assertSwitchHopsAssumed(t *testing.T, g Graph) {
	t.Helper()
	ids := switchLinkIDs(g)
	if len(ids) == 0 {
		t.Fatal("tree has no switch hop")
	}
	for _, id := range ids {
		l, _ := g.Link(id)
		if l.CapacityBPS != pcieGen4x16 || !l.CapacityAssumed || l.NegotiatedBPS != 0 {
			t.Errorf("%s: capacity %d assumed %v negotiated %d, want %d assumed with none", id, l.CapacityBPS, l.CapacityAssumed, l.NegotiatedBPS, pcieGen4x16)
		}
	}
}

// assertLocalMemCeiling checks the affinitive local-memory path of rnic0 is
// bounded by the Fig.4 bus, not by a switch hop's sysfs speed, and that no
// hop claims a negotiated rate above its capacity.
func assertLocalMemCeiling(t *testing.T, g Graph) {
	t.Helper()
	if pcieGen4x16 != baseline.Fig4BusBPS {
		t.Fatalf("pcieGen4x16 %d drifted from the Fig.4 bus %d", pcieGen4x16, baseline.Fig4BusBPS)
	}
	var found bool
	for _, p := range g.Paths {
		if p.RNIC != "rnic0" || p.Distance != model.DistanceLocalMem {
			continue
		}
		found = true
		if !p.Affinitive {
			t.Errorf("%s->%s: local memory path not affinitive", p.RNIC, p.Endpoint)
		}
		if got := g.PathCapacity(p); got != baseline.Fig4BusBPS {
			t.Errorf("%s->%s over %v: path capacity %d, want the Fig.4 bus %d", p.RNIC, p.Endpoint, p.LinkIDs, got, baseline.Fig4BusBPS)
		}
	}
	if !found {
		t.Fatal("no local memory path for rnic0")
	}
	for _, l := range g.Links {
		if l.NegotiatedBPS > l.CapacityBPS {
			t.Errorf("%s: negotiated %d above capacity %d", l.ID, l.NegotiatedBPS, l.CapacityBPS)
		}
	}
}

// mkSwitchRNIC400 registers rnic0 at bdf as a 400G port on NUMA node 0.
func mkSwitchRNIC400(t *testing.T, root, bdf string) {
	t.Helper()
	mkIB(t, root, "rdma0", bdf, "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "class/infiniband/rdma0/ports/1/rate"), "400 Gb/sec (4X NDR)\n")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
}

// mkCascadedSwitchTree builds a 400G RNIC on a Gen5 x16 link behind two
// cascaded switch levels:
//
//	rp ─ us1 ═ ds1 ┄ us2 ═ ds2 ─ rnic
//
// The root-port link and the RNIC link are physical Gen5 x16. The link between
// the upper switch's downstream port (ds1) and the lower switch's upstream port
// (us2) is an in-package virtual link that reports 5 GT/s x16 as both current
// and maximum on both ends, although loopback through it carries several times
// that. With withType false no bridge has a pcie_type attribute, as on kernels
// that do not expose one.
func mkCascadedSwitchTree(t *testing.T, withType bool) (root string, us2, ds2 string) {
	t.Helper()
	const (
		gen1 = "5.0 GT/s PCIe"
		gen5 = "32.0 GT/s PCIe"
	)
	typ := func(v string) string {
		if withType {
			return v
		}
		return ""
	}
	root = t.TempDir()
	rp := "devices/pci0000:20/0000:20:00.0"
	us1 := rp + "/0000:21:00.0"
	ds1 := us1 + "/0000:22:00.0"
	usLow := ds1 + "/0000:23:00.0"
	dsLow := usLow + "/0000:24:00.0"
	nic := dsLow + "/0000:25:00.0"
	mkPCI(t, root, rp, "0x060400", typ("4"), "", "0")
	mkPCI(t, root, us1, "0x060400", typ("5"), "", "0")
	mkPCI(t, root, ds1, "0x060400", typ("6"), "", "0")
	mkPCI(t, root, usLow, "0x060400", typ("5"), "", "0")
	mkPCI(t, root, dsLow, "0x060400", typ("6"), "", "0")
	mkPCI(t, root, nic, "0x020000", typ("0"), "", "0")
	mkLinkAttrs(t, root, rp, gen5, 16, gen5, 16)
	mkLinkAttrs(t, root, us1, gen5, 16, gen5, 16)
	// The virtual inter-switch link, as seen from both of its ends.
	mkLinkAttrs(t, root, ds1, gen1, 16, gen1, 16)
	mkLinkAttrs(t, root, usLow, gen1, 16, gen1, 16)
	mkLinkAttrs(t, root, dsLow, gen5, 16, gen5, 16)
	mkLinkAttrs(t, root, nic, gen5, 16, gen5, 16)
	mkSwitchRNIC400(t, root, "0000:25:00.0")
	return root, "sw.0000:23:00.0", "sw.0000:24:00.0"
}

// assertAttributableLinksRead checks the RNIC and root-port links of the
// cascaded tree are still read from sysfs.
func assertAttributableLinksRead(t *testing.T, g Graph) {
	t.Helper()
	gen5x16 := pci.EffectiveBPS(32, 16)
	for _, id := range []string{"pcie.rnic0", "rp.0000:20:00.0"} {
		l, ok := g.Link(id)
		if !ok || l.CapacityBPS != gen5x16 || l.NegotiatedBPS != gen5x16 || l.CapacityAssumed {
			t.Errorf("%s: %+v, want capacity and negotiated %d read from sysfs", id, l, gen5x16)
		}
	}
}

// Without a pcie_type attribute every bridge below the root port becomes its
// own switch hop, downstream ports included. The lower switch's downstream
// port trained to Gen5 x16 on the link below it, but pci.ReadLink pairs it
// with its parent — the same switch's upstream port, whose maximum is the
// virtual link's 5 GT/s — which is no link partner. Taken as a capacity that
// is 64 Gb/s against ~504 Gb/s negotiated, and it would hold every affinitive
// path of the RNIC to 64 Gb/s.
func TestDiscoverSwitchDownstreamPortNotPairedWithUpstream(t *testing.T) {
	root, _, ds2 := mkCascadedSwitchTree(t, false)
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := switchLinkIDs(g); len(got) != 4 {
		t.Fatalf("switch hops %v, want every bridge port below the root port", got)
	}
	if _, ok := g.Link(ds2); !ok {
		t.Fatalf("no downstream-port hop %s", ds2)
	}
	assertSwitchHopsAssumed(t, g)
	assertLocalMemCeiling(t, g)
	assertAttributableLinksRead(t, g)
}

// With pcie_type present each switch level is one hop, identified by its
// upstream port. The lower switch's hop is the in-package virtual link, whose
// fictitious 5 GT/s x16 (64 Gb/s) must not become the ceiling of the RNIC's
// affinitive paths.
func TestDiscoverCascadedSwitchVirtualLinkIgnored(t *testing.T) {
	root, us2, _ := mkCascadedSwitchTree(t, true)
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := switchLinkIDs(g); len(got) != 2 {
		t.Fatalf("switch hops %v, want one per switch level", got)
	}
	if _, ok := g.Link(us2); !ok {
		t.Fatalf("no lower-switch hop %s", us2)
	}
	assertSwitchHopsAssumed(t, g)
	assertLocalMemCeiling(t, g)
	assertAttributableLinksRead(t, g)
}

// A tree without link attributes keeps the Gen4 x16 fallback and records no
// negotiated value.
func TestDiscoverPCIeCapacityFallback(t *testing.T) {
	root := t.TempDir()
	rp := "devices/pci0000:3a/0000:3a:00.0"
	mkPCI(t, root, rp, "0x060400", "4", "", "0")
	mkPCI(t, root, rp+"/0000:3c:00.0", "0x020000", "0", "0x15b3", "0")
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")

	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range g.Links {
		switch l.Kind {
		case "pcie_rnic", "root_port":
			if l.CapacityBPS != pcieGen4x16 || l.NegotiatedBPS != 0 {
				t.Errorf("%s: capacity %d negotiated %d, want the %d fallback and none", l.ID, l.CapacityBPS, l.NegotiatedBPS, pcieGen4x16)
			}
			if !l.CapacityAssumed {
				t.Errorf("%s: fallback capacity not marked assumed", l.ID)
			}
		case "memory", "upi":
			if !l.CapacityAssumed {
				t.Errorf("%s: nominal %s capacity not marked assumed", l.ID, l.Kind)
			}
		}
	}
}

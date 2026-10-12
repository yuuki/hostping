package topology

import (
	"path/filepath"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// mkIBLink is an RNIC whose port 1 has link layer InfiniBand: GID 0 carries
// the subnet prefix, and no GID is RoCEv2. All values are synthetic.
func mkIBLink(t *testing.T, root, name, bdf, state, lid, gid0 string) {
	t.Helper()
	mkIBDevice(t, root, name, bdf)
	dev := filepath.Join(root, "class/infiniband", name)
	mustWrite(t, filepath.Join(dev, "ports/1/state"), state+"\n")
	mustWrite(t, filepath.Join(dev, "ports/1/rate"), "200 Gb/sec (4X HDR)\n")
	mustWrite(t, filepath.Join(dev, "ports/1/link_layer"), "InfiniBand\n")
	mustWrite(t, filepath.Join(dev, "ports/1/lid"), lid+"\n")
	mustWrite(t, filepath.Join(dev, "ports/1/gids/0"), gid0+"\n")
	mustWrite(t, filepath.Join(dev, "ports/1/gid_attrs/types/0"), "IB/RoCE v1\n")
}

func ibRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0", "0x060400", "4", "", "0")
	for _, bdf := range []string{"0000:3c:00.0", "0000:3d:00.0", "0000:3e:00.0", "0000:3f:00.0", "0000:40:00.0"} {
		mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/"+bdf, "0x020700", "0", "0x15b3", "0")
	}
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	return root
}

func TestDiscoverInfiniBandRailKey(t *testing.T) {
	root := ibRoot(t)
	mkIBLink(t, root, "mlx5_0", "0000:3c:00.0", "4: ACTIVE", "0x11", "fe80:0000:0000:0001:0002:c903:0000:0001")
	mkIBLink(t, root, "mlx5_1", "0000:3d:00.0", "4: ACTIVE", "0x12", "fe80:0000:0000:0001:0002:c903:0000:0002")
	mkIBLink(t, root, "mlx5_2", "0000:3e:00.0", "4: ACTIVE", "0x13", "fe80:0000:0000:0002:0002:c903:0000:0003")
	mkIBLink(t, root, "mlx5_3", "0000:3f:00.0", "4: ACTIVE", "0x0", "fe80:0000:0000:0001:0002:c903:0000:0004")
	mkIBLink(t, root, "mlx5_4", "0000:40:00.0", "1: DOWN", "0x15", "fe80:0000:0000:0001:0002:c903:0000:0005")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ib:fe80000000000001", "ib:fe80000000000001", "ib:fe80000000000002", "unknown", "unknown"}
	for i, n := range g.RNICs {
		if n.Fabric != want[i] {
			t.Errorf("%s fabric = %q, want %q", n.Device, n.Fabric, want[i])
		}
		if n.LinkLayer != model.LinkLayerInfiniBand {
			t.Errorf("%s link layer = %q", n.Device, n.LinkLayer)
		}
	}
	// InfiniBand RNICs stay out of the mutual probe by default, in every
	// scope: separate fabrics can share the default subnet prefix.
	for _, scope := range []model.MutualScope{model.MutualScopeRail, model.MutualScopeHost} {
		if pairs := g.MutualPairs(scope); len(pairs) != 0 {
			t.Fatalf("scope %s pairs InfiniBand RNICs by default: %+v", scope, pairs)
		}
		if pairs := g.MutualPairsFor(scope, false); len(pairs) != 0 {
			t.Fatalf("scope %s pairs InfiniBand RNICs without the opt-in: %+v", scope, pairs)
		}
	}
	if got := g.InfiniBandRNICs(); len(got) != 5 {
		t.Fatalf("InfiniBand RNICs = %v, want all 5", got)
	}
	// With --mutual-infiniband: same subnet prefix pairs under rail scope; a
	// port without a LID or not ACTIVE pairs with nothing.
	pairs := g.MutualPairsFor(model.MutualScopeRail, true)
	if len(pairs) != 1 || pairs[0][0].Device != "mlx5_0" || pairs[0][1].Device != "mlx5_1" {
		t.Fatalf("rail pairs = %+v", pairs)
	}
	if got := len(g.MutualPairsFor(model.MutualScopeHost, true)); got != 3 {
		t.Fatalf("host scope pairs %d, want 3 (the three addressable IB RNICs)", got)
	}
}

// RoCE RNICs on a host that also has InfiniBand ones keep pairing without
// the opt-in.
func TestMutualPairsKeepRoCEWhenInfiniBandExcluded(t *testing.T) {
	g := Graph{RNICs: []model.RNIC{
		{ID: "rnic0", Device: "mlx5_0", Fabric: "v4:192.168.1.0", LinkLayer: model.LinkLayerEthernet},
		{ID: "rnic1", Device: "mlx5_1", Fabric: "v4:192.168.1.0", LinkLayer: model.LinkLayerEthernet},
		{ID: "rnic2", Device: "mlx5_2", Fabric: "ib:fe80000000000000", LinkLayer: model.LinkLayerInfiniBand},
		{ID: "rnic3", Device: "mlx5_3", Fabric: "ib:fe80000000000000", LinkLayer: model.LinkLayerInfiniBand},
	}}
	pairs := g.MutualPairs(model.MutualScopeHost)
	if len(pairs) != 1 || pairs[0][0].ID != "rnic0" || pairs[0][1].ID != "rnic1" {
		t.Fatalf("pairs = %+v, want only the RoCE pair", pairs)
	}
	if got := len(g.MutualPairsFor(model.MutualScopeRail, true)); got != 2 {
		t.Fatalf("opt-in rail pairs = %d, want 2", got)
	}
}

func TestDiscoverRoCELinkLayerUnchanged(t *testing.T) {
	root := ibRoot(t)
	mkIB(t, root, "mlx5_0", "0000:3c:00.0", "0000:0000:0000:0000:0000:ffff:c0a8:0101")
	mustWrite(t, filepath.Join(root, "class/infiniband/mlx5_0/ports/1/link_layer"), "Ethernet\n")
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	if n := g.RNICs[0]; n.Fabric != "v4:192.168.1.0" || n.LinkLayer != model.LinkLayerEthernet {
		t.Fatalf("RoCE RNIC = fabric %q link layer %q", n.Fabric, n.LinkLayer)
	}
}

func TestMutualPairNeverCrossesLinkLayers(t *testing.T) {
	roce := model.RNIC{ID: "a", Device: "mlx5_0", Fabric: "v4:192.168.1.0"}
	ib := model.RNIC{ID: "b", Device: "mlx5_1", Fabric: "ib:fe80000000000001"}
	for _, scope := range []model.MutualScope{model.MutualScopeRail, model.MutualScopeHost} {
		if MutualPair(roce, ib, scope) {
			t.Errorf("scope %s pairs RoCE with InfiniBand", scope)
		}
	}
	ib2 := model.RNIC{ID: "c", Device: "mlx5_2", Fabric: "ib:fe80000000000002"}
	if MutualPair(ib, ib2, model.MutualScopeRail) {
		t.Error("distinct subnet prefixes pair under rail scope")
	}
	if !MutualPair(ib, ib2, model.MutualScopeHost) {
		t.Error("host scope pairs InfiniBand RNICs across subnet prefixes")
	}
}

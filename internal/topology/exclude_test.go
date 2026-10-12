package topology

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// mgmtRoot is a host with two data-path InfiniBand RNICs (mlx5_0, mlx5_1)
// whose subnet manager is elsewhere and which have no IPoIB netdev, and a
// two-function management HCA (mlx5_2, mlx5_3) on a narrow link whose ports
// carry the subnet manager's LID themselves and whose IPoIB netdevs are
// administratively down. All values are synthetic.
func mgmtRoot(t *testing.T) string {
	t.Helper()
	root := ibRoot(t)
	mkPCI(t, root, "devices/pci0000:3a/0000:3a:00.0/0000:41:00.1", "0x020700", "0", "0x15b3", "0")
	data := "fe80:0000:0000:0001:0002:c903:0000:00"
	mkIBLink(t, root, "mlx5_0", "0000:3c:00.0", "4: ACTIVE", "0x11", data+"01")
	mkIBLink(t, root, "mlx5_1", "0000:3d:00.0", "4: ACTIVE", "0x12", data+"02")
	mgmt := "fe80:0000:0000:0000:0002:c903:0000:00"
	mkIBLink(t, root, "mlx5_2", "0000:40:00.0", "4: ACTIVE", "0x1", mgmt+"03")
	mkIBLink(t, root, "mlx5_3", "0000:41:00.1", "4: ACTIVE", "0x2", mgmt+"04")
	for dev, sm := range map[string]string{"mlx5_0": "0x40", "mlx5_1": "0x40", "mlx5_2": "0x1", "mlx5_3": "0x1"} {
		mustWrite(t, filepath.Join(root, "class/infiniband", dev, "ports/1/sm_lid"), sm+"\n")
	}
	for dev, nd := range map[string]string{"mlx5_2": "ibs0", "mlx5_3": "ibs1"} {
		mustWrite(t, filepath.Join(root, "class/infiniband", dev, "device/net", nd, "flags"), "0x1002\n")
		mustWrite(t, filepath.Join(root, "class/net", nd, "flags"), "0x1002\n")
	}
	return root
}

func rnicDevices(g Graph) []string {
	var out []string
	for _, n := range g.RNICs {
		out = append(out, n.Device)
	}
	return out
}

func TestDiscoverExcludesRNICByNameOrBDF(t *testing.T) {
	quietLog(t)
	root := mgmtRoot(t)
	g, err := DiscoverWith(root, DiscoverOptions{Exclude: []string{"mlx5_2", "0000:41:00.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rnicDevices(g), ","); got != "mlx5_0,mlx5_1" {
		t.Fatalf("rnics = %s, want mlx5_0,mlx5_1", got)
	}
	want := []model.ExcludedRNIC{
		{Device: "mlx5_2", PCIeBDF: "0000:40:00.0", Reason: model.ExcludeReasonOperator},
		{Device: "mlx5_3", PCIeBDF: "0000:41:00.1", Reason: model.ExcludeReasonOperator},
	}
	if len(g.Excluded) != len(want) {
		t.Fatalf("excluded = %+v", g.Excluded)
	}
	for i := range want {
		if g.Excluded[i] != want[i] {
			t.Fatalf("excluded[%d] = %+v, want %+v", i, g.Excluded[i], want[i])
		}
	}
	// No trace of the excluded devices in the graph: no link, no path, no
	// pair, and the remaining RNICs are numbered without a gap.
	for _, l := range g.Links {
		if strings.Contains(l.ID, "0000:40:00.0") || strings.Contains(l.ID, "0000:41:00.1") {
			t.Fatalf("link of an excluded RNIC: %+v", l)
		}
	}
	for i, n := range g.RNICs {
		if n.ID != "rnic"+string(rune('0'+i)) {
			t.Fatalf("rnic ids %+v", g.RNICs)
		}
	}
	for _, p := range g.Paths {
		if p.RNIC != "rnic0" && p.RNIC != "rnic1" {
			t.Fatalf("path from unknown RNIC %+v", p)
		}
	}
	for _, pr := range g.MutualPairsFor(model.MutualScopeHost, true) {
		if pr[0].Device == "mlx5_2" || pr[0].Device == "mlx5_3" || pr[1].Device == "mlx5_2" || pr[1].Device == "mlx5_3" {
			t.Fatalf("pair with an excluded RNIC: %+v", pr)
		}
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverExcludeUnknownDeviceFails(t *testing.T) {
	quietLog(t)
	root := mgmtRoot(t)
	_, err := DiscoverWith(root, DiscoverOptions{Exclude: []string{"mlx5_2", "mlx5_9"}})
	if err == nil || !strings.Contains(err.Error(), `"mlx5_9"`) {
		t.Fatalf("err = %v, want one naming mlx5_9", err)
	}
}

func TestDiscoverExcludeEverythingFails(t *testing.T) {
	quietLog(t)
	root := mgmtRoot(t)
	_, err := DiscoverWith(root, DiscoverOptions{Exclude: []string{"mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3"}})
	if err == nil || !strings.Contains(err.Error(), "excludes every") {
		t.Fatalf("err = %v", err)
	}
}

// The management HCA warning names each such device once and suggests
// --exclude-rnic; data-path InfiniBand RNICs and excluded devices draw none.
func TestDiscoverWarnsOnManagementHCA(t *testing.T) {
	logs := quietLog(t)
	root := mgmtRoot(t)
	if _, err := DiscoverFrom(root); err != nil {
		t.Fatal(err)
	}
	warned := map[string]int{}
	for _, l := range *logs {
		if !strings.Contains(l, "looks like a management HCA") {
			continue
		}
		for _, dev := range []string{"mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3"} {
			if strings.Contains(l, dev+" (") {
				warned[dev]++
				if !strings.Contains(l, "--exclude-rnic "+dev) {
					t.Errorf("warning does not suggest the flag: %s", l)
				}
				if !strings.Contains(l, "subnet manager") || !strings.Contains(l, "administratively down") {
					t.Errorf("warning does not give both reasons: %s", l)
				}
			}
		}
	}
	if warned["mlx5_2"] != 1 || warned["mlx5_3"] != 1 || warned["mlx5_0"] != 0 || warned["mlx5_1"] != 0 {
		t.Fatalf("warnings per device = %v, logs %q", warned, *logs)
	}

	*logs = nil
	if _, err := DiscoverWith(root, DiscoverOptions{Exclude: []string{"mlx5_2", "mlx5_3"}}); err != nil {
		t.Fatal(err)
	}
	for _, l := range *logs {
		if strings.Contains(l, "looks like a management HCA") {
			t.Fatalf("excluded device warned: %s", l)
		}
	}
}

// Each hint alone is enough to warn: either is unusual on a data-path port.
func TestManagementHCAHints(t *testing.T) {
	root := mgmtRoot(t)
	local := localIBLIDs(root, []string{"mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3"})
	if got := managementHCAHints(root, "mlx5_0", "", local); len(got) != 0 {
		t.Fatalf("data-path RNIC hints %q", got)
	}
	if got := managementHCAHints(root, "mlx5_2", "", local); len(got) != 1 || !strings.Contains(got[0], "subnet manager") {
		t.Fatalf("local SM hints %q", got)
	}
	mustWrite(t, filepath.Join(root, "class/net/ib9/flags"), "0x1002\n")
	if got := managementHCAHints(root, "mlx5_0", "ib9", local); len(got) != 1 || !strings.Contains(got[0], "administratively down") {
		t.Fatalf("down netdev hints %q", got)
	}
	mustWrite(t, filepath.Join(root, "class/net/ib9/flags"), "0x1003\n")
	if got := managementHCAHints(root, "mlx5_0", "ib9", local); len(got) != 0 {
		t.Fatalf("up netdev hints %q", got)
	}
}

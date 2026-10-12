package topology

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// TestMain keeps discovery tests off the host's interfaces: unless a test
// installs its own lookup, every RoCEv2 IPv4 address is a /24.
func TestMain(m *testing.M) {
	netdevPrefixLen = func(string, net.IP) (int, error) { return 24, nil }
	os.Exit(m.Run())
}

// fakePrefixLens answers netdevPrefixLen from a netdev → prefix length map;
// a missing netdev fails the lookup.
func fakePrefixLens(t *testing.T, lens map[string]int) {
	t.Helper()
	old := netdevPrefixLen
	netdevPrefixLen = func(netdev string, _ net.IP) (int, error) {
		n, ok := lens[netdev]
		if !ok {
			return 0, fmt.Errorf("no interface %s", netdev)
		}
		return n, nil
	}
	t.Cleanup(func() { netdevPrefixLen = old })
}

// mkRailTree builds one RNIC per GID, each under its own root port, with the
// RoCEv2 GID bound to netdev ethN.
func mkRailTree(t *testing.T, gids ...string) string {
	t.Helper()
	root := t.TempDir()
	for i, gid := range gids {
		rp := fmt.Sprintf("devices/pci0000:%02x/0000:%02x:00.0", 0x30+0x10*i, 0x30+0x10*i)
		bdf := fmt.Sprintf("0000:%02x:00.0", 0x31+0x10*i)
		mkPCI(t, root, rp, "0x060400", "4", "", "0")
		mkPCI(t, root, rp+"/"+bdf, "0x020000", "0", "0x15b3", "0")
		name := fmt.Sprintf("mlx5_%d", i)
		mkIB(t, root, name, bdf, gid)
		mustWrite(t, filepath.Join(root, "class/infiniband", name, "ports/1/gid_attrs/ndevs/1"), fmt.Sprintf("eth%d\n", i))
	}
	mustWrite(t, filepath.Join(root, "devices/system/node/node0"), "")
	return root
}

const (
	gid10_0_0_10  = "0000:0000:0000:0000:0000:ffff:0a00:000a" // 10.0.0.10
	gid10_0_0_20  = "0000:0000:0000:0000:0000:ffff:0a00:0014" // 10.0.0.20
	gid10_0_0_70  = "0000:0000:0000:0000:0000:ffff:0a00:0046" // 10.0.0.70
	gid10_0_1_10  = "0000:0000:0000:0000:0000:ffff:0a00:010a" // 10.0.1.10
	gid192_0_2_10 = "0000:0000:0000:0000:0000:ffff:c000:020a" // 192.0.2.10
)

// prefixLogs keeps the rail-prefix fallback lines; the minimal fixture logs
// an unrelated CPU-package fallback too.
func prefixLogs(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, "IPv4 prefix") {
			out = append(out, l)
		}
	}
	return out
}

func railKeys(t *testing.T, root string) (Graph, []string) {
	t.Helper()
	g, err := DiscoverFrom(root)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, len(g.RNICs))
	for i, n := range g.RNICs {
		keys[i] = n.Fabric
	}
	return g, keys
}

func TestRailKeySubnetNarrowerThan24(t *testing.T) {
	logs := quietLog(t)
	fakePrefixLens(t, map[string]int{"eth0": 26, "eth1": 26, "eth2": 26})
	// 10.0.0.10 and 10.0.0.20 share 10.0.0.0/26; 10.0.0.70 is in 10.0.0.64/26.
	g, keys := railKeys(t, mkRailTree(t, gid10_0_0_10, gid10_0_0_70, gid10_0_0_20))
	want := []string{"v4:10.0.0.0/26", "v4:10.0.0.64/26", "v4:10.0.0.0/26"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	pairs := g.MutualPairs(model.MutualScopeRail)
	if len(pairs) != 1 || pairs[0][0].Device != "mlx5_0" || pairs[0][1].Device != "mlx5_2" {
		t.Fatalf("rail pairs %v, want only mlx5_0-mlx5_2 (same /26)", pairs)
	}
	if got := len(g.MutualPairs(model.MutualScopeHost)); got != 3 {
		t.Fatalf("host scope pairs %d, want 3 (cross-subnet pairs stay allowed)", got)
	}
	if got := prefixLogs(*logs); len(got) != 0 {
		t.Fatalf("readable prefixes must not log: %v", got)
	}
}

func TestRailKeySlash24Unchanged(t *testing.T) {
	logs := quietLog(t)
	fakePrefixLens(t, map[string]int{"eth0": 24, "eth1": 24, "eth2": 24})
	g, keys := railKeys(t, mkRailTree(t, gid10_0_0_10, gid10_0_1_10, gid10_0_0_70))
	want := []string{"v4:10.0.0.0", "v4:10.0.1.0", "v4:10.0.0.0"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v, want the historical /24 form %v", keys, want)
	}
	if got := len(g.MutualPairs(model.MutualScopeRail)); got != 1 {
		t.Fatalf("rail pairs %d, want 1", got)
	}
	if got := prefixLogs(*logs); len(got) != 0 {
		t.Fatalf("readable prefixes must not log: %v", got)
	}
}

func TestRailKeySubnetWiderThan24(t *testing.T) {
	fakePrefixLens(t, map[string]int{"eth0": 16, "eth1": 16})
	g, keys := railKeys(t, mkRailTree(t, gid10_0_0_10, gid10_0_1_10))
	want := []string{"v4:10.0.0.0/16", "v4:10.0.0.0/16"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	if got := len(g.MutualPairs(model.MutualScopeRail)); got != 1 {
		t.Fatalf("rail pairs %d, want 1 (one /16 subnet)", got)
	}
}

func TestRailKeyFallsBackTo24(t *testing.T) {
	logs := quietLog(t)
	fakePrefixLens(t, map[string]int{"eth1": 26})
	_, keys := railKeys(t, mkRailTree(t, gid192_0_2_10, gid10_0_0_70))
	want := []string{"v4:192.0.2.0", "v4:10.0.0.64/26"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	got := prefixLogs(*logs)
	if len(got) != 1 || !strings.Contains(got[0], "mlx5_0") || !strings.Contains(got[0], "/24") {
		t.Fatalf("want one fallback line for mlx5_0, got %v", got)
	}
}

// Without gid_attrs/ndevs the netdev is the device's first net interface.
func TestRailKeyNetdevFromDevice(t *testing.T) {
	var asked []string
	old := netdevPrefixLen
	netdevPrefixLen = func(netdev string, _ net.IP) (int, error) {
		asked = append(asked, netdev)
		return 26, nil
	}
	t.Cleanup(func() { netdevPrefixLen = old })
	root := mkRailTree(t, gid10_0_0_70)
	if err := os.Remove(filepath.Join(root, "class/infiniband/mlx5_0/ports/1/gid_attrs/ndevs/1")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "bus/pci/devices/0000:31:00.0/net/eth9/ifindex"), "9\n")
	_, keys := railKeys(t, root)
	if !reflect.DeepEqual(keys, []string{"v4:10.0.0.64/26"}) || !reflect.DeepEqual(asked, []string{"eth9"}) {
		t.Fatalf("keys %v asked %v", keys, asked)
	}
}

// interfacePrefixLen reads the mask through the standard library; the
// loopback interface carries 127.0.0.1/8 on every platform the tests run on.
func TestInterfacePrefixLenLoopback(t *testing.T) {
	ifs, err := net.Interfaces()
	if err != nil {
		t.Skip(err)
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagLoopback == 0 {
			continue
		}
		ones, err := interfacePrefixLen(ifi.Name, net.IPv4(127, 0, 0, 1).To4())
		if err != nil {
			t.Skipf("%s: %v", ifi.Name, err)
		}
		if ones != 8 {
			t.Fatalf("%s: prefix /%d, want /8", ifi.Name, ones)
		}
		if _, err := interfacePrefixLen(ifi.Name, net.IPv4(192, 0, 2, 1).To4()); err == nil {
			t.Fatal("an address the interface does not carry must not resolve")
		}
		return
	}
	t.Skip("no loopback interface")
}

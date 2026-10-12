package topology

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// ibFabricPrefix marks an InfiniBand rail key. RoCE keys ("v4:", "gid:")
// and InfiniBand keys never pair: the two link layers cannot reach each other.
const ibFabricPrefix = "ib:"

// portLinkLayer reads ports/N/link_layer and spells it as the model does.
// A missing or unrecognised value is "" (unknown), which every caller treats
// as RoCE, as before link layers were read.
func portLinkLayer(portDir string) string {
	v := strings.TrimSpace(readFile(filepath.Join(portDir, "link_layer")))
	switch {
	case strings.EqualFold(v, model.LinkLayerInfiniBand):
		return model.LinkLayerInfiniBand
	case strings.EqualFold(v, model.LinkLayerEthernet):
		return model.LinkLayerEthernet
	}
	return ""
}

// rnicLinkLayer is the link layer of the port the probes use: the first
// ACTIVE one, as fabricKey picks it.
func rnicLinkLayer(sysroot, ibdev string) string {
	port := firstActiveIBPort(sysroot, ibdev)
	if port == "" {
		return ""
	}
	return portLinkLayer(filepath.Join(sysroot, "class/infiniband", ibdev, "ports", port))
}

// ibFabricKey is the rail key of an InfiniBand port: "ib:" and the subnet
// prefix, the top 64 bits of GID index 0, in hex. A port the loopback probe
// cannot address (not ACTIVE, LID 0, no GID 0) is "unknown", so it never
// pairs.
func ibFabricKey(portDir string) string {
	if !portActive(portDir) {
		return "unknown"
	}
	if portLID(portDir) == 0 {
		return "unknown"
	}
	hex := gidHex(readFile(filepath.Join(portDir, "gids", "0")))
	if len(hex) != 32 || gidAllZero(hex) {
		return "unknown"
	}
	return ibFabricPrefix + hex[:16]
}

// portLID reads ports/N/lid ("0x7"); 0 when missing or unparsable.
func portLID(portDir string) uint64 {
	return parseLID(readFile(filepath.Join(portDir, "lid")))
}

// parseLID parses a sysfs LID ("0x7"); 0 when unparsable.
func parseLID(raw string) uint64 {
	v := strings.TrimSpace(raw)
	n, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(v), "0x"), 16, 16)
	if err != nil {
		return 0
	}
	return n
}

// IsInfiniBand reports whether an RNIC's probed port has link layer
// InfiniBand, read from sysfs or implied by an "ib:" rail key.
func IsInfiniBand(n model.RNIC) bool {
	return n.LinkLayer == model.LinkLayerInfiniBand || isIBFabric(n.Fabric)
}

// InfiniBandRNICs returns the IDs of the graph's InfiniBand RNICs, in graph
// order.
func (g Graph) InfiniBandRNICs() []string {
	var out []string
	for _, n := range g.RNICs {
		if IsInfiniBand(n) {
			out = append(out, n.ID)
		}
	}
	return out
}

// isIBFabric reports whether a rail key is an InfiniBand subnet.
func isIBFabric(fabric string) bool {
	return strings.HasPrefix(fabric, ibFabricPrefix)
}

// localIBLIDs returns the LIDs of every port of the host's RDMA devices,
// LID 0 (unassigned, or an Ethernet port) left out.
func localIBLIDs(sysroot string, devs []string) map[uint64]bool {
	out := map[uint64]bool{}
	for _, dev := range devs {
		ports, err := os.ReadDir(filepath.Join(sysroot, "class/infiniband", dev, "ports"))
		if err != nil {
			continue
		}
		for _, p := range ports {
			if lid := portLID(filepath.Join(sysroot, "class/infiniband", dev, "ports", p.Name())); lid != 0 {
				out[lid] = true
			}
		}
	}
	return out
}

// managementHCAHints says why an InfiniBand RNIC looks like the HCA a host
// uses to manage a fabric of its own (an NVLink fabric's subnet, say) rather
// than a data-path NIC: the subnet manager of its port runs on this host, or
// its IPoIB netdev is administratively down while the IB port is ACTIVE.
// Either alone is legitimate on some data-path hosts (a host running the
// cluster's SM, an RDMA-only port), so the hints only warn; nothing is
// excluded without --exclude-rnic.
func managementHCAHints(sysroot, ibdev, netdev string, localLIDs map[uint64]bool) []string {
	port := firstActiveIBPort(sysroot, ibdev)
	if port == "" {
		return nil
	}
	base := filepath.Join(sysroot, "class/infiniband", ibdev, "ports", port)
	if !portActive(base) {
		return nil
	}
	var why []string
	if sm := portSMLID(base); sm != 0 && localLIDs[sm] {
		why = append(why, fmt.Sprintf("the subnet manager of port %s (LID %d) runs on this host", port, sm))
	}
	if netdev != "" && netdevAdminDown(sysroot, netdev) {
		why = append(why, fmt.Sprintf("its netdev %s is administratively down while the IB port is ACTIVE", netdev))
	}
	return why
}

func portActive(portDir string) bool {
	st := strings.ToUpper(readFile(filepath.Join(portDir, "state")))
	return strings.Contains(st, "ACTIVE") || strings.HasPrefix(strings.TrimSpace(st), "4:")
}

// portSMLID reads ports/N/sm_lid; 0 when missing or unparsable.
func portSMLID(portDir string) uint64 {
	return parseLID(readFile(filepath.Join(portDir, "sm_lid")))
}

// netdevAdminDown reports a netdev without IFF_UP in its flags. Unreadable
// flags are not down: nothing is claimed about a netdev hostping cannot read.
func netdevAdminDown(sysroot, netdev string) bool {
	v := strings.TrimSpace(readFile(filepath.Join(sysroot, "class/net", netdev, "flags")))
	if v == "" {
		return false
	}
	flags, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(v), "0x"), 16, 32)
	if err != nil {
		return false
	}
	return flags&0x1 == 0 // IFF_UP
}

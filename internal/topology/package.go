package topology

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// logf is where discovery reports a fallback; tests replace it.
var logf = log.Printf

// ParseCPUList parses a kernel CPU list such as "0,56" or "0-3,8".
func ParseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, r := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(r, "-")
		l, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("cpu list %q", s)
		}
		h := l
		if isRange {
			if h, err = strconv.Atoi(hi); err != nil || h < l {
				return nil, fmt.Errorf("cpu list %q", s)
			}
		}
		for c := l; c <= h; c++ {
			out = append(out, c)
		}
	}
	return out, nil
}

// numaPackages maps every NUMA node that has CPUs to the physical package of
// those CPUs (/sys/devices/system/node/nodeN/cpulist, then
// /sys/devices/system/cpu/cpuK/topology/physical_package_id). Memory-only
// nodes (an empty cpulist: CXL, HBM, GPU memory) have no package and are left
// out. It fails, saying why, when there is no NUMA node at all or the package
// of a CPU node cannot be read or is not unique.
func numaPackages(sysroot string) (map[int]int, error) {
	dir := filepath.Join(sysroot, "devices/system/node")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("NUMA nodes: %w", err)
	}
	out := map[int]int{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "node") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(name, "node"))
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name, "cpulist"))
		if err != nil {
			return nil, fmt.Errorf("%s cpulist: %w", name, err)
		}
		cpus, err := ParseCPUList(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if len(cpus) == 0 {
			continue // memory-only node
		}
		pkg := -1
		for _, c := range cpus {
			p := readInt(filepath.Join(sysroot, "devices/system/cpu", fmt.Sprintf("cpu%d", c), "topology/physical_package_id"), -1)
			if p < 0 {
				return nil, fmt.Errorf("%s: cpu%d has no physical_package_id", name, c)
			}
			if pkg >= 0 && p != pkg {
				return nil, fmt.Errorf("%s: CPUs span packages %d and %d", name, pkg, p)
			}
			pkg = p
		}
		out[n] = pkg
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no NUMA node with CPUs under %s", dir)
	}
	return out, nil
}

// sockets turns a NUMA node into the socket hostping models it on.
//
// With sub-NUMA clustering (some package holds more than one CPU NUMA node)
// the socket is the node's physical package. Otherwise — one CPU NUMA node per
// package, package information unreadable, or a node that has no package
// (memory-only) — the socket is the NUMA node number, as hostping always
// modeled it, so a host without SNC keeps the socket fields, the upi.A-B link
// IDs and the state fingerprint it had whatever its package numbering.
type sockets struct {
	pkg map[int]int // NUMA node → package; nil unless SNC is on
}

func discoverSockets(sysroot string) sockets {
	pkgs, err := numaPackages(sysroot)
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(sysroot, "devices/system/node")); statErr == nil {
			logf("hostping: topology: CPU package of NUMA nodes unreadable, modeling each NUMA node as a socket: %v", err)
		}
		return sockets{}
	}
	if !snc(pkgs) {
		return sockets{}
	}
	return sockets{pkg: pkgs}
}

// snc reports whether some package holds more than one NUMA node.
func snc(pkgs map[int]int) bool {
	seen := map[int]bool{}
	for _, p := range pkgs {
		if seen[p] {
			return true
		}
		seen[p] = true
	}
	return false
}

func (s sockets) of(numa int) int {
	if p, ok := s.pkg[numa]; ok {
		return p
	}
	return numa
}

// meshID is the snc_mesh link between NUMA nodes a and b of one package.
func meshID(a, b int) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("mesh.%d-%d", a, b)
}

// meshLinks joins every two NUMA nodes that share a socket: the intra-package
// fabric under sub-NUMA clustering. A host without SNC has one NUMA node per
// socket and gets none. Nothing in sysfs states the fabric's bandwidth and no
// live source reads its utilization, so the capacity is unknown (0) and
// assumed.
func meshLinks(g Graph) []model.Link {
	bySocket := map[int]map[int]bool{}
	note := func(sock, numa int) {
		if bySocket[sock] == nil {
			bySocket[sock] = map[int]bool{}
		}
		bySocket[sock][numa] = true
	}
	for _, n := range g.RNICs {
		note(n.Socket, n.NUMA)
	}
	for _, e := range g.Endpoints {
		note(e.Socket, e.NUMA)
	}
	socks := make([]int, 0, len(bySocket))
	for s := range bySocket {
		socks = append(socks, s)
	}
	sort.Ints(socks)
	var out []model.Link
	for _, s := range socks {
		nodes := make([]int, 0, len(bySocket[s]))
		for n := range bySocket[s] {
			nodes = append(nodes, n)
		}
		sort.Ints(nodes)
		for i := 0; i < len(nodes); i++ {
			for j := i + 1; j < len(nodes); j++ {
				out = append(out, model.Link{ID: meshID(nodes[i], nodes[j]), Kind: model.LinkKindSNCMesh, Socket: s, CapacityAssumed: true})
			}
		}
	}
	return out
}

// Package sysfs holds the primitives that pci, topology and monitor read
// /sys with: one file reader, the integer readers in their two signednesses,
// the walk of /sys/devices/system/node and the CPU package lookup. Each of
// those packages used to carry its own copy, and the copies had drifted:
// topology's integer reader dropped negative values while pci's and monitor's
// passed them through. Both are kept here under different names so that every
// caller keeps the semantics it had (see ReadInt and ReadIntNonNegative).
//
// Every function takes a sysroot or a full path, never assuming /sys, so tests
// can inject a tree.
package sysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ReadString returns the contents of path as they are, trailing newline
// included, or "" when the file cannot be read. Callers that want a value trim
// it; callers that want to know whether the file exists use ReadUint64OK or
// os.ReadFile.
func ReadString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// ReadInt parses the trimmed contents of path as a signed decimal integer and
// returns def when the file is missing, empty or not an integer. A negative
// value is returned as read: a numa_node of -1 (no NUMA affinity) stays -1,
// which is what pci.ReadLink reports and what the uncore package-id checks
// rely on.
func ReadInt(path string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(ReadString(path)))
	if err != nil {
		return def
	}
	return n
}

// ReadIntNonNegative is ReadInt for a value that is only meaningful when it
// is zero or more: a negative value is returned as def, exactly like a
// missing or unparsable file. Topology discovery reads numa_node this way
// with def 0, so a device without NUMA affinity is modeled on node 0 rather
// than on a node -1 that no memory endpoint has; that is the NUMA field the
// RNICs and GPUs of a discovered graph carry.
func ReadIntNonNegative(path string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(ReadString(path)))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// ReadUint64OK parses the trimmed contents of path as an unsigned decimal
// integer. ok is false when the file is missing or does not hold one (a
// negative number included), so a counter that could not be read is told
// apart from one that reads 0.
func ReadUint64OK(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ParseCPUList parses a kernel CPU list such as "0,56" or "0-3,8". An empty
// (or blank) list is nil with no error.
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

// Node is one NUMA node directory, /sys/devices/system/node/nodeN, with its
// cpulist read and parsed. Exactly one of the following holds:
//
//   - ReadErr is set: cpulist could not be read (missing, most often). CPUs is
//     nil and ParseErr is nil.
//   - ParseErr is set: cpulist was read but is not a CPU list. CPUs is nil.
//   - Neither: CPUs holds the list, nil when it is empty (a memory-only node:
//     CXL, HBM, GPU memory).
//
// Callers decide what each case means to them: the monitor's CPU load treats
// an unreadable cpulist as a node without CPUs, topology's package lookup
// treats it as an error, and discovery treats an empty one as memory-only.
type Node struct {
	ID       int    // N of nodeN
	Name     string // "nodeN"
	CPUs     []int  // parsed cpulist; nil when empty or not read
	ReadErr  error  // the os.ReadFile error of cpulist
	ParseErr error  // the ParseCPUList error of cpulist
}

// Nodes lists the nodeN directories under sysroot/devices/system/node, in
// directory order (lexical: node0, node1, node10, ...), with each cpulist
// read. Entries that are not "node" followed by an integer are left out. The
// error is that of reading the directory, unwrapped, so a sysroot without
// NUMA nodes answers os.IsNotExist.
func Nodes(sysroot string) ([]Node, error) {
	dir := filepath.Join(sysroot, "devices/system/node")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []Node{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "node") {
			continue
		}
		id, err := strconv.Atoi(strings.TrimPrefix(name, "node"))
		if err != nil {
			continue
		}
		n := Node{ID: id, Name: name}
		raw, err := os.ReadFile(filepath.Join(dir, name, "cpulist"))
		if err != nil {
			n.ReadErr = err
		} else {
			n.CPUs, n.ParseErr = ParseCPUList(string(raw))
		}
		out = append(out, n)
	}
	return out, nil
}

// PackageID is the physical package (socket) of cpu, from
// sysroot/devices/system/cpu/cpuK/topology/physical_package_id, or -1 when the
// file is missing or unparsable. A negative value in the file is returned as
// read; every caller treats anything below 0 as "no package".
func PackageID(sysroot string, cpu int) int {
	return ReadInt(filepath.Join(sysroot, "devices/system/cpu", fmt.Sprintf("cpu%d", cpu), "topology/physical_package_id"), -1)
}

// PortActive reports whether the InfiniBand port directory portDir
// (class/infiniband/<dev>/ports/<N>) has an ACTIVE state file: it contains
// "ACTIVE" in any case, or starts with the numeric state 4. A missing or
// unreadable state is not active.
//
// The match is by substring, so "5: ACTIVE_DEFER" also counts as active, as
// it always has for the monitor and discovery.
func PortActive(portDir string) bool {
	st := strings.ToUpper(ReadString(filepath.Join(portDir, "state")))
	return strings.Contains(st, "ACTIVE") || strings.HasPrefix(strings.TrimSpace(st), "4:")
}

// FirstActiveIBPort is the port of the InfiniBand device dev
// (sysroot/class/infiniband/<dev>/ports) that discovery and the counters
// use: the first ACTIVE port by port number, else the first listed port.
// Port names are sorted numerically ("2" before "10"), with any non-numeric
// name after them in lexical order. ok is false when the ports directory
// cannot be listed or is empty; port is "" then.
//
// Both topology discovery and the monitor pick their port with this, so the
// rate, link layer and rail key of an RNIC and the counters read for it
// always come from the same port, whatever its number.
func FirstActiveIBPort(sysroot, dev string) (port string, ok bool) {
	dir := filepath.Join(sysroot, "class/infiniband", dev, "ports")
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		return "", false
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Slice(names, func(i, j int) bool {
		a, ea := strconv.Atoi(names[i])
		b, eb := strconv.Atoi(names[j])
		switch {
		case ea == nil && eb == nil:
			return a < b
		case ea == nil || eb == nil:
			return ea == nil // numeric names first
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		if PortActive(filepath.Join(dir, name)) {
			return name, true
		}
	}
	return names[0], true
}

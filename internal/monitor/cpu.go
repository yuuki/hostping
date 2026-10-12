package monitor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// The idle/busy decision reads only the RNICs (throughput, PFC, drops) and the
// GPUs. A host whose CPUs are loaded still counts as idle, yet CPU load on a
// package contends for the package's on-die fabric, which every loopback path
// through the package crosses: with every core of one socket streaming memory
// reads, the paths of that socket's RNICs dropped to a quarter of their
// baselines, to every NUMA node's memory, with no RNIC traffic and no memory
// channel near its capacity. The paired bandwidth re-read confirms such a drop,
// because the load is constant and its references sit on unloaded sockets, so
// the run reports a root-port failure on a healthy host.
//
// /proc/stat needs no privilege and exists on every Linux platform, unlike the
// uncore PMUs. The monitor reads it on every poll, so a NUMA node's busy
// fraction covers the same interval as the RNIC counter rates, and a path
// through a package with a node at or above --cpu-busy-high is judged
// cpu_busy instead of abnormal (analyzer.applyCPUBusy).

// CPUTicks is the cumulative CPU time of one NUMA node's CPUs, in USER_HZ
// ticks: Total over every state, Busy over every state but idle and iowait.
type CPUTicks struct {
	Busy  uint64
	Total uint64
}

// CPUSampler is an optional Sampler extension: CPU returns the cumulative CPU
// time of every NUMA node, keyed by node number. A node with no CPUs may be
// absent or zero. A sampler that does not implement it has no CPU signal, and
// the decision carries no CPU load.
type CPUSampler interface {
	CPU(ctx context.Context) (map[int]CPUTicks, error)
}

// CPU reads /proc/stat and the node cpulists under /sys.
func (s SysfsSampler) CPU(context.Context) (map[int]CPUTicks, error) {
	return ReadCPUFrom("/proc", "/sys")
}

// ReadCPUFrom sums /proc/stat per NUMA node, using the node cpulists under
// sysroot (normally /sys) so tests can inject a tree. A kernel without NUMA
// nodes in sysfs has every CPU on node 0. A CPU that is offline is absent from
// /proc/stat and adds nothing.
func ReadCPUFrom(procroot, sysroot string) (map[int]CPUTicks, error) {
	perCPU, err := readProcStat(filepath.Join(procroot, "stat"))
	if err != nil {
		return nil, err
	}
	nodes, err := nodeCPUs(sysroot)
	if err != nil {
		return nil, err
	}
	out := map[int]CPUTicks{}
	if nodes == nil {
		var t CPUTicks
		for _, c := range perCPU {
			t.Busy += c.Busy
			t.Total += c.Total
		}
		out[0] = t
		return out, nil
	}
	for n, cpus := range nodes {
		var t CPUTicks
		for _, c := range cpus {
			if v, ok := perCPU[c]; ok {
				t.Busy += v.Busy
				t.Total += v.Total
			}
		}
		out[n] = t
	}
	return out, nil
}

// readProcStat returns the per-CPU lines of /proc/stat. The fields are user,
// nice, system, idle, iowait, irq, softirq, steal, then guest and guest_nice,
// which user and nice already include, so only the first eight are summed.
func readProcStat(path string) (map[int]CPUTicks, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[int]CPUTicks{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") || fields[0] == "cpu" {
			continue
		}
		cpu, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu"))
		if err != nil {
			continue
		}
		var t CPUTicks
		for i, f := range fields[1:] {
			if i >= 8 {
				break
			}
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, fields[0], err)
			}
			t.Total += v
			if i != 3 && i != 4 { // idle, iowait
				t.Busy += v
			}
		}
		out[cpu] = t
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no per-CPU lines", path)
	}
	return out, nil
}

// nodeCPUs maps every NUMA node under sysroot to its CPUs; nil when sysfs has
// no NUMA nodes.
func nodeCPUs(sysroot string) (map[int][]int, error) {
	dir := filepath.Join(sysroot, "devices/system/node")
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out map[int][]int
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "node") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "node"))
		if err != nil {
			continue
		}
		cpus, err := topology.ParseCPUList(readTrim(filepath.Join(dir, e.Name(), "cpulist")))
		if err != nil {
			return nil, fmt.Errorf("%s cpulist: %w", e.Name(), err)
		}
		if out == nil {
			out = map[int][]int{}
		}
		out[n] = cpus
	}
	return out, nil
}

// cpuNodes maps each NUMA node the graph places an RNIC or endpoint on to its
// CPU package. A node the graph does not know carries no path, so its load is
// not tracked.
func cpuNodes(g topology.Graph) map[int]int {
	out := map[int]int{}
	for _, n := range g.RNICs {
		out[n.NUMA] = n.Socket
	}
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointMemory {
			out[e.NUMA] = e.Socket
		}
	}
	return out
}

// pollCPU reads the CPU load of every NUMA node the graph knows over the
// interval since the previous poll. It returns nil loads and known when the
// sampler has no CPU signal. A failed read, or a read right after one, gives
// no loads and known false: unknown load is neither idle nor busy.
func (m *Monitor) pollCPU(ctx context.Context) ([]model.CPULoad, bool) {
	cs, ok := m.Sampler.(CPUSampler)
	if !ok {
		return nil, true
	}
	cur, err := cs.CPU(ctx)
	if err != nil {
		m.cpuPrev = nil
		if !m.cpuFailed {
			m.cpuFailed = true
			m.logf("hostping: CPU load could not be read (%v); the host is not idle until it reads again", err)
		}
		return nil, false
	}
	if m.cpuFailed {
		m.cpuFailed = false
		m.logf("hostping: CPU load reads again; re-baselined")
	}
	prev := m.cpuPrev
	m.cpuPrev = cur
	if prev == nil {
		// The first reading, or the first after a failure or a run: a
		// baseline, like the RNIC counters on the same poll.
		return nil, false
	}
	var loads []model.CPULoad
	for numa, socket := range cpuNodes(m.Graph) {
		c, okc := cur[numa]
		p, okp := prev[numa]
		if !okc || !okp {
			continue
		}
		if c.Total <= p.Total || c.Busy < p.Busy {
			// No CPU, or a counter that went backwards (CPU hotplug):
			// nothing measured on this node.
			continue
		}
		busy := float64(c.Busy-p.Busy) / float64(c.Total-p.Total)
		if busy > 1 {
			busy = 1
		}
		loads = append(loads, model.CPULoad{
			NUMA:   numa,
			Socket: socket,
			Busy:   busy,
			High:   m.Cfg.CPUBusyHigh > 0 && busy >= m.Cfg.CPUBusyHigh,
		})
	}
	sort.Slice(loads, func(i, j int) bool { return loads[i].NUMA < loads[j].NUMA })
	return loads, true
}

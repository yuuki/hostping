package monitor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// Inter-socket bus utilization from the Intel uncore UPI link-layer PMUs, for
// the overloaded-inter-socket-bus rule of NSDI'23 §4.1.2 / ToN'24 §IV-A-3.
//
// The kernel exposes one PMU per UPI link agent, uncore_upi_<n>, with a
// cpumask of one CPU per die, but publishes no named events for it. The data
// flits a link sends are UNC_UPI_TxL_FLITS.ALL_DATA, event 0x02 umask 0x0f on
// every UPI generation, encoded here through the PMU's format/ fields.
//
// How many bytes of data one count stands for is not published by the kernel
// and differs from what the public tools assume: perf's metrics and Intel PCM
// both use 64/9 B (nine flits per 64-byte line), yet on Sapphire Rapids and
// Emerald Rapids one 64-byte line counts twelve, measured against a known
// cross-socket read and non-temporal write load and the IMC CAS counters.
// So the reader only
// trusts a ratio measured on the processor model (upiBytesPerFlit); any other
// model stays unknown unless the operator measured and set
// --upi-bytes-per-flit.
//
// Which package each link reaches comes from the PMU's die<N> files ("upi_M,
// die_E": link n of die N is wired to link M of die E). The kernel leaves the
// file out for some links that do carry traffic, so a missing file does not
// mean the link is unwired: on a host with two packages such a link can only
// reach the other one (and an unwired link counts no data), while on more
// packages the traffic leaving that die cannot be attributed and its
// directions stay unknown rather than undercounted. The traffic from package
// P to Q is the sum of the data sent on every link of P wired to Q. A upi.A-B link stands for all of
// the physical links between two packages, and since they are full duplex
// its utilization is the busier direction over the operator's per-direction
// --upi-capacity.

// upiPMURe matches the UPI link-layer PMUs.
var upiPMURe = regexp.MustCompile(`^uncore_upi_\d+$`)

// upiTxDataEvent is UNC_UPI_TxL_FLITS.ALL_DATA, the data flits a link sends.
const upiTxDataEvent = "event=0x02,umask=0x0f"

// upiBytesPerFlit is the bytes of data one UNC_UPI_TxL_FLITS.ALL_DATA count
// carries, by Intel family 6 model. Only ratios measured on the model are
// listed.
var upiBytesPerFlit = map[int]float64{
	0x8f: 64.0 / 12, // Sapphire Rapids: twelve counts per 64-byte line
	0xcf: 64.0 / 12, // Emerald Rapids: same as Sapphire Rapids
}

// upiDieRe parses a die<N> file of a UPI PMU.
var upiDieRe = regexp.MustCompile(`^upi_(\d+),die_(\d+)$`)

// cpuModel reads the vendor, family and model of the first CPU from a
// /proc/cpuinfo-format file.
func cpuModel(path string) (vendor string, family, model int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	family, model = -1, -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			if vendor != "" {
				break // end of the first CPU's block
			}
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "vendor_id":
			vendor = v
		case "cpu family":
			family, _ = strconv.Atoi(v)
		case "model":
			model, _ = strconv.Atoi(v)
		}
	}
	if err := sc.Err(); err != nil {
		return "", 0, 0, err
	}
	if vendor == "" || family < 0 || model < 0 {
		return "", 0, 0, fmt.Errorf("%s: no vendor_id, cpu family or model", path)
	}
	return vendor, family, model, nil
}

// upiFlitBytes is the bytes per data-flit count to use: the operator's
// override when set, else the measured ratio of this processor model.
func upiFlitBytes(cpuinfo string, override float64) (float64, error) {
	if override > 0 {
		return override, nil
	}
	vendor, family, mdl, err := cpuModel(cpuinfo)
	if err != nil {
		return 0, fmt.Errorf("%w: CPU model: %v", errUncoreUnsupported, err)
	}
	if vendor != "GenuineIntel" {
		return 0, fmt.Errorf("%w: %s inter-socket links (xGMI) are not read, only Intel UPI", errUncoreUnsupported, vendor)
	}
	if b, ok := upiBytesPerFlit[mdl]; ok && family == 6 {
		return b, nil
	}
	return 0, fmt.Errorf("%w: no measured UPI data bytes per flit for Intel family %d model %d (%#x); measure it and set --upi-bytes-per-flit", errUncoreUnsupported, family, mdl, mdl)
}

// upiGroup names the traffic sent from package from to package to.
func upiGroup(from, to int) string { return fmt.Sprintf("upi:%d>%d", from, to) }

// discoverUPIEvents lists one data-flit counter per UPI link and die, each
// in the group of the package pair it carries traffic between. unresolved
// names the packages with a link whose peer sysfs does not give on a host of
// more than two packages; the traffic they send cannot be attributed.
func discoverUPIEvents(sysroot string, bytesPerFlit float64) (evs []uncoreEvent, unresolved map[int]bool, err error) {
	dir := filepath.Join(sysroot, "bus/event_source/devices")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("perf PMUs: %w", err)
	}
	var pmus []string
	for _, e := range ents {
		if upiPMURe.MatchString(e.Name()) {
			pmus = append(pmus, e.Name())
		}
	}
	if len(pmus) == 0 {
		return nil, nil, fmt.Errorf("%w: no uncore_upi_<n> PMU under %s (not an Intel multi-socket server, or uncore driver not loaded)", errUncoreUnsupported, dir)
	}
	sort.Strings(pmus)
	type link struct {
		pmu  string
		typ  uint32
		c    [3]uint64
		cpus []int
		// peer[i] is the logical die link i of die i is wired to, -1 when
		// sysfs names none.
		peer []int
	}
	var links []link
	topo := false
	for _, pmu := range pmus {
		d := filepath.Join(dir, pmu)
		typ, err := strconv.ParseUint(readTrim(filepath.Join(d, "type")), 10, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: type: %v", errUncoreUnsupported, pmu, err)
		}
		var cpus []int
		cpus, err = parseCPUList(readTrim(filepath.Join(d, "cpumask")))
		if err != nil || len(cpus) == 0 {
			return nil, nil, fmt.Errorf("%w: %s: no cpumask", errUncoreUnsupported, pmu)
		}
		sort.Ints(cpus)
		format, err := readPMUFormat(filepath.Join(d, "format"))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %v", errUncoreUnsupported, pmu, err)
		}
		c0, c1, c2, err := encodeEvent(upiTxDataEvent, format)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s: %v", errUncoreUnsupported, pmu, err)
		}
		l := link{pmu: pmu, typ: uint32(typ), c: [3]uint64{c0, c1, c2}, cpus: cpus, peer: make([]int, len(cpus))}
		for i := range cpus {
			l.peer[i] = -1
			s := readTrim(filepath.Join(d, fmt.Sprintf("die%d", i)))
			if s == "" {
				continue
			}
			m := upiDieRe.FindStringSubmatch(s)
			if m == nil {
				return nil, nil, fmt.Errorf("%w: %s/die%d: malformed %q", errUncoreUnsupported, pmu, i, s)
			}
			e, _ := strconv.Atoi(m[2])
			if e >= len(cpus) {
				return nil, nil, fmt.Errorf("%w: %s/die%d: die %d is not in the cpumask", errUncoreUnsupported, pmu, i, e)
			}
			l.peer[i] = e
			topo = true
		}
		links = append(links, l)
	}
	// A die's logical ID is its position in the cpumask: the kernel numbers
	// dies in the order their first CPU comes up, and the cpumask holds one
	// CPU per die in CPU order.
	cpus := links[0].cpus
	pkgs := make([]int, len(cpus))
	for i, c := range cpus {
		p := readIntDef(filepath.Join(sysroot, "devices/system/cpu", fmt.Sprintf("cpu%d", c), "topology/physical_package_id"), -1)
		if p < 0 {
			return nil, nil, fmt.Errorf("%w: cpu%d has no physical_package_id", errUncoreUnsupported, c)
		}
		pkgs[i] = p
	}
	distinct := map[int]bool{}
	for _, p := range pkgs {
		distinct[p] = true
	}
	if !topo && len(distinct) != 2 {
		return nil, nil, fmt.Errorf("%w: sysfs does not say which package each UPI link reaches (no die<N> files), and with %d packages it cannot be inferred", errUncoreUnsupported, len(distinct))
	}
	var out []uncoreEvent
	unresolved = map[int]bool{}
	for _, l := range links {
		if fmt.Sprint(l.cpus) != fmt.Sprint(cpus) {
			return nil, nil, fmt.Errorf("%w: %s: cpumask differs from %s", errUncoreUnsupported, l.pmu, links[0].pmu)
		}
		for i, cpu := range l.cpus {
			from, to := pkgs[i], -1
			switch {
			case l.peer[i] >= 0:
				to = pkgs[l.peer[i]]
			case len(distinct) == 2:
				for p := range distinct {
					if p != from {
						to = p
					}
				}
			default:
				unresolved[from] = true
				continue
			}
			if to == from {
				continue // between dies of one package
			}
			die, err := cpuDie(sysroot, cpu)
			if err != nil {
				return nil, nil, fmt.Errorf("%w: %s: cpu%d: %v", errUncoreUnsupported, l.pmu, cpu, err)
			}
			out = append(out, uncoreEvent{
				PMU: l.pmu, Name: "txl_flits_all_data", Type: l.typ,
				Config: l.c[0], Config1: l.c[1], Config2: l.c[2],
				CPU: cpu, Die: die, Group: upiGroup(from, to), BytesPerCount: bytesPerFlit,
			})
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%w: no UPI link is wired between packages", errUncoreUnsupported)
	}
	return out, unresolved, nil
}

// upiLinkGroups maps each inter-socket link that has a read (not assumed)
// capacity to the traffic groups of its two directions. A upi.A-B link joins
// hostping sockets A and B, which are packages under sub-NUMA clustering and
// NUMA nodes otherwise; both resolve to a package through the CPUs of a
// NUMA node of that socket. Links that cannot be mapped are returned with
// the reason.
func upiLinkGroups(g topology.Graph, sysroot string) (map[string][]string, map[string]string) {
	byNode, _ := nodeDies(sysroot)
	pkgOf := map[int]int{} // hostping socket → package
	note := func(socket, numa int) {
		die, ok := byNode[numa]
		if !ok {
			return
		}
		p, err := strconv.Atoi(strings.SplitN(die, "/", 2)[0])
		if err == nil {
			pkgOf[socket] = p
		}
	}
	for _, n := range g.RNICs {
		note(n.Socket, n.NUMA)
	}
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointMemory {
			note(e.Socket, e.NUMA)
		}
	}
	links := map[string][]string{}
	skipped := map[string]string{}
	for _, l := range g.Links {
		if l.Kind != model.LinkKindUPI {
			continue
		}
		if _, ok := utilCapacity(g, l.ID); !ok {
			skipped[l.ID] = "capacity assumed; set --upi-capacity"
			continue
		}
		var a, b int
		if _, err := fmt.Sscanf(l.ID, "upi.%d-%d", &a, &b); err != nil {
			skipped[l.ID] = "link ID names no socket pair"
			continue
		}
		pa, okA := pkgOf[a]
		pb, okB := pkgOf[b]
		if !okA || !okB {
			skipped[l.ID] = "CPU package of its sockets not in sysfs"
			continue
		}
		if pa == pb {
			skipped[l.ID] = fmt.Sprintf("sockets %d and %d are one package %d", a, b, pa)
			continue
		}
		links[l.ID] = []string{upiGroup(pa, pb), upiGroup(pb, pa)}
	}
	return links, skipped
}

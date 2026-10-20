package monitor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sysfs"
	"github.com/yuuki/hostping/internal/topology"
)

// Memory-channel utilization from the Intel uncore integrated memory
// controller (IMC) PMUs, for the overloaded-memory rule of NSDI'23 §4.1.2 /
// ToN'24 §IV-A-3.
//
// The kernel exposes one PMU per IMC channel box, uncore_imc_<n>, under
// /sys/bus/event_source/devices. Each names its DRAM CAS events in events/
// (cas_count_read / cas_count_write, or cas_count_{read,write}_sch<k> per
// sub-channel on newer parts) with the encoding in terms of the format/
// fields, and a scale and unit that turn a count into bytes (one CAS moves a
// 64-byte line). A PMU's cpumask names one CPU per package (or die): opening
// the event on that CPU counts that die's box. Summing every box and event of
// a die gives the die's DRAM traffic, reads plus writes, since the channels
// are half duplex.
//
// A mem.N link stands for one NUMA node's memory. With one CPU NUMA node per
// die the die's traffic is the link's. Under sub-NUMA clustering a die holds
// several nodes, and sysfs does not say which IMC serves which node, so those
// links stay unknown rather than being handed a share by guess.
//
// Inter-socket buses (UPI) are read by the same machinery; see upi.go.

// imcPMURe matches the per-channel IMC PMUs. uncore_imc_free_running_<n>
// counts the same traffic again and is left out.
var imcPMURe = regexp.MustCompile(`^uncore_imc_\d+$`)

// casEventRe matches the CAS count events the kernel names.
var casEventRe = regexp.MustCompile(`^cas_count_(read|write)(_sch\d+)?$`)

// casLineBytes is the bytes one CAS command transfers: one 64-byte cache
// line. It is used only for a CAS event whose sysfs carries neither scale nor
// unit.
const casLineBytes = 64

// errUncoreUnsupported marks platforms the reader does not cover.
var errUncoreUnsupported = errors.New("uncore utilization unsupported")

// uncoreEvent is one counter to open: a PMU event on the CPU that stands for
// one die.
type uncoreEvent struct {
	PMU                      string
	Name                     string
	Type                     uint32
	Config, Config1, Config2 uint64
	CPU                      int
	// Die is the "package/die" the counter covers.
	Die string
	// Group is the traffic the counter adds to: an IMC counter's die, or a
	// UPI counter's package pair and direction (upiGroup). A group is read
	// whole or not at all.
	Group string
	// BytesPerCount turns a raw count into bytes.
	BytesPerCount float64
}

// perfCounter is one open perf event. Read returns the raw count and the time
// the event was enabled and actually running, in ns.
type perfCounter interface {
	Read() (count, enabled, running uint64, err error)
	Close() error
}

type perfOpener func(uncoreEvent) (perfCounter, error)

// gnrModel is Granite Rapids, Intel family 6.
const gnrModel = 0xad

// gnrSubChannel1 reports whether cpuinfo names a Granite Rapids CPU, whose
// IMC channels have a second DDR5 sub-channel counted by its own CAS event
// (see addSubChannel1).
func gnrSubChannel1(cpuinfo string) bool {
	vendor, family, model, err := cpuModel(cpuinfo)
	return err == nil && vendor == "GenuineIntel" && family == 6 && model == gnrModel
}

// discoverIMCEvents lists every IMC CAS counter under sysroot, one per PMU,
// event and die. It fails when no IMC PMU exists (saying so plainly on AMD,
// whose memory PMUs this reader does not read) or none names both a read and
// a write CAS event in bytes. gnr adds the sub-channel 1 counters a Granite
// Rapids kernel may leave unnamed (addSubChannel1).
func discoverIMCEvents(sysroot string, gnr bool) ([]uncoreEvent, error) {
	dir := filepath.Join(sysroot, "bus/event_source/devices")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("perf PMUs: %w", err)
	}
	var pmus []string
	amd := false
	for _, e := range ents {
		name := e.Name()
		switch {
		case imcPMURe.MatchString(name):
			pmus = append(pmus, name)
		case name == "amd_df" || strings.HasPrefix(name, "amd_umc"):
			amd = true
		}
	}
	if len(pmus) == 0 {
		if amd {
			return nil, fmt.Errorf("%w: AMD data fabric / UMC PMUs are not read, only Intel uncore_imc", errUncoreUnsupported)
		}
		return nil, fmt.Errorf("%w: no uncore_imc_<n> PMU under %s (not an Intel server, or uncore driver not loaded)", errUncoreUnsupported, dir)
	}
	sort.Strings(pmus)
	var out []uncoreEvent
	var why []string
	for _, pmu := range pmus {
		evs, err := imcPMUEvents(sysroot, filepath.Join(dir, pmu), pmu, gnr)
		if err != nil {
			why = append(why, err.Error())
			continue
		}
		out = append(out, evs...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no usable IMC CAS events (%s)", errUncoreUnsupported, strings.Join(why, "; "))
	}
	return out, nil
}

// pmuInfo is what a PMU's sysfs directory says about opening its events: its
// perf event type, the CPUs its events are opened on (one per package or
// die) and its format/ fields.
type pmuInfo struct {
	typ    uint32
	cpus   []int
	format map[string]formatField
}

// readPMU reads a PMU's type, cpumask and format/ fields from its sysfs
// directory. The error names only what is wrong; the caller prefixes the PMU.
func readPMU(dir string) (pmuInfo, error) {
	typ, err := strconv.ParseUint(readTrim(filepath.Join(dir, "type")), 10, 32)
	if err != nil {
		return pmuInfo{}, fmt.Errorf("type: %v", err)
	}
	cpus, err := parseCPUList(readTrim(filepath.Join(dir, "cpumask")))
	if err != nil || len(cpus) == 0 {
		return pmuInfo{}, errors.New("no cpumask")
	}
	format, err := readPMUFormat(filepath.Join(dir, "format"))
	if err != nil {
		return pmuInfo{}, err
	}
	return pmuInfo{typ: uint32(typ), cpus: cpus, format: format}, nil
}

func imcPMUEvents(sysroot, dir, pmu string, gnr bool) ([]uncoreEvent, error) {
	info, err := readPMU(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", pmu, err)
	}
	typ, cpus, format := info.typ, info.cpus, info.format
	ents, err := os.ReadDir(filepath.Join(dir, "events"))
	if err != nil {
		return nil, fmt.Errorf("%s: events: %v", pmu, err)
	}
	type enc struct {
		name       string
		c, c1, c2  uint64
		bytesCount float64
	}
	var encs []enc
	sawRead, sawWrite := false, false
	raw := map[string]string{}
	bytesOf := map[string]float64{}
	for _, e := range ents {
		m := casEventRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		base := filepath.Join(dir, "events", e.Name())
		raw[e.Name()] = readTrim(base)
		c, c1, c2, err := encodeEvent(raw[e.Name()], format)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %v", pmu, e.Name(), err)
		}
		bpc, err := eventBytes(base)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %v", pmu, e.Name(), err)
		}
		bytesOf[e.Name()] = bpc
		encs = append(encs, enc{e.Name(), c, c1, c2, bpc})
		if m[1] == "read" {
			sawRead = true
		} else {
			sawWrite = true
		}
	}
	if gnr {
		for name, s := range addSubChannel1(raw) {
			c, c1, c2, err := encodeEvent(s, format)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %v", pmu, name, err)
			}
			encs = append(encs, enc{name, c, c1, c2, bytesOf[strings.TrimSuffix(name, "_sch1")]})
		}
	}
	// Reads alone (or writes alone) would pass for a lightly loaded channel.
	if !sawRead || !sawWrite {
		return nil, fmt.Errorf("%s: needs both cas_count_read and cas_count_write events", pmu)
	}
	var out []uncoreEvent
	seen := map[string]bool{}
	for _, cpu := range cpus {
		die, err := cpuDie(sysroot, cpu)
		if err != nil {
			return nil, fmt.Errorf("%s: cpu%d: %v", pmu, cpu, err)
		}
		if seen[die] {
			continue
		}
		seen[die] = true
		for _, x := range encs {
			out = append(out, uncoreEvent{
				PMU: pmu, Name: x.name, Type: typ,
				Config: x.c, Config1: x.c1, Config2: x.c2,
				CPU: cpu, Die: die, Group: die, BytesPerCount: x.bytesCount,
			})
		}
	}
	return out, nil
}

// addSubChannel1 returns the sub-channel 1 CAS events a Granite Rapids IMC
// PMU leaves unnamed. Each channel there has two DDR5 sub-channels counted by
// separate events, CAS event 0x05 for sub-channel 0 and 0x06 for
// sub-channel 1 with the same umask; upstream Linux names both
// (cas_count_read_sch0 / _sch1), but some kernels publish only
// cas_count_read / cas_count_write encoded as event 0x05, which is
// sub-channel 0 alone and reads half the traffic (measured against a known
// load). For each such event the result holds its event 0x06 twin, named
// <event>_sch1, to be summed with the same scale. A PMU that names any _sch
// event is left as published.
func addSubChannel1(raw map[string]string) map[string]string {
	for name := range raw {
		if strings.Contains(name, "_sch") {
			return nil
		}
	}
	out := map[string]string{}
	for _, name := range []string{"cas_count_read", "cas_count_write"} {
		s, ok := raw[name]
		if !ok {
			continue
		}
		var terms []string
		sch0 := false
		for _, term := range strings.Split(s, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(term), "=")
			if n, err := strconv.ParseUint(v, 0, 64); err == nil && k == "event" && n == 0x05 {
				sch0 = true
				term = "event=0x06"
			}
			terms = append(terms, term)
		}
		if sch0 {
			out[name+"_sch1"] = strings.Join(terms, ",")
		}
	}
	return out
}

// formatField is one format/ file: which attr config word a term lands in
// and at which bits, low bits of the value first.
type formatField struct {
	word   int // 0 config, 1 config1, 2 config2
	ranges [][2]int
}

func readPMUFormat(dir string) (map[string]formatField, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("format: %v", err)
	}
	out := map[string]formatField{}
	for _, e := range ents {
		f, err := parseFormat(readTrim(filepath.Join(dir, e.Name())))
		if err != nil {
			return nil, fmt.Errorf("format/%s: %v", e.Name(), err)
		}
		out[e.Name()] = f
	}
	return out, nil
}

// parseFormat reads a format/ file such as "config:0-7", "config1:0-31" or
// "config:0-7,32-35".
func parseFormat(s string) (formatField, error) {
	word, bits, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return formatField{}, fmt.Errorf("malformed %q", s)
	}
	var f formatField
	switch word {
	case "config":
		f.word = 0
	case "config1":
		f.word = 1
	case "config2":
		f.word = 2
	default:
		return formatField{}, fmt.Errorf("unsupported config word %q", word)
	}
	for _, r := range strings.Split(bits, ",") {
		lo, hi, isRange := strings.Cut(r, "-")
		l, err := strconv.Atoi(lo)
		if err != nil {
			return formatField{}, fmt.Errorf("malformed %q", s)
		}
		h := l
		if isRange {
			if h, err = strconv.Atoi(hi); err != nil {
				return formatField{}, fmt.Errorf("malformed %q", s)
			}
		}
		if l < 0 || h < l || h > 63 {
			return formatField{}, fmt.Errorf("bits out of range in %q", s)
		}
		f.ranges = append(f.ranges, [2]int{l, h})
	}
	return f, nil
}

// encodeEvent turns an events/ string such as "event=0x04,umask=0x03" into
// perf_event_attr config words using the PMU's format fields. A term without
// a value is 1, as in perf's own parser.
func encodeEvent(s string, format map[string]formatField) (c0, c1, c2 uint64, err error) {
	var words [3]uint64
	for _, term := range strings.Split(strings.TrimSpace(s), ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		name, val, hasVal := strings.Cut(term, "=")
		v := uint64(1)
		if hasVal {
			v, err = strconv.ParseUint(val, 0, 64)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("term %q: %v", term, err)
			}
		}
		f, ok := format[name]
		if !ok {
			return 0, 0, 0, fmt.Errorf("term %q has no format field", name)
		}
		rest := v
		for _, r := range f.ranges {
			n := r[1] - r[0] + 1
			mask := uint64(1)<<n - 1
			if n == 64 {
				mask = ^uint64(0)
			}
			words[f.word] |= (rest & mask) << r[0]
			if n == 64 {
				rest = 0
			} else {
				rest >>= n
			}
		}
		if rest != 0 {
			return 0, 0, 0, fmt.Errorf("term %q does not fit its format field", term)
		}
	}
	return words[0], words[1], words[2], nil
}

// eventBytes is the bytes one count of the event stands for: its .scale times
// its .unit. A CAS event with neither file counts 64-byte lines.
func eventBytes(base string) (float64, error) {
	scaleS := readTrim(base + ".scale")
	unitS := readTrim(base + ".unit")
	if scaleS == "" && unitS == "" {
		return casLineBytes, nil
	}
	scale := 1.0
	if scaleS != "" {
		v, err := strconv.ParseFloat(scaleS, 64)
		if err != nil || v <= 0 {
			return 0, fmt.Errorf("scale %q", scaleS)
		}
		scale = v
	}
	if unitS == "" {
		return 0, fmt.Errorf("scale without a unit")
	}
	unit, ok := map[string]float64{
		"B": 1, "Bytes": 1, "bytes": 1,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30,
		"KB": 1e3, "kB": 1e3, "MB": 1e6, "GB": 1e9,
	}[unitS]
	if !ok {
		return 0, fmt.Errorf("unit %q is not a byte unit", unitS)
	}
	return scale * unit, nil
}

// parseCPUList parses a kernel CPU list such as "0,56" or "0-3,8". Topology
// discovery reads the same lists to find each NUMA node's CPU package.
var parseCPUList = topology.ParseCPUList

// cpuDie names the package and die a CPU belongs to. die_id is absent on
// kernels and parts without multi-die packages, which is die 0.
func cpuDie(sysroot string, cpu int) (string, error) {
	pkg := sysfs.PackageID(sysroot, cpu)
	if pkg < 0 {
		return "", errors.New("no physical_package_id")
	}
	dieID := sysfs.ReadInt(filepath.Join(sysroot, "devices/system/cpu", fmt.Sprintf("cpu%d", cpu), "topology/die_id"), 0)
	return fmt.Sprintf("%d/%d", pkg, dieID), nil
}

// nodeDies maps every NUMA node with CPUs to its die, and each die to its
// nodes. A node whose CPUs span dies has no single die and is left out, as
// is one whose cpulist is missing, empty or malformed (nothing is claimed
// about it; the memory link of such a node is reported as unmapped).
func nodeDies(sysroot string) (map[int]string, map[string][]int) {
	byNode := map[int]string{}
	byDie := map[string][]int{}
	nodes, err := sysfs.Nodes(sysroot)
	if err != nil {
		return byNode, byDie
	}
	for _, nd := range nodes {
		n, cpus := nd.ID, nd.CPUs
		if nd.ParseErr != nil || len(cpus) == 0 {
			continue
		}
		die := ""
		for _, c := range cpus {
			d, err := cpuDie(sysroot, c)
			if err != nil || (die != "" && d != die) {
				die = ""
				break
			}
			die = d
		}
		if die == "" {
			continue
		}
		byNode[n] = die
		byDie[die] = append(byDie[die], n)
	}
	return byNode, byDie
}

// memLink is where a memory link's traffic is counted: the IMC boxes of a die
// that holds only its node, or, under sub-NUMA clustering, the boxes of the
// die that serve its node (Nodes lists every CPU node of the die, to be told
// apart by mapSNCBoxes).
type memLink struct {
	Die   string
	Node  int
	Nodes []int // set only under sub-NUMA clustering
}

// memLinkDies maps each memory link that has a read (not assumed) capacity to
// the die whose IMC traffic holds its own. Links that cannot be mapped are
// returned with the reason.
func memLinkDies(g topology.Graph, sysroot string) (map[string]memLink, map[string]string) {
	byNode, byDie := nodeDies(sysroot)
	links := map[string]memLink{}
	skipped := map[string]string{}
	for _, l := range g.Links {
		if l.Kind != model.LinkKindMemory {
			continue
		}
		if _, ok := utilCapacity(g, l.ID); !ok {
			skipped[l.ID] = "capacity assumed; set --mem-channel-capacity"
			continue
		}
		e, ok := g.Endpoint(l.Device)
		if !ok || e.Kind != model.EndpointMemory {
			skipped[l.ID] = "no memory endpoint"
			continue
		}
		die, ok := byNode[e.NUMA]
		if !ok {
			skipped[l.ID] = fmt.Sprintf("NUMA node %d has no CPU package in sysfs", e.NUMA)
			continue
		}
		m := memLink{Die: die, Node: e.NUMA}
		if len(byDie[die]) > 1 {
			m.Nodes = append([]int(nil), byDie[die]...)
			sort.Ints(m.Nodes)
		}
		links[l.ID] = m
	}
	return links, skipped
}

// Uncore keeps the IMC and UPI counters open across polls and turns the delta
// between two polls into memory-link and inter-socket-link utilization.
type Uncore struct {
	mu    sync.Mutex
	now   func() time.Time
	links map[string][]string // link ID → traffic groups; the busiest counts
	caps  map[string]uint64
	ctrs  []uncoreCounter
	// Skipped names the memory and inter-socket links left unknown, with the
	// reason.
	Skipped map[string]string
	// SNCBoxes describes, per sub-NUMA-clustered die, which IMC boxes the
	// start-up measurement gave to which NUMA node, for the log.
	SNCBoxes map[string]string

	prevAt  time.Time
	hasPrev bool
	util    map[string]float64
}

type uncoreCounter struct {
	ev   uncoreEvent
	c    perfCounter
	prev perfReading
}

type perfReading struct {
	ok                      bool
	count, enabled, running uint64
}

// UncoreOptions tunes NewUncore.
type UncoreOptions struct {
	// UPIBytesPerFlit, when positive, is the bytes of data one UPI data-flit
	// count carries, replacing the measured per-model ratio.
	UPIBytesPerFlit float64
}

// NewUncore opens the IMC counters for every memory link of g that has an
// operator-supplied capacity and maps one-to-one onto a die, and the UPI
// data-flit counters for every inter-socket link that has one. It fails when
// no link can be read: not Linux, not an Intel uncore IMC/UPI, perf access
// denied (perf_event_paranoid / CAP_PERFMON), no measured UPI flit size for
// the processor, or no link with a capacity.
func NewUncore(g topology.Graph, opts UncoreOptions) (*Uncore, error) {
	if !perfSupported {
		return nil, fmt.Errorf("%w: perf_event_open is Linux-only", errUncoreUnsupported)
	}
	return newUncore(g, "/sys", "/proc/cpuinfo", opts, openPerfCounter)
}

func newUncore(g topology.Graph, sysroot, cpuinfo string, opts UncoreOptions, open perfOpener) (*Uncore, error) {
	plan := &uncorePlan{want: map[string][]string{}, skipped: map[string]string{}}
	sncBoxes := plan.memLinks(g, sysroot, cpuinfo, open)
	plan.upiLinks(g, sysroot, cpuinfo, opts)
	if len(plan.want) == 0 {
		return nil, fmt.Errorf("no memory or inter-socket link to read (%s)", joinReasons(plan.skipped))
	}

	u := &Uncore{now: time.Now, links: map[string][]string{}, caps: map[string]uint64{}, Skipped: plan.skipped, SNCBoxes: describeSNC(sncBoxes)}
	byGroup, failed := openGroups(plan.evs, groupSet(plan.want), open)
	u.bindLinks(g, plan.want, byGroup, failed)
	// Keep only the counters of groups some read link uses.
	u.ctrs = pruneGroups(byGroup, groupSet(u.links))
	if len(u.links) == 0 {
		u.Close()
		return nil, fmt.Errorf("no memory or inter-socket link to read (%s)", joinReasons(plan.skipped))
	}
	return u, nil
}

// uncorePlan is what the links of a host need before any counter is opened
// for good: the traffic groups each readable link's utilization comes from,
// why the other links are left unknown, and the counters that feed the
// groups.
type uncorePlan struct {
	want    map[string][]string // link ID → traffic groups
	skipped map[string]string   // link ID → reason
	evs     []uncoreEvent
}

// memLinks plans the memory links of g: each one maps to the IMC traffic of
// its die, or, under sub-NUMA clustering, of the die's boxes the start-up
// measurement gave its node. It returns that measurement's result per die,
// for the log.
func (p *uncorePlan) memLinks(g topology.Graph, sysroot, cpuinfo string, open perfOpener) map[string]map[string]int {
	memLinks, memSkipped := memLinkDies(g, sysroot)
	for id, why := range memSkipped {
		p.skipped[id] = why
	}
	if len(memLinks) == 0 {
		return nil
	}
	imc, err := discoverIMCEvents(sysroot, gnrSubChannel1(cpuinfo))
	var sncBoxes map[string]map[string]int
	var sncErr map[string]error
	if err == nil {
		sncBoxes, sncErr = regroupSNC(memLinks, imc, open)
	}
	for id, m := range memLinks {
		switch {
		case err != nil:
			p.skipped[id] = err.Error()
		case m.Nodes == nil:
			p.want[id] = []string{m.Die}
		case sncErr[m.Die] != nil:
			p.skipped[id] = fmt.Sprintf("package/die %s holds %d NUMA nodes (sub-NUMA clustering) and which IMC serves which could not be measured: %v", m.Die, len(m.Nodes), sncErr[m.Die])
		default:
			p.want[id] = []string{sncGroup(m.Die, m.Node)}
		}
	}
	p.evs = append(p.evs, imc...)
	return sncBoxes
}

// upiLinks plans the inter-socket links of g: each one maps to the UPI
// traffic groups of its two directions.
func (p *uncorePlan) upiLinks(g topology.Graph, sysroot, cpuinfo string, opts UncoreOptions) {
	upiLinks, upiSkipped := upiLinkGroups(g, sysroot)
	for id, why := range upiSkipped {
		p.skipped[id] = why
	}
	if len(upiLinks) == 0 {
		return
	}
	var upi []uncoreEvent
	var unresolved map[int]bool
	bpf, err := upiFlitBytes(cpuinfo, opts.UPIBytesPerFlit)
	if err == nil {
		upi, unresolved, err = discoverUPIEvents(sysroot, bpf)
	}
	for id, groups := range upiLinks {
		if err != nil {
			p.skipped[id] = err.Error()
			continue
		}
		if pkg := unresolvedSender(groups, unresolved); pkg >= 0 {
			p.skipped[id] = fmt.Sprintf("a UPI link of package %d names no peer in sysfs, so the traffic it sends cannot be attributed", pkg)
			continue
		}
		p.want[id] = groups
	}
	p.evs = append(p.evs, upi...)
}

// unresolvedSender is a package of unresolved that sends on one of groups,
// or -1 when none does.
func unresolvedSender(groups []string, unresolved map[int]bool) int {
	for pkg := range unresolved {
		for _, gr := range groups {
			if strings.HasPrefix(gr, fmt.Sprintf("upi:%d>", pkg)) {
				return pkg
			}
		}
	}
	return -1
}

// groupSet is the set of traffic groups the links of m use.
func groupSet(m map[string][]string) map[string]bool {
	set := map[string]bool{}
	for _, groups := range m {
		for _, gr := range groups {
			set[gr] = true
		}
	}
	return set
}

// openGroups opens every counter of the wanted groups, in evs order. A group
// missing one of its boxes or links would undercount, so when a counter
// fails to open its group's already opened counters are closed and the group
// is returned in failed, with why, instead of in byGroup.
func openGroups(evs []uncoreEvent, wanted map[string]bool, open perfOpener) (byGroup map[string][]uncoreCounter, failed map[string]error) {
	byGroup = map[string][]uncoreCounter{}
	failed = map[string]error{}
	for _, ev := range evs {
		if !wanted[ev.Group] || failed[ev.Group] != nil {
			continue
		}
		c, err := open(ev)
		if err != nil {
			failed[ev.Group] = fmt.Errorf("%s/%s on cpu%d: %w", ev.PMU, ev.Name, ev.CPU, err)
			closeCounters(byGroup[ev.Group])
			delete(byGroup, ev.Group)
			continue
		}
		byGroup[ev.Group] = append(byGroup[ev.Group], uncoreCounter{ev: ev, c: c})
	}
	return byGroup, failed
}

// bindLinks reads every link of want whose groups all have open counters,
// with its capacity, and skips the others with the reason.
func (u *Uncore) bindLinks(g topology.Graph, want map[string][]string, byGroup map[string][]uncoreCounter, failed map[string]error) {
	for id, groups := range want {
		if why := unreadableGroup(groups, byGroup, failed); why != "" {
			u.Skipped[id] = why
			continue
		}
		u.links[id] = groups
		u.caps[id], _ = utilCapacity(g, id)
	}
}

// unreadableGroup says why the first of groups that has no open counters
// cannot be read, or "" when every group can.
func unreadableGroup(groups []string, byGroup map[string][]uncoreCounter, failed map[string]error) string {
	for _, gr := range groups {
		if err := failed[gr]; err != nil {
			return err.Error()
		}
		if len(byGroup[gr]) == 0 {
			return "no uncore counter covers " + groupDesc(gr)
		}
	}
	return ""
}

// pruneGroups closes the counters of every group of byGroup that is not
// used and returns the others, in group-name order.
func pruneGroups(byGroup map[string][]uncoreCounter, used map[string]bool) []uncoreCounter {
	names := make([]string, 0, len(byGroup))
	for gr := range byGroup {
		names = append(names, gr)
	}
	sort.Strings(names)
	var ctrs []uncoreCounter
	for _, gr := range names {
		if !used[gr] {
			closeCounters(byGroup[gr])
			continue
		}
		ctrs = append(ctrs, byGroup[gr]...)
	}
	return ctrs
}

func closeCounters(ctrs []uncoreCounter) {
	for _, x := range ctrs {
		_ = x.c.Close()
	}
}

// groupDesc names a traffic group for a log line.
func groupDesc(gr string) string {
	if strings.HasPrefix(gr, "upi:") {
		from, to, _ := strings.Cut(strings.TrimPrefix(gr, "upi:"), ">")
		return "UPI traffic from package " + from + " to package " + to
	}
	return "package/die " + gr
}

func joinReasons(m map[string]string) string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+": "+m[id])
	}
	if len(parts) == 0 {
		return "no memory or inter-socket links"
	}
	return strings.Join(parts, "; ")
}

// Poll reads every counter and, unless reset or this is the first read,
// computes each link's utilization over the interval since the previous one.
// reset drops the previous reading: that interval contained an engine run,
// whose own loopback traffic to DRAM and across sockets must not be read as
// host load.
func (u *Uncore) Poll(reset bool) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	cur := make([]perfReading, len(u.ctrs))
	for i, c := range u.ctrs {
		n, en, run, err := c.c.Read()
		cur[i] = perfReading{ok: err == nil, count: n, enabled: en, running: run}
	}
	dt := now.Sub(u.prevAt).Seconds()
	u.util = nil
	if u.hasPrev && !reset && dt > 0 {
		bytes := map[string]float64{}
		bad := map[string]bool{}
		for i, c := range u.ctrs {
			gr := c.ev.Group
			p, q := c.prev, cur[i]
			switch {
			case !p.ok || !q.ok,
				q.count < p.count, q.enabled < p.enabled, q.running < p.running,
				// Multiplexed: the counter was off part of the interval.
				q.running-p.running < q.enabled-p.enabled:
				bad[gr] = true
				continue
			}
			bytes[gr] += float64(q.count-p.count) * c.ev.BytesPerCount
		}
		util := map[string]float64{}
	links:
		for id, groups := range u.links {
			peak := 0.0
			for _, gr := range groups {
				if bad[gr] {
					continue links
				}
				peak = max(peak, bytes[gr])
			}
			util[id] = peak * 8 / dt / float64(u.caps[id])
		}
		if len(util) > 0 {
			u.util = util
		}
	}
	for i := range u.ctrs {
		u.ctrs[i].prev = cur[i]
	}
	u.prevAt = now
	u.hasPrev = true
}

// Util is the utilization of the last Poll interval, keyed by mem link ID;
// nil when there is none yet.
func (u *Uncore) Util() map[string]float64 {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.util == nil {
		return nil
	}
	out := make(map[string]float64, len(u.util))
	for k, v := range u.util {
		out[k] = v
	}
	return out
}

// Links is the memory and inter-socket links the reader covers.
func (u *Uncore) Links() []string {
	if u == nil {
		return nil
	}
	out := make([]string, 0, len(u.links))
	for id := range u.links {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Close releases every counter.
func (u *Uncore) Close() {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	closeCounters(u.ctrs)
	u.ctrs = nil
}

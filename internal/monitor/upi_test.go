package monitor

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// sprCPUInfo is the head of /proc/cpuinfo on a Sapphire Rapids host.
const sprCPUInfo = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 143
model name	: Intel(R) Xeon(R) Platinum

processor	: 1
vendor_id	: GenuineIntel
cpu family	: 6
model		: 143
`

// upiPMU is the sysfs of one uncore_upi_<n> PMU in the kernel's layout: no
// events/, and die<N> files naming the link and die it is wired to.
func upiPMU(name, typ, cpumask string, dies map[int]string) map[string]string {
	d := "bus/event_source/devices/" + name + "/"
	files := map[string]string{
		d + "type":          typ,
		d + "cpumask":       cpumask,
		d + "format/event":  "config:0-7",
		d + "format/umask":  "config:8-15,32-55",
		d + "format/edge":   "config:18",
		d + "format/thresh": "config:24-31",
	}
	for i, v := range dies {
		files[fmt.Sprintf("%sdie%d", d, i)] = v
	}
	return files
}

// fakeUPIHost is fakeHost's two-package host with nodes 0 (CPUs 0-3) and 1
// (CPUs 4-7), three UPI PMUs (links 0 and 1 cross-wired between the
// packages, link 2 with no die<N> files, as the kernel leaves some wired
// links) and a Sapphire Rapids cpuinfo.
func fakeUPIHost(t *testing.T) (root, cpuinfo string) {
	t.Helper()
	root = fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	files := map[string]string{}
	for k, v := range upiPMU("uncore_upi_0", "30", "0,4", map[int]string{0: "upi_1,die_1", 1: "upi_1,die_0"}) {
		files[k] = v
	}
	for k, v := range upiPMU("uncore_upi_1", "31", "0,4", map[int]string{0: "upi_0,die_1", 1: "upi_0,die_0"}) {
		files[k] = v
	}
	for k, v := range upiPMU("uncore_upi_2", "32", "0,4", nil) {
		files[k] = v
	}
	files["cpuinfo"] = sprCPUInfo
	writeTree(t, root, files)
	return root, filepath.Join(root, "cpuinfo")
}

// upiGraph is memGraph plus the upi.0-1 link discovery adds between its two
// sockets, with the given capacity.
func upiGraph(memCap, upiCap uint64, upiAssumed bool) topology.Graph {
	g := memGraph(memCap, memCap == 0)
	g.Links = append(g.Links, model.Link{ID: "upi.0-1", Kind: model.LinkKindUPI, CapacityBPS: upiCap, CapacityAssumed: upiAssumed})
	return g
}

func advanceGroup(table map[string]perfReading, group string, count uint64, dt time.Duration) {
	for k, r := range table {
		if !strings.HasSuffix(k, "/"+group) {
			continue
		}
		r.count += count
		r.enabled += uint64(dt)
		r.running += uint64(dt)
		table[k] = r
	}
}

func TestUPIFlitBytes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"spr":   sprCPUInfo,
		"other": "vendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 106",
		"amd":   "vendor_id\t: AuthenticAMD\ncpu family\t: 25\nmodel\t\t: 17",
		"emr":   "vendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 207",
	})
	got, err := upiFlitBytes(filepath.Join(root, "spr"), 0)
	if err != nil || math.Abs(got-64.0/12) > 1e-12 {
		t.Fatalf("measured model: %v %v, want 64/12", got, err)
	}
	if got, err := upiFlitBytes(filepath.Join(root, "emr"), 0); err != nil || math.Abs(got-64.0/12) > 1e-12 {
		t.Fatalf("Emerald Rapids: %v %v, want 64/12", got, err)
	}
	if got, err := upiFlitBytes(filepath.Join(root, "other"), 7.5); err != nil || got != 7.5 {
		t.Fatalf("the operator's ratio must win: %v %v", got, err)
	}
	if _, err := upiFlitBytes(filepath.Join(root, "other"), 0); !errors.Is(err, errUncoreUnsupported) || !strings.Contains(err.Error(), "--upi-bytes-per-flit") || !strings.Contains(err.Error(), "model 106") {
		t.Fatalf("an unmeasured model must stay unknown and say how to fix it: %v", err)
	}
	if _, err := upiFlitBytes(filepath.Join(root, "amd"), 0); !errors.Is(err, errUncoreUnsupported) || !strings.Contains(err.Error(), "xGMI") {
		t.Fatalf("AMD must be reported as unsupported: %v", err)
	}
	if _, err := upiFlitBytes(filepath.Join(root, "missing"), 0); !errors.Is(err, errUncoreUnsupported) {
		t.Fatalf("unreadable cpuinfo: %v", err)
	}
}

func TestDiscoverUPIEventsFollowsLinkTopology(t *testing.T) {
	root, _ := fakeUPIHost(t)
	evs, unresolved, err := discoverUPIEvents(root, 5)
	if err != nil || len(unresolved) != 0 {
		t.Fatal(err, unresolved)
	}
	// Every link on both dies: with two packages, link 2 can only reach the
	// other one, and it carries traffic like the others.
	if len(evs) != 6 {
		t.Fatalf("got %d events, want 6: %+v", len(evs), evs)
	}
	for _, ev := range evs {
		if want := uint64(0x0f02); ev.Config != want || ev.Config1 != 0 || ev.Config2 != 0 {
			t.Errorf("%s config %#x, want %#x", ev.PMU, ev.Config, want)
		}
		wantGroup := map[int]string{0: "upi:0>1", 4: "upi:1>0"}[ev.CPU]
		if ev.Group != wantGroup || ev.BytesPerCount != 5 {
			t.Errorf("cpu%d: group %q bytes %v, want %q 5", ev.CPU, ev.Group, ev.BytesPerCount, wantGroup)
		}
	}
}

func TestDiscoverUPIEventsWithoutTopology(t *testing.T) {
	// Two packages: every link goes to the other one.
	root := fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	writeTree(t, root, upiPMU("uncore_upi_0", "30", "0,4", nil))
	evs, _, err := discoverUPIEvents(root, 5)
	if err != nil || len(evs) != 2 {
		t.Fatalf("%+v %v", evs, err)
	}
	// More packages: which link reaches which cannot be told.
	root = t.TempDir()
	files := upiPMU("uncore_upi_0", "30", "0,2,4", nil)
	for c := 0; c < 6; c++ {
		files[fmt.Sprintf("devices/system/cpu/cpu%d/topology/physical_package_id", c)] = fmt.Sprint(c / 2)
	}
	writeTree(t, root, files)
	if _, _, err := discoverUPIEvents(root, 5); !errors.Is(err, errUncoreUnsupported) || !strings.Contains(err.Error(), "3 packages") {
		t.Fatalf("a multi-package host without link topology must stay unknown: %v", err)
	}
	// Not an Intel multi-socket server.
	if _, _, err := discoverUPIEvents(fakeHost(t, map[int]string{0: "0-7"}), 5); !errors.Is(err, errUncoreUnsupported) {
		t.Fatalf("no UPI PMU: %v", err)
	}
}

func TestUncoreUPIUtilizationIsTheBusierDirection(t *testing.T) {
	root, cpuinfo := fakeUPIHost(t)
	const capBPS = 1_000_000_000_000 // 1 Tb/s per direction
	// Memory capacities assumed: only the inter-socket link is read.
	g := upiGraph(0, capBPS, false)
	table := map[string]perfReading{}
	fds := &fakeFDs{}
	u, err := newUncore(g, root, cpuinfo, UncoreOptions{}, fds.opener(table, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if got := strings.Join(u.Links(), ","); got != "upi.0-1" {
		t.Fatalf("links %q", got)
	}
	// 3 links × 2 dies, every one kept; no IMC counter is opened for the
	// links left unknown.
	if len(fds.all) != 6 || len(u.ctrs) != 6 {
		t.Fatalf("%d counters opened, %d kept, want 6 and 6", len(fds.all), len(u.ctrs))
	}
	fds.check(t, u)
	if !strings.Contains(u.Skipped["mem.0"], "--mem-channel-capacity") {
		t.Errorf("mem.0 skip reason %q", u.Skipped["mem.0"])
	}
	now := time.Unix(1000, 0)
	u.now = func() time.Time { return now }
	u.Poll(false)

	// Package 0 → 1: each of its 3 links sends 1e9 counts in 1 s, at 64/12
	// B per count: 3e9 × 64/12 B × 8 = 128 Gb/s. The reverse direction
	// carries half of that.
	now = now.Add(time.Second)
	advanceGroup(table, "upi:0>1", 1_000_000_000, time.Second)
	advanceGroup(table, "upi:1>0", 500_000_000, time.Second)
	u.Poll(false)
	if got, want := u.Util()["upi.0-1"], 128e9/capBPS; math.Abs(got-want) > 1e-9 {
		t.Errorf("upi.0-1 = %v, want %v", got, want)
	}

	// One direction unreadable: the link is unknown, not the other side's
	// share.
	now = now.Add(time.Second)
	advanceGroup(table, "upi:0>1", 1, time.Second)
	for k, r := range table {
		if strings.HasSuffix(k, "/upi:1>0") {
			r.ok = false
			table[k] = r
			break
		}
	}
	u.Poll(false)
	if v, ok := u.Util()["upi.0-1"]; ok {
		t.Errorf("a direction with an unreadable link must leave the link unknown, got %v", v)
	}
}

func TestUncoreUPINeedsCapacityAndMeasuredModel(t *testing.T) {
	root, cpuinfo := fakeUPIHost(t)
	fds := &fakeFDs{}
	_, err := newUncore(upiGraph(0, 300_000_000_000, true), root, cpuinfo, UncoreOptions{}, fds.opener(map[string]perfReading{}, nil))
	if err == nil || !strings.Contains(err.Error(), "--upi-capacity") {
		t.Fatalf("an assumed capacity must leave the link unknown: %v", err)
	}
	writeTree(t, root, map[string]string{"cpuinfo": "vendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 173"})
	_, err = newUncore(upiGraph(0, 1e12, false), root, cpuinfo, UncoreOptions{}, fds.opener(map[string]perfReading{}, nil))
	if err == nil || !strings.Contains(err.Error(), "--upi-bytes-per-flit") {
		t.Fatalf("an unmeasured model must leave the link unknown: %v", err)
	}
	// Neither attempt had a link to read, so neither opened a counter.
	if len(fds.all) != 0 {
		t.Fatalf("%d counters opened with no link to read", len(fds.all))
	}
	u, err := newUncore(upiGraph(0, 1e12, false), root, cpuinfo, UncoreOptions{UPIBytesPerFlit: 6}, fds.opener(map[string]perfReading{}, nil))
	if err != nil {
		t.Fatalf("the operator's ratio makes it readable: %v", err)
	}
	defer u.Close()
	for _, c := range u.ctrs {
		if c.ev.BytesPerCount != 6 {
			t.Fatalf("%+v", c.ev)
		}
	}
	if len(fds.all) != 6 || len(u.ctrs) != 6 {
		t.Fatalf("%d counters opened, %d kept, want 6 and 6", len(fds.all), len(u.ctrs))
	}
	fds.check(t, u)
}

func TestUncoreReadsMemoryAndUPITogether(t *testing.T) {
	root, cpuinfo := fakeUPIHost(t)
	fds := &fakeFDs{}
	u, err := newUncore(upiGraph(1e12, 1e12, false), root, cpuinfo, UncoreOptions{}, fds.opener(map[string]perfReading{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(u.Links(), ","); got != "mem.0,mem.1,upi.0-1" {
		t.Fatalf("links %q", got)
	}
	// 8 IMC and 6 UPI counters, every one kept.
	if len(fds.all) != 14 || len(u.ctrs) != 14 {
		t.Fatalf("%d counters opened, %d kept, want 14 and 14", len(fds.all), len(u.ctrs))
	}
	fds.check(t, u)
	u.Close()
	fds.check(t, nil)

	// The UPI link opening fails: memory is still read.
	fds = &fakeFDs{}
	u, err = newUncore(upiGraph(1e12, 1e12, false), root, cpuinfo, UncoreOptions{}, fds.opener(map[string]perfReading{}, func(ev uncoreEvent) error {
		if strings.HasPrefix(ev.PMU, "uncore_upi") {
			return errors.New("perf_event_open denied")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(u.Links(), ","); got != "mem.0,mem.1" || !strings.Contains(u.Skipped["upi.0-1"], "denied") {
		t.Fatalf("links %q, upi skip %q", got, u.Skipped["upi.0-1"])
	}
	for _, c := range u.ctrs {
		if strings.HasPrefix(c.ev.PMU, "uncore_upi") {
			t.Fatalf("counter of a dropped link left open: %+v", c.ev)
		}
	}
	// Each direction's first open was denied, so no UPI counter was ever
	// handed out; the 8 IMC counters are kept.
	if len(fds.all) != 8 || len(u.ctrs) != 8 {
		t.Fatalf("%d counters opened, %d kept, want 8 and 8", len(fds.all), len(u.ctrs))
	}
	fds.check(t, u)
	u.Close()
	fds.check(t, nil)

	// Only the 1 → 0 direction fails: 0 → 1 opened whole, but a link is read
	// in both directions or not at all, so its counters are released as
	// unused.
	fds = &fakeFDs{}
	u, err = newUncore(upiGraph(1e12, 1e12, false), root, cpuinfo, UncoreOptions{}, fds.opener(map[string]perfReading{}, func(ev uncoreEvent) error {
		if ev.Group == "upi:1>0" {
			return errors.New("perf_event_open denied")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(u.Links(), ","); got != "mem.0,mem.1" || !strings.Contains(u.Skipped["upi.0-1"], "denied") {
		t.Fatalf("links %q, upi skip %q", got, u.Skipped["upi.0-1"])
	}
	if len(fds.all) != 11 || len(u.ctrs) != 8 {
		t.Fatalf("%d counters opened, %d kept, want 11 (8 IMC + 3 UPI 0 → 1) and 8", len(fds.all), len(u.ctrs))
	}
	if n := fds.closed(t, func(ev uncoreEvent) bool { return ev.Group == "upi:0>1" }); n != 3 {
		t.Errorf("%d counters of the unused direction closed, want 3", n)
	}
	fds.check(t, u)
	u.Close()
	fds.check(t, nil)
}

func TestUncoreNothingUsableClosesEveryCounter(t *testing.T) {
	root, cpuinfo := fakeUPIHost(t)
	// Memory capacities assumed, so only the inter-socket link could be
	// read. Events come link by link, each on die 0 (0 → 1) then die 1
	// (1 → 0). uncore_upi_0's two open; uncore_upi_1 on die 0 is denied,
	// which closes uncore_upi_0's 0 → 1 counter and drops that direction;
	// uncore_upi_1 and uncore_upi_2 on die 1 open and complete 1 → 0. The
	// link needs both directions and is skipped, so no link is readable:
	// the 1 → 0 counters are released as unused and the start fails.
	fds := &fakeFDs{}
	u, err := newUncore(upiGraph(0, 1e12, false), root, cpuinfo, UncoreOptions{}, fds.opener(map[string]perfReading{}, func(ev uncoreEvent) error {
		if ev.PMU == "uncore_upi_1" && ev.Group == "upi:0>1" {
			return errors.New("perf_event_open denied")
		}
		return nil
	}))
	if err == nil || !strings.Contains(err.Error(), "upi.0-1: uncore_upi_1/txl_flits_all_data on cpu0: perf_event_open denied") {
		t.Fatalf("nothing readable must fail naming the denied counter: %v (%v)", err, u)
	}
	if len(fds.all) != 4 {
		t.Fatalf("%d counters opened, want 4 (uncore_upi_0 0 → 1; all three 1 → 0)", len(fds.all))
	}
	if n := fds.closed(t, func(ev uncoreEvent) bool { return ev.Group == "upi:0>1" }); n != 1 {
		t.Errorf("%d counters of the failed direction closed, want 1", n)
	}
	if n := fds.closed(t, func(ev uncoreEvent) bool { return ev.Group == "upi:1>0" }); n != 3 {
		t.Errorf("%d counters of the unused direction closed, want 3", n)
	}
	fds.check(t, nil)
}

func TestUPILinkGroupsUnderSubNUMAClustering(t *testing.T) {
	// Package 0 holds nodes 0 and 1, package 1 nodes 2 and 3: sockets are
	// packages and the link is upi.0-1.
	root := fakeHost(t, map[int]string{0: "0-1", 1: "2-3", 2: "4-5", 3: "6-7"})
	g := topology.Graph{
		Endpoints: []model.Endpoint{
			{ID: "mem0", Kind: model.EndpointMemory, Index: 0, Socket: 0, NUMA: 0},
			{ID: "mem3", Kind: model.EndpointMemory, Index: 3, Socket: 1, NUMA: 3},
		},
		Links: []model.Link{{ID: "upi.0-1", Kind: model.LinkKindUPI, CapacityBPS: 1e12}},
	}
	links, skipped := upiLinkGroups(g, root)
	if got := fmt.Sprint(links["upi.0-1"]); got != "[upi:0>1 upi:1>0]" || len(skipped) != 0 {
		t.Fatalf("groups %v skipped %v", links, skipped)
	}
	// Without SNC, sockets are NUMA node numbers: node 1 on package 1.
	root = fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	links, _ = upiLinkGroups(upiGraph(0, 1e12, false), root)
	if got := fmt.Sprint(links["upi.0-1"]); got != "[upi:0>1 upi:1>0]" {
		t.Fatalf("groups %v", links)
	}
}

func TestUPILinkWithoutPeerOnMorePackages(t *testing.T) {
	// Three packages in a ring; package 2's link 1 names no peer, so what
	// package 2 sends cannot be attributed, while 0 → 1 and 1 → 0 can.
	root := t.TempDir()
	files := map[string]string{"cpuinfo": sprCPUInfo}
	for k, v := range upiPMU("uncore_upi_0", "30", "0,2,4", map[int]string{0: "upi_0,die_1", 1: "upi_0,die_0", 2: "upi_1,die_0"}) {
		files[k] = v
	}
	for k, v := range upiPMU("uncore_upi_1", "31", "0,2,4", map[int]string{0: "upi_0,die_2", 1: "upi_1,die_2"}) {
		files[k] = v
	}
	for c := 0; c < 6; c++ {
		files[fmt.Sprintf("devices/system/cpu/cpu%d/topology/physical_package_id", c)] = fmt.Sprint(c / 2)
	}
	for n := 0; n < 3; n++ {
		files[fmt.Sprintf("devices/system/node/node%d/cpulist", n)] = fmt.Sprintf("%d-%d", 2*n, 2*n+1)
	}
	writeTree(t, root, files)
	_, unresolved, err := discoverUPIEvents(root, 5)
	if err != nil || fmt.Sprint(unresolved) != "map[2:true]" {
		t.Fatalf("unresolved %v err %v", unresolved, err)
	}
	g := topology.Graph{}
	for n := 0; n < 3; n++ {
		g.Endpoints = append(g.Endpoints, model.Endpoint{ID: fmt.Sprintf("mem%d", n), Kind: model.EndpointMemory, Index: n, Socket: n, NUMA: n})
	}
	for _, id := range []string{"upi.0-1", "upi.0-2", "upi.1-2"} {
		g.Links = append(g.Links, model.Link{ID: id, Kind: model.LinkKindUPI, CapacityBPS: 1e12})
	}
	fds := &fakeFDs{}
	u, err := newUncore(g, root, filepath.Join(root, "cpuinfo"), UncoreOptions{}, fds.opener(map[string]perfReading{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if got := strings.Join(u.Links(), ","); got != "upi.0-1" {
		t.Fatalf("links %q, want only the pair whose both directions are attributed", got)
	}
	for _, id := range []string{"upi.0-2", "upi.1-2"} {
		if !strings.Contains(u.Skipped[id], "package 2 names no peer") {
			t.Errorf("%s skip reason %q", id, u.Skipped[id])
		}
	}
	// Only the wanted directions' counters (0 → 1 and 1 → 0, one link each)
	// are opened; those of the skipped links are never tried.
	if len(fds.all) != 2 || len(u.ctrs) != 2 {
		t.Fatalf("%d counters opened, %d kept, want 2 and 2", len(fds.all), len(u.ctrs))
	}
	fds.check(t, u)
}

package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// casScale is the kernel's cas_count scale: 64 B / 1 MiB.
const casScale = "6.103515625e-5"

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, v := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// imcPMU is the sysfs of one uncore_imc_<n> PMU in the kernel's layout.
func imcPMU(name, typ, cpumask string) map[string]string {
	d := "bus/event_source/devices/" + name + "/"
	return map[string]string{
		d + "type":                         typ,
		d + "cpumask":                      cpumask,
		d + "format/event":                 "config:0-7",
		d + "format/umask":                 "config:8-15",
		d + "events/clockticks":            "event=0xff,umask=0x00",
		d + "events/cas_count_read":        "event=0x04,umask=0x03",
		d + "events/cas_count_read.scale":  casScale,
		d + "events/cas_count_read.unit":   "MiB",
		d + "events/cas_count_write":       "event=0x04,umask=0x0c",
		d + "events/cas_count_write.scale": casScale,
		d + "events/cas_count_write.unit":  "MiB",
	}
}

// fakeHost is a two-package host, CPUs 0-3 on package 0 and 4-7 on package
// 1, with nodes as given (node → cpulist) and two IMC PMUs.
func fakeHost(t *testing.T, nodes map[int]string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		// Counts the same traffic again; must be ignored.
		"bus/event_source/devices/uncore_imc_free_running_0/type": "20",
		"bus/event_source/devices/cpu/type":                       "4",
	}
	for k, v := range imcPMU("uncore_imc_0", "13", "0,4") {
		files[k] = v
	}
	for k, v := range imcPMU("uncore_imc_1", "14", "0,4") {
		files[k] = v
	}
	for c := 0; c < 8; c++ {
		files[fmt.Sprintf("devices/system/cpu/cpu%d/topology/physical_package_id", c)] = fmt.Sprint(c / 4)
	}
	for n, cpus := range nodes {
		files[fmt.Sprintf("devices/system/node/node%d/cpulist", n)] = cpus
	}
	writeTree(t, root, files)
	return root
}

func memGraph(capBPS uint64, assumed bool) topology.Graph {
	return topology.Graph{
		Name: "t",
		Endpoints: []model.Endpoint{
			{ID: "mem0", Kind: model.EndpointMemory, Index: 0, Socket: 0, NUMA: 0},
			{ID: "mem1", Kind: model.EndpointMemory, Index: 1, Socket: 1, NUMA: 1},
		},
		Links: []model.Link{
			{ID: "mem.0", Kind: "memory", Device: "mem0", Socket: 0, CapacityBPS: capBPS, CapacityAssumed: assumed},
			{ID: "mem.1", Kind: "memory", Device: "mem1", Socket: 1, CapacityBPS: capBPS, CapacityAssumed: assumed},
		},
	}
}

func TestDiscoverIMCEvents(t *testing.T) {
	root := fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	evs, err := discoverIMCEvents(root, false)
	if err != nil {
		t.Fatal(err)
	}
	// 2 PMUs × 2 dies × {read, write}; clockticks and free-running left out.
	if len(evs) != 8 {
		t.Fatalf("got %d events, want 8: %+v", len(evs), evs)
	}
	for _, ev := range evs {
		if ev.PMU == "uncore_imc_free_running_0" || ev.Name == "clockticks" {
			t.Fatalf("unexpected event %+v", ev)
		}
		want := map[string]uint64{"cas_count_read": 0x0304, "cas_count_write": 0x0c04}[ev.Name]
		if ev.Config != want || ev.Config1 != 0 || ev.Config2 != 0 {
			t.Errorf("%s/%s config %#x, want %#x", ev.PMU, ev.Name, ev.Config, want)
		}
		if math.Abs(ev.BytesPerCount-64) > 1e-9 {
			t.Errorf("%s bytes/count %v, want 64 (scale × MiB)", ev.Name, ev.BytesPerCount)
		}
		wantDie := map[int]string{0: "0/0", 4: "1/0"}[ev.CPU]
		if ev.Die != wantDie {
			t.Errorf("cpu%d die %q, want %q", ev.CPU, ev.Die, wantDie)
		}
		if wantType := map[string]uint32{"uncore_imc_0": 13, "uncore_imc_1": 14}[ev.PMU]; ev.Type != wantType {
			t.Errorf("%s type %d", ev.PMU, ev.Type)
		}
	}
}

func TestDiscoverIMCSubChannelEvents(t *testing.T) {
	root := t.TempDir()
	d := "bus/event_source/devices/uncore_imc_0/"
	writeTree(t, root, map[string]string{
		d + "type":                             "13",
		d + "cpumask":                          "0",
		d + "format/event":                     "config:0-7",
		d + "format/umask":                     "config:8-15",
		d + "events/cas_count_read_sch0":       "event=0x05,umask=0xcf",
		d + "events/cas_count_read_sch0.scale": casScale,
		d + "events/cas_count_read_sch0.unit":  "MiB",
		d + "events/cas_count_read_sch1":       "event=0x06,umask=0xcf",
		d + "events/cas_count_read_sch1.scale": casScale,
		d + "events/cas_count_read_sch1.unit":  "MiB",
		// No scale or unit: one CAS is one 64-byte line.
		d + "events/cas_count_write_sch0":                      "event=0x05,umask=0xf0",
		"devices/system/cpu/cpu0/topology/physical_package_id": "0",
		"devices/system/cpu/cpu0/topology/die_id":              "1",
	})
	evs, err := discoverIMCEvents(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("every sub-channel event is summed, got %+v", evs)
	}
	for _, ev := range evs {
		if ev.Die != "0/1" || math.Abs(ev.BytesPerCount-64) > 1e-9 {
			t.Errorf("%+v", ev)
		}
	}
}

func TestDiscoverIMCNeedsReadAndWrite(t *testing.T) {
	root := t.TempDir()
	d := "bus/event_source/devices/uncore_imc_0/"
	writeTree(t, root, map[string]string{
		d + "type":                  "13",
		d + "cpumask":               "0",
		d + "format/event":          "config:0-7",
		d + "events/cas_count_read": "event=0x04",
		"devices/system/cpu/cpu0/topology/physical_package_id": "0",
	})
	if _, err := discoverIMCEvents(root, false); !errors.Is(err, errUncoreUnsupported) || !strings.Contains(err.Error(), "cas_count_write") {
		t.Fatalf("reads alone must not pass for a channel's traffic: %v", err)
	}
}

func TestDiscoverIMCUnsupportedPlatforms(t *testing.T) {
	amd := t.TempDir()
	writeTree(t, amd, map[string]string{"bus/event_source/devices/amd_df/type": "11"})
	if _, err := discoverIMCEvents(amd, false); !errors.Is(err, errUncoreUnsupported) || !strings.Contains(err.Error(), "AMD") {
		t.Fatalf("AMD must be reported as unsupported: %v", err)
	}
	none := t.TempDir()
	writeTree(t, none, map[string]string{"bus/event_source/devices/cpu/type": "4"})
	if _, err := discoverIMCEvents(none, false); !errors.Is(err, errUncoreUnsupported) {
		t.Fatalf("no IMC PMU must be unsupported: %v", err)
	}
}

func TestEncodeEvent(t *testing.T) {
	format := map[string]formatField{}
	for name, s := range map[string]string{
		"event": "config:0-7,32-35",
		"umask": "config:8-15",
		"edge":  "config:18",
		"ch":    "config1:0-3",
	} {
		f, err := parseFormat(s)
		if err != nil {
			t.Fatal(err)
		}
		format[name] = f
	}
	c0, c1, c2, err := encodeEvent("event=0x1ff,umask=3,edge,ch=0xa", format)
	if err != nil {
		t.Fatal(err)
	}
	// 0x1ff splits: 0xff in bits 0-7, 0x1 in bits 32-35.
	if want := uint64(0xff) | 3<<8 | 1<<18 | 1<<32; c0 != want || c1 != 0xa || c2 != 0 {
		t.Fatalf("config %#x/%#x/%#x, want %#x/0xa/0", c0, c1, c2, want)
	}
	if _, _, _, err := encodeEvent("umask=0x100", format); err == nil {
		t.Error("a value wider than its field must fail")
	}
	if _, _, _, err := encodeEvent("thresh=1", format); err == nil {
		t.Error("a term without a format field must fail")
	}
	for _, bad := range []string{"config", "config3:0-7", "config:7-0", "config:0-64"} {
		if _, err := parseFormat(bad); err == nil {
			t.Errorf("parseFormat(%q) must fail", bad)
		}
	}
}

func TestEventBytes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.scale": casScale, "a.unit": "MiB",
		"b.scale": "1", "b.unit": "flits",
		"c.scale": "2",
		"d.unit":  "B",
	})
	for name, want := range map[string]float64{"a": 64, "d": 1, "e": 64} {
		got, err := eventBytes(filepath.Join(root, name))
		if err != nil || math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: %v %v, want %v", name, got, err, want)
		}
	}
	for _, name := range []string{"b", "c"} {
		if _, err := eventBytes(filepath.Join(root, name)); err == nil {
			t.Errorf("%s: a count that is not bytes must be rejected", name)
		}
	}
}

// fakeCounter reads from a shared table the test advances, and counts how
// often it was closed: a real counter is a perf fd, closed exactly once.
type fakeCounter struct {
	ev     uncoreEvent
	key    string
	table  map[string]perfReading
	closes int
}

func (f *fakeCounter) Read() (uint64, uint64, uint64, error) {
	r := f.table[f.key]
	if !r.ok {
		return 0, 0, 0, errors.New("read failed")
	}
	return r.count, r.enabled, r.running, nil
}

func (f *fakeCounter) Close() error {
	f.closes++
	return nil
}

func (f *fakeCounter) String() string {
	return fmt.Sprintf("%s/%s on cpu%d (%s)", f.ev.PMU, f.ev.Name, f.ev.CPU, f.ev.Group)
}

func counterKey(ev uncoreEvent) string { return ev.PMU + "/" + ev.Name + "/" + ev.Group }

// fakeFDs records every counter its opener handed out, in order, so a test
// can check which ones newUncore (or mapSNCBoxes) closed. An open the fail
// hook refuses hands out nothing, as perf_event_open returns no fd.
type fakeFDs struct {
	all []*fakeCounter
}

func (r *fakeFDs) opener(table map[string]perfReading, fail func(uncoreEvent) error) perfOpener {
	return func(ev uncoreEvent) (perfCounter, error) {
		if fail != nil {
			if err := fail(ev); err != nil {
				return nil, err
			}
		}
		k := counterKey(ev)
		if _, ok := table[k]; !ok {
			table[k] = perfReading{ok: true}
		}
		c := &fakeCounter{ev: ev, key: k, table: table}
		r.all = append(r.all, c)
		return c, nil
	}
}

// check fails the test unless every counter handed out that u does not hold
// was closed exactly once, and every counter u holds is still open. With u
// nil (newUncore failed, or u was closed) every counter must be closed once.
func (r *fakeFDs) check(t *testing.T, u *Uncore) {
	t.Helper()
	mine := map[*fakeCounter]bool{}
	for _, c := range r.all {
		mine[c] = true
	}
	live := map[*fakeCounter]bool{}
	if u != nil {
		for _, c := range u.ctrs {
			fc, ok := c.c.(*fakeCounter)
			if !ok {
				t.Fatalf("live counter %T is not a fake", c.c)
			}
			if !mine[fc] {
				t.Fatalf("live counter %s was not handed out by this opener", fc)
			}
			live[fc] = true
		}
	}
	for _, c := range r.all {
		switch {
		case live[c] && c.closes != 0:
			t.Errorf("live counter %s closed %d times", c, c.closes)
		case !live[c] && c.closes != 1:
			t.Errorf("counter %s not kept, closed %d times, want 1 (fd leak or double close)", c, c.closes)
		}
	}
}

// closed is how many of the counters handed out for which ev holds were
// closed exactly once; the test fails if one of them was closed more often.
func (r *fakeFDs) closed(t *testing.T, which func(uncoreEvent) bool) int {
	t.Helper()
	n := 0
	for _, c := range r.all {
		if !which(c.ev) {
			continue
		}
		if c.closes > 1 {
			t.Errorf("counter %s closed %d times", c, c.closes)
		}
		if c.closes == 1 {
			n++
		}
	}
	return n
}

// fakeOpener is an opener whose counters' closes the test does not check.
func fakeOpener(table map[string]perfReading, fail func(uncoreEvent) error) perfOpener {
	return (&fakeFDs{}).opener(table, fail)
}

// seedTable gives the table a reading for every event, as opening them
// would, so a touch that walks the table sees them before anything is open.
func seedTable(table map[string]perfReading, evs []uncoreEvent) {
	for _, ev := range evs {
		if k := counterKey(ev); !table[k].ok {
			table[k] = perfReading{ok: true}
		}
	}
}

// advance adds counts to every counter of die over dt, fully running unless
// multiplexed.
func advance(table map[string]perfReading, die string, count uint64, dt time.Duration, multiplexed bool) {
	for k, r := range table {
		if !strings.HasSuffix(k, "/"+die) {
			continue
		}
		r.count += count
		r.enabled += uint64(dt)
		if multiplexed {
			r.running += uint64(dt) / 2
		} else {
			r.running += uint64(dt)
		}
		table[k] = r
	}
}

func TestUncoreUtilizationOverThePollInterval(t *testing.T) {
	root := fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	const capBPS = 1_000_000_000_000 // 1 Tb/s
	g := memGraph(capBPS, false)
	table := map[string]perfReading{}
	fds := &fakeFDs{}
	u, err := newUncore(g, root, "", UncoreOptions{}, fds.opener(table, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	// 2 PMUs × 2 dies × {read, write}, every one kept open.
	if len(fds.all) != 8 || len(u.ctrs) != 8 {
		t.Fatalf("%d counters opened, %d kept, want 8 and 8", len(fds.all), len(u.ctrs))
	}
	fds.check(t, u)
	now := time.Unix(1000, 0)
	u.now = func() time.Time { return now }

	u.Poll(false)
	if got := u.Util(); got != nil {
		t.Fatalf("first poll is a baseline, got %v", got)
	}

	// Die 0: each of 4 counters (2 PMUs × read/write) moves 250M lines in 2 s:
	// 1e9 × 64 B × 8 / 2 s = 256 Gb/s.
	now = now.Add(2 * time.Second)
	advance(table, "0/0", 250_000_000, 2*time.Second, false)
	// Die 1 was multiplexed: its count covers part of the interval only.
	advance(table, "1/0", 250_000_000, 2*time.Second, true)
	u.Poll(false)
	got := u.Util()
	if want := 256e9 / capBPS; math.Abs(got["mem.0"]-want) > 1e-9 {
		t.Errorf("mem.0 = %v, want %v", got["mem.0"], want)
	}
	if v, ok := got["mem.1"]; ok {
		t.Errorf("a multiplexed interval must be unknown, got %v", v)
	}

	// An idle interval is a measured 0, not unknown.
	now = now.Add(time.Second)
	advance(table, "0/0", 0, time.Second, false)
	advance(table, "1/0", 0, time.Second, false)
	u.Poll(false)
	if v, ok := u.Util()["mem.1"]; !ok || v != 0 {
		t.Errorf("idle mem.1 = %v ok=%v, want a measured 0", v, ok)
	}

	// The poll after a run only re-baselines: its interval holds the probes.
	now = now.Add(time.Second)
	advance(table, "0/0", 1<<30, time.Second, false)
	u.Poll(true)
	if got := u.Util(); got != nil {
		t.Fatalf("reset poll must yield nothing, got %v", got)
	}
	now = now.Add(time.Second)
	advance(table, "0/0", 0, time.Second, false)
	u.Poll(false)
	if v := u.Util()["mem.0"]; v != 0 {
		t.Errorf("run traffic leaked into the next interval: %v", v)
	}

	// A failed read makes its die unknown.
	now = now.Add(time.Second)
	for k, r := range table {
		if strings.HasSuffix(k, "/0/0") && strings.HasPrefix(k, "uncore_imc_1/cas_count_write") {
			r.ok = false
			table[k] = r
		}
	}
	u.Poll(false)
	if _, ok := u.Util()["mem.0"]; ok {
		t.Error("a die with an unreadable counter must be unknown")
	}
}

func TestUncoreCloseReleasesEveryCounter(t *testing.T) {
	root := fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	fds := &fakeFDs{}
	u, err := newUncore(memGraph(1e12, false), root, "", UncoreOptions{}, fds.opener(map[string]perfReading{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(fds.all) != 8 {
		t.Fatalf("%d counters opened, want 8", len(fds.all))
	}
	fds.check(t, u)
	u.Close()
	// Every counter closed exactly once, and none is kept.
	if len(u.ctrs) != 0 {
		t.Fatalf("%d counters still held after Close", len(u.ctrs))
	}
	fds.check(t, nil)
	// Close drops the counters it released, so a second Close (the engine
	// shutting down twice, or Close after a failed start) closes nothing
	// again.
	u.Close()
	fds.check(t, nil)
	// Polling a closed reader reads nothing rather than a closed fd.
	u.Poll(false)
	if got := u.Util(); got != nil {
		t.Fatalf("closed reader yielded %v", got)
	}
}

func TestUncoreNeedsCapacity(t *testing.T) {
	root := fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	opened := 0
	_, err := newUncore(memGraph(400_000_000_000, true), root, "", UncoreOptions{}, func(uncoreEvent) (perfCounter, error) {
		opened++
		return nil, errors.New("unreachable")
	})
	if err == nil || !strings.Contains(err.Error(), "--mem-channel-capacity") {
		t.Fatalf("assumed capacities must leave memory unknown: %v", err)
	}
	if opened != 0 {
		t.Fatalf("opened %d counters with nothing to divide by", opened)
	}
}

func TestUncoreSubNUMAClusteringStaysUnknown(t *testing.T) {
	// Package 0 split into two nodes; package 1 whole. Touching a node moves
	// no counter, so which box serves which node cannot be told.
	stubTouch(t, func(int) error { return nil })
	root := fakeHost(t, map[int]string{0: "0-1", 1: "2-3", 2: "4-7"})
	g := memGraph(1e12, false)
	g.Endpoints = append(g.Endpoints, model.Endpoint{ID: "mem2", Kind: model.EndpointMemory, Index: 2, Socket: 2, NUMA: 2})
	g.Links = append(g.Links, model.Link{ID: "mem.2", Kind: "memory", Device: "mem2", Socket: 2, CapacityBPS: 1e12})
	fds := &fakeFDs{}
	u, err := newUncore(g, root, "", UncoreOptions{}, fds.opener(map[string]perfReading{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(u.Links(), ","); got != "mem.2" {
		t.Fatalf("links %q, want only the unsplit package's node", got)
	}
	for _, id := range []string{"mem.0", "mem.1"} {
		if !strings.Contains(u.Skipped[id], "sub-NUMA") {
			t.Errorf("%s skip reason %q", id, u.Skipped[id])
		}
	}
	// The measurement opened die 0/0's 4 counters and closed them; die 0/0 is
	// then not wanted, so only die 1/0's 4 are opened for reading and kept.
	if len(fds.all) != 8 || len(u.ctrs) != 4 {
		t.Fatalf("%d counters opened, %d kept, want 8 and 4", len(fds.all), len(u.ctrs))
	}
	if n := fds.closed(t, func(ev uncoreEvent) bool { return ev.Die == "0/0" }); n != 4 {
		t.Errorf("%d of the SNC measurement's counters closed, want 4", n)
	}
	fds.check(t, u)
	u.Close()
	fds.check(t, nil)
}

func TestUncoreOpenFailure(t *testing.T) {
	root := fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	denied := errors.New("perf_event_open denied")
	all := &fakeFDs{}
	if _, err := newUncore(memGraph(1e12, false), root, "", UncoreOptions{}, all.opener(map[string]perfReading{}, func(uncoreEvent) error { return denied })); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied access must be reported: %v", err)
	}
	// The first counter of each die fails, so the die's other counters are
	// never tried and nothing is left to close.
	if len(all.all) != 0 {
		t.Fatalf("%d counters opened although every open was denied", len(all.all))
	}
	// One die failing drops that die only, never part of its boxes.
	fds := &fakeFDs{}
	u, err := newUncore(memGraph(1e12, false), root, "", UncoreOptions{}, fds.opener(map[string]perfReading{}, func(ev uncoreEvent) error {
		if ev.Die == "0/0" && ev.PMU == "uncore_imc_1" {
			return denied
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(u.Links(), ","); got != "mem.1" {
		t.Fatalf("links %q, want mem.1", got)
	}
	for _, c := range u.ctrs {
		if c.ev.Die == "0/0" {
			t.Fatalf("counter of a dropped die left open: %+v", c.ev)
		}
	}
	// Events come PMU by PMU: uncore_imc_0's read and write on die 0/0 were
	// open when uncore_imc_1's read on it was denied, and must have been
	// closed then; uncore_imc_1's write on die 0/0 is never tried. Die 1/0's
	// 4 counters are kept.
	if len(fds.all) != 6 || len(u.ctrs) != 4 {
		t.Fatalf("%d counters opened, %d kept, want 6 and 4", len(fds.all), len(u.ctrs))
	}
	if n := fds.closed(t, func(ev uncoreEvent) bool { return ev.Die == "0/0" }); n != 2 {
		t.Errorf("%d counters of the dropped die closed, want 2 (uncore_imc_0 read and write)", n)
	}
	fds.check(t, u)
	u.Close()
	fds.check(t, nil)
}

func TestSysfsSamplerBusMergesUncore(t *testing.T) {
	root := fakeHost(t, map[int]string{0: "0-3", 1: "4-7"})
	g := memGraph(1e12, false)
	table := map[string]perfReading{}
	u, err := newUncore(g, root, "", UncoreOptions{}, fakeOpener(table, nil))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(0, 0)
	u.now = func() time.Time { return now }
	s := SysfsSampler{Graph: g, Uncore: u}
	s.PollBus(context.Background(), false)
	if got := s.Bus(context.Background()); got != nil {
		t.Fatalf("no interval yet: %v", got)
	}
	now = now.Add(time.Second)
	advance(table, "0/0", 1000, time.Second, false)
	advance(table, "1/0", 1000, time.Second, false)
	s.PollBus(context.Background(), false)
	got := s.Bus(context.Background())
	if len(got) != 2 || got["mem.0"] <= 0 {
		t.Fatalf("bus %v", got)
	}
	if got := (SysfsSampler{Graph: g}).Bus(context.Background()); got != nil {
		t.Fatalf("without a reader memory must be unknown, got %v", got)
	}
}

func TestParseCPUList(t *testing.T) {
	got, err := parseCPUList("0-2,8,10-11")
	if err != nil || fmt.Sprint(got) != "[0 1 2 8 10 11]" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := parseCPUList("3-1"); err == nil {
		t.Error("descending range must fail")
	}
}

// gnrIMCPMU is a Granite Rapids IMC PMU as some kernels publish it: only
// cas_count_read / cas_count_write, both sub-channel 0 (event 0x05).
func gnrIMCPMU(name, typ, cpumask string) map[string]string {
	d := "bus/event_source/devices/" + name + "/"
	return map[string]string{
		d + "type":                         typ,
		d + "cpumask":                      cpumask,
		d + "format/event":                 "config:0-7",
		d + "format/umask":                 "config:8-15,32-55",
		d + "events/clockticks":            "event=0x01,umask=0x00",
		d + "events/cas_count_read":        "event=0x05,umask=0xcf",
		d + "events/cas_count_read.scale":  casScale,
		d + "events/cas_count_read.unit":   "MiB",
		d + "events/cas_count_write":       "event=0x05,umask=0xf0",
		d + "events/cas_count_write.scale": casScale,
		d + "events/cas_count_write.unit":  "MiB",
	}
}

func TestDiscoverIMCAddsGraniteRapidsSubChannel1(t *testing.T) {
	root := t.TempDir()
	files := gnrIMCPMU("uncore_imc_0", "13", "0")
	files["devices/system/cpu/cpu0/topology/physical_package_id"] = "0"
	writeTree(t, root, files)

	evs, err := discoverIMCEvents(root, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{}
	for _, ev := range evs {
		got[ev.Name] = ev.Config
		if math.Abs(ev.BytesPerCount-64) > 1e-9 || ev.Group != "0/0" {
			t.Errorf("%+v", ev)
		}
	}
	want := map[string]uint64{
		"cas_count_read": 0xcf05, "cas_count_read_sch1": 0xcf06,
		"cas_count_write": 0xf005, "cas_count_write_sch1": 0xf006,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events %#v, want %#v", got, want)
	}

	// Other models read what the kernel publishes.
	if evs, err := discoverIMCEvents(root, false); err != nil || len(evs) != 2 {
		t.Fatalf("non-Granite Rapids: %+v %v", evs, err)
	}
}

func TestAddSubChannel1LeavesNamedOrOtherEventsAlone(t *testing.T) {
	named := map[string]string{
		"cas_count_read_sch0": "event=0x05,umask=0xcf", "cas_count_read_sch1": "event=0x06,umask=0xcf",
		"cas_count_write_sch0": "event=0x05,umask=0xf0", "cas_count_write_sch1": "event=0x06,umask=0xf0",
	}
	if got := addSubChannel1(named); len(got) != 0 {
		t.Errorf("a kernel naming the sub-channels must be left as is: %v", got)
	}
	other := map[string]string{"cas_count_read": "event=0x04,umask=0x03", "cas_count_write": "event=0x04,umask=0x0c"}
	if got := addSubChannel1(other); len(got) != 0 {
		t.Errorf("a CAS event that is not sub-channel 0 has no twin: %v", got)
	}
	if got := addSubChannel1(map[string]string{"cas_count_read": "umask=0xcf,event=5"}); got["cas_count_read_sch1"] != "umask=0xcf,event=0x06" {
		t.Errorf("got %v", got)
	}
}

func TestUncoreSumsGraniteRapidsSubChannels(t *testing.T) {
	root := t.TempDir()
	files := gnrIMCPMU("uncore_imc_0", "13", "0,4")
	for c := 0; c < 8; c++ {
		files[fmt.Sprintf("devices/system/cpu/cpu%d/topology/physical_package_id", c)] = fmt.Sprint(c / 4)
	}
	files["devices/system/node/node0/cpulist"] = "0-3"
	files["devices/system/node/node1/cpulist"] = "4-7"
	files["cpuinfo"] = "vendor_id\t: GenuineIntel\ncpu family\t: 6\nmodel\t\t: 173"
	writeTree(t, root, files)
	const capBPS = 1_000_000_000_000
	table := map[string]perfReading{}
	fds := &fakeFDs{}
	u, err := newUncore(memGraph(capBPS, false), root, filepath.Join(root, "cpuinfo"), UncoreOptions{}, fds.opener(table, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	// 1 PMU × 2 dies × {read, write} × 2 sub-channels, every one kept.
	if len(fds.all) != 8 || len(u.ctrs) != 8 {
		t.Fatalf("%d counters opened, %d kept, want 8 and 8", len(fds.all), len(u.ctrs))
	}
	fds.check(t, u)
	now := time.Unix(0, 0)
	u.now = func() time.Time { return now }
	u.Poll(false)
	// Each of die 0's four counters (two sub-channels × read/write) moves
	// 250M lines in 1 s: 1e9 × 64 B × 8 = 512 Gb/s.
	now = now.Add(time.Second)
	advance(table, "0/0", 250_000_000, time.Second, false)
	advance(table, "1/0", 0, time.Second, false)
	u.Poll(false)
	if got, want := u.Util()["mem.0"], 512e9/capBPS; math.Abs(got-want) > 1e-9 {
		t.Fatalf("mem.0 = %v, want %v (both sub-channels)", got, want)
	}
}

// physical_package_id is read signed: a negative value, like a missing or
// blank file, is "no package", and the die of such a CPU is an error.
func TestCPUDieRejectsNegativeOrMissingPackage(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"devices/system/cpu/cpu0/topology/physical_package_id": "1\n",
		"devices/system/cpu/cpu0/topology/die_id":              "2\n",
		"devices/system/cpu/cpu1/topology/physical_package_id": "-1\n",
		"devices/system/cpu/cpu2/topology/physical_package_id": "\n",
		"devices/system/cpu/cpu3/topology/physical_package_id": "0\n",
	})
	if die, err := cpuDie(root, 0); err != nil || die != "1/2" {
		t.Fatalf("cpu0 = %q, %v; want 1/2", die, err)
	}
	if die, err := cpuDie(root, 3); err != nil || die != "0/0" {
		t.Fatalf("cpu3 without die_id = %q, %v; want 0/0", die, err)
	}
	for _, c := range []int{1, 2, 4} {
		if _, err := cpuDie(root, c); err == nil {
			t.Errorf("cpu%d: want an error for a CPU without a package", c)
		}
	}
}

// nodeDies claims nothing about a node whose cpulist is missing, empty or
// malformed, and nothing about one whose CPUs span dies.
func TestNodeDiesSkipsUnusableNodes(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"devices/system/node/node0/cpulist": "0-1\n",
		"devices/system/node/node1/cpulist": "\n",
		"devices/system/node/node2/cpulist": "x\n",
		"devices/system/node/node3/cpulist": "2-3\n", // spans packages 0 and 1
		"devices/system/node/node4/cpulist": "4\n",   // cpu4 has no package
		"devices/system/node/node5/cpulist": "5\n",
	}
	for c, pkg := range map[int]string{0: "0", 1: "0", 2: "0", 3: "1", 5: "1"} {
		files[fmt.Sprintf("devices/system/cpu/cpu%d/topology/physical_package_id", c)] = pkg + "\n"
	}
	writeTree(t, root, files)
	if err := os.MkdirAll(filepath.Join(root, "devices/system/node/node6"), 0o755); err != nil {
		t.Fatal(err)
	}
	byNode, byDie := nodeDies(root)
	wantNode := map[int]string{0: "0/0", 5: "1/0"}
	wantDie := map[string][]int{"0/0": {0}, "1/0": {5}}
	if !reflect.DeepEqual(byNode, wantNode) || !reflect.DeepEqual(byDie, wantDie) {
		t.Fatalf("byNode %v byDie %v, want %v %v", byNode, byDie, wantNode, wantDie)
	}
	if byNode, _ := nodeDies(filepath.Join(root, "nowhere")); len(byNode) != 0 {
		t.Fatalf("no node directory: %v", byNode)
	}
}

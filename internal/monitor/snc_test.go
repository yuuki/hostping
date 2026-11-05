package monitor

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// stubTouch replaces touchNode for one test.
func stubTouch(t *testing.T, f func(node int) error) {
	t.Helper()
	old := touchNode
	touchNode = f
	t.Cleanup(func() { touchNode = old })
}

// sncHost is a two-package host whose package 0 holds NUMA nodes 0 (CPUs
// 0-1) and 1 (CPUs 2-3) and package 1 node 2 (CPUs 4-7), with four IMC boxes
// per die. On package 0 boxes 0 and 2 serve node 0 and boxes 1 and 3 node 1,
// interleaved as on real parts.
func sncHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 4; i++ {
		for k, v := range imcPMU(fmt.Sprintf("uncore_imc_%d", i), fmt.Sprint(13+i), "0,4") {
			files[k] = v
		}
	}
	for c := 0; c < 8; c++ {
		files[fmt.Sprintf("devices/system/cpu/cpu%d/topology/physical_package_id", c)] = fmt.Sprint(c / 4)
	}
	for n, cpus := range map[int]string{0: "0-1", 1: "2-3", 2: "4-7"} {
		files[fmt.Sprintf("devices/system/node/node%d/cpulist", n)] = cpus
	}
	writeTree(t, root, files)
	return root
}

var sncBoxNode = map[string]int{"uncore_imc_0": 0, "uncore_imc_1": 1, "uncore_imc_2": 0, "uncore_imc_3": 1}

func sncGraph() topology.Graph {
	g := topology.Graph{Name: "t"}
	for n := 0; n < 3; n++ {
		sock := 0
		if n == 2 {
			sock = 1
		}
		id := fmt.Sprintf("mem%d", n)
		g.Endpoints = append(g.Endpoints, model.Endpoint{ID: id, Kind: model.EndpointMemory, Index: n, Socket: sock, NUMA: n})
		g.Links = append(g.Links, model.Link{ID: fmt.Sprintf("mem.%d", n), Kind: "memory", Device: id, Socket: sock, CapacityBPS: 1e12})
	}
	return g
}

// touchMoves returns a touchNode that adds bytes of CAS traffic to the boxes
// of die 0/0 that serve the touched node, spread over their counters, plus
// noise bytes on every box of the die.
func touchMoves(table map[string]perfReading, boxNode map[string]int, bytes, noise uint64) func(int) error {
	return func(n int) error {
		var mine, all []string
		for k := range table {
			pmu, rest, _ := strings.Cut(k, "/")
			if !strings.HasSuffix(rest, "/0/0") {
				continue
			}
			all = append(all, k)
			if boxNode[pmu] == n {
				mine = append(mine, k)
			}
		}
		for _, k := range mine {
			r := table[k]
			r.count += bytes / 64 / uint64(len(mine))
			table[k] = r
		}
		for _, k := range all {
			r := table[k]
			r.count += noise / 64 / uint64(len(all))
			table[k] = r
		}
		return nil
	}
}

func TestMapSNCBoxesFindsInterleavedBoxes(t *testing.T) {
	root := sncHost(t)
	evs, err := discoverIMCEvents(root, false)
	if err != nil {
		t.Fatal(err)
	}
	table := map[string]perfReading{}
	// The table knows every counter before the touch runs.
	seedTable(table, evs)
	fds := &fakeFDs{}
	// Host traffic during the mapping lands on every box alike.
	stubTouch(t, touchMoves(table, sncBoxNode, sncTouchBytes, sncTouchBytes/10))
	got, err := mapSNCBoxes("0/0", []int{0, 1}, evs, fds.opener(table, nil))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(sncBoxNode) {
		t.Fatalf("boxes %v, want %v", got, sncBoxNode)
	}
	// The measurement opens die 0/0's counters only (4 boxes × read/write)
	// and closes every one of them.
	if len(fds.all) != 8 {
		t.Fatalf("%d counters opened, want 8", len(fds.all))
	}
	fds.check(t, nil)
}

func TestMapSNCBoxesClosesWhatItOpenedOnFailure(t *testing.T) {
	root := sncHost(t)
	evs, _ := discoverIMCEvents(root, false)
	table := map[string]perfReading{}
	seedTable(table, evs)
	stubTouch(t, touchMoves(table, sncBoxNode, sncTouchBytes, 0))
	fds := &fakeFDs{}
	// Boxes 0 and 1 open before box 2 is denied.
	_, err := mapSNCBoxes("0/0", []int{0, 1}, evs, fds.opener(table, func(ev uncoreEvent) error {
		if ev.PMU == "uncore_imc_2" {
			return errors.New("perf_event_open denied")
		}
		return nil
	}))
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("a denied box must fail the measurement: %v", err)
	}
	if len(fds.all) != 4 {
		t.Fatalf("%d counters opened, want 4 (boxes 0 and 1, read and write)", len(fds.all))
	}
	fds.check(t, nil)
}

func TestMapSNCBoxesRefusesUnclearReadings(t *testing.T) {
	root := sncHost(t)
	evs, _ := discoverIMCEvents(root, false)
	for name, touch := range map[string]func(map[string]perfReading) func(int) error{
		// Background traffic as large as the touch: no box stands out.
		"noisy": func(tb map[string]perfReading) func(int) error {
			return touchMoves(tb, sncBoxNode, sncTouchBytes, 4*sncTouchBytes)
		},
		// The touch stayed in the cache: too little reached DRAM.
		"cached": func(tb map[string]perfReading) func(int) error {
			return touchMoves(tb, sncBoxNode, sncTouchBytes/10, 0)
		},
		// One node gets every box.
		"one node": func(tb map[string]perfReading) func(int) error {
			return touchMoves(tb, map[string]int{"uncore_imc_0": 0, "uncore_imc_1": 0, "uncore_imc_2": 0, "uncore_imc_3": 0}, sncTouchBytes, 0)
		},
		"touch fails": func(map[string]perfReading) func(int) error {
			return func(int) error { return errors.New("mbind: EINVAL") }
		},
	} {
		table := map[string]perfReading{}
		seedTable(table, evs)
		fds := &fakeFDs{}
		stubTouch(t, touch(table))
		if got, err := mapSNCBoxes("0/0", []int{0, 1}, evs, fds.opener(table, nil)); err == nil {
			t.Errorf("%s: mapped %v, want an error", name, got)
		}
		// Whatever the reason, the counters opened for the measurement are
		// closed.
		if len(fds.all) != 8 {
			t.Errorf("%s: %d counters opened, want 8", name, len(fds.all))
		}
		fds.check(t, nil)
	}
}

// sncBoxEvents is the CAS read and write events of each box on die 0/0,
// opened on cpu2, and of one box on die 0/1 that a measurement of 0/0 leaves
// alone. A count is one 64-byte line.
func sncBoxEvents(boxes ...string) []uncoreEvent {
	var evs []uncoreEvent
	for _, b := range append(boxes, "uncore_imc_9") {
		die, cpu := "0/0", 2
		if b == "uncore_imc_9" {
			die, cpu = "0/1", 4
		}
		for _, name := range []string{"cas_count_read", "cas_count_write"} {
			evs = append(evs, uncoreEvent{PMU: b, Name: name, CPU: cpu, Die: die, Group: die, BytesPerCount: 64})
		}
	}
	return evs
}

// What mapSNCBoxes concludes from given CAS traffic, and that it closes every
// counter it opened whatever the outcome.
func TestMapSNCBoxesOutcomes(t *testing.T) {
	const (
		imc0, imc1, imc2, imc3 = "uncore_imc_0", "uncore_imc_1", "uncore_imc_2", "uncore_imc_3"
		imc4, imc5             = "uncore_imc_4", "uncore_imc_5"
	)
	two, three := []string{imc0, imc1}, []string{imc0, imc1, imc2}
	cases := []struct {
		name  string
		boxes []string
		nodes []int
		// moves is the MiB each node's touch adds to a box: three quarters
		// on its read counter, one quarter on its write counter.
		moves map[int]map[string]uint64
		// unreadable is a counter (counterKey) that cannot be read at all;
		// breaks is one that the given node's touch leaves unreadable, and
		// touchFails the nodes whose touch fails.
		unreadable string
		breaks     map[int]string
		touchFails map[int]bool
		openFails  string
		want       map[string]int
		wantErr    string
		opened     int
		touched    []int
	}{
		{
			name:  "interleaved boxes go to the node that moves them",
			boxes: []string{imc0, imc1, imc2, imc3},
			moves: map[int]map[string]uint64{
				0: {imc0: 400, imc2: 400, imc1: 10, imc3: 10},
				1: {imc1: 400, imc3: 400, imc0: 10, imc2: 10},
			},
			want:   map[string]int{imc0: 0, imc1: 1, imc2: 0, imc3: 1},
			opened: 8, touched: []int{0, 1},
		},
		{
			// As many boxes as nodes is enough.
			name:  "a box moved exactly four times more by one node is clear",
			boxes: two,
			moves: map[int]map[string]uint64{0: {imc0: 400, imc1: 100}, 1: {imc0: 100, imc1: 400}},
			want:  map[string]int{imc0: 0, imc1: 1}, opened: 4, touched: []int{0, 1},
		},
		{
			name:    "a runner-up touched before the leader keeps the box unclear",
			boxes:   two,
			moves:   map[int]map[string]uint64{0: {imc0: 200}, 1: {imc0: 300, imc1: 400}},
			wantErr: "no node stands out for uncore_imc_0", opened: 4, touched: []int{0, 1},
		},
		{
			name:    "a runner-up touched after the leader keeps the box unclear",
			boxes:   two,
			moves:   map[int]map[string]uint64{0: {imc0: 300}, 1: {imc0: 200, imc1: 400}},
			wantErr: "no node stands out for uncore_imc_0", opened: 4, touched: []int{0, 1},
		},
		{
			name:    "a box no touch moves stands out for no node",
			boxes:   three,
			moves:   map[int]map[string]uint64{0: {imc2: 400}, 1: {imc1: 400}},
			wantErr: "no node stands out for uncore_imc_0", opened: 6, touched: []int{0, 1},
		},
		{
			name:    "every unclear box is named, in order",
			boxes:   []string{imc5, imc4, imc3, imc2, imc1, imc0},
			moves:   map[int]map[string]uint64{0: {imc0: 400}, 1: {imc1: 400}},
			wantErr: "no node stands out for uncore_imc_2, uncore_imc_3, uncore_imc_4, uncore_imc_5",
			opened:  12, touched: []int{0, 1},
		},
		{
			name:    "an unclear box is reported before a node's short share",
			boxes:   three,
			moves:   map[int]map[string]uint64{0: {imc0: 100}, 1: {imc1: 100}},
			wantErr: "no node stands out for uncore_imc_2", opened: 6, touched: []int{0, 1},
		},
		{
			name:    "a node whose boxes counted under a quarter of the touch is refused",
			boxes:   two,
			moves:   map[int]map[string]uint64{0: {imc0: 200}, 1: {imc1: 400}},
			wantErr: "node 0's boxes counted 200 MiB of the 1024 MiB touched", opened: 4, touched: []int{0, 1},
		},
		{
			name:    "every node's share is checked",
			boxes:   two,
			moves:   map[int]map[string]uint64{0: {imc0: 400}, 1: {imc1: 255}},
			wantErr: "node 1's boxes counted 255 MiB of the 1024 MiB touched", opened: 4, touched: []int{0, 1},
		},
		{
			name:  "a node's boxes add up to exactly a quarter",
			boxes: three,
			moves: map[int]map[string]uint64{0: {imc0: 128, imc2: 128}, 1: {imc1: 400}},
			want:  map[string]int{imc0: 0, imc1: 1, imc2: 0}, opened: 6, touched: []int{0, 1},
		},
		{
			name:  "fewer boxes than nodes are refused before any touch",
			boxes: two, nodes: []int{0, 1, 2},
			wantErr: "2 IMC boxes for 3 NUMA nodes", opened: 4,
		},
		{
			name:  "a box that cannot be opened fails the measurement",
			boxes: three, openFails: imc1,
			wantErr: "uncore_imc_1/cas_count_read on cpu2: perf_event_open denied", opened: 2,
		},
		{
			name:  "a counter unreadable before the first touch fails the measurement",
			boxes: two, unreadable: imc1 + "/cas_count_write/0/0",
			wantErr: "uncore_imc_1/cas_count_write: read failed", opened: 4,
		},
		{
			name:    "a counter the touch leaves unreadable fails the measurement",
			boxes:   two,
			moves:   map[int]map[string]uint64{0: {imc0: 400}, 1: {imc1: 400}},
			breaks:  map[int]string{1: imc0 + "/cas_count_read/0/0"},
			wantErr: "uncore_imc_0/cas_count_read: read failed", opened: 4, touched: []int{0, 1},
		},
		{
			name:  "a failed touch fails the measurement",
			boxes: two, touchFails: map[int]bool{1: true},
			moves:   map[int]map[string]uint64{0: {imc0: 400}, 1: {imc1: 400}},
			wantErr: "touching node 1: mbind: EINVAL", opened: 4, touched: []int{0, 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs := sncBoxEvents(c.boxes...)
			table := map[string]perfReading{}
			// Counters do not start at zero.
			for _, ev := range evs {
				table[counterKey(ev)] = perfReading{ok: counterKey(ev) != c.unreadable, count: 1000}
			}
			var touched []int
			stubTouch(t, func(n int) error {
				touched = append(touched, n)
				for box, mib := range c.moves[n] {
					lines := mib << 20 / 64
					for name, share := range map[string]uint64{"cas_count_read": lines / 4 * 3, "cas_count_write": lines / 4} {
						k := box + "/" + name + "/0/0"
						r := table[k]
						r.count += share
						table[k] = r
					}
				}
				if k, ok := c.breaks[n]; ok {
					r := table[k]
					r.ok = false
					table[k] = r
				}
				if c.touchFails[n] {
					return errors.New("mbind: EINVAL")
				}
				return nil
			})
			fds := &fakeFDs{}
			nodes := c.nodes
			if nodes == nil {
				nodes = []int{0, 1}
			}
			got, err := mapSNCBoxes("0/0", nodes, evs, fds.opener(table, func(ev uncoreEvent) error {
				if ev.PMU == c.openFails {
					return errors.New("perf_event_open denied")
				}
				return nil
			}))
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if gotErr != c.wantErr || !reflect.DeepEqual(got, c.want) {
				t.Errorf("mapSNCBoxes = %v, %q; want %v, %q", got, gotErr, c.want, c.wantErr)
			}
			if !reflect.DeepEqual(touched, c.touched) {
				t.Errorf("touched nodes %v, want %v", touched, c.touched)
			}
			if len(fds.all) != c.opened {
				t.Errorf("%d counters opened, want %d", len(fds.all), c.opened)
			}
			fds.check(t, nil)
		})
	}
}

func TestUncoreReadsMemoryUnderSubNUMAClustering(t *testing.T) {
	root := sncHost(t)
	table := map[string]perfReading{}
	evs, _ := discoverIMCEvents(root, false)
	seedTable(table, evs)
	fds := &fakeFDs{}
	stubTouch(t, touchMoves(table, sncBoxNode, sncTouchBytes, 0))
	u, err := newUncore(sncGraph(), root, "", UncoreOptions{}, fds.opener(table, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if got := strings.Join(u.Links(), ","); got != "mem.0,mem.1,mem.2" {
		t.Fatalf("links %q skipped %v", got, u.Skipped)
	}
	if got := u.SNCBoxes["0/0"]; got != "node0=uncore_imc_0,uncore_imc_2 node1=uncore_imc_1,uncore_imc_3" {
		t.Errorf("SNC boxes %q", got)
	}
	// The measurement opened and closed die 0/0's 8 counters; the 16 opened
	// for reading (both dies, 4 boxes × read/write) are all kept, die 0/0's
	// under their node's group.
	if len(fds.all) != 24 || len(u.ctrs) != 16 {
		t.Fatalf("%d counters opened, %d kept, want 24 and 16", len(fds.all), len(u.ctrs))
	}
	if n := fds.closed(t, func(ev uncoreEvent) bool { return ev.Group == "0/0" }); n != 8 {
		t.Errorf("%d of the measurement's counters closed, want 8", n)
	}
	fds.check(t, u)
	now := time.Unix(0, 0)
	u.now = func() time.Time { return now }
	u.Poll(false)
	// Node 1's boxes (1 and 3) move 1e9 lines in 1 s: 512 Gb/s; node 0's
	// boxes are idle.
	now = now.Add(time.Second)
	for k, r := range table {
		r.enabled += uint64(time.Second)
		r.running += uint64(time.Second)
		if strings.HasPrefix(k, "uncore_imc_1/") || strings.HasPrefix(k, "uncore_imc_3/") {
			if strings.HasSuffix(k, "#node1") {
				r.count += 250_000_000
			}
		}
		table[k] = r
	}
	u.Poll(false)
	util := u.Util()
	if math.Abs(util["mem.1"]-0.512) > 1e-9 || util["mem.0"] != 0 {
		t.Fatalf("util %v, want mem.1 0.512 and mem.0 0", util)
	}
}

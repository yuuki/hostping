package monitor

import (
	"errors"
	"fmt"
	"math"
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
	open := fakeOpener(table, nil)
	// Open once so the table knows every counter before the touch runs.
	for _, ev := range evs {
		if _, err := open(ev); err != nil {
			t.Fatal(err)
		}
	}
	// Host traffic during the mapping lands on every box alike.
	stubTouch(t, touchMoves(table, sncBoxNode, sncTouchBytes, sncTouchBytes/10))
	got, err := mapSNCBoxes("0/0", []int{0, 1}, evs, open)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(sncBoxNode) {
		t.Fatalf("boxes %v, want %v", got, sncBoxNode)
	}
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
		open := fakeOpener(table, nil)
		for _, ev := range evs {
			_, _ = open(ev)
		}
		stubTouch(t, touch(table))
		if got, err := mapSNCBoxes("0/0", []int{0, 1}, evs, open); err == nil {
			t.Errorf("%s: mapped %v, want an error", name, got)
		}
	}
}

func TestUncoreReadsMemoryUnderSubNUMAClustering(t *testing.T) {
	root := sncHost(t)
	table := map[string]perfReading{}
	open := fakeOpener(table, nil)
	evs, _ := discoverIMCEvents(root, false)
	for _, ev := range evs {
		_, _ = open(ev)
	}
	stubTouch(t, touchMoves(table, sncBoxNode, sncTouchBytes, 0))
	u, err := newUncore(sncGraph(), root, "", UncoreOptions{}, open)
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

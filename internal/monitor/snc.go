package monitor

import (
	"fmt"
	"sort"
	"strings"
)

// Memory utilization under sub-NUMA clustering.
//
// With SNC a die holds several NUMA nodes, each served by its own subset of
// the die's IMC boxes, but sysfs does not say which. Nor does the box number:
// measured on Emerald Rapids and Granite Rapids, a node's boxes are neither
// contiguous nor in the same order from one generation to the next. So
// the reader finds out when it starts: for each NUMA node of such a die, it
// writes and reads back a buffer bound to that node (touchNode) and sees
// which boxes' CAS counts rise. A box belongs to the node whose touch moved
// it far more than any other node's did. The mapping is used only when every
// box falls clearly to one node, every node gets a box, and each node's boxes
// carried a fair share of what was touched; otherwise the die's mem.N links
// stay unknown, as before. Traffic the host itself generates while mapping
// lands on all nodes alike and does not move a box from one node to another.

// sncTouchBytes is what touchNode writes and reads back on one node: well
// above a die's last-level cache, so most of it reaches DRAM within the
// measurement.
const sncTouchBytes = 1 << 30

// sncDominance is how much more a box must move under its node's touch than
// under any other node's.
const sncDominance = 4

// sncMinShare is the fraction of sncTouchBytes a node's boxes must have
// counted during its touch: less means the touch did not reach DRAM there
// and the reading cannot be trusted.
const sncMinShare = 0.25

// touchNode writes and reads back sncTouchBytes of memory bound to a NUMA
// node. Tests replace it.
var touchNode = touchNodeMemory

// sncGroup is the traffic group of one NUMA node on an SNC die.
func sncGroup(die string, node int) string { return fmt.Sprintf("%s#node%d", die, node) }

// mapSNCBoxes finds which of die's IMC boxes serve each of its nodes. evs are
// the IMC events; those of die are opened for the measurement and closed
// again. The result maps a box (PMU name) to its node.
func mapSNCBoxes(die string, nodes []int, evs []uncoreEvent, open perfOpener) (map[string]int, error) {
	type ctr struct {
		ev uncoreEvent
		c  perfCounter
	}
	var ctrs []ctr
	defer func() {
		for _, x := range ctrs {
			_ = x.c.Close()
		}
	}()
	boxes := map[string]bool{}
	for _, ev := range evs {
		if ev.Die != die {
			continue
		}
		c, err := open(ev)
		if err != nil {
			return nil, fmt.Errorf("%s/%s on cpu%d: %w", ev.PMU, ev.Name, ev.CPU, err)
		}
		ctrs = append(ctrs, ctr{ev, c})
		boxes[ev.PMU] = true
	}
	if len(boxes) < len(nodes) {
		return nil, fmt.Errorf("%d IMC boxes for %d NUMA nodes", len(boxes), len(nodes))
	}
	read := func() (map[string]float64, error) {
		out := map[string]float64{}
		for _, x := range ctrs {
			n, _, _, err := x.c.Read()
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", x.ev.PMU, x.ev.Name, err)
			}
			out[x.ev.PMU] += float64(n) * x.ev.BytesPerCount
		}
		return out, nil
	}
	moved := map[string]map[int]float64{} // box → node touched → bytes
	for _, n := range nodes {
		before, err := read()
		if err != nil {
			return nil, err
		}
		if err := touchNode(n); err != nil {
			return nil, fmt.Errorf("touching node %d: %w", n, err)
		}
		after, err := read()
		if err != nil {
			return nil, err
		}
		for b := range boxes {
			if moved[b] == nil {
				moved[b] = map[int]float64{}
			}
			moved[b][n] = max(after[b]-before[b], 0)
		}
	}
	out := map[string]int{}
	share := map[int]float64{}
	var unclear []string
	for b := range boxes {
		best, first, second := -1, 0.0, 0.0
		for _, n := range nodes {
			switch v := moved[b][n]; {
			case v > first:
				best, first, second = n, v, first
			case v > second:
				second = v
			}
		}
		if best < 0 || first < sncDominance*second {
			unclear = append(unclear, b)
			continue
		}
		out[b] = best
		share[best] += first
	}
	if len(unclear) > 0 {
		sort.Strings(unclear)
		return nil, fmt.Errorf("no node stands out for %s", strings.Join(unclear, ", "))
	}
	for _, n := range nodes {
		if share[n] < sncMinShare*sncTouchBytes {
			return nil, fmt.Errorf("node %d's boxes counted %.0f MiB of the %d MiB touched", n, share[n]/(1<<20), sncTouchBytes>>20)
		}
	}
	return out, nil
}

// describeSNC renders mapSNCBoxes results per die, e.g.
// "node0=uncore_imc_0,uncore_imc_1 node1=uncore_imc_2,uncore_imc_3".
func describeSNC(byDie map[string]map[string]int) map[string]string {
	out := map[string]string{}
	for die, boxes := range byDie {
		byNode := map[int][]string{}
		for b, n := range boxes {
			byNode[n] = append(byNode[n], b)
		}
		nodes := make([]int, 0, len(byNode))
		for n := range byNode {
			nodes = append(nodes, n)
		}
		sort.Ints(nodes)
		var parts []string
		for _, n := range nodes {
			bs := byNode[n]
			sort.Slice(bs, func(i, j int) bool {
				return len(bs[i]) < len(bs[j]) || len(bs[i]) == len(bs[j]) && bs[i] < bs[j]
			})
			parts = append(parts, fmt.Sprintf("node%d=%s", n, strings.Join(bs, ",")))
		}
		out[die] = strings.Join(parts, " ")
	}
	return out
}

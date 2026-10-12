package topology

import (
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// Only a path between two root ports has a pair, and the pair does not depend
// on the direction the path crosses them.
func TestRootPortPair(t *testing.T) {
	g := Fig2()
	cases := []struct {
		rnic, ep string
		want     model.RootPortPair
		ok       bool
	}{
		{"rnic0", "gpu2", model.RootPortPair{"rp.0", "rp.1"}, true}, // cross_root
		{"rnic1", "gpu0", model.RootPortPair{"rp.0", "rp.1"}, true}, // the same pair, crossed the other way
		{"rnic0", "gpu6", model.RootPortPair{"rp.0", "rp.3"}, true}, // cross_socket
		{"rnic0", "gpu0", model.RootPortPair{}, false},              // same_switch: no root port
		{"rnic0", "mem0", model.RootPortPair{}, false},              // local_mem: one root port
		{"rnic0", "mem1", model.RootPortPair{}, false},              // remote_mem: one root port
	}
	for _, c := range cases {
		p, _ := g.Path(c.rnic, c.ep)
		got, ok := g.RootPortPair(p)
		if got != c.want || ok != c.ok {
			t.Errorf("%s->%s: %v %v, want %v %v", c.rnic, c.ep, got, ok, c.want, c.ok)
		}
	}
	pairs := g.RootPortPairs()
	if len(pairs) != 6 || !pairs[model.NewRootPortPair("rp.3", "rp.2")] {
		t.Errorf("pairs = %v, want the 6 pairs of 4 root ports", pairs)
	}
}

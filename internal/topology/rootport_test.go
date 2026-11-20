package topology

import (
	"slices"
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

// RNICsProbing names the RNICs a run has to sweep to put a loopback through a
// link: in an idle run every RNIC reaches every GPU, in a busy run only the
// affinitive one does, and a memory path stays under its own root port.
func TestRNICsProbing(t *testing.T) {
	g := Fig2()
	for _, tc := range []struct {
		link string
		mode model.RunMode
		want []string
	}{
		{"pcie.rnic2", model.ModeBusy, []string{"rnic2"}},
		{"pcie.rnic2", model.ModeIdle, []string{"rnic2"}},
		{"pcie.gpu5", model.ModeBusy, []string{"rnic2"}},
		{"pcie.gpu5", model.ModeIdle, []string{"rnic0", "rnic1", "rnic2", "rnic3"}},
		{"rp.3", model.ModeBusy, []string{"rnic3"}},
		{"sw.0", model.ModeBusy, []string{"rnic0"}},
		{"mem.1", model.ModeBusy, []string{"rnic0", "rnic1", "rnic2", "rnic3"}},
		{"upi.0", model.ModeBusy, []string{"rnic0", "rnic1", "rnic2", "rnic3"}},
		{"pcie.gpu9", model.ModeIdle, nil},
		{"", model.ModeBusy, nil},
	} {
		if got := g.RNICsProbing(tc.link, tc.mode); !slices.Equal(got, tc.want) {
			t.Errorf("RNICsProbing(%q, %s) = %v, want %v", tc.link, tc.mode, got, tc.want)
		}
	}
}

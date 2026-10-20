package topology

import (
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func capGraph() Graph {
	return Graph{Name: "t", Links: []model.Link{
		{ID: "mem.0", Kind: "memory", CapacityBPS: memCap, CapacityAssumed: true},
		{ID: "mem.1", Kind: "memory", CapacityBPS: memCap, CapacityAssumed: true},
		{ID: "upi.0-1", Kind: "upi", CapacityBPS: upiCap, CapacityAssumed: true},
		{ID: "pcie.rnic0", Kind: "pcie_rnic", CapacityBPS: 252e9},
	}}
}

func TestSetCapacity(t *testing.T) {
	g := capGraph()
	if err := g.SetCapacity("memory", 1600e9, map[string]uint64{"mem.1": 800e9}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]uint64{"mem.0": 1600e9, "mem.1": 800e9} {
		l, _ := g.Link(id)
		if l.CapacityBPS != want || l.CapacityAssumed {
			t.Errorf("%s = %+v, want %d read", id, l, want)
		}
	}
	if l, _ := g.Link("upi.0-1"); !l.CapacityAssumed || l.CapacityBPS != upiCap {
		t.Errorf("another kind must be untouched: %+v", l)
	}
}

func TestSetCapacityNothingGivenKeepsAssumed(t *testing.T) {
	g := capGraph()
	if err := g.SetCapacity("upi", 0, nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := g.Link("upi.0-1"); !l.CapacityAssumed {
		t.Errorf("no capacity given must leave the link assumed: %+v", l)
	}
}

func TestSetCapacityRejectsUnknownLinks(t *testing.T) {
	g := capGraph()
	if err := g.SetCapacity("memory", 0, map[string]uint64{"mem.7": 1e9}); err == nil {
		t.Error("a missing link must be an error")
	}
	if err := g.SetCapacity("upi", 0, map[string]uint64{"pcie.rnic0": 1e9}); err == nil {
		t.Error("a link of another kind must be an error")
	}
}

package topology

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// railGraph builds RNICs carrying the given fabric keys. The keys are opaque:
// discovery writes "v4:<subnet>" or "gid:<prefix>", but MutualPair compares them
// as strings, so what matters here is only which ones are equal.
func railGraph(fabrics ...string) Graph {
	var g Graph
	for i, f := range fabrics {
		g.RNICs = append(g.RNICs, model.RNIC{
			ID:     fmt.Sprintf("rnic%d", i),
			Device: fmt.Sprintf("dev%d", i),
			Fabric: f,
		})
	}
	return g
}

// One rail per RNIC is the standard rail-optimized layout, and it leaves the
// mutual probe with nothing to measure. The fabric keys are what tells an
// operator why, so they must survive as distinct values.
func TestMutualPairsEmptyOnRailOptimizedHost(t *testing.T) {
	g := railGraph("rail0", "rail1", "rail2")
	if pairs := g.MutualPairs(model.MutualScopeRail); len(pairs) != 0 {
		t.Fatalf("distinct fabrics must not pair: %+v", pairs)
	}
	keys := g.FabricKeys()
	if len(keys) != 3 {
		t.Fatalf("want one key per rail, got %v", keys)
	}
	if got := strings.Join(keys, " "); got != "rail0 rail1 rail2" {
		t.Fatalf("keys must be distinct and sorted, got %q", got)
	}
}

func TestMutualPairsOnSharedFabric(t *testing.T) {
	// Two RNICs on one rail plus a third on its own: exactly one pair.
	g := railGraph("rail0", "rail0", "rail1")
	pairs := g.MutualPairs(model.MutualScopeRail)
	if len(pairs) != 1 {
		t.Fatalf("want the one same-rail pair, got %+v", pairs)
	}
	if pairs[0][0].ID != "rnic0" || pairs[0][1].ID != "rnic1" {
		t.Fatalf("wrong pair %s/%s", pairs[0][0].ID, pairs[0][1].ID)
	}
	if keys := g.FabricKeys(); len(keys) != 2 {
		t.Fatalf("want the distinct keys, got %v", keys)
	}

	// The Fig.2 (--sim) world carries no fabric and pairs every distinct
	// device: 4 RNICs → 6 unordered pairs.
	if pairs := Fig2().MutualPairs(model.MutualScopeRail); len(pairs) != 6 {
		t.Fatalf("Fig.2 must pair every distinct device, got %d", len(pairs))
	}
}

// Host scope pairs every RNIC with a RoCEv2 GID, across rails too, and marks
// the pairs that leave a rail. Rail scope on the same graph keeps only the
// same-rail pair, so the default is what it was before the scope existed.
func TestMutualPairsHostScope(t *testing.T) {
	g := railGraph("rail0", "rail0", "rail1", "unknown")
	if pairs := g.MutualPairs(model.MutualScopeRail); len(pairs) != 1 {
		t.Fatalf("rail scope must keep only the same-rail pair, got %d", len(pairs))
	}
	if pairs := g.MutualPairs(""); len(pairs) != 1 {
		t.Fatalf("an empty scope must behave as rail, got %d", len(pairs))
	}

	pairs := g.MutualPairs(model.MutualScopeHost)
	var got []string
	for _, p := range pairs {
		got = append(got, fmt.Sprintf("%s-%s:%v", p[0].ID, p[1].ID, CrossRail(p[0], p[1])))
	}
	// rnic3 has no RoCEv2 GID and pairs with nobody in either scope.
	want := "rnic0-rnic1:false rnic0-rnic2:true rnic1-rnic2:true"
	if s := strings.Join(got, " "); s != want {
		t.Fatalf("host scope pairs = %q, want %q", s, want)
	}

	// The rail-optimized host that rail scope leaves without pairs is a full
	// mesh of cross-rail pairs under host scope.
	g = railGraph("rail0", "rail1", "rail2", "rail3")
	pairs = g.MutualPairs(model.MutualScopeHost)
	if len(pairs) != 6 {
		t.Fatalf("host scope on 4 rails: want 6 pairs, got %d", len(pairs))
	}
	for _, p := range pairs {
		if !CrossRail(p[0], p[1]) {
			t.Fatalf("%s-%s must be cross-rail", p[0].ID, p[1].ID)
		}
	}

	// Fig.2 carries no fabric: host scope pairs the same devices and none of
	// them crosses a rail.
	for _, p := range Fig2().MutualPairs(model.MutualScopeHost) {
		if CrossRail(p[0], p[1]) {
			t.Fatalf("Fig.2 pair %s-%s marked cross-rail", p[0].ID, p[1].ID)
		}
	}
}

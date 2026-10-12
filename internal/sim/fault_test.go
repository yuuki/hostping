package sim_test

import (
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

func TestComputeModeFault(t *testing.T) {
	w := sim.New(topology.Fig2())
	if err := w.ApplyFault("compute_mode:gpu0; compute_mode:gpu1=Prohibited; compute_mode:gpu2=Default"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"gpu0": model.ComputeModeExclusiveProcess,
		"gpu1": model.ComputeModeProhibited,
		"gpu2": model.ComputeModeDefault,
	}
	for id, m := range want {
		if w.ComputeMode[id] != m {
			t.Errorf("ComputeMode[%s] = %q, want %q", id, w.ComputeMode[id], m)
		}
	}
	for _, bad := range []string{"compute_mode:mem0", "compute_mode:gpu9", "compute_mode:gpu0=Exclusive"} {
		if err := sim.New(topology.Fig2()).ApplyFault(bad); err == nil {
			t.Errorf("ApplyFault(%q) accepted", bad)
		}
	}
}

// fast_rp makes the paths across one root-port pair read at the same_switch
// level, and leaves every other path, even of the same distance, alone.
func TestFastRootPortPairFault(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault("fast_rp:rp.1,rp.0"); err != nil {
		t.Fatal(err)
	}
	same, _ := g.Path("rnic0", "gpu0")
	fast, _ := g.Path("rnic1", "gpu0")
	slow, _ := g.Path("rnic2", "gpu6")
	if w.PathBW(fast) != w.PathBW(same) || w.PathLatNs(fast) != w.PathLatNs(same) {
		t.Errorf("fast pair path reads %d/%dns, want the same_switch %d/%dns", w.PathBW(fast), w.PathLatNs(fast), w.PathBW(same), w.PathLatNs(same))
	}
	if healthy := sim.New(g); w.PathBW(slow) != healthy.PathBW(slow) || w.PathBW(slow) >= w.PathBW(same) {
		t.Errorf("other cross_root path reads %d, want the cross_root level %d", w.PathBW(slow), healthy.PathBW(slow))
	}
	for _, bad := range []string{"fast_rp:rp.0", "fast_rp:rp.0,rp.0", "fast_rp:rp.0,sw.1", "fast_rp:rp.0,rp.9"} {
		if err := sim.New(g).ApplyFault(bad); err == nil {
			t.Errorf("ApplyFault(%q) accepted", bad)
		}
	}
}

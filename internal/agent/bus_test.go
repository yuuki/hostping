package agent

import (
	"testing"

	"github.com/yuuki/hostping/internal/config"
)

func TestLoadGraphAppliesOperatorCapacities(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.MemCapacity = config.LinkCapacity{ByLink: map[string]uint64{"mem.1": 800e9}}
	cfg.UPICapacity = config.LinkCapacity{All: 384e9}
	g, err := LoadGraph(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]uint64{"mem.1": 800e9, "upi.0": 384e9} {
		l, ok := g.Link(id)
		if !ok || l.CapacityBPS != want || l.CapacityAssumed {
			t.Errorf("%s = %+v, want capacity %d", id, l, want)
		}
	}

	cfg.MemCapacity = config.LinkCapacity{ByLink: map[string]uint64{"mem.9": 1e9}}
	if _, err := LoadGraph(cfg); err == nil {
		t.Fatal("a capacity for a link the topology lacks must fail")
	}
}

func TestLiveSamplerWithoutUncoreFlag(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sim = true
	g, err := LoadGraph(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, closeFn := liveSampler(g, cfg)
	defer closeFn()
	if s.Uncore != nil {
		t.Fatal("the uncore reader is opt-in")
	}
}

// --exclude-rnic filters discovery; a simulated or file topology has nothing
// to filter, so the flag is an error there rather than silently ignored.
func TestLoadGraphRejectsExcludeWithoutDiscovery(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.ExcludeRNICs = []string{"rnic0"}
	if _, err := LoadGraph(cfg); err == nil {
		t.Fatal("--exclude-rnic with --sim must fail")
	}
}

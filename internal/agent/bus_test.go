package agent

import (
	"fmt"
	"slices"
	"strings"
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

// --drop-ignore-rnic applies to any topology, but a name that matches none of
// its RNICs fails start-up, so a typo cannot leave the drops diagnosed.
func TestNewRigValidatesDropIgnore(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.DropIgnoreRNICs = []string{"rnic1"}
	var logged []string
	rt, err := newRig(cfg, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	if err != nil {
		t.Fatal(err)
	}
	rt.close()
	if !slices.ContainsFunc(logged, func(l string) bool { return strings.Contains(l, "rnic1") && strings.Contains(l, "--drop-ignore-rnic") }) {
		t.Errorf("the ignored RNIC is not logged: %q", logged)
	}
	cfg.DropIgnoreRNICs = []string{"mlx5_99"}
	if _, err := newRig(cfg, func(string, ...any) {}); err == nil || !strings.Contains(err.Error(), "mlx5_99") {
		t.Fatalf("newRig = %v, want an error naming mlx5_99", err)
	}
}

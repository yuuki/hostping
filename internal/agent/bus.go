package agent

import (
	"log"
	"strings"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/topology"
)

// applyCapacities replaces the nominal memory and inter-socket capacities with
// the operator's --mem-channel-capacity / --upi-capacity, which also makes
// those links eligible for a utilization.
func applyCapacities(g *topology.Graph, cfg config.Config) error {
	if err := g.SetCapacity("memory", cfg.MemCapacity.All, cfg.MemCapacity.ByLink); err != nil {
		return err
	}
	return g.SetCapacity("upi", cfg.UPICapacity.All, cfg.UPICapacity.ByLink)
}

// liveSampler is the hardware sampler, with the uncore memory and
// inter-socket reader when --uncore-util asks for it. A reader that cannot
// start leaves those links unknown and says why; it never stops the agent.
// The returned func closes the counters.
func liveSampler(g topology.Graph, cfg config.Config) (monitor.SysfsSampler, func()) {
	s := monitor.SysfsSampler{Graph: g, GPUUtilInterval: cfg.GPUUtilInterval}
	if !cfg.UncoreUtil {
		return s, func() {}
	}
	u, err := monitor.NewUncore(g, monitor.UncoreOptions{UPIBytesPerFlit: cfg.UPIBytesPerFlit})
	if err != nil {
		log.Printf("hostping: uncore utilization unavailable, memory and inter-socket links stay unknown: %v", err)
		return s, func() {}
	}
	for id, why := range u.Skipped {
		log.Printf("hostping: uncore: %s stays unknown: %s", id, why)
	}
	for die, boxes := range u.SNCBoxes {
		log.Printf("hostping: uncore: sub-NUMA clustering on package/die %s, IMC boxes measured per node: %s", die, boxes)
	}
	log.Printf("hostping: uncore utilization on %s", strings.Join(u.Links(), ", "))
	s.Uncore = u
	return s, u.Close
}

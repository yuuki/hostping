package analyzer_test

import (
	"context"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// BenchmarkSimRun times one engine run on the simulated Fig.2 host: the
// probe sweep, rechecks, Algorithm 1 and the report, without RDMA hardware.
// This is the CPU hostping itself spends per run on top of the probes.
func BenchmarkSimRun(b *testing.B) {
	for _, tc := range []struct {
		name, fault string
		mode        model.RunMode
		hostIdle    bool
	}{
		{"healthy_idle", "", model.ModeIdle, true},
		{"rnic_pcie_idle", "rnic_pcie:rnic0", model.ModeIdle, false},
		{"mem_busy", "mem:mem0;busy:rnic0,rnic2", model.ModeBusy, false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			g := topology.Fig2()
			w := sim.New(g)
			if err := w.ApplyFault(tc.fault); err != nil {
				b.Fatal(err)
			}
			cfg := config.Defaults()
			req := engine.RunRequest{
				Trigger: model.TriggerManual, Mode: tc.mode, BusyRNICs: w.Busy,
				Util: w.LinkUtil, PCIe: w.PCIe, Hints: map[string]string{},
				HostIdle: tc.hostIdle, GPUComputeMode: w.ComputeMode,
			}
			b.ReportAllocs()
			for b.Loop() {
				// A fresh engine per run, so streaks from the previous
				// iteration do not change what this one reports.
				e := engine.New(cfg, g, sim.Prober{World: w})
				e.OneShot = true
				e.Logf = func(string, ...any) {}
				if _, err := e.Run(context.Background(), req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

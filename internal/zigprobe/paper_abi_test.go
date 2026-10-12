package zigprobe_test

import (
	"context"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
	"github.com/yuuki/hostping/internal/zigprobe"
)

func TestPaperCasesThroughZigABI(t *testing.T) {
	g := topology.Fig2()
	cases := []struct {
		fault string
		mode  model.RunMode
		want  string
	}{
		{"rnic_pcie:rnic0", model.ModeIdle, model.CauseRNICPCIeFailure},
		{"gpu_pcie:gpu0", model.ModeIdle, model.CauseGPUPCIeFailure},
		{"acs", model.ModeIdle, model.CauseACSEnabled},
		{"rnic_pcie:rnic0;busy:rnic0", model.ModeBusy, model.CauseRNICPCIeFailure},
	}
	for _, tc := range cases {
		t.Run(tc.fault, func(t *testing.T) {
			w := sim.New(g)
			if err := w.ApplyFault(tc.fault); err != nil {
				t.Fatal(err)
			}
			cfg := config.Defaults()
			e := engine.New(cfg, g, zigprobe.Prober{Sim: w, Graph: g, Iters: cfg.Iters, TimeoutMS: 1000})
			req := engine.RunRequest{
				Trigger: model.TriggerManual, Mode: tc.mode, BusyRNICs: w.Busy,
				Util: w.LinkUtil, PCIe: w.PCIe,
			}
			rep, err := e.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			ok := false
			for _, c := range rep.Causes {
				if c.Code == tc.want {
					ok = true
					break
				}
			}
			if !ok {
				t.Fatalf("zig ABI fault %s: want %s, got %v", tc.fault, tc.want, rep.Causes)
			}
			for _, c := range rep.Causes {
				if tc.want == model.CauseRNICPCIeFailure && c.Code == model.CauseGPUPCIeFailure {
					t.Fatalf("zig ABI fault %s: extra %s", tc.fault, c.Code)
				}
				if tc.want == model.CauseGPUPCIeFailure && c.Code == model.CauseCPURootPortFailure {
					t.Fatalf("zig ABI fault %s: extra %s", tc.fault, c.Code)
				}
			}
		})
	}
}

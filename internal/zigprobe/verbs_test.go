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

// The verb timings the sim world injects cross the C ABI and reach the
// analyzer: a slow_mr GPU is reported through the production --sim prober,
// and a healthy world reports nothing.
func TestSimVerbDurationsDriveSlowMR(t *testing.T) {
	g := topology.Fig2()
	for _, tc := range []struct {
		fault string
		want  []string
	}{
		{"", nil},
		{"slow_mr:gpu5", []string{"gpu5"}},
	} {
		t.Run("fault="+tc.fault, func(t *testing.T) {
			w := sim.New(g)
			if err := w.ApplyFault(tc.fault); err != nil {
				t.Fatal(err)
			}
			p := zigprobe.Prober{Sim: w, Graph: g, Iters: 16, TimeoutMS: 1000}
			e := engine.New(config.Defaults(), g, p)
			e.Logf = func(string, ...any) {}
			// The cause needs the GPU slow in two consecutive idle runs.
			var rep model.Report
			for range 2 {
				var err error
				rep, err = e.Run(context.Background(), engine.RunRequest{Trigger: model.TriggerManual, Mode: model.ModeIdle, HostIdle: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, m := range rep.Paths {
				if m.Verbs.MRReg == 0 || m.Verbs.QPSetup == 0 {
					t.Fatalf("%s->%s: verb timings lost across the C ABI: %+v", m.RNIC, m.Endpoint, m.Verbs)
				}
				if (m.EndpointKind == model.EndpointGPU) != (m.Verbs.GPUAlloc != 0) {
					t.Fatalf("%s->%s (%s): gpu_alloc %d", m.RNIC, m.Endpoint, m.EndpointKind, m.Verbs.GPUAlloc)
				}
			}
			var got []string
			for _, c := range rep.Causes {
				if c.Code == model.CauseGPUMRRegistrationSlow {
					got = append(got, c.Endpoints...)
				}
			}
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Fatalf("slow MR endpoints = %v, want %v", got, tc.want)
			}
		})
	}
}

package monitor

import (
	"context"
	"slices"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func TestParseNvidiaGPUsComputeMode(t *testing.T) {
	out := "00000000:01:00.0, 0, Default\n" +
		"00000000:02:00.0, 97, Exclusive_Process\n" +
		"0000:03:00.0, [N/A], Prohibited\n" + // util unreadable, mode still read
		"04:00.0, 5, EXCLUSIVE_THREAD\n" + // short domain, any case
		"00000001:05:00.0, 3, [Not Supported]\n" + // mode unknown, util still read
		"00000000:06:00.0, 1\n" + // older two-column output: mode unknown
		"00000000:07:00.0, 2, bogus\n" +
		"garbage\n" +
		"\n"
	s := parseNvidiaGPUs(out)

	wantMode := map[string]string{
		"01:00.0": model.ComputeModeDefault,
		"02:00.0": model.ComputeModeExclusiveProcess,
		"03:00.0": model.ComputeModeProhibited,
		"04:00.0": model.ComputeModeExclusiveThread,
	}
	if len(s.mode) != len(wantMode) {
		t.Fatalf("modes = %v, want %v", s.mode, wantMode)
	}
	for bdf, m := range wantMode {
		if s.mode[bdf] != m {
			t.Errorf("mode[%s] = %q, want %q", bdf, s.mode[bdf], m)
		}
	}
	wantUtil := map[string]float64{"01:00.0": 0, "02:00.0": 97, "04:00.0": 5, "05:00.0": 3, "06:00.0": 1, "07:00.0": 2}
	if len(s.util) != len(wantUtil) {
		t.Fatalf("util = %v, want %v", s.util, wantUtil)
	}
	for bdf, u := range wantUtil {
		if got, ok := s.util[bdf]; !ok || got != u {
			t.Errorf("util[%s] = %v (ok=%v), want %v", bdf, got, ok, u)
		}
	}
}

func TestNvidiaGPUsByBDFQueriesComputeMode(t *testing.T) {
	var args []string
	s := nvidiaGPUsByBDF(context.Background(), func(_ context.Context, _ string, a ...string) ([]byte, error) {
		args = a
		return []byte("00000000:01:00.0, 0, Exclusive_Process\n"), nil
	})
	if !slices.Contains(args, "--query-gpu=pci.bus_id,utilization.gpu,compute_mode") {
		t.Fatalf("query args = %v", args)
	}
	if s.mode["01:00.0"] != model.ComputeModeExclusiveProcess {
		t.Fatalf("mode = %v", s.mode)
	}
}

// The monitor hands the compute modes it read to the run, keyed by endpoint;
// a GPU whose mode is unknown is left out rather than reported as "".
func TestMonitorDecisionCarriesComputeModes(t *testing.T) {
	g := topology.Graph{
		RNICs: []model.RNIC{{ID: "rnic0", LineRate: 100}},
		Endpoints: []model.Endpoint{
			{ID: "gpu0", Kind: model.EndpointGPU},
			{ID: "gpu1", Kind: model.EndpointGPU},
		},
	}
	m := New(config.Defaults(), g, StaticSampler{GPU: []model.GPUStatus{
		{ID: "gpu0", Idle: true, ComputeMode: model.ComputeModeExclusiveProcess},
		{ID: "gpu1", Idle: true},
	}})
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.GPUComputeMode) != 1 || d.GPUComputeMode["gpu0"] != model.ComputeModeExclusiveProcess {
		t.Fatalf("GPUComputeMode = %v, want only gpu0", d.GPUComputeMode)
	}
}

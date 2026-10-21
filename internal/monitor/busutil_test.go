package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

const (
	smiIndexOut = "0, 00000000:18:00.0\n1, 00000000:19:00.0\n2, 00000000:38:00.0\n"
	// dmon names GPUs by index; gpu2 does not support the counter.
	dmonOut = "# gpu  rxpci  txpci \n# Idx   MB/s   MB/s \n    0   3150  12600 \n    1      0      0 \n    2      -      - \n"
)

// fakeSMI answers the index query and dmon; any other nvidia-smi call fails.
func fakeSMI(t *testing.T, calls *int, dmon string, dmonErr error) runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls++
		if name != "nvidia-smi" {
			t.Fatalf("ran %q, want nvidia-smi", name)
		}
		switch {
		case len(args) > 0 && args[0] == "dmon":
			if got := strings.Join(args, " "); got != "dmon -s t -c 1" {
				t.Fatalf("dmon args %q", got)
			}
			return []byte(dmon), dmonErr
		case len(args) > 0 && args[0] == "--query-gpu=index,pci.bus_id":
			return []byte(smiIndexOut), nil
		}
		return nil, errors.New("unexpected query")
	}
}

func TestNvidiaPCIeByBDF(t *testing.T) {
	calls := 0
	got := nvidiaPCIeByBDF(context.Background(), fakeSMI(t, &calls, dmonOut, nil))
	if got["0000:18:00.0"] != 12600e6 {
		t.Errorf("gpu0 = %v, want max(rx, tx) = 12600 MB/s", got["0000:18:00.0"])
	}
	if v, ok := got["0000:19:00.0"]; !ok || v != 0 {
		t.Errorf("a measured 0 must stay 0, got %v ok=%v", v, ok)
	}
	if _, ok := got["0000:38:00.0"]; ok {
		t.Errorf("an unsupported counter must be absent, got %v", got["0000:38:00.0"])
	}
}

func TestNvidiaPCIeByBDFFailureIsUnknown(t *testing.T) {
	calls := 0
	if got := nvidiaPCIeByBDF(context.Background(), fakeSMI(t, &calls, "", errors.New("no driver"))); got != nil {
		t.Fatalf("failed dmon must be unknown, got %v", got)
	}
	noSMI := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("not found") }
	if got := nvidiaPCIeByBDF(context.Background(), noSMI); got != nil {
		t.Fatalf("missing nvidia-smi must be unknown, got %v", got)
	}
}

// Columns are found by name, so a driver that adds columns still parses.
func TestParseDmonPCIeLocatesColumns(t *testing.T) {
	out := "# gpu    Id  txpci  rxpci\n# Idx    Id   MB/s   MB/s\n    0  a0     10     20\n"
	got := parseDmonPCIe(out, map[string]string{"0": "0000:18:00.0"})
	if got["0000:18:00.0"] != 20e6 {
		t.Fatalf("got %v, want 20 MB/s", got)
	}
	if got := parseDmonPCIe("    0  10  20\n", map[string]string{"0": "0000:18:00.0"}); got != nil {
		t.Fatalf("rows without a header must not be guessed at: %v", got)
	}
}

func TestGPUPCIeUtilNeedsReadCapacity(t *testing.T) {
	g := topology.Fig2()
	rate := map[string]float64{"0000:18:00.0": 12600e6, "0000:19:00.0": 0}
	got := gpuPCIeUtil(g, rate)
	c, _ := g.Link("pcie.gpu0")
	if want := 12600e6 * 8 / float64(c.CapacityBPS); got["pcie.gpu0"] != want {
		t.Errorf("pcie.gpu0 = %v, want %v", got["pcie.gpu0"], want)
	}
	if v, ok := got["pcie.gpu1"]; !ok || v != 0 {
		t.Errorf("pcie.gpu1 = %v ok=%v, want a measured 0", v, ok)
	}
	if _, ok := got["pcie.gpu2"]; ok {
		t.Errorf("a GPU without a rate must be absent")
	}

	// A capacity discovery had to assume is no denominator.
	for i := range g.Links {
		if g.Links[i].ID == "pcie.gpu0" {
			g.Links[i].CapacityAssumed = true
		}
	}
	if _, ok := gpuPCIeUtil(g, rate)["pcie.gpu0"]; ok {
		t.Errorf("utilization computed against an assumed capacity")
	}
}

func TestSysfsSamplerBusReadsGPUPCIe(t *testing.T) {
	old := gpuPCIe
	defer func() { gpuPCIe = old }()
	clk := newFakeClock()
	calls := 0
	gpuPCIe = &gpuPCIeCache{now: clk.now, run: fakeSMI(t, &calls, dmonOut, nil)}

	s := SysfsSampler{Graph: topology.Fig2(), GPUUtilInterval: 10 * time.Second}
	got := s.Bus(context.Background())
	if got["pcie.gpu0"] <= 0 {
		t.Fatalf("pcie.gpu0 missing: %v", got)
	}
	for _, id := range []string{"pcie.rnic0", "mem.0", "upi.0", "pcie.gpu2"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s has no live source here and must be absent, got %v", id, got[id])
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want the index query and one dmon", calls)
	}
	// Within the interval the sample is reused; past it, refreshed once.
	clk.add(5 * time.Second)
	s.Bus(context.Background())
	if calls != 2 {
		t.Fatalf("calls=%d inside the interval, want 2", calls)
	}
	clk.add(5 * time.Second)
	s.Bus(context.Background())
	if calls != 4 {
		t.Fatalf("calls=%d past the interval, want 4", calls)
	}
}

func TestSysfsSamplerBusSkipsGPULessHost(t *testing.T) {
	old := gpuPCIe
	defer func() { gpuPCIe = old }()
	gpuPCIe = &gpuPCIeCache{run: func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("nvidia-smi forked on a host without GPUs")
		return nil, nil
	}}
	g := topology.Graph{RNICs: []model.RNIC{{ID: "rnic0"}}}
	if got := (SysfsSampler{Graph: g}).Bus(context.Background()); got != nil {
		t.Fatalf("got %v, want unknown", got)
	}
}

func TestRNICUtil(t *testing.T) {
	g := topology.Fig2()
	c, _ := g.Link("pcie.rnic0")
	u, ok := rnicUtil(g, "rnic0", c.CapacityBPS/2)
	if !ok || u != 0.5 {
		t.Fatalf("util=%v ok=%v, want 0.5", u, ok)
	}
	if _, ok := rnicUtil(g, "nope", 1); ok {
		t.Fatal("an RNIC without a PCIe hop has no utilization")
	}
}

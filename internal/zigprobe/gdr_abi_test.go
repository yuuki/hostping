package zigprobe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func TestLoopbackReqABISize(t *testing.T) {
	// device[64]+endpoint[64]+8 ints/uints + pad + 4×u64 + gpu_bdf[64] +
	// 3×u64 simulated verb times + mem_warmup_ns
	const want = 64 + 64 + 4 + 4 + 4 + 4 + 4 + 4 + 4 + 4 + 8 + 8 + 8 + 8 + 64 + 8 + 8 + 8 + 8
	got := loopbackReqABISize()
	if got != want {
		t.Fatalf("hostping_loopback_req size %d want %d (Zig extern and C header must match)", got, want)
	}
}

func TestLoopbackResABISize(t *testing.T) {
	// rc + err[256] + 4×u64 + completions + pad + 3×u64 verb times +
	// mem_warmup_ns + addr_mode + port_num + lid + port_fault
	const want = 4 + 256 + 4 /* pad */ + 8 + 8 + 8 + 8 + 4 + 4 /* pad */ + 8 + 8 + 8 + 8 + 4 + 4 + 4 + 4
	got := loopbackResABISize()
	if got != want {
		t.Fatalf("hostping_loopback_res size %d want %d (Zig extern and C header must match)", got, want)
	}
}

func TestLiveGPUEmptyBDFIsNoGDRMissingGPU(t *testing.T) {
	p := Prober{Iters: 1, TimeoutMS: 200}
	_, err := p.Loopback(context.Background(),
		model.RNIC{ID: "rnic0", Device: "mlx5_0"},
		model.Endpoint{ID: "gpu0", Kind: model.EndpointGPU, Index: 0, PCIeBDF: ""},
		1, 128*1024)
	if err == nil {
		t.Fatal("GPU loopback must not succeed with a host-DRAM stand-in")
	}
	if !errors.Is(err, model.ErrNoGDR) {
		t.Fatalf("want ErrNoGDR, got %v", err)
	}
	if !strings.Contains(err.Error(), "NoGDR: missing gpu") {
		t.Fatalf("want specific reason NoGDR: missing gpu, got %v", err)
	}
}

func TestLiveMemoryPathIsNotNoGDR(t *testing.T) {
	p := Prober{Iters: 1, TimeoutMS: 200}
	_, err := p.Loopback(context.Background(),
		model.RNIC{ID: "rnic0", Device: "mlx5_0"},
		model.Endpoint{ID: "mem0", Kind: model.EndpointMemory, Index: 0, NUMA: 0},
		1, 128*1024)
	if errors.Is(err, model.ErrNoGDR) {
		t.Fatalf("memory path must not return NoGDR: %v", err)
	}
}

func TestLiveGPUNonEmptyBDFWithoutGDRIsNoGDR(t *testing.T) {
	p := Prober{Iters: 1, TimeoutMS: 200}
	_, err := p.Loopback(context.Background(),
		model.RNIC{ID: "rnic0", Device: "mlx5_0"},
		model.Endpoint{ID: "gpu0", Kind: model.EndpointGPU, Index: 0, PCIeBDF: "ffff:ff:1f.7"},
		1, 128*1024)
	if err == nil {
		t.Fatal("GPU loopback must not succeed with a host-DRAM stand-in")
	}
	if !errors.Is(err, model.ErrNoGDR) {
		t.Fatalf("want ErrNoGDR, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "NoGDR: no cuda") &&
		!strings.Contains(msg, "NoGDR: missing gpu") &&
		!strings.Contains(msg, "NoGDR: alloc") &&
		!strings.Contains(msg, "NoGDR: cuda drv init") &&
		!strings.Contains(msg, "NoGDR: cuda drv alloc") &&
		!strings.Contains(msg, "NoGDR: reg_mr") {
		t.Fatalf("want a specific NoGDR reason, got %v", err)
	}
}

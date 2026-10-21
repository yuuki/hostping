package monitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// normalizeBDF keeps the PCI domain: it narrows nvidia-smi's 8-digit domain
// to sysfs's four and gives a bare bus:dev.fn domain 0, but never drops it.
func TestNormalizeBDFKeepsDomain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0000:18:00.0", "0000:18:00.0"},
		{"00000000:18:00.0", "0000:18:00.0"},
		{"18:00.0", "0000:18:00.0"},
		{"0000:3C:00.1", "0000:3c:00.1"},
		{"0001:3c:00.0", "0001:3c:00.0"},
		{"00000001:3C:00.0", "0001:3c:00.0"},
		{"not a bdf", ""},
	}
	for _, tc := range cases {
		if got := normalizeBDF(tc.in); got != tc.want {
			t.Fatalf("normalizeBDF(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	if normalizeBDF("0000:3c:00.0") == normalizeBDF("0001:3c:00.0") {
		t.Fatal("two domains at the same bus:dev.fn must not collide")
	}
}

// stubGPUUtil points the package-global nvidia-smi cache at a fake for one
// test and forces its next sample.
func stubGPUUtil(t *testing.T, run runner) {
	t.Helper()
	gpuUtil.mu.Lock()
	oldRun, oldValid, oldLast, oldAt := gpuUtil.run, gpuUtil.valid, gpuUtil.last, gpuUtil.at
	gpuUtil.run, gpuUtil.valid = run, false
	gpuUtil.mu.Unlock()
	t.Cleanup(func() {
		gpuUtil.mu.Lock()
		gpuUtil.run, gpuUtil.valid, gpuUtil.last, gpuUtil.at = oldRun, oldValid, oldLast, oldAt
		gpuUtil.mu.Unlock()
	})
}

// Two GPUs at bus 3c in PCI domains 0 and 1 (nvidia-smi prints the domain
// with eight digits) keep their own utilization and compute mode.
func TestParseNvidiaGPUsTwoDomains(t *testing.T) {
	s := parseNvidiaGPUs("00000000:3C:00.0, 10, Default\n00000001:3C:00.0, 90, Exclusive_Process\n")
	if len(s.util) != 2 || len(s.mode) != 2 {
		t.Fatalf("util=%v mode=%v, want one entry per domain", s.util, s.mode)
	}
	if s.util["0000:3c:00.0"] != 10 || s.util["0001:3c:00.0"] != 90 {
		t.Fatalf("util=%v", s.util)
	}
	if s.mode["0000:3c:00.0"] != model.ComputeModeDefault || s.mode["0001:3c:00.0"] != model.ComputeModeExclusiveProcess {
		t.Fatalf("mode=%v", s.mode)
	}
}

// ReadGPUs matches the graph's PCIeBDF against the nvidia-smi sample on the
// full address, so the domain-1 GPU gets its own utilization.
func TestReadGPUsTwoDomains(t *testing.T) {
	stubGPUUtil(t, func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte("00000000:3C:00.0, 2, Default\n00000001:3C:00.0, 90, Default\n"), nil
	})
	g := topology.Graph{Endpoints: []model.Endpoint{
		{ID: "gpu0", Kind: model.EndpointGPU, PCIeBDF: "0000:3c:00.0"},
		{ID: "gpu1", Kind: model.EndpointGPU, PCIeBDF: "0001:3c:00.0"},
	}}
	got := ReadGPUs(context.Background(), g, 0)
	if len(got) != 2 || got[0].Unknown || got[1].Unknown {
		t.Fatalf("got %+v", got)
	}
	if got[0].Util != 2 || !got[0].Idle || got[1].Util != 90 || got[1].Idle {
		t.Fatalf("gpu0=%+v gpu1=%+v, want util 2 (idle) and 90 (busy)", got[0], got[1])
	}
}

// The dmon index → BDF map and the graph lookup both keep the domain, so the
// PCIe utilization of the domain-1 GPU is its own row, not the domain-0 one.
func TestNvidiaPCIeByBDFTwoDomains(t *testing.T) {
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "dmon" {
			return []byte("# gpu  rxpci  txpci \n# Idx   MB/s   MB/s \n    0    100    200 \n    1   5000   9000 \n"), nil
		}
		return []byte("0, 00000000:3C:00.0\n1, 00000001:3C:00.0\n"), nil
	}
	rate := nvidiaPCIeByBDF(context.Background(), run)
	if len(rate) != 2 || rate["0000:3c:00.0"] != 200e6 || rate["0001:3c:00.0"] != 9000e6 {
		t.Fatalf("rate=%v, want 200 MB/s for domain 0 and 9000 MB/s for domain 1", rate)
	}
	g := topology.Graph{
		Endpoints: []model.Endpoint{
			{ID: "gpu0", Kind: model.EndpointGPU, PCIeBDF: "0000:3c:00.0"},
			{ID: "gpu1", Kind: model.EndpointGPU, PCIeBDF: "0001:3c:00.0"},
		},
		Links: []model.Link{
			{ID: "pcie.gpu0", Kind: model.LinkKindPCIeGPU, CapacityBPS: 1e9},
			{ID: "pcie.gpu1", Kind: model.LinkKindPCIeGPU, CapacityBPS: 1e9},
		},
	}
	got := gpuPCIeUtil(g, rate)
	if got["pcie.gpu0"] != 200e6*8/1e9 || got["pcie.gpu1"] != 9000e6*8/1e9 {
		t.Fatalf("util=%v", got)
	}
}

// mkDRM adds a DRM card whose device link resolves to a PCI function (or any
// other device dir) carrying gpu_busy_percent.
func mkDRM(t *testing.T, root, card, devRel, busy string) {
	t.Helper()
	dev := filepath.Join(root, devRel)
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "gpu_busy_percent"), []byte(busy+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cardDir := filepath.Join(root, "class/drm", card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dev, filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
}

// drmBusy matches a DRM card on the full BDF: the domain-1 GPU must not be
// answered with the domain-0 card that sorts first in /sys/class/drm.
func TestDRMBusyKeepsDomain(t *testing.T) {
	root := t.TempDir()
	mkDRM(t, root, "card0", "devices/pci0000:3c/0000:3c:00.0", "37")
	mkDRM(t, root, "card1", "devices/pci0001:3c/0001:3c:00.0", "91")
	mkDRM(t, root, "card2", "devices/platform/display", "7") // not a PCI function
	if u, ok := drmBusy(root, "0000:3c:00.0"); !ok || u != 37 {
		t.Fatalf("domain 0: %v ok=%v, want 37", u, ok)
	}
	if u, ok := drmBusy(root, "0001:3c:00.0"); !ok || u != 91 {
		t.Fatalf("domain 1: %v ok=%v, want 91 (its own card, not card0)", u, ok)
	}
	if u, ok := drmBusy(root, "00000001:3C:00.0"); !ok || u != 91 {
		t.Fatalf("8-digit domain 1: %v ok=%v, want 91", u, ok)
	}
	if u, ok := drmBusy(root, "0002:3c:00.0"); ok {
		t.Fatalf("absent domain 2 matched %v", u)
	}
	if u, ok := drmBusy(root, ""); ok {
		t.Fatalf("a GPU without a BDF must not match the non-PCI card: %v", u)
	}
}

package pci

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

const (
	gen4 = "16.0 GT/s PCIe"
	gen5 = "32.0 GT/s PCIe"
)

func TestGTs(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"2.5 GT/s PCIe", 2.5},
		{"5.0 GT/s PCIe", 5},
		{"8.0 GT/s PCIe", 8},
		{"16.0 GT/s PCIe", 16},
		{"32.0 GT/s PCIe", 32},
		{"64.0 GT/s PCIe", 64},
		{"8GT/s", 8},
		{"Unknown", 0},
		{"", 0},
		{"GT/s", 0},
		{"x16 8.0 GT/s", 8},
	}
	for _, tc := range cases {
		if got := GTs(tc.in); got != tc.want {
			t.Errorf("GTs(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveBPS(t *testing.T) {
	cases := []struct {
		gts   float64
		width int
		want  uint64
	}{
		{16, 16, 252_061_538_461}, // Gen4 x16, 128b/130b
		{32, 16, 504_123_076_923}, // Gen5 x16
		{5, 16, 64_000_000_000},   // Gen2 x16, 8b/10b
		{0, 16, 0},
		{16, 0, 0},
	}
	for _, tc := range cases {
		if got := EffectiveBPS(tc.gts, tc.width); got != tc.want {
			t.Errorf("EffectiveBPS(%v, %d)=%d want %d", tc.gts, tc.width, got, tc.want)
		}
	}
}

func TestCapabilityAndDownTrain(t *testing.T) {
	cases := []struct {
		name              string
		cfg               model.PCIeConfig
		capGTs            float64
		capWidth          int
		speedLow, widthLo bool
	}{
		{"at capability", model.PCIeConfig{Speed: gen5, Width: 16, MaxSpeed: gen5, MaxWidth: 16, PortMaxSpeed: gen5, PortMaxWidth: 16},
			32, 16, false, false},
		{"gen5 slot trained to gen4", model.PCIeConfig{Speed: gen4, Width: 16, MaxSpeed: gen5, MaxWidth: 16, PortMaxSpeed: gen5, PortMaxWidth: 16},
			32, 16, true, false},
		{"x16 slot trained to x8", model.PCIeConfig{Speed: gen5, Width: 8, MaxSpeed: gen5, MaxWidth: 16, PortMaxSpeed: gen5, PortMaxWidth: 16},
			32, 16, false, true},
		// The slot, not the card, is the limit: not a down-train.
		{"gen5 card in a gen4 slot", model.PCIeConfig{Speed: gen4, Width: 16, MaxSpeed: gen5, MaxWidth: 16, PortMaxSpeed: gen4, PortMaxWidth: 16},
			16, 16, false, false},
		{"x16 card in an x8 port", model.PCIeConfig{Speed: gen5, Width: 8, MaxSpeed: gen5, MaxWidth: 16, PortMaxSpeed: gen5, PortMaxWidth: 8},
			32, 8, false, false},
		// Without the port's maximum the device's own says nothing about the slot.
		{"port max unknown", model.PCIeConfig{Speed: gen4, Width: 8, MaxSpeed: gen5, MaxWidth: 16},
			0, 0, false, false},
		{"negotiated unknown", model.PCIeConfig{Speed: "Unknown", MaxSpeed: gen5, MaxWidth: 16, PortMaxSpeed: gen5, PortMaxWidth: 16},
			32, 16, false, false},
	}
	for _, tc := range cases {
		gts, width := Capability(tc.cfg)
		if gts != tc.capGTs || width != tc.capWidth {
			t.Errorf("%s: capability %v x%d, want %v x%d", tc.name, gts, width, tc.capGTs, tc.capWidth)
		}
		s, w := DownTrain(tc.cfg)
		if s != tc.speedLow || w != tc.widthLo {
			t.Errorf("%s: DownTrain speed=%v width=%v, want %v %v", tc.name, s, w, tc.speedLow, tc.widthLo)
		}
	}
}

// mkTreeDev writes one PCI function at rel under root/devices and links it
// from bus/pci/devices, the way the kernel lays sysfs out.
func mkTreeDev(t *testing.T, root, rel string, attrs map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "devices", rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range attrs {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bus := filepath.Join(root, "bus/pci/devices")
	if err := os.MkdirAll(bus, 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(bus, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bus, filepath.Base(rel))); err != nil {
		t.Fatal(err)
	}
}

// A Gen5 x16 device under a Gen5 x16 port that trained to Gen4: ReadLink must
// pick up the port's maximum, the capability is Gen5 x16, and the speed half
// is a down-train while the negotiated value is kept as evidence.
func TestReadLinkUpstreamPortCapability(t *testing.T) {
	root := t.TempDir()
	port := "pci0000:3a/0000:3a:00.0"
	mkTreeDev(t, root, port, map[string]string{
		"current_link_speed": gen4, "current_link_width": "16",
		"max_link_speed": gen5, "max_link_width": "16",
	})
	mkTreeDev(t, root, port+"/0000:3b:00.0", map[string]string{
		"current_link_speed": gen4, "current_link_width": "16",
		"max_link_speed": gen5, "max_link_width": "16",
	})

	c, err := ReadLink(root, "0000:3b:00.0")
	if err != nil {
		t.Fatal(err)
	}
	if c.PortMaxSpeed != gen5 || c.PortMaxWidth != 16 {
		t.Fatalf("upstream port max %q x%d, want %q x16", c.PortMaxSpeed, c.PortMaxWidth, gen5)
	}
	if got, want := CapacityBPS(c), EffectiveBPS(32, 16); got != want {
		t.Fatalf("capacity %d, want Gen5 x16 %d", got, want)
	}
	if got, want := NegotiatedBPS(c), EffectiveBPS(16, 16); got != want {
		t.Fatalf("negotiated %d, want Gen4 x16 %d", got, want)
	}
	if speed, width := DownTrain(c); !speed || width {
		t.Fatalf("DownTrain speed=%v width=%v, want a speed down-train only", speed, width)
	}

	// The root port hangs off a host bridge, not a PCI function: no port max.
	rp, err := ReadLink(root, "0000:3a:00.0")
	if err != nil {
		t.Fatal(err)
	}
	if rp.PortMaxSpeed != "" || rp.PortMaxWidth != 0 {
		t.Fatalf("root port must have no upstream port: %+v", rp)
	}
}

package pci

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// mkDev writes the sysfs attributes of one PCI device under root.
func mkDev(t *testing.T, root, bdf string, attrs map[string]string, cfg []byte) {
	t.Helper()
	dir := filepath.Join(root, "bus/pci/devices", bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range attrs {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if cfg != nil {
		if err := os.WriteFile(filepath.Join(dir, "config"), cfg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// acsATSConfig builds config space with ACS P2P request redirect on and ATS
// enabled, so both capabilities are present and set.
func acsATSConfig() []byte {
	cfg := make([]byte, 0x200)
	binary.LittleEndian.PutUint16(cfg[0x100:], 0x000D)
	binary.LittleEndian.PutUint16(cfg[0x102:], 0x110<<4)
	binary.LittleEndian.PutUint16(cfg[0x106:], 0x0004)
	binary.LittleEndian.PutUint16(cfg[0x110:], 0x000F)
	binary.LittleEndian.PutUint16(cfg[0x112:], 0)
	binary.LittleEndian.PutUint16(cfg[0x116:], 0x8000)
	return cfg
}

func TestReadLinkFull(t *testing.T) {
	root := t.TempDir()
	mkDev(t, root, "0000:3c:00.0", map[string]string{
		"current_link_speed": "16.0 GT/s PCIe",
		"max_link_speed":     "16.0 GT/s PCIe",
		"current_link_width": "4",
		"max_link_width":     "16",
		"numa_node":          "1",
	}, acsATSConfig())

	c, err := ReadLink(root, "0000:3c:00.0")
	if err != nil {
		t.Fatal(err)
	}
	if c.BDF != "0000:3c:00.0" || c.Speed != "16.0 GT/s PCIe" || c.MaxSpeed != "16.0 GT/s PCIe" {
		t.Fatalf("speeds %+v", c)
	}
	if c.Width != 4 || c.MaxWidth != 16 {
		t.Fatalf("width=%d max=%d, want 4/16 (a down-trained link)", c.Width, c.MaxWidth)
	}
	if c.NUMA != 1 {
		t.Fatalf("NUMA=%d, want 1", c.NUMA)
	}
	if !c.ACSKnown || !c.ACSEnabled {
		t.Fatalf("ACS %+v, want known and on", c)
	}
	if !c.ATSKnown || !c.ATSEnabled {
		t.Fatalf("ATS %+v, want known and on", c)
	}
}

func TestReadLinkOptionalFilesMissing(t *testing.T) {
	root := t.TempDir()
	// Only current_link_speed: everything else stays at its unknown value.
	mkDev(t, root, "0000:3c:00.0", map[string]string{"current_link_speed": "8.0 GT/s PCIe"}, nil)
	c, err := ReadLink(root, "0000:3c:00.0")
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxSpeed != "" || c.MaxWidth != 0 || c.Width != 0 {
		t.Fatalf("missing files must stay unknown: %+v", c)
	}
	if c.NUMA != -1 {
		t.Fatalf("NUMA=%d, want -1 when numa_node is absent", c.NUMA)
	}
	if c.ACSKnown || c.ATSKnown {
		t.Fatalf("no config space must leave ACS/ATS unknown: %+v", c)
	}
}

// A NUMA-less device reports -1, which must not be parsed as an unsigned 0.
func TestReadLinkNegativeNUMA(t *testing.T) {
	root := t.TempDir()
	mkDev(t, root, "0000:3c:00.0", map[string]string{
		"current_link_speed": "16.0 GT/s PCIe",
		"numa_node":          "-1",
	}, nil)
	c, err := ReadLink(root, "0000:3c:00.0")
	if err != nil {
		t.Fatal(err)
	}
	if c.NUMA != -1 {
		t.Fatalf("NUMA=%d, want -1", c.NUMA)
	}
}

func TestReadLinkMissingDeviceIsError(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadLink(root, "0000:3c:00.0"); err == nil {
		t.Fatal("a device with no current_link_speed must be an error")
	}
	if _, err := ReadLink(root, "rp.0"); err == nil {
		t.Fatal("a non-BDF must be an error")
	}
}

// nvidia-smi prints the domain with %08x; sysfs names its directories with
// %04x, so an 8-digit BDF has to be narrowed before it is used as a path.
func TestReadLinkAcceptsEightDigitDomain(t *testing.T) {
	root := t.TempDir()
	mkDev(t, root, "0000:3c:00.0", map[string]string{"current_link_speed": "16.0 GT/s PCIe"}, nil)
	c, err := ReadLink(root, "00000000:3C:00.0")
	if err != nil {
		t.Fatal(err)
	}
	if c.BDF != "0000:3c:00.0" {
		t.Fatalf("BDF=%q, want the canonical sysfs spelling", c.BDF)
	}
}

func TestCanonicalBDF(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0000:18:00.0", "0000:18:00.0"},
		{"00000000:18:00.0", "0000:18:00.0"},
		{"18:00.0", "0000:18:00.0"},
		{"0000:3C:00.1", "0000:3c:00.1"},
		{"00010000:18:00.0", ""}, // domain > 0xffff must not be truncated to 0000
		{"0001:18:00.0", "0001:18:00.0"},
		{"00000001:3C:00.0", "0001:3c:00.0"},    // nvidia-smi's 8-digit domain, non-zero
		{" 00000000:3C:00.0\n", "0000:3c:00.0"}, // trimmed, lower-cased
		{"rp.0", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := CanonicalBDF(tc.in); got != tc.want {
			t.Errorf("CanonicalBDF(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	// One spelling per function: every form of a domain-0 address meets,
	// and the same bus:dev.fn in another domain stays apart.
	if a, b := CanonicalBDF("00000000:3C:00.0"), CanonicalBDF("3c:00.0"); a != b {
		t.Errorf("domain 0 spellings differ: %q vs %q", a, b)
	}
	if a, b := CanonicalBDF("0000:3c:00.0"), CanonicalBDF("0001:3c:00.0"); a == b {
		t.Errorf("domains 0 and 1 collide on %q", a)
	}
}

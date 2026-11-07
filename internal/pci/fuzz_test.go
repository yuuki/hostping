package pci

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// extCaps lays out a 4 KiB config space whose extended capability list holds
// the given (id, control) entries in order, 0x40 apart from 0x100.
func extCaps(caps ...[2]uint16) []byte {
	cfg := make([]byte, 0x1000)
	for i, c := range caps {
		off := 0x100 + 0x40*i
		next := 0
		if i+1 < len(caps) {
			next = off + 0x40
		}
		binary.LittleEndian.PutUint16(cfg[off:], c[0])
		binary.LittleEndian.PutUint16(cfg[off+2:], uint16(next<<4))
		binary.LittleEndian.PutUint16(cfg[off+6:], c[1])
	}
	return cfg
}

// FuzzParseConfig walks arbitrary config space (sysfs config is whatever the
// device and kernel expose, and the capability list is a linked list read
// from it): it never panics or loops, an enabled bit is never reported for a
// capability that was not found, and a buffer too short for the first
// extended capability says nothing.
func FuzzParseConfig(f *testing.F) {
	f.Add(extCaps([2]uint16{0x000D, 0x0001}, [2]uint16{0x000F, 0x8000}))
	f.Add(extCaps([2]uint16{0x000D, 0x003c}))
	f.Add(extCaps([2]uint16{0x0001, 0}, [2]uint16{0x000F, 0}))
	self := extCaps([2]uint16{0x0001, 0})
	binary.LittleEndian.PutUint16(self[0x102:], 0x100<<4) // points at itself
	f.Add(self)
	f.Add(make([]byte, 0x107))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, cfg []byte) {
		c := ParseConfig(cfg)
		if c.ACS && !c.ACSPresent {
			t.Fatalf("ACS enabled without an ACS capability: %+v", c)
		}
		if c.ATS && !c.ATSPresent {
			t.Fatalf("ATS enabled without an ATS capability: %+v", c)
		}
		if len(cfg) < 0x108 && c != (Caps{}) {
			t.Fatalf("%d-byte config space parsed as %+v, want nothing", len(cfg), c)
		}
		if again := ParseConfig(cfg); again != c {
			t.Fatalf("ParseConfig is not deterministic: %+v then %+v", c, again)
		}
	})
}

// FuzzGTs: a link speed is a finite, non-negative rate, 0 when unreadable,
// and the same whatever the case of its unit.
func FuzzGTs(f *testing.F) {
	for _, s := range []string{"16.0 GT/s PCIe", "8GT/s", "2.5 GT/s", "Unknown", "", "32.0 GT/s PCIe\n", "1e400 GT/s", "-8 GT/s", "..GT/s", "16.0.0 GT/s"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v := GTs(s)
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			t.Fatalf("GTs(%q) = %v, want a finite non-negative rate", s, v)
		}
		if !strings.Contains(strings.ToLower(s), "gt") && v != 0 {
			t.Fatalf("GTs(%q) = %v without a GT/s unit", s, v)
		}
		if u := GTs(strings.ToUpper(s)); isASCII(s) && u != v {
			t.Fatalf("GTs(%q) = %v but GTs(%q) = %v", s, v, strings.ToUpper(s), u)
		}
	})
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// FuzzCanonicalBDF: a PCI address from sysfs, a flag or nvidia-smi is either
// rejected or turned into the one sysfs spelling, which is a BDF and its own
// canonical form.
func FuzzCanonicalBDF(f *testing.F) {
	for _, s := range []string{"0000:3a:00.0", "00000000:17:00.0", "3C:00.0", " 0001:65:00.1 ", "10000:00:00.0", "rp.0", "", "0000:3a:00", "g000:3a:00.0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := CanonicalBDF(s)
		if out == "" {
			return
		}
		if !IsBDF(out) {
			t.Fatalf("CanonicalBDF(%q) = %q, not a BDF", s, out)
		}
		if out != strings.ToLower(out) {
			t.Fatalf("CanonicalBDF(%q) = %q, not lower case", s, out)
		}
		if again := CanonicalBDF(out); again != out {
			t.Fatalf("CanonicalBDF(%q) = %q, but CanonicalBDF(%q) = %q", s, out, out, again)
		}
	})
}

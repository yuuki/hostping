package pci

import (
	"encoding/binary"
	"testing"
)

func TestParseConfigACSATS(t *testing.T) {
	cfg := make([]byte, 0x200)
	// ACS at 0x100, next 0x110, control at +6 with source validation enable.
	binary.LittleEndian.PutUint16(cfg[0x100:], 0x000D)
	binary.LittleEndian.PutUint16(cfg[0x102:], 0x110<<4)
	binary.LittleEndian.PutUint16(cfg[0x106:], 0x0001)
	// ATS at 0x110, enable bit clear.
	binary.LittleEndian.PutUint16(cfg[0x110:], 0x000F)
	binary.LittleEndian.PutUint16(cfg[0x112:], 0)
	binary.LittleEndian.PutUint16(cfg[0x116:], 0)
	c := ParseConfig(cfg)
	if !c.ACSPresent || c.ACS {
		t.Fatalf("source-validation-only ACS must not count as redirect: %+v", c)
	}
	if !c.ATSPresent || c.ATS {
		t.Fatalf("ATS should be present and off: %+v", c)
	}
}

func TestParseConfigACSRedirect(t *testing.T) {
	cfg := make([]byte, 0x200)
	binary.LittleEndian.PutUint16(cfg[0x100:], 0x000D)
	binary.LittleEndian.PutUint16(cfg[0x102:], 0)
	binary.LittleEndian.PutUint16(cfg[0x106:], 0x0004) // P2P Request Redirect
	c := ParseConfig(cfg)
	if !c.ACSPresent || !c.ACS {
		t.Fatalf("P2P redirect ACS %+v", c)
	}
}

func TestIsBDF(t *testing.T) {
	if !IsBDF("0000:3a:00.0") || IsBDF("rp.0") {
		t.Fatal("BDF parse")
	}
}

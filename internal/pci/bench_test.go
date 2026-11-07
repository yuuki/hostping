package pci

import "testing"

// BenchmarkParseConfig times walking a config space whose ATS capability is
// the last of eight extended capabilities.
func BenchmarkParseConfig(b *testing.B) {
	cfg := extCaps([2]uint16{0x0001, 0}, [2]uint16{0x0002, 0}, [2]uint16{0x0003, 0}, [2]uint16{0x0004, 0},
		[2]uint16{0x000B, 0}, [2]uint16{0x000D, 0x001d}, [2]uint16{0x0010, 0}, [2]uint16{0x000F, 0x8000})
	b.ReportAllocs()
	for b.Loop() {
		ParseConfig(cfg)
	}
}

package baseline

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
)

// bandwidthUnits and latencyUnits are the spellings ParseBandwidth and
// ParseLatency accept, with what each multiplies its number by.
var bandwidthUnits = []struct {
	suffix string
	mult   float64
}{
	{"bps", 1}, {"kbps", 1e3}, {"Mbps", 1e6}, {"Gbps", 1e9}, {"Tbps", 1e12},
	{"Gb/s", 1e9}, {"GB", 1e9}, {"tb", 1e12},
}

var latencyUnits = []struct {
	suffix string
	mult   float64
}{
	{"ns", 1}, {"us", 1e3}, {"µs", 1e3}, {"ms", 1e6},
}

// checkScaled is the contract both parsers share for a number they accept: a
// value that fits uint64 is that value truncated, and one that does not is an
// error, never a wrapped or implementation-defined uint64.
func checkScaled(t *testing.T, s string, scaled float64, got uint64, err error) {
	t.Helper()
	if scaled >= 0x1p64 {
		if err == nil {
			t.Fatalf("%q = %d, want an overflow error (%g does not fit uint64)", s, got, scaled)
		}
		return
	}
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	if want := uint64(scaled); got != want {
		t.Fatalf("%q = %d, want %d", s, got, want)
	}
}

// FuzzParseBandwidthValue: every positive finite number with every unit is
// read as that number times the unit, or rejected when it does not fit.
func FuzzParseBandwidthValue(f *testing.F) {
	f.Add(270.0, uint8(3))
	f.Add(0.5, uint8(0))
	f.Add(-1.0, uint8(3))
	f.Add(math.Inf(1), uint8(0))
	f.Add(math.NaN(), uint8(0))
	f.Add(0x1p64, uint8(0)) // float64(math.MaxUint64): used to wrap to 2^63
	f.Add(1.8446744073709552e10, uint8(3))
	f.Fuzz(func(t *testing.T, v float64, unit uint8) {
		u := bandwidthUnits[int(unit)%len(bandwidthUnits)]
		s := strconv.FormatFloat(v, 'g', -1, 64) + u.suffix
		got, err := ParseBandwidth(s)
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			if err == nil {
				t.Fatalf("%q = %d, want an error", s, got)
			}
			return
		}
		checkScaled(t, s, v*u.mult, got, err)
	})
}

// FuzzParseLatencyValue is FuzzParseBandwidthValue for latencies, where 0 is
// a valid value.
func FuzzParseLatencyValue(f *testing.F) {
	f.Add(1400.0, uint8(0))
	f.Add(1.4, uint8(1))
	f.Add(0.0, uint8(3))
	f.Add(-1.0, uint8(0))
	f.Add(math.NaN(), uint8(0))
	f.Add(0x1p64, uint8(0))
	f.Fuzz(func(t *testing.T, v float64, unit uint8) {
		u := latencyUnits[int(unit)%len(latencyUnits)]
		s := strconv.FormatFloat(v, 'g', -1, 64) + u.suffix
		got, err := ParseLatency(s)
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			if err == nil {
				t.Fatalf("%q = %d, want an error", s, got)
			}
			return
		}
		checkScaled(t, s, v*u.mult, got, err)
	})
}

// FuzzParseSpecs: a --baseline value either fails or yields at least one
// distance, each with a bandwidth or a latency, and every entry it yields
// reads back the same from its canonical KEY=Nbps,Nns spelling.
func FuzzParseSpecs(f *testing.F) {
	for _, s := range []string{
		"local_mem=270Gbps,1400ns",
		"local_mem=270Gbps;remote_mem=200Gbps,1.8us",
		"cross_root=180Gb/s,cross_socket=150Gbps,2us",
		"same_switch=1400ns",
		"LOCAL_MEM = 1_000Mbps",
		"remote_mem=",
		"=270Gbps",
		"local_mem=0bps",
		"local_mem=18446744073709551616bps",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := ParseSpecs(s)
		if err != nil {
			return
		}
		if len(out) == 0 {
			t.Fatalf("ParseSpecs(%q) succeeded with no distance", s)
		}
		for d, b := range out {
			if b.BandwidthBPS == 0 && b.LatencyNs == 0 {
				t.Fatalf("ParseSpecs(%q): %v has neither bandwidth nor latency", s, d)
			}
			spec := d.String() + "="
			if b.BandwidthBPS > 0 {
				spec += fmt.Sprintf("%dbps", b.BandwidthBPS)
			}
			if b.LatencyNs > 0 {
				if b.BandwidthBPS > 0 {
					spec += ","
				}
				spec += fmt.Sprintf("%dns", b.LatencyNs)
			}
			d2, b2, err := ParseSpec(spec)
			if err != nil {
				t.Fatalf("ParseSpecs(%q): %v does not read back from %q: %v", s, d, spec, err)
			}
			if d2 != d || b2.BandwidthBPS != b.BandwidthBPS || b2.LatencyNs != b.LatencyNs {
				t.Fatalf("ParseSpecs(%q): %v=%+v reads back from %q as %v=%+v", s, d, b, spec, d2, b2)
			}
		}
	})
}

// FuzzParseJSON: a --baseline-file body either fails or yields at least one
// distance or table section.
func FuzzParseJSON(f *testing.F) {
	if b, err := os.ReadFile("../analyzer/testdata/subclass_baseline.json"); err == nil {
		f.Add(b)
	}
	for _, s := range []string{
		`{"local_mem":{"bandwidth":"270Gbps","latency":"1400ns"}}`,
		`{"remote_mem":{"bandwidth_bps":200000000000,"latency_ns":1800}}`,
		`{"cross_root":{"sub_classes":[{"name":"a","bandwidth":"150Gbps","root_port_pairs":[["rp.0","rp.1"]]}]}}`,
		`{"tables":{"200Gbps":{"local_mem":{"bandwidth":"190Gbps"}}}}`,
		`{"tables":{}}`,
		`{"local_mem":{"bandwith":"270Gbps"}}`, //nolint:misspell // an unknown (misspelt) key must be rejected
		`{"local_mem":{"bandwidth_bps":1,"bandwidth":"1bps"}}`,
		`{}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ov, err := parseJSON(data)
		if err != nil {
			return
		}
		if len(ov.Distances) == 0 && len(ov.Tables) == 0 {
			t.Fatalf("parseJSON(%q) succeeded with no distance and no table", data)
		}
	})
}

package config

import (
	"math"
	"strconv"
	"testing"
)

// capacityUnits are spellings ParseCapacity accepts, with what each
// multiplies its number by before bytes become bits.
var capacityUnits = []struct {
	suffix string
	mult   float64
	bytes  bool
}{
	{"bps", 1, false}, {"Gbps", 1e9, false}, {"Gb/s", 1e9, false}, {"Tbps", 1e12, false},
	{"GB/s", 1e9, true}, {"GiB/s", 1 << 30, true}, {"MBps", 1e6, true}, {"kb/s", 1e3, false},
}

// FuzzParseCapacityValue: every positive finite number with every unit is
// read as that many bits per second, or rejected when that is below 1 or
// does not fit uint64, never wrapped.
func FuzzParseCapacityValue(f *testing.F) {
	f.Add(400.0, uint8(1))
	f.Add(46.5, uint8(5))
	f.Add(0.1, uint8(0))
	f.Add(-1.0, uint8(1))
	f.Add(math.NaN(), uint8(0))
	f.Add(0x1p64, uint8(0)) // float64(math.MaxUint64): used to wrap to 2^63
	f.Add(0x1p61, uint8(4))
	f.Fuzz(func(t *testing.T, v float64, unit uint8) {
		u := capacityUnits[int(unit)%len(capacityUnits)]
		s := strconv.FormatFloat(v, 'g', -1, 64) + u.suffix
		got, err := ParseCapacity(s)
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			if err == nil {
				t.Fatalf("%q = %d, want an error", s, got)
			}
			return
		}
		bits := v * u.mult
		if u.bytes {
			bits *= 8
		}
		if bits < 1 || bits >= 0x1p64 {
			if err == nil {
				t.Fatalf("%q = %d, want an out-of-range error (%g bps)", s, got, bits)
			}
			return
		}
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if want := uint64(bits); got != want {
			t.Fatalf("%q = %d, want %d", s, got, want)
		}
	})
}

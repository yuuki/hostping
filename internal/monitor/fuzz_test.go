package monitor

import (
	"fmt"
	"math"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
)

// FuzzParseEthtoolStats: whatever ethtool -S prints, every counter filed as
// a drop is one by name, and the families reported are the ones filed.
func FuzzParseEthtoolStats(f *testing.F) {
	f.Add(ethtoolPauseOut)
	f.Add("NIC statistics:\n     rx_discards_phy: 12\n     rx_prio3_discards: 4\n     tx_dropped: 1\n     rx_bytes: 99\n")
	f.Add("     RX_PRIO0_PAUSE_DURATION : 18446744073709551615\n     rx_prio0_pause_duration: 1\n")
	f.Add("no colon here\n:\n x: -1\n port_rcv_errors: 5\n symbol_error: 7\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, out string) {
		var c model.RNICCounters
		var acc pauseAccum
		pause, drops := parseEthtoolStats(out, &c, &acc)
		if drops != (len(c.Drops) > 0) {
			t.Fatalf("drops=%v but %d drop counters filed", drops, len(c.Drops))
		}
		for name := range c.Drops {
			if !looksLikeDrop(name) {
				t.Fatalf("%q filed as a drop", name)
			}
		}
		if !pause && acc != (pauseAccum{}) {
			t.Fatalf("pause=false but pause durations accumulated: %+v", acc)
		}
		if c.TxPauseDurationUs != 0 || c.RxPauseDurationUs != 0 {
			t.Fatalf("pause durations written before flush: %+v", c)
		}
	})
}

// FuzzParseNvidiaGPUs: every GPU nvidia-smi reports is keyed by its sysfs
// BDF, and its compute mode is one hostping knows or absent.
func FuzzParseNvidiaGPUs(f *testing.F) {
	f.Add(smiOut)
	f.Add("00000000:3C:00.0, 10, Default\n00000001:3C:00.0, 90, Exclusive_Process\n")
	f.Add("00000000:17:00.0, [N/A], Prohibited\n00000000:65:00.0, 3, [Not Supported]\n")
	f.Add("garbage\n,\n00000000:17:00.0\n0000:17:00.0,,exclusive_thread\n")
	f.Fuzz(func(t *testing.T, out string) {
		s := parseNvidiaGPUs(out)
		for bdf := range s.util {
			if pci.CanonicalBDF(bdf) != bdf {
				t.Fatalf("utilization keyed by %q, not a canonical BDF", bdf)
			}
		}
		for bdf, m := range s.mode {
			if pci.CanonicalBDF(bdf) != bdf {
				t.Fatalf("compute mode keyed by %q, not a canonical BDF", bdf)
			}
			switch m {
			case model.ComputeModeDefault, model.ComputeModeExclusiveProcess, model.ComputeModeExclusiveThread, model.ComputeModeProhibited:
			default:
				t.Fatalf("compute mode %q of %s", m, bdf)
			}
		}
	})
}

// FuzzParseDmonPCIe: a GPU PCIe throughput is a finite, non-negative rate of
// a GPU the index query named, or absent (unknown, never a made-up value).
func FuzzParseDmonPCIe(f *testing.F) {
	f.Add(dmonOut)
	f.Add("# gpu  rxpci  txpci\n    0   nan  10\n    1   10   +Inf\n")
	f.Add("# txpci gpu extra rxpci\n 5 1 x 7\n")
	f.Add("    0  10  20\n")
	bdf := map[string]string{"0": "0000:18:00.0", "1": "0000:19:00.0", "2": "0000:38:00.0"}
	known := map[string]bool{}
	for _, b := range bdf {
		known[b] = true
	}
	f.Fuzz(func(t *testing.T, out string) {
		got := parseDmonPCIe(out, bdf)
		if got != nil && len(got) == 0 {
			t.Fatal("an empty result must be nil (unknown)")
		}
		for b, v := range got {
			if !known[b] {
				t.Fatalf("throughput for %q, which the index query did not name", b)
			}
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				t.Fatalf("throughput of %s = %v from %q", b, v, out)
			}
		}
	})
}

// FuzzEncodeEvent: a value encoded into a PMU format field reads back from
// the field's bits, it fails exactly when it is wider than the field, and it
// touches no other config word.
func FuzzEncodeEvent(f *testing.F) {
	f.Add("config:0-7,32-35", uint64(0x1ff))
	f.Add("config1:0-3", uint64(0xa))
	f.Add("config2:0-63", uint64(math.MaxUint64))
	f.Add("config:18", uint64(2))
	f.Add("config:8-15", uint64(0x100))
	f.Add("config:0-7,4-11", uint64(1))
	f.Fuzz(func(t *testing.T, spec string, v uint64) {
		field, err := parseFormat(spec)
		if err != nil {
			return
		}
		var used uint64
		width := 0
		overlap := false
		for _, r := range field.ranges {
			if r[0] < 0 || r[1] < r[0] || r[1] > 63 {
				t.Fatalf("parseFormat(%q) range %v", spec, r)
			}
			n := r[1] - r[0] + 1
			width += n
			bits := (^uint64(0) >> (64 - n)) << r[0]
			overlap = overlap || used&bits != 0
			used |= bits
		}
		if overlap {
			return // no kernel format has overlapping ranges; the bits alias
		}
		var words [3]uint64
		words[0], words[1], words[2], err = encodeEvent(fmt.Sprintf("x=%#x", v), map[string]formatField{"x": field})
		fits := width >= 64 || v>>width == 0
		if !fits {
			if err == nil {
				t.Fatalf("%#x encoded into the %d-bit field %q", v, width, spec)
			}
			return
		}
		if err != nil {
			t.Fatalf("%#x into %q: %v", v, spec, err)
		}
		for i, w := range words {
			if i != field.word && w != 0 {
				t.Fatalf("%#x into %q wrote config word %d: %#x", v, spec, i, w)
			}
		}
		var back uint64
		shift := 0
		for _, r := range field.ranges {
			n := r[1] - r[0] + 1
			back |= ((words[field.word] >> r[0]) & (^uint64(0) >> (64 - n))) << shift
			shift += n
		}
		if back != v {
			t.Fatalf("%#x into %q reads back as %#x (words %#x)", v, spec, back, words)
		}
	})
}

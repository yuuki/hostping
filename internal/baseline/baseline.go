package baseline

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// PathBaseline is the idle-host loopback bandwidth and small-message latency
// for one communication distance (NSDI'23 Fig.4 / §5).
//
// SubClasses only ever come from a baseline file: the Fig.4 defaults and the
// --baseline flags have none.
type PathBaseline struct {
	BandwidthBPS uint64
	LatencyNs    uint64
	SubClasses   []SubClass
}

// DefaultRateBPS is the line rate assumed when the RNIC's own is unknown.
const DefaultRateBPS uint64 = 200_000_000_000

// Fig4BusBPS is the effective bandwidth of the PCIe Gen4 x16 bus Fig.4 was
// measured behind. It is the highest rate the Fig.4 ratios are scaled to.
const Fig4BusBPS uint64 = 252_000_000_000

// DerivedRate is the rate the Fig.4 ratios scale from: the lower of the wire
// and the PCIe hop feeding it, with the hop held to the Fig.4 bus.
//
// Fig.4 was measured on 200G RNICs behind a Gen4 x16 bus, where 0.985×line is
// ~197 Gb/s and sits well under the ~252 Gb/s the bus delivers, so scaling by
// line rate alone is meaningful. Once the wire outruns the bus that inverts:
// on a 400G RNIC the same ratio asks for 394 Gb/s from a hop that cannot carry
// it, every host-DRAM loopback falls short, and a healthy host is diagnosed as
// ten failed RNIC PCIe links. Every intra-host loopback from an RNIC crosses
// that RNIC's own PCIe hop, so clamping to it keeps the default reachable.
// hopCapBPS of 0 means discovery did not report one and nothing is clamped.
//
// A hop wider than the Fig.4 bus is held to Fig4BusBPS. A Gen5 x16 slot can
// carry ~504 Gb/s, but host-DRAM loopback from a healthy 400G RNIC in one can
// top out around 270 Gb/s, far under 0.985×400G = 394 Gb/s: past the Gen4 bus
// the loopback is bounded by more than the slot, and Fig.4 says nothing about
// by how much. Scaling past it would mark a healthy host's memory paths
// abnormal, so the default stays in the regime the paper measured. The cost is sensitivity: a
// Gen5 slot down-trained to Gen4 still carries what the Gen4 default asks for,
// which is why the analyzer checks the negotiated link against its capability
// directly, and why an operator on such a host should measure and pass
// --baseline to tighten the bandwidth floor.
func DerivedRate(lineRate, hopCapBPS uint64) uint64 {
	if lineRate == 0 {
		lineRate = DefaultRateBPS
	}
	if hopCapBPS > Fig4BusBPS {
		hopCapBPS = Fig4BusBPS
	}
	if hopCapBPS > 0 && hopCapBPS < lineRate {
		return hopCapBPS
	}
	return lineRate
}

// Defaults are the Fig.4 numbers scaled to lineRate alone, with no bus clamp.
func Defaults(lineRate uint64) map[model.Distance]PathBaseline {
	return DefaultsFor(lineRate, 0)
}

// DefaultsFor are the measured Fig.4 GDR numbers plus local/remote memory at
// the derived rate (see DerivedRate).
func DefaultsFor(lineRate, hopCapBPS uint64) map[model.Distance]PathBaseline {
	rate := DerivedRate(lineRate, hopCapBPS)
	local := uint64(float64(rate) * 0.985) // ~197 Gb/s on 200 Gb/s RNICs
	scale := float64(rate) / float64(DefaultRateBPS)
	if scale <= 0 {
		scale = 1
	}
	return map[model.Distance]PathBaseline{
		model.DistanceSameSwitch:  {BandwidthBPS: local, LatencyNs: 1000},
		model.DistanceMultiSwitch: {BandwidthBPS: local, LatencyNs: 1050},
		model.DistanceCrossRoot:   {BandwidthBPS: uint64(125_500_000_000 * scale), LatencyNs: 2200},
		model.DistanceCrossSocket: {BandwidthBPS: uint64(116_400_000_000 * scale), LatencyNs: 2400},
		model.DistanceLocalMem:    {BandwidthBPS: local, LatencyNs: 1000},
		model.DistanceRemoteMem:   {BandwidthBPS: local, LatencyNs: 1400},
	}
}

func ForPath(b map[model.Distance]PathBaseline, d model.Distance) PathBaseline {
	if v, ok := b[d]; ok {
		return v
	}
	if v, ok := b[model.DistanceSameSwitch]; ok {
		return v
	}
	return PathBaseline{}
}

// Apply copies base and overlays user-set fields. Zero BandwidthBPS / LatencyNs
// in override mean "leave the default", and no SubClasses keeps base's: a
// --baseline flag replaces a distance's own values and leaves the file's
// sub-classes of that distance in force.
func Apply(base, override map[model.Distance]PathBaseline) map[model.Distance]PathBaseline {
	out := make(map[model.Distance]PathBaseline, len(base)+len(override))
	for d, v := range base {
		out[d] = v
	}
	for d, ov := range override {
		cur := out[d]
		if ov.BandwidthBPS > 0 {
			cur.BandwidthBPS = ov.BandwidthBPS
		}
		if ov.LatencyNs > 0 {
			cur.LatencyNs = ov.LatencyNs
		}
		if len(ov.SubClasses) > 0 {
			cur.SubClasses = ov.SubClasses
		}
		out[d] = cur
	}
	return out
}

// ForRNIC scales Fig.4 numbers to min(line rate, PCIe hop capacity), then
// applies user overrides, which always win over the derived default.
func ForRNIC(n model.RNIC, override map[model.Distance]PathBaseline) map[model.Distance]PathBaseline {
	return Apply(DefaultsFor(n.LineRate, n.PCIeCapBPS), override)
}

// Table is the effective baseline for one line rate and PCIe hop capacity,
// with default vs user tags.
func Table(lineRate, hopCapBPS uint64, override map[model.Distance]PathBaseline) model.BaselineTable {
	def := DefaultsFor(lineRate, hopCapBPS)
	eff := Apply(def, override)
	t := model.BaselineTable{LineRateBPS: lineRate, PCIeCapBPS: hopCapBPS, DerivedFromBPS: DerivedRate(lineRate, hopCapBPS)}
	if t.LineRateBPS == 0 {
		t.LineRateBPS = DefaultRateBPS
	}
	for _, d := range model.Distances() {
		ov := override[d]
		bwSrc, latSrc := model.BaselineSourceDefault, model.BaselineSourceDefault
		if ov.BandwidthBPS > 0 {
			bwSrc = model.BaselineSourceUser
		}
		if ov.LatencyNs > 0 {
			latSrc = model.BaselineSourceUser
		}
		e := eff[d]
		entry := model.BaselineEntry{
			Distance:        d.Key(),
			BandwidthBPS:    e.BandwidthBPS,
			LatencyNs:       e.LatencyNs,
			BandwidthSource: bwSrc,
			LatencySource:   latSrc,
		}
		for _, sc := range e.SubClasses {
			v, _ := e.ForPair(sc.RootPortPairs[0])
			entry.SubClasses = append(entry.SubClasses, model.BaselineSubClass{
				Name:          sc.Name,
				BandwidthBPS:  v.BandwidthBPS,
				LatencyNs:     v.LatencyNs,
				RootPortPairs: slices.Clone(sc.RootPortPairs),
			})
		}
		t.Entries = append(t.Entries, entry)
	}
	return t
}

// Tables returns one effective table per distinct (line rate, PCIe hop
// capacity) pair. Two RNICs on the same wire but different buses derive
// different baselines, so the pair — not the line rate alone — keys the table.
// Each table takes the overlay's section for its key, or the top level.
func Tables(rnics []model.RNIC, ov Overlay) []model.BaselineTable {
	type key struct{ rate, hop uint64 }
	seen := map[key]struct{}{}
	var keys []key
	for _, n := range rnics {
		k := key{rate: n.LineRate, hop: n.PCIeCapBPS}
		if k.rate == 0 {
			k.rate = DefaultRateBPS
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		keys = []key{{rate: DefaultRateBPS}}
	}
	out := make([]model.BaselineTable, 0, len(keys))
	for _, k := range keys {
		m, sec := ov.Of(TableKey(k.rate, k.hop))
		t := Table(k.rate, k.hop, m)
		t.Section = sec
		out = append(out, t)
	}
	return out
}

// DescribeSources is the start-up line naming which distances take their
// bandwidth and latency from --baseline-file / --baseline and which fall back
// to the Fig.4 default, or "" when the operator set nothing (every distance
// is Fig.4, and each run's "hostping baseline" line already says so). Each
// "tables" section of the file adds one more line, naming its key; the lines
// are joined with newlines.
//
// Unset latency is not a Fig.4 fallback: the Fig.4 latencies are never used
// for the latency verdict, which compares a path with its siblings instead.
func DescribeSources(ov Overlay) string {
	var lines []string
	if line := describeSources(ov.Distances); line != "" {
		lines = append(lines, "baselines: "+line)
	}
	for _, k := range ov.SectionKeys() {
		if line := describeSources(ov.Tables[k]); line != "" {
			lines = append(lines, "baselines: tables section "+k+": "+line)
		}
	}
	return strings.Join(lines, "\n")
}

func describeSources(override map[model.Distance]PathBaseline) string {
	var bwUser, bwDef, latUser []string
	for _, d := range model.Distances() {
		ov := override[d]
		if ov.BandwidthBPS > 0 {
			bwUser = append(bwUser, d.Key())
		} else {
			bwDef = append(bwDef, d.Key())
		}
		if ov.LatencyNs > 0 {
			latUser = append(latUser, d.Key())
		}
	}
	subs := describeSubClasses(override)
	if len(bwUser) == 0 && len(latUser) == 0 && subs == "" {
		return ""
	}
	list := func(v []string) string {
		if len(v) == 0 {
			return "none"
		}
		return strings.Join(v, ",")
	}
	line := fmt.Sprintf("bandwidth from --baseline-file/--baseline for %s; Fig.4 default for %s; latency from --baseline-file/--baseline for %s (other distances: sibling comparison)",
		list(bwUser), list(bwDef), list(latUser))
	if subs != "" {
		line += "; sub-classes from --baseline-file: " + subs
	}
	return line
}

// TprocNs is the RNIC+CPU posting overhead used when synthesizing loopback times
// from intra-host latency (paper Eq.1). Fig.4's 1.0 µs same-switch figure is
// Tproc+Lathost; we split it as 400 ns Tproc + 600 ns host.
const TprocNs uint64 = 400

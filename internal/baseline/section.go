package baseline

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// TablesField is the baseline-file key of the per-table sections.
const TablesField = "tables"

// Overlay is what --baseline-file and --baseline set: per-distance values
// for every RNIC, and optional per-table sections that replace them for the
// RNICs deriving one Fig.4 table.
//
// A host can carry RNICs that derive different Fig.4 tables, e.g. a
// lower-line-rate RNIC in a narrow slot next to faster RNICs on wide links.
// One value per distance cannot serve both: calibrated on the majority, it
// puts every path of the minority RNIC far below its distance's baseline.
// hostpingctl calibrate writes the majority table's values at the top level
// and each other table's under "tables", keyed by TableKey.
type Overlay struct {
	// Distances are the top-level values, for every RNIC whose table has no
	// section.
	Distances map[model.Distance]PathBaseline
	// Tables are the sections, keyed by TableKey. A section replaces
	// Distances for its RNICs as a whole: a distance it leaves out keeps the
	// RNIC's own Fig.4 default, not the top-level value, which was measured
	// on another table.
	Tables map[string]map[model.Distance]PathBaseline
}

// Uniform is an overlay with no sections: m applies to every RNIC.
func Uniform(m map[model.Distance]PathBaseline) Overlay {
	return Overlay{Distances: m}
}

// TableKey names the Fig.4 table an RNIC derives: its line rate and the
// capacity of its PCIe hop, the pair baseline.Tables and the "hostping
// baseline" log key tables by, e.g. "line_rate=200Gbps,pcie_hop=126.03Gbps".
// A line rate of 0 is DefaultRateBPS, and an unknown hop (0) is left out.
// Values are in Gb/s rounded to two decimals, without trailing zeros, so a
// key is stable and ParseTableKey accepts exactly what this returns.
func TableKey(lineRate, hopCapBPS uint64) string {
	if lineRate == 0 {
		lineRate = DefaultRateBPS
	}
	k := "line_rate=" + keyBPS(lineRate)
	if hopCapBPS > 0 {
		k += ",pcie_hop=" + keyBPS(hopCapBPS)
	}
	return k
}

// RNICTable is TableKey for n.
func RNICTable(n model.RNIC) string {
	return TableKey(n.LineRate, n.PCIeCapBPS)
}

func keyBPS(v uint64) string {
	g := math.Round(float64(v)/1e7) / 100
	return strconv.FormatFloat(g, 'f', -1, 64) + "Gbps"
}

// ValidateTableKey accepts a key exactly as TableKey writes it. A key in any
// other spelling would never match an RNIC, and its section would be
// silently ignored.
func ValidateTableKey(k string) error {
	rate, hop, ok := strings.Cut(k, ",")
	rv, okRate := strings.CutPrefix(rate, "line_rate=")
	if !okRate {
		return fmt.Errorf("table key %q: want line_rate=<N>Gbps[,pcie_hop=<N>Gbps]", k)
	}
	lr, err := ParseBandwidth(rv)
	if err != nil {
		return fmt.Errorf("table key %q: %w", k, err)
	}
	var hc uint64
	if ok {
		hv, okHop := strings.CutPrefix(hop, "pcie_hop=")
		if !okHop {
			return fmt.Errorf("table key %q: want line_rate=<N>Gbps[,pcie_hop=<N>Gbps]", k)
		}
		if hc, err = ParseBandwidth(hv); err != nil {
			return fmt.Errorf("table key %q: %w", k, err)
		}
	}
	if want := TableKey(lr, hc); want != k {
		return fmt.Errorf("table key %q: write it as %q", k, want)
	}
	return nil
}

// Of is the overlay for an RNIC on table key: the section for key, or the
// top-level values when there is none, and the key of the section used (""
// for the top level).
func (o Overlay) Of(key string) (map[model.Distance]PathBaseline, string) {
	if s, ok := o.Tables[key]; ok {
		return s, key
	}
	return o.Distances, ""
}

// ForRNIC is the overlay for n (see Of).
func (o Overlay) ForRNIC(n model.RNIC) map[model.Distance]PathBaseline {
	m, _ := o.Of(RNICTable(n))
	return m
}

// Effective is n's full baseline table: the Fig.4 defaults for n's table
// with n's overlay applied (see ForRNIC).
func (o Overlay) Effective(n model.RNIC) map[model.Distance]PathBaseline {
	return ForRNIC(n, o.ForRNIC(n))
}

// apply overlays m on the top level and on every section.
func (o Overlay) apply(m map[model.Distance]PathBaseline) Overlay {
	out := Overlay{Distances: Apply(o.Distances, m)}
	if len(o.Tables) > 0 {
		out.Tables = make(map[string]map[model.Distance]PathBaseline, len(o.Tables))
		for k, s := range o.Tables {
			out.Tables[k] = Apply(s, m)
		}
	}
	return out
}

// SectionKeys are the section keys, sorted.
func (o Overlay) SectionKeys() []string {
	return slices.Sorted(maps.Keys(o.Tables))
}

// DescribeTables is the start-up line saying which of this host's Fig.4
// tables take their values from which part of the baseline file, and which
// sections match no RNIC here, or "" when the file has no sections.
func DescribeTables(o Overlay, rnics []model.RNIC) string {
	if len(o.Tables) == 0 {
		return ""
	}
	var keys []string
	devs := map[string][]string{}
	for _, n := range rnics {
		k := RNICTable(n)
		if _, ok := devs[k]; !ok {
			keys = append(keys, k)
		}
		devs[k] = append(devs[k], n.ID)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		src := "top-level values"
		if _, ok := o.Tables[k]; ok {
			src = "tables section"
		}
		parts = append(parts, fmt.Sprintf("%s (%s) from %s", k, strings.Join(devs[k], ","), src))
	}
	line := "baselines: per Fig.4 table: " + strings.Join(parts, "; ")
	var unused []string
	for _, k := range o.SectionKeys() {
		if _, ok := devs[k]; !ok {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 {
		line += "; tables sections matching no RNIC here, ignored: " + strings.Join(unused, ", ")
	}
	return line
}

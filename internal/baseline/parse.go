package baseline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuuki/hostping/internal/model"
)

// Load merges a JSON file (if path is set) with flag specs. Later specs win,
// and a spec overrides the file's top level and every "tables" section alike.
func Load(file string, specs []string) (Overlay, error) {
	out := Overlay{Distances: map[model.Distance]PathBaseline{}}
	if file != "" {
		ov, err := LoadFile(file)
		if err != nil {
			return Overlay{}, err
		}
		out = ov.apply(nil)
	}
	for _, s := range specs {
		m, err := ParseSpecs(s)
		if err != nil {
			return Overlay{}, err
		}
		out = out.apply(m)
	}
	return out, nil
}

// LoadFile reads a JSON object of distance → {bandwidth_bps|bandwidth,
// latency_ns|latency, sub_classes}. Each optional sub_classes element is
// {name, bandwidth_bps|bandwidth, latency_ns|latency, root_port_pairs}; see
// SubClass. The optional "tables" object maps a TableKey to an object of
// the same distance entries; see Overlay.
func LoadFile(path string) (Overlay, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Overlay{}, fmt.Errorf("baseline file: %w", err)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Overlay{}, fmt.Errorf("baseline file %s is empty", path)
	}
	if data[0] != '{' {
		return Overlay{}, fmt.Errorf("baseline file %s: must be a JSON object", path)
	}
	ov, err := parseJSON(data)
	if err != nil {
		return Overlay{}, fmt.Errorf("baseline file %s: %w", path, err)
	}
	return ov, nil
}

// fileValues are the value fields a distance entry and a sub-class share.
type fileValues struct {
	BandwidthBPS *uint64 `json:"bandwidth_bps"`
	LatencyNs    *uint64 `json:"latency_ns"`
	Bandwidth    string  `json:"bandwidth"`
	Latency      string  `json:"latency"`
}

type fileEntry struct {
	fileValues
	SubClasses []fileSubClass `json:"sub_classes"`
}

// fileSubClass is one learned sub-class of a distance entry (see SubClass).
type fileSubClass struct {
	Name string `json:"name"`
	fileValues
	RootPortPairs [][]string `json:"root_port_pairs"`
}

func parseJSON(data []byte) (Overlay, error) {
	raw := map[string]json.RawMessage{}
	if err := strictDecode(data, &raw); err != nil {
		return Overlay{}, err
	}
	var ov Overlay
	entries := map[string]fileEntry{}
	for k, v := range raw {
		if k == TablesField {
			continue
		}
		var e fileEntry
		if err := strictDecode(v, &e); err != nil {
			return Overlay{}, fmt.Errorf("%s: %w", k, err)
		}
		entries[k] = e
	}
	if len(entries) > 0 {
		m, err := entriesToOverrides(entries)
		if err != nil {
			return Overlay{}, err
		}
		ov.Distances = m
	}
	if v, ok := raw[TablesField]; ok {
		var sections map[string]map[string]fileEntry
		if err := strictDecode(v, &sections); err != nil {
			return Overlay{}, fmt.Errorf("%s: %w", TablesField, err)
		}
		if len(sections) == 0 {
			return Overlay{}, fmt.Errorf("%s: no sections", TablesField)
		}
		ov.Tables = map[string]map[model.Distance]PathBaseline{}
		for k, sec := range sections {
			if err := ValidateTableKey(k); err != nil {
				return Overlay{}, fmt.Errorf("%s: %w", TablesField, err)
			}
			m, err := entriesToOverrides(sec)
			if err != nil {
				return Overlay{}, fmt.Errorf("%s %s: %w", TablesField, k, err)
			}
			ov.Tables[k] = m
		}
	}
	if len(ov.Distances) == 0 && len(ov.Tables) == 0 {
		return Overlay{}, fmt.Errorf("no distances")
	}
	return ov, nil
}

// strictDecode decodes data into v, rejecting unknown fields.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func entriesToOverrides(raw map[string]fileEntry) (map[model.Distance]PathBaseline, error) {
	out := map[model.Distance]PathBaseline{}
	for k, e := range raw {
		d, err := model.ParseDistance(k)
		if err != nil {
			return nil, err
		}
		b, err := entryToBaseline(e)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[d] = b
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no distances")
	}
	return out, nil
}

func entryToBaseline(e fileEntry) (PathBaseline, error) {
	b, err := e.fileValues.parse()
	if err != nil {
		return PathBaseline{}, err
	}
	for i, fs := range e.SubClasses {
		v, err := fs.fileValues.parse()
		if err != nil {
			return PathBaseline{}, fmt.Errorf("sub_classes[%d] %q: %w", i, fs.Name, err)
		}
		sc := SubClass{Name: fs.Name, BandwidthBPS: v.BandwidthBPS, LatencyNs: v.LatencyNs}
		for j, p := range fs.RootPortPairs {
			if len(p) != 2 {
				return PathBaseline{}, fmt.Errorf("sub_classes[%d] %q: root_port_pairs[%d] needs exactly two root-port IDs, got %d", i, fs.Name, j, len(p))
			}
			sc.RootPortPairs = append(sc.RootPortPairs, model.RootPortPair{p[0], p[1]})
		}
		b.SubClasses = append(b.SubClasses, sc)
	}
	if err := validateSubClasses(b.SubClasses); err != nil {
		return PathBaseline{}, err
	}
	if b.BandwidthBPS == 0 && b.LatencyNs == 0 && len(b.SubClasses) == 0 {
		return PathBaseline{}, fmt.Errorf("need bandwidth, latency or sub_classes")
	}
	return b, nil
}

// parse reads the values, either of which may be absent.
func (e fileValues) parse() (PathBaseline, error) {
	var b PathBaseline
	if e.BandwidthBPS != nil && e.Bandwidth != "" {
		return b, fmt.Errorf("set only one of bandwidth_bps and bandwidth")
	}
	if e.LatencyNs != nil && e.Latency != "" {
		return b, fmt.Errorf("set only one of latency_ns and latency")
	}
	switch {
	case e.BandwidthBPS != nil:
		b.BandwidthBPS = *e.BandwidthBPS
	case e.Bandwidth != "":
		v, err := ParseBandwidth(e.Bandwidth)
		if err != nil {
			return b, err
		}
		b.BandwidthBPS = v
	}
	switch {
	case e.LatencyNs != nil:
		b.LatencyNs = *e.LatencyNs
	case e.Latency != "":
		v, err := ParseLatency(e.Latency)
		if err != nil {
			return b, err
		}
		b.LatencyNs = v
	}
	return b, nil
}

// ParseSpecs reads one or more KEY=BW[,LAT] tokens (semicolon- or comma-separated).
func ParseSpecs(s string) (map[model.Distance]PathBaseline, error) {
	out := map[model.Distance]PathBaseline{}
	specs := splitSpecs(s)
	if len(specs) == 0 {
		return nil, fmt.Errorf("empty baseline spec")
	}
	for _, spec := range specs {
		d, b, err := ParseSpec(spec)
		if err != nil {
			return nil, err
		}
		prev := out[d]
		if b.BandwidthBPS > 0 {
			prev.BandwidthBPS = b.BandwidthBPS
		}
		if b.LatencyNs > 0 {
			prev.LatencyNs = b.LatencyNs
		}
		out[d] = prev
	}
	return out, nil
}

func splitSpecs(s string) []string {
	var specs []string
	for _, chunk := range strings.Split(s, ";") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		specs = append(specs, splitCommaSpecs(chunk)...)
	}
	return specs
}

func splitCommaSpecs(s string) []string {
	parts := strings.Split(s, ",")
	var specs []string
	var cur string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(p, "=") {
			if cur != "" {
				specs = append(specs, cur)
			}
			cur = p
			continue
		}
		if cur == "" {
			cur = p
			continue
		}
		cur += "," + p
	}
	if cur != "" {
		specs = append(specs, cur)
	}
	return specs
}

// ParseSpec parses KEY=270Gbps[,1400ns]. Bandwidth and latency may appear alone.
func ParseSpec(s string) (model.Distance, PathBaseline, error) {
	key, rest, ok := strings.Cut(strings.TrimSpace(s), "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(rest) == "" {
		return 0, PathBaseline{}, fmt.Errorf("baseline spec %q: want KEY=BW[,LAT]", s)
	}
	d, err := model.ParseDistance(key)
	if err != nil {
		return 0, PathBaseline{}, err
	}
	var b PathBaseline
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if looksLikeLatency(part) {
			v, err := ParseLatency(part)
			if err != nil {
				return 0, PathBaseline{}, fmt.Errorf("%s: %w", strings.TrimSpace(key), err)
			}
			b.LatencyNs = v
			continue
		}
		v, err := ParseBandwidth(part)
		if err != nil {
			return 0, PathBaseline{}, fmt.Errorf("%s: %w", strings.TrimSpace(key), err)
		}
		b.BandwidthBPS = v
	}
	if b.BandwidthBPS == 0 && b.LatencyNs == 0 {
		return 0, PathBaseline{}, fmt.Errorf("%s: need bandwidth or latency", strings.TrimSpace(key))
	}
	return d, b, nil
}

// ParseBandwidth accepts bps / Gbps (also kbps, Mbps, Tbps, and Gb/s).
func ParseBandwidth(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "_", "")
	lower := strings.ToLower(s)
	lower = strings.ReplaceAll(lower, "/s", "")
	var mult float64
	switch {
	case strings.HasSuffix(lower, "tbps") || strings.HasSuffix(lower, "tb"):
		mult = 1e12
		lower = trimUnit(lower, "tbps", "tb")
	case strings.HasSuffix(lower, "gbps") || strings.HasSuffix(lower, "gb"):
		mult = 1e9
		lower = trimUnit(lower, "gbps", "gb")
	case strings.HasSuffix(lower, "mbps") || strings.HasSuffix(lower, "mb"):
		mult = 1e6
		lower = trimUnit(lower, "mbps", "mb")
	case strings.HasSuffix(lower, "kbps") || strings.HasSuffix(lower, "kb"):
		mult = 1e3
		lower = trimUnit(lower, "kbps", "kb")
	case strings.HasSuffix(lower, "bps"):
		mult = 1
		lower = strings.TrimSuffix(lower, "bps")
	default:
		return 0, fmt.Errorf("bandwidth %q: need unit bps or Gbps", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(lower), 64)
	if err != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("bandwidth %q: invalid number", s)
	}
	scaled := n * mult
	if scaled > float64(math.MaxUint64) {
		return 0, fmt.Errorf("bandwidth %q: overflow", s)
	}
	return uint64(scaled), nil
}

// ParseLatency accepts ns / us (also µs and ms).
func ParseLatency(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "_", "")
	lower := strings.ToLower(s)
	lower = strings.ReplaceAll(lower, "µs", "us")
	var mult float64
	switch {
	case strings.HasSuffix(lower, "ms"):
		mult = 1e6
		lower = strings.TrimSuffix(lower, "ms")
	case strings.HasSuffix(lower, "us"):
		mult = 1e3
		lower = strings.TrimSuffix(lower, "us")
	case strings.HasSuffix(lower, "ns"):
		mult = 1
		lower = strings.TrimSuffix(lower, "ns")
	default:
		return 0, fmt.Errorf("latency %q: need unit ns or us", s)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(lower), 64)
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("latency %q: invalid number", s)
	}
	scaled := n * mult
	if scaled > float64(math.MaxUint64) {
		return 0, fmt.Errorf("latency %q: overflow", s)
	}
	return uint64(scaled), nil
}

func looksLikeLatency(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	lower = strings.ReplaceAll(lower, "µs", "us")
	for _, suf := range []string{"ms", "us", "ns"} {
		if !strings.HasSuffix(lower, suf) {
			continue
		}
		rest := strings.TrimSuffix(lower, suf)
		if rest == "" {
			return false
		}
		last := rune(rest[len(rest)-1])
		return unicode.IsDigit(last) || last == '.'
	}
	return false
}

func trimUnit(s string, units ...string) string {
	for _, u := range units {
		if strings.HasSuffix(s, u) {
			return strings.TrimSuffix(s, u)
		}
	}
	return s
}

// FormatTable is the one-line diagnose log for an effective baseline table.
func FormatTable(t model.BaselineTable) string {
	var b strings.Builder
	fmt.Fprintf(&b, "line_rate=%s", formatBPS(t.LineRateBPS))
	if t.DerivedFromBPS > 0 && t.DerivedFromBPS != t.LineRateBPS {
		// Say so rather than leaving the operator to wonder why a 400G RNIC
		// is judged against a 248 Gb/s floor.
		if t.PCIeCapBPS > t.DerivedFromBPS {
			// The hop is wider than the bus Fig.4 was measured on.
			fmt.Fprintf(&b, " derived_from=%s (fig.4 bus; pcie hop %s)", formatBPS(t.DerivedFromBPS), formatBPS(t.PCIeCapBPS))
		} else {
			fmt.Fprintf(&b, " derived_from=%s (pcie hop)", formatBPS(t.DerivedFromBPS))
		}
	}
	if t.Section != "" {
		// The user values below come from this section, not the top level.
		fmt.Fprintf(&b, " (baseline file tables section %s)", t.Section)
	}
	for _, e := range t.Entries {
		fmt.Fprintf(&b, " %s=%s/%s (%s/%s)", e.Distance, formatBPS(e.BandwidthBPS), formatNs(e.LatencyNs), e.BandwidthSource, e.LatencySource)
		for _, sc := range e.SubClasses {
			fmt.Fprintf(&b, " %s/%s=%s/%s (sub-class, %d root-port pair(s))", e.Distance, sc.Name, formatBPS(sc.BandwidthBPS), formatNs(sc.LatencyNs), len(sc.RootPortPairs))
		}
	}
	return b.String()
}

func formatBPS(v uint64) string {
	if v%1_000_000_000 == 0 {
		return fmt.Sprintf("%dGbps", v/1_000_000_000)
	}
	return fmt.Sprintf("%.2fGbps", float64(v)/1e9)
}

func formatNs(v uint64) string {
	if v >= 1000 && v%1000 == 0 {
		return fmt.Sprintf("%dus", v/1000)
	}
	return fmt.Sprintf("%dns", v)
}

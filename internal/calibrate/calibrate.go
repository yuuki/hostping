// Package calibrate derives per-distance idle baselines from idle-mode runs on
// a host believed healthy (hostpingctl calibrate), and merges the results of
// several such hosts into one file for their hardware class.
//
// The output is a --baseline-file JSON object and nothing else: the loader
// rejects unknown fields, so the spread that says how tight each number is
// goes to the human-readable table instead.
package calibrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// Options tunes Summarize.
type Options struct {
	// Latency also derives a small-message latency baseline per distance.
	// Off by default: a latency you set is a strict reference with no floor
	// (unlike the sibling comparison it replaces), so it is opt-in.
	Latency bool
	// AbnormalTh is diagnose's --abnormal-th. A path whose median reading is
	// this far below the calibrated baseline would diagnose abnormal against
	// it, and is reported so the operator can check it before distributing.
	AbnormalTh float64
}

// DistanceResult is the calibrated baseline of one communication distance.
type DistanceResult struct {
	Distance model.Distance
	// BandwidthBPS is the median over runs of each run's median path
	// bandwidth. Zero means no path of this distance was measured, and the
	// distance is left out of the file (it keeps its Fig.4 default).
	BandwidthBPS uint64
	// LatencyNs is the same statistic over small-message p50 latency; zero
	// unless Options.Latency.
	LatencyNs uint64
	// Paths is the number of distinct RNIC→endpoint paths that contributed,
	// and Runs the number of runs that measured at least one of them.
	Paths, Runs int
	// BW and Lat summarize every individual reading (paths × runs).
	BW, Lat Spread
	// SubClasses is the learned sub-class of a distance whose paths read at
	// two levels along root-port pairs (see Bimodal). When it is set, the
	// values above are drawn from the slow cluster's paths only.
	SubClasses []SubClassResult
}

// SubClassResult is a learned sub-class: the fast cluster of a bimodal
// distance, with the same statistic as DistanceResult over its paths.
type SubClassResult struct {
	Name         string
	BandwidthBPS uint64
	LatencyNs    uint64
	Paths, Runs  int
	BW, Lat      Spread
	// RootPortPairs are the pairs whose every path is in the fast cluster,
	// sorted.
	RootPortPairs []model.RootPortPair
}

// fastSubClass is the name calibrate gives the fast cluster's sub-class.
const fastSubClass = "fast"

// Outlier is a path whose median reading sits more than AbnormalTh below the
// calibrated baseline of its distance: it would diagnose abnormal against the
// file this run produced. On a healthy host that is either a fault the
// operator did not know about or a path that legitimately reads lower than
// its siblings; either way it needs a look before the file is distributed.
type Outlier struct {
	RNIC, Endpoint string
	Distance       model.Distance
	// SubClass is the learned sub-class whose baseline the path falls
	// below, "" for the distance's own.
	SubClass    string
	MedianBPS   uint64
	BaselineBPS uint64
	// Table is the key of the section whose baseline the path falls below,
	// "" for the top level.
	Table string
}

// class is the name the outlier's baseline is reported under.
func (o Outlier) class() string {
	c := o.Distance.Key()
	if o.SubClass != "" {
		c = baseline.ClassName(o.Distance, o.SubClass)
	}
	return inTable(o.Table, c)
}

// inTable qualifies a class name with the section it belongs to.
func inTable(table, class string) string {
	if table == "" {
		return class
	}
	return "tables " + table + " " + class
}

// Section is the calibration of the RNICs that derive one Fig.4 table other
// than the top level's, written under "tables" in the file.
type Section struct {
	// Key is the table key (see baseline.TableKey).
	Key string
	// RNICs are the measured RNICs on the table, sorted.
	RNICs []string
	// Distances holds every distance in model.Distances order, as
	// Result.Distances does for the top level.
	Distances []DistanceResult
}

// Result is the calibration of one host.
type Result struct {
	Runs      int
	Discarded int
	Latency   bool
	// Distances holds every distance in model.Distances order, measured or not.
	Distances []DistanceResult
	Outliers  []Outlier
	// Bimodal holds the distances whose paths read at two levels, in
	// model.Distances order.
	Bimodal []Bimodal
	// AbnormalTh is Options.AbnormalTh, which the warnings are judged by.
	AbnormalTh float64
	// RateTables is the number of distinct (line rate, PCIe hop) baseline
	// tables the runs reported. More than one means the Fig.4 defaults differ
	// between RNICs.
	RateTables int
	// TopTable is the table key the top-level values were measured on, and
	// TopRNICs its measured RNICs; both empty when every measured RNIC
	// derives one table (then Sections is empty too).
	TopTable string
	TopRNICs []string
	// Sections are the other tables' calibrations, sorted by key.
	Sections []Section
}

type pathKey struct{ rnic, ep string }

// Summarize reduces idle-mode reports to per-distance baselines.
//
// Every path of a distance counts, whatever status the run gave it: the point
// of calibrating is that the default may be wrong for this hardware, so a
// path the default called abnormal is exactly the evidence wanted. Only
// readings that do not describe the path's idle bandwidth are left out: a
// path with no route in the graph (no distance to file it under), a path
// judged on a busy RNIC, a zero reading, and anything from a run that was not
// in idle mode. Endpoints the engine skipped (no GDR, compute mode) never
// reach the report.
//
// Each distance is summarized in two steps — the median of the paths within
// a run, then the median of those medians across runs — so one slow path
// cannot move the result within a run, and one disturbed run cannot move it
// across runs. Using every distance's own paths is safe for GPUs because the
// distances never mix affinitive and non-affinitive paths: same_switch and
// multi_switch are always affinitive and cross_root and cross_socket never
// are, so a non-affinitive GPU's lower reading cannot pull down the floor of
// an affinitive one.
//
// A distance whose paths read at two levels (see Bimodal) gets a learned
// sub-class when the split follows root-port pairs: every pair's paths all
// sit in one cluster, and every fast path crosses a pair. The fast cluster
// then becomes the sub-class "fast", listing the fast pairs, and both it and
// the distance take the same statistic over their own cluster's paths only.
// Otherwise the distance keeps the statistic over all its paths, and the
// warning says why no sub-class was written.
//
// The RNICs of one host can derive different Fig.4 tables (line rate / PCIe
// hop), and then read at different levels at every distance. Paths are
// grouped by their RNIC's table and each group is summarized on its own, as
// above. The group with the most measured RNICs (on a tie, the first table
// key in sort order) becomes the top level of the file, exactly as on a host
// with one table; every other group becomes a section under its table key.
func Summarize(g topology.Graph, reports []model.Report, opt Options) Result {
	res := Result{Runs: len(reports), Latency: opt.Latency, AbnormalTh: opt.AbnormalTh}
	tables := map[[2]uint64]struct{}{}
	for _, rep := range reports {
		for _, t := range rep.Baselines {
			tables[[2]uint64{t.LineRateBPS, t.PCIeCapBPS}] = struct{}{}
		}
	}
	res.RateTables = len(tables)
	groups := tableGroups(g, reports)
	if len(groups) <= 1 {
		res.Distances, res.Outliers, res.Bimodal = summarizeGroup(g, reports, opt, nil, "")
		return res
	}
	top := groups[0]
	for _, gr := range groups[1:] {
		if len(gr.rnics) > len(top.rnics) {
			top = gr
		}
	}
	res.TopTable, res.TopRNICs = top.key, top.ids()
	in := func(gr tableGroup) func(string) bool {
		return func(id string) bool { return gr.rnics[id] }
	}
	res.Distances, res.Outliers, res.Bimodal = summarizeGroup(g, reports, opt, in(top), "")
	for _, gr := range groups {
		if gr.key == top.key {
			continue
		}
		dists, outs, bims := summarizeGroup(g, reports, opt, in(gr), gr.key)
		res.Sections = append(res.Sections, Section{Key: gr.key, RNICs: gr.ids(), Distances: dists})
		res.Outliers = append(res.Outliers, outs...)
		res.Bimodal = append(res.Bimodal, bims...)
	}
	return res
}

// tableGroup is the measured RNICs that derive one Fig.4 table.
type tableGroup struct {
	key   string
	rnics map[string]bool
}

func (t tableGroup) ids() []string {
	out := make([]string, 0, len(t.rnics))
	for id := range t.rnics {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// tableGroups groups the RNICs with a usable idle reading by the Fig.4 table
// they derive, sorted by key.
func tableGroups(g topology.Graph, reports []model.Report) []tableGroup {
	byKey := map[string]map[string]bool{}
	for _, rep := range reports {
		if rep.Mode != model.ModeIdle {
			continue
		}
		for _, m := range rep.Paths {
			if _, ok := usable(g, m); !ok {
				continue
			}
			n, _ := g.RNIC(m.RNIC)
			k := baseline.RNICTable(n)
			if byKey[k] == nil {
				byKey[k] = map[string]bool{}
			}
			byKey[k][m.RNIC] = true
		}
	}
	out := make([]tableGroup, 0, len(byKey))
	for k, ids := range byKey {
		out = append(out, tableGroup{key: k, rnics: ids})
	}
	slices.SortFunc(out, func(a, b tableGroup) int { return strings.Compare(a.key, b.key) })
	return out
}

// summarizeGroup is Summarize over the paths of the RNICs keep accepts (all
// of them when keep is nil). table is the section key the warnings name, ""
// for the top level.
func summarizeGroup(g topology.Graph, reports []model.Report, opt Options, keep func(string) bool, table string) ([]DistanceResult, []Outlier, []Bimodal) {
	var dists []DistanceResult
	var outliersAll []Outlier
	var bims []Bimodal
	accs := map[model.Distance]*acc{}
	for _, rep := range reports {
		if rep.Mode != model.ModeIdle {
			continue
		}
		run := map[model.Distance][]reading{}
		for _, m := range rep.Paths {
			if keep != nil && !keep(m.RNIC) {
				continue
			}
			d, ok := usable(g, m)
			if !ok {
				continue
			}
			a := accs[d]
			if a == nil {
				a = &acc{paths: map[pathKey][]uint64{}}
				accs[d] = a
			}
			k := pathKey{m.RNIC, m.Endpoint}
			a.paths[k] = append(a.paths[k], m.BandwidthBPS)
			rd := reading{key: k, bw: m.BandwidthBPS}
			if opt.Latency {
				rd.lat = m.SmallLatNsP50
			}
			run[d] = append(run[d], rd)
		}
		for d, v := range run {
			accs[d].runs = append(accs[d].runs, v)
		}
	}
	for _, d := range model.Distances() {
		r := DistanceResult{Distance: d}
		a := accs[d]
		if a == nil {
			dists = append(dists, r)
			continue
		}
		r.set(a.level(nil))
		parent := a.paths
		var outs []Outlier
		b, ok := bimodal(d, a.paths, r.BandwidthBPS, opt.AbnormalTh)
		if ok {
			fast := map[pathKey]bool{}
			for _, p := range b.Fast {
				fast[pathKey{p.RNIC, p.Endpoint}] = true
			}
			b.FastPairs, b.MixedPairs, b.Unpaired = pairSplit(g, b)
			if len(b.MixedPairs) == 0 && len(b.Unpaired) == 0 && len(b.FastPairs) > 0 {
				r.set(a.level(func(k pathKey) bool { return !fast[k] }))
				hi := a.level(func(k pathKey) bool { return fast[k] })
				sc := SubClassResult{
					Name:         fastSubClass,
					BandwidthBPS: hi.bw, LatencyNs: hi.lat,
					Paths: hi.paths, Runs: hi.runs,
					BW: hi.bwSpread, Lat: hi.latSpread,
					RootPortPairs: b.FastPairs,
				}
				r.SubClasses = []SubClassResult{sc}
				b.SubClass, b.SubClassBPS, b.BaselineBPS = sc.Name, sc.BandwidthBPS, r.BandwidthBPS
				parent = filterPaths(a.paths, func(k pathKey) bool { return !fast[k] })
				outs = outliers(d, sc.Name, filterPaths(a.paths, func(k pathKey) bool { return fast[k] }), sc.BandwidthBPS, opt.AbnormalTh)
			}
			b.Table = table
			bims = append(bims, b)
		}
		outs = append(outs, outliers(d, "", parent, r.BandwidthBPS, opt.AbnormalTh)...)
		slices.SortFunc(outs, func(x, y Outlier) int {
			return comparePath(PathRef{x.RNIC, x.Endpoint}, PathRef{y.RNIC, y.Endpoint})
		})
		for i := range outs {
			outs[i].Table = table
		}
		outliersAll = append(outliersAll, outs...)
		dists = append(dists, r)
	}
	return dists, outliersAll, bims
}

// reading is one path's idle reading in one run; lat is 0 unless
// Options.Latency.
type reading struct {
	key     pathKey
	bw, lat uint64
}

// acc collects one distance's readings.
type acc struct {
	// runs holds each idle run's readings of the distance, one slice per
	// run that measured at least one of its paths.
	runs [][]reading
	// paths is every reading of each path across runs.
	paths map[pathKey][]uint64
}

// level is the calibrated statistic over a set of a distance's paths.
type level struct {
	bw, lat             uint64
	paths, runs         int
	bwSpread, latSpread Spread
}

// level is the median over runs of each run's median reading, over the paths
// keep accepts (all of them when keep is nil).
func (a *acc) level(keep func(pathKey) bool) level {
	in := func(k pathKey) bool { return keep == nil || keep(k) }
	var perBW, perLat, allBW, allLat []uint64
	for _, run := range a.runs {
		var bw, lat []uint64
		for _, rd := range run {
			if !in(rd.key) {
				continue
			}
			bw = append(bw, rd.bw)
			if rd.lat > 0 {
				lat = append(lat, rd.lat)
			}
		}
		if len(bw) > 0 {
			perBW = append(perBW, Median(bw))
			allBW = append(allBW, bw...)
		}
		if len(lat) > 0 {
			perLat = append(perLat, Median(lat))
			allLat = append(allLat, lat...)
		}
	}
	n := 0
	for k := range a.paths {
		if in(k) {
			n++
		}
	}
	return level{
		bw: Median(perBW), lat: Median(perLat),
		paths: n, runs: len(perBW),
		bwSpread: SpreadOf(allBW), latSpread: SpreadOf(allLat),
	}
}

func (r *DistanceResult) set(l level) {
	r.BandwidthBPS, r.LatencyNs = l.bw, l.lat
	r.Paths, r.Runs = l.paths, l.runs
	r.BW, r.Lat = l.bwSpread, l.latSpread
}

func filterPaths(paths map[pathKey][]uint64, keep func(pathKey) bool) map[pathKey][]uint64 {
	out := map[pathKey][]uint64{}
	for k, v := range paths {
		if keep(k) {
			out[k] = v
		}
	}
	return out
}

// pairSplit maps the clusters of b onto root-port pairs. A pair is fast when
// every path across it is in the fast cluster, and mixed when its paths sit
// in both; unpaired are the fast paths that cross no root-port pair. A
// sub-class can be learned only with no mixed pair and no unpaired fast path.
func pairSplit(g topology.Graph, b Bimodal) (fast, mixed []model.RootPortPair, unpaired []PathRef) {
	type count struct{ slow, fast int }
	counts := map[model.RootPortPair]*count{}
	tally := func(p PathRef, isFast bool) {
		path, ok := g.Path(p.RNIC, p.Endpoint)
		pair, paired := g.RootPortPair(path)
		if !ok || !paired {
			if isFast {
				unpaired = append(unpaired, p)
			}
			return
		}
		c := counts[pair]
		if c == nil {
			c = &count{}
			counts[pair] = c
		}
		if isFast {
			c.fast++
		} else {
			c.slow++
		}
	}
	for _, p := range b.Slow {
		tally(p, false)
	}
	for _, p := range b.Fast {
		tally(p, true)
	}
	for pair, c := range counts {
		switch {
		case c.fast > 0 && c.slow > 0:
			mixed = append(mixed, pair)
		case c.fast > 0:
			fast = append(fast, pair)
		}
	}
	slices.SortFunc(fast, comparePairs)
	slices.SortFunc(mixed, comparePairs)
	return fast, mixed, unpaired
}

func comparePairs(a, b model.RootPortPair) int {
	if c := strings.Compare(a[0], b[0]); c != 0 {
		return c
	}
	return strings.Compare(a[1], b[1])
}

// usable reports whether m is an idle reading of a routed path, and its distance.
func usable(g topology.Graph, m model.PathMeasurement) (model.Distance, bool) {
	if m.BandwidthBPS == 0 {
		return 0, false
	}
	switch m.UncertainReason {
	case model.UncertainNoPath, model.UncertainRNICBusy:
		return 0, false
	}
	p, ok := g.Path(m.RNIC, m.Endpoint)
	if !ok {
		return 0, false
	}
	return p.Distance, true
}

func outliers(d model.Distance, sub string, paths map[pathKey][]uint64, base uint64, th float64) []Outlier {
	if base == 0 || th <= 0 {
		return nil
	}
	var out []Outlier
	for k, v := range paths {
		med := Median(v)
		if below(med, base, th) {
			out = append(out, Outlier{RNIC: k.rnic, Endpoint: k.ep, Distance: d, SubClass: sub, MedianBPS: med, BaselineBPS: base})
		}
	}
	return out
}

// below mirrors the analyzer's idle verdict: v is more than th below ref.
func below(v, ref uint64, th float64) bool {
	return ref > 0 && float64(v) < float64(ref)*(1-th)
}

// Baselines is the measured part of r as a --baseline-file overlay: the top
// level, and a section per other Fig.4 table. Distances with no measured
// path are absent, so they keep their Fig.4 default.
func (r Result) Baselines() baseline.Overlay {
	ov := baseline.Overlay{Distances: distanceBaselines(r.Distances)}
	for _, s := range r.Sections {
		m := distanceBaselines(s.Distances)
		if len(m) == 0 {
			continue
		}
		if ov.Tables == nil {
			ov.Tables = map[string]map[model.Distance]baseline.PathBaseline{}
		}
		ov.Tables[s.Key] = m
	}
	return ov
}

func distanceBaselines(dists []DistanceResult) map[model.Distance]baseline.PathBaseline {
	out := map[model.Distance]baseline.PathBaseline{}
	for _, d := range dists {
		if d.BandwidthBPS == 0 {
			continue
		}
		b := baseline.PathBaseline{BandwidthBPS: d.BandwidthBPS, LatencyNs: d.LatencyNs}
		for _, sc := range d.SubClasses {
			b.SubClasses = append(b.SubClasses, baseline.SubClass{
				Name:          sc.Name,
				BandwidthBPS:  sc.BandwidthBPS,
				LatencyNs:     sc.LatencyNs,
				RootPortPairs: slices.Clone(sc.RootPortPairs),
			})
		}
		out[d.Distance] = b
	}
	return out
}

// fileEntry is the subset of the --baseline-file entry calibrate writes.
type fileEntry struct {
	BandwidthBPS uint64         `json:"bandwidth_bps,omitempty"`
	LatencyNs    uint64         `json:"latency_ns,omitempty"`
	SubClasses   []fileSubClass `json:"sub_classes,omitempty"`
}

type fileSubClass struct {
	Name          string               `json:"name"`
	BandwidthBPS  uint64               `json:"bandwidth_bps,omitempty"`
	LatencyNs     uint64               `json:"latency_ns,omitempty"`
	RootPortPairs []model.RootPortPair `json:"root_port_pairs"`
}

// MarshalFile renders ov as a --baseline-file JSON object: the top-level
// distances in model.Distances order, then, when ov has sections, a "tables"
// object with one such object per section in key order. A file with no
// sections is exactly what calibrate wrote before sections existed. It
// refuses an empty overlay, which the loader rejects.
func MarshalFile(ov baseline.Overlay) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{")
	n, err := writeDistances(&buf, ov.Distances, "  ")
	if err != nil {
		return nil, err
	}
	secs := 0
	for _, k := range ov.SectionKeys() {
		var sec bytes.Buffer
		m, err := writeDistances(&sec, ov.Tables[k], "      ")
		if err != nil {
			return nil, err
		}
		if m == 0 {
			continue
		}
		if secs == 0 {
			if n > 0 {
				buf.WriteString(",")
			}
			fmt.Fprintf(&buf, "\n  %q: {", baseline.TablesField)
		} else {
			buf.WriteString(",")
		}
		fmt.Fprintf(&buf, "\n    %q: {%s\n    }", k, sec.Bytes())
		secs++
	}
	if secs > 0 {
		buf.WriteString("\n  }")
	}
	if n == 0 && secs == 0 {
		return nil, fmt.Errorf("no distance was measured, so there is no baseline to write")
	}
	buf.WriteString("\n}\n")
	return buf.Bytes(), nil
}

// writeDistances writes b's entries, one per line at indent, comma
// separated, and returns how many it wrote.
func writeDistances(buf *bytes.Buffer, b map[model.Distance]baseline.PathBaseline, indent string) (int, error) {
	n := 0
	for _, d := range model.Distances() {
		v, ok := b[d]
		if !ok || (v.BandwidthBPS == 0 && v.LatencyNs == 0 && len(v.SubClasses) == 0) {
			continue
		}
		fe := fileEntry{BandwidthBPS: v.BandwidthBPS, LatencyNs: v.LatencyNs}
		for _, sc := range v.SubClasses {
			fe.SubClasses = append(fe.SubClasses, fileSubClass{Name: sc.Name, BandwidthBPS: sc.BandwidthBPS, LatencyNs: sc.LatencyNs, RootPortPairs: sc.RootPortPairs})
		}
		e, err := json.Marshal(fe)
		if err != nil {
			return 0, err
		}
		if n > 0 {
			buf.WriteString(",")
		}
		fmt.Fprintf(buf, "\n%s%q: %s", indent, d.Key(), e)
		n++
	}
	return n, nil
}

// WriteTable writes the human-readable summary: the baseline of each distance
// with the spread of the readings behind it, the distances left at Fig.4,
// and the warnings an operator should read before distributing the file.
// A host whose RNICs derive several Fig.4 tables gets one such table for the
// top level and one per section.
func (r Result) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "hostping calibrate: %d idle-mode runs used, %d discarded; baseline = median over runs of each run's median path\n", r.Runs, r.Discarded)
	if r.TopTable != "" {
		fmt.Fprintf(w, "top level: Fig.4 table %s (RNICs %s)\n", r.TopTable, strings.Join(r.TopRNICs, ", "))
	}
	r.writeDistances(w, r.Distances, "")
	for _, s := range r.Sections {
		fmt.Fprintf(w, "tables section %s (RNICs %s):\n", s.Key, strings.Join(s.RNICs, ", "))
		r.writeDistances(w, s.Distances, s.Key)
	}
	for _, o := range r.Outliers {
		fmt.Fprintf(w, "WARNING: %s->%s (%s) reads %s, %.0f%% below the calibrated %s: it will diagnose abnormal against this file. Check the path before distributing it.\n",
			o.RNIC, o.Endpoint, o.class(), gbps(o.MedianBPS),
			100*(1-float64(o.MedianBPS)/float64(o.BaselineBPS)), gbps(o.BaselineBPS))
	}
	for _, b := range r.Bimodal {
		r.writeBimodal(w, b)
	}
	switch {
	case len(r.Sections) > 0:
		keys := make([]string, len(r.Sections))
		for i, s := range r.Sections {
			keys[i] = s.Key
		}
		fmt.Fprintf(w, "NOTE: this host's RNICs derive %d different Fig.4 tables (line rate / PCIe hop); the top level holds the values of %s, and the \"tables\" sections those of %s. Each RNIC takes the section for its table, or the top level when there is none.\n",
			max(r.RateTables, len(r.Sections)+1), r.TopTable, strings.Join(keys, ", "))
	case r.RateTables > 1:
		fmt.Fprintf(w, "NOTE: this host's RNICs derive %d different Fig.4 tables (line rate / PCIe hop), but only one table's RNICs were measured; the file's top level applies to the others too.\n", r.RateTables)
	}
}

// writeDistances writes the table of one set of distances, then the
// sub-classes and the unmeasured distances. table is the section key, ""
// for the top level.
func (r Result) writeDistances(w io.Writer, dists []DistanceResult, table string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	hdr := "distance\tbaseline\tpaths×runs\tmin\tp25\tmedian\tp75\tmax"
	if r.Latency {
		hdr += "\tlatency\tlat p25..p75"
	}
	fmt.Fprintln(tw, hdr)
	var unmeasured []string
	for _, d := range dists {
		if d.BandwidthBPS == 0 {
			unmeasured = append(unmeasured, d.Distance.Key())
			continue
		}
		row := func(name string, bw, lat uint64, paths, runs int, bwS, latS Spread) {
			fmt.Fprintf(tw, "%s\t%s\t%d×%d (n=%d)\t%s\t%s\t%s\t%s\t%s",
				name, gbps(bw), paths, runs, bwS.N,
				gbps(bwS.Min), gbps(bwS.P25), gbps(bwS.Median), gbps(bwS.P75), gbps(bwS.Max))
			if r.Latency {
				if lat > 0 {
					fmt.Fprintf(tw, "\t%dns\t%d..%dns", lat, latS.P25, latS.P75)
				} else {
					fmt.Fprintf(tw, "\t-\t-")
				}
			}
			fmt.Fprintln(tw)
		}
		row(d.Distance.Key(), d.BandwidthBPS, d.LatencyNs, d.Paths, d.Runs, d.BW, d.Lat)
		for _, sc := range d.SubClasses {
			row(baseline.ClassName(d.Distance, sc.Name), sc.BandwidthBPS, sc.LatencyNs, sc.Paths, sc.Runs, sc.BW, sc.Lat)
		}
	}
	_ = tw.Flush()
	for _, d := range dists {
		for _, sc := range d.SubClasses {
			fmt.Fprintf(w, "sub-class %s: paths across root-port pairs %s; %s keeps the other paths\n",
				inTable(table, baseline.ClassName(d.Distance, sc.Name)), pairList(sc.RootPortPairs), d.Distance.Key())
		}
	}
	if len(unmeasured) > 0 {
		fmt.Fprintf(w, "no measured path (left out of the file, keep the Fig.4 default): %s\n", strings.Join(unmeasured, ", "))
	}
}

// writeBimodal reports a distance one baseline cannot serve. When the split
// follows root-port pairs, the fast cluster became a sub-class and this is a
// note saying so. Otherwise it is a warning: the distance keeps one baseline,
// and the operator reads which level it sits at and why no sub-class was
// written.
func (r Result) writeBimodal(w io.Writer, b Bimodal) {
	fast := make([]string, len(b.Fast))
	for i, p := range b.Fast {
		fast[i] = p.String()
	}
	if b.SubClass != "" {
		fmt.Fprintf(w, "NOTE: %s reads at two levels: %d paths at %s and %d paths at %s (%.2fx), split along root-port pairs.\n",
			inTable(b.Table, b.Distance.Key()), len(b.Fast), gbps(b.FastBPS), len(b.Slow), gbps(b.SlowBPS), float64(b.FastBPS)/float64(b.SlowBPS))
		fmt.Fprintf(w, "  fast paths: %s\n", strings.Join(fast, ", "))
		fmt.Fprintf(w, "  wrote sub-class %s (%s) for root-port pairs %s; %s keeps the slow level (%s), so a fast path that degrades to it diagnoses abnormal.\n",
			inTable(b.Table, baseline.ClassName(b.Distance, b.SubClass)), gbps(b.SubClassBPS), pairList(b.FastPairs), b.Distance.Key(), gbps(b.BaselineBPS))
		return
	}
	fmt.Fprintf(w, "WARNING: %s reads at two levels: %d paths at %s and %d paths at %s (%.2fx); one baseline cannot serve both.\n",
		inTable(b.Table, b.Distance.Key()), len(b.Fast), gbps(b.FastBPS), len(b.Slow), gbps(b.SlowBPS), float64(b.FastBPS)/float64(b.SlowBPS))
	fmt.Fprintf(w, "  fast paths: %s\n", strings.Join(fast, ", "))
	if b.FastUnflagged(r.AbnormalTh) {
		fmt.Fprintf(w, "  a fast path that degrades to the slow level stays normal against the calibrated %s.\n", gbps(b.BaselineBPS))
	} else {
		fmt.Fprintf(w, "  the calibrated %s sits at the fast level, so the slow paths diagnose abnormal against it.\n", gbps(b.BaselineBPS))
	}
	var why []string
	if len(b.MixedPairs) > 0 {
		why = append(why, "root-port pairs with paths at both levels: "+pairList(b.MixedPairs))
	}
	if len(b.Unpaired) > 0 {
		un := make([]string, len(b.Unpaired))
		for i, p := range b.Unpaired {
			un[i] = p.String()
		}
		why = append(why, "fast paths that cross no root-port pair: "+strings.Join(un, ", "))
	}
	fmt.Fprintf(w, "  the split does not follow root-port pairs, so no sub-class was written (%s).\n", strings.Join(why, "; "))
}

func pairList(pairs []model.RootPortPair) string {
	s := make([]string, len(pairs))
	for i, p := range pairs {
		s[i] = p.String()
	}
	return strings.Join(s, ", ")
}

func gbps(v uint64) string {
	return fmt.Sprintf("%.2fGbps", float64(v)/1e9)
}

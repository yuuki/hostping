package calibrate

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
)

// MergeInput is one host's baseline file.
type MergeInput struct {
	Name  string
	Bases baseline.Overlay
}

// MergeRow is the fleet baseline of one distance.
type MergeRow struct {
	Distance model.Distance
	// BandwidthBPS is the lower quartile of the hosts' bandwidths; zero when
	// no input set one.
	BandwidthBPS uint64
	// LatencyNs is the upper quartile of the hosts' latencies; zero when no
	// input set one.
	LatencyNs uint64
	BW, Lat   Spread
	// Low names the inputs that would diagnose abnormal against the merged
	// bandwidth: more than AbnormalTh below it.
	Low []string
	// SubClasses are the merged learned sub-classes of the distance, in the
	// order the inputs first name them.
	SubClasses []MergeSubClass
	// Disagree lists the sub-class root-port pairs that not every input
	// holding the distance lists.
	Disagree []PairDisagreement
}

// MergeSubClass is one learned sub-class merged across hosts.
type MergeSubClass struct {
	Name string
	// BandwidthBPS and LatencyNs take the same quartiles as the distance,
	// over the inputs that carry the sub-class.
	BandwidthBPS, LatencyNs uint64
	BW, Lat                 Spread
	// RootPortPairs are the pairs listed in the sub-class by at least half
	// of the inputs that hold the distance, sorted.
	RootPortPairs []model.RootPortPair
	// Low names the inputs whose sub-class bandwidth is more than
	// AbnormalTh below the merged one.
	Low []string
}

// PairDisagreement is a root-port pair some inputs list in a sub-class and
// others holding the same distance do not.
type PairDisagreement struct {
	SubClass string
	Pair     model.RootPortPair
	// In and NotIn name the inputs that list the pair in the sub-class and
	// those that hold the distance without listing it.
	In, NotIn []string
	// Kept reports whether the pair is in the merged sub-class.
	Kept bool
}

// MergeResult is the fleet baseline of one hardware class.
type MergeResult struct {
	Files int
	// Rows are the merged top level, one per distance.
	Rows []MergeRow
	// Sections are the merged "tables" sections, sorted by key.
	Sections []MergeSection
}

// MergeSection is one "tables" section merged across the files that have it.
type MergeSection struct {
	Key string
	// Files names the inputs that have the section, and Missing those that
	// do not.
	Files, Missing []string
	Rows           []MergeRow
}

// Merge combines per-host baselines of one hardware class into one file.
//
// Bandwidth takes the lower quartile across hosts, not the median: healthy
// hosts of one class still differ by a few percent (DIMM population, BIOS
// settings, silicon), and a fleet baseline at the median puts half of them
// below it, where the slowest healthy hosts sit closest to --abnormal-th.
// The lower quartile keeps the floor under the slower healthy hosts, while a
// single bad calibration — one host that was itself degraded — still cannot
// drag it down the way the minimum would. Latency is a ceiling rather than a
// floor, so the same reasoning takes its upper quartile.
//
// Each distance is merged over the inputs that set it; one that no input
// sets is left out and keeps its Fig.4 default.
//
// Learned sub-classes are merged by name. A root-port pair belongs to the
// merged sub-class when at least half of the inputs that hold the distance
// list it there: the hosts of one class share a board layout, so a pair one
// host learned fast and another did not is either a measurement the majority
// settles, or a sign the hosts are not of one class, which the disagreement
// warning names. The sub-class's values take the distance's quartiles over
// the inputs that carry it. A pair that the majority rule puts in two
// sub-classes of one distance is an error, as the loader would reject it.
//
// "tables" sections are merged by key, each over the inputs that have it,
// with the same rules as the top level. A section only some inputs have is
// kept, and the warning names the inputs without it.
func Merge(in []MergeInput, abnormalTh float64) (MergeResult, error) {
	if len(in) == 0 {
		return MergeResult{}, fmt.Errorf("merge needs at least one baseline file")
	}
	res := MergeResult{Files: len(in)}
	top := make([]table, len(in))
	keys := map[string]bool{}
	for i, f := range in {
		top[i] = table{Name: f.Name, Bases: f.Bases.Distances}
		for k := range f.Bases.Tables {
			keys[k] = true
		}
	}
	rows, err := mergeRows(top, abnormalTh)
	if err != nil {
		return MergeResult{}, err
	}
	res.Rows = rows
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		sec := MergeSection{Key: k}
		var tabs []table
		for _, f := range in {
			if b, ok := f.Bases.Tables[k]; ok {
				sec.Files = append(sec.Files, f.Name)
				tabs = append(tabs, table{Name: f.Name, Bases: b})
			} else {
				sec.Missing = append(sec.Missing, f.Name)
			}
		}
		if sec.Rows, err = mergeRows(tabs, abnormalTh); err != nil {
			return MergeResult{}, fmt.Errorf("%s %s: %w", baseline.TablesField, k, err)
		}
		res.Sections = append(res.Sections, sec)
	}
	return res, nil
}

// table is one input's top level or one of its sections.
type table struct {
	Name  string
	Bases map[model.Distance]baseline.PathBaseline
}

// mergeRows merges one table across the inputs, one row per distance.
func mergeRows(in []table, abnormalTh float64) ([]MergeRow, error) {
	var rows []MergeRow
	for _, d := range model.Distances() {
		var bws, lats []uint64
		for _, f := range in {
			b, ok := f.Bases[d]
			if !ok {
				continue
			}
			if b.BandwidthBPS > 0 {
				bws = append(bws, b.BandwidthBPS)
			}
			if b.LatencyNs > 0 {
				lats = append(lats, b.LatencyNs)
			}
		}
		row := MergeRow{
			Distance:     d,
			BandwidthBPS: Quantile(bws, 0.25),
			LatencyNs:    Quantile(lats, 0.75),
			BW:           SpreadOf(bws),
			Lat:          SpreadOf(lats),
		}
		for _, f := range in {
			if b, ok := f.Bases[d]; ok && b.BandwidthBPS > 0 && below(b.BandwidthBPS, row.BandwidthBPS, abnormalTh) {
				row.Low = append(row.Low, f.Name)
			}
		}
		if err := mergeSubClasses(&row, in, abnormalTh); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// mergeSubClasses merges the learned sub-classes of row's distance across
// the inputs that hold it, by name.
func mergeSubClasses(row *MergeRow, in []table, abnormalTh float64) error {
	holders, names := distanceHolders(in, row.Distance)
	owner := map[model.RootPortPair]string{}
	for _, name := range names {
		bws, lats, pairs, listed := collectSubClass(holders, name)
		m := MergeSubClass{
			Name:         name,
			BandwidthBPS: Quantile(bws, 0.25),
			LatencyNs:    Quantile(lats, 0.75),
			BW:           SpreadOf(bws),
			Lat:          SpreadOf(lats),
		}
		kept, disagree, err := majorityPairs(row.Distance, name, holders, pairs, listed, owner)
		if err != nil {
			return err
		}
		row.Disagree = append(row.Disagree, disagree...)
		if len(kept) == 0 {
			continue
		}
		m.RootPortPairs = kept
		m.Low = lowInputs(holders, name, m.BandwidthBPS, abnormalTh)
		row.SubClasses = append(row.SubClasses, m)
	}
	return nil
}

// holder is one input that holds the distance being merged, with its
// sub-classes by name.
type holder struct {
	name string
	sub  map[string]baseline.SubClass
}

// distanceHolders is the inputs that hold d, in input order, and the
// sub-class names they use, in the order the inputs first name them.
func distanceHolders(in []table, d model.Distance) ([]holder, []string) {
	var holders []holder
	var names []string
	for _, f := range in {
		b, ok := f.Bases[d]
		if !ok {
			continue
		}
		h := holder{name: f.Name, sub: map[string]baseline.SubClass{}}
		for _, sc := range b.SubClasses {
			if !slices.Contains(names, sc.Name) {
				names = append(names, sc.Name)
			}
			h.sub[sc.Name] = sc
		}
		holders = append(holders, h)
	}
	return holders, names
}

// collectSubClass gathers the sub-class name across the holders that carry
// it: the bandwidths and latencies set, the root-port pairs listed (sorted),
// and the holders listing each pair, in holder order.
func collectSubClass(holders []holder, name string) (bws, lats []uint64, pairs []model.RootPortPair, listed map[model.RootPortPair][]string) {
	listed = map[model.RootPortPair][]string{}
	for _, h := range holders {
		sc, ok := h.sub[name]
		if !ok {
			continue
		}
		if sc.BandwidthBPS > 0 {
			bws = append(bws, sc.BandwidthBPS)
		}
		if sc.LatencyNs > 0 {
			lats = append(lats, sc.LatencyNs)
		}
		for _, p := range sc.RootPortPairs {
			if _, seen := listed[p]; !seen {
				pairs = append(pairs, p)
			}
			listed[p] = append(listed[p], h.name)
		}
	}
	slices.SortFunc(pairs, comparePairs)
	return bws, lats, pairs, listed
}

// majorityPairs applies the majority rule to the pairs of sub-class name:
// a pair at least half of the holders list is kept, and one not every holder
// lists is a disagreement, in pair order. owner maps each kept pair of the
// distance to its sub-class, across names; a pair a second sub-class would
// keep is an error.
func majorityPairs(d model.Distance, name string, holders []holder, pairs []model.RootPortPair, listed map[model.RootPortPair][]string, owner map[model.RootPortPair]string) ([]model.RootPortPair, []PairDisagreement, error) {
	var kept []model.RootPortPair
	var disagree []PairDisagreement
	for _, p := range pairs {
		files := listed[p]
		keep := 2*len(files) >= len(holders)
		if keep {
			if prev, ok := owner[p]; ok {
				return nil, nil, fmt.Errorf("%s: root-port pair %s is in sub-classes %q and %q for at least half of the files each", d.Key(), p, prev, name)
			}
			owner[p] = name
			kept = append(kept, p)
		}
		if len(files) < len(holders) {
			var notIn []string
			for _, h := range holders {
				if !slices.Contains(files, h.name) {
					notIn = append(notIn, h.name)
				}
			}
			disagree = append(disagree, PairDisagreement{SubClass: name, Pair: p, In: files, NotIn: notIn, Kept: keep})
		}
	}
	return kept, disagree, nil
}

// lowInputs names the holders whose sub-class name reads more than
// abnormalTh below the merged bandwidth.
func lowInputs(holders []holder, name string, merged uint64, abnormalTh float64) []string {
	var low []string
	for _, h := range holders {
		if sc, ok := h.sub[name]; ok && sc.BandwidthBPS > 0 && below(sc.BandwidthBPS, merged, abnormalTh) {
			low = append(low, h.name)
		}
	}
	return low
}

// empty reports a row no input set anything for.
func (row MergeRow) empty() bool {
	return row.BandwidthBPS == 0 && row.LatencyNs == 0 && len(row.SubClasses) == 0
}

// Baselines is the merged result as a --baseline-file overlay.
func (r MergeResult) Baselines() baseline.Overlay {
	return overlayOf(r.Rows, r.Sections)
}

func (row MergeRow) entry() (model.Distance, baseline.PathBaseline, bool) {
	if row.empty() {
		return row.Distance, baseline.PathBaseline{}, false
	}
	b := baseline.PathBaseline{BandwidthBPS: row.BandwidthBPS, LatencyNs: row.LatencyNs}
	for _, sc := range row.SubClasses {
		b.SubClasses = append(b.SubClasses, subClassEntry(sc.Name, sc.BandwidthBPS, sc.LatencyNs, sc.RootPortPairs))
	}
	return row.Distance, b, true
}

func (s MergeSection) keyed() (string, []MergeRow) { return s.Key, s.Rows }

// WriteTable writes the merged values with the spread across hosts: the top
// level, then each section.
func (r MergeResult) WriteTable(w io.Writer) {
	fmt.Fprintf(w, "hostping calibrate merge: %d files; bandwidth = lower quartile across hosts, latency = upper quartile\n", r.Files)
	writeRows(w, r.Rows, "")
	for _, s := range r.Sections {
		fmt.Fprintf(w, "tables section %s (%d of %d files):\n", s.Key, len(s.Files), r.Files)
		writeRows(w, s.Rows, s.Key)
	}
	for _, s := range r.Sections {
		if len(s.Missing) > 0 {
			fmt.Fprintf(w, "WARNING: tables section %s is in %s but not in %s; hosts of one hardware class should derive the same Fig.4 tables, so check the files without it before distributing.\n",
				s.Key, strings.Join(s.Files, ", "), strings.Join(s.Missing, ", "))
		}
	}
}

// writeRows writes one merged table and its warnings. table is the section
// key, "" for the top level.
func writeRows(w io.Writer, rows []MergeRow, table string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "distance\tbandwidth\thosts\tmin\tp25\tmedian\tp75\tmax\tlatency\tlat hosts")
	line := func(name string, bw, lat uint64, bwS, latS Spread) {
		if bw > 0 {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s", name, gbps(bw), bwS.N,
				gbps(bwS.Min), gbps(bwS.P25), gbps(bwS.Median), gbps(bwS.P75), gbps(bwS.Max))
		} else {
			fmt.Fprintf(tw, "%s\t-\t0\t-\t-\t-\t-\t-", name)
		}
		if lat > 0 {
			fmt.Fprintf(tw, "\t%dns\t%d", lat, latS.N)
		} else {
			fmt.Fprintf(tw, "\t-\t0")
		}
		fmt.Fprintln(tw)
	}
	var absent []string
	for _, row := range rows {
		if row.empty() {
			absent = append(absent, row.Distance.Key())
			continue
		}
		line(row.Distance.Key(), row.BandwidthBPS, row.LatencyNs, row.BW, row.Lat)
		for _, sc := range row.SubClasses {
			line(baseline.ClassName(row.Distance, sc.Name), sc.BandwidthBPS, sc.LatencyNs, sc.BW, sc.Lat)
		}
	}
	_ = tw.Flush()
	for _, row := range rows {
		for _, sc := range row.SubClasses {
			fmt.Fprintf(w, "sub-class %s: root-port pairs %s (each listed by at least half of the files with %s)\n",
				inTable(table, baseline.ClassName(row.Distance, sc.Name)), pairList(sc.RootPortPairs), row.Distance.Key())
		}
	}
	if len(absent) > 0 {
		fmt.Fprintf(w, "in no file (left out, keep the Fig.4 default): %s\n", strings.Join(absent, ", "))
	}
	for _, row := range rows {
		for _, name := range row.Low {
			fmt.Fprintf(w, "WARNING: %s: %s is more than --abnormal-th below the merged %s; that host would diagnose abnormal against this file (a different hardware class, or not healthy when calibrated).\n",
				name, inTable(table, row.Distance.Key()), gbps(row.BandwidthBPS))
		}
		for _, sc := range row.SubClasses {
			for _, name := range sc.Low {
				fmt.Fprintf(w, "WARNING: %s: %s is more than --abnormal-th below the merged %s; that host would diagnose abnormal against this file (a different hardware class, or not healthy when calibrated).\n",
					name, inTable(table, baseline.ClassName(row.Distance, sc.Name)), gbps(sc.BandwidthBPS))
			}
		}
		for _, dis := range row.Disagree {
			verdict := "Left out (listed by fewer than half"
			if dis.Kept {
				verdict = "Kept (listed by at least half"
			}
			fmt.Fprintf(w, "WARNING: files disagree on root-port pair %s for %s: listed by %s; not by %s. %s of the %d files with %s.) Hosts of one board layout should agree; check the minority before distributing.\n",
				dis.Pair, inTable(table, baseline.ClassName(row.Distance, dis.SubClass)), strings.Join(dis.In, ", "), strings.Join(dis.NotIn, ", "),
				verdict, len(dis.In)+len(dis.NotIn), row.Distance.Key())
		}
	}
}

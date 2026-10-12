package baseline

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// SubClass is a learned sub-class of one distance: the paths that cross one
// of RootPortPairs are judged against its values instead of the distance's.
//
// A distance can hold paths at two clearly different levels on healthy
// hardware: on some platforms the P2P route between certain root-port pairs
// costs nothing while between others it halves the bandwidth, and nothing in
// sysfs tells the two apart. One baseline then sits at one level and a path of
// the other level is judged against a value that is not its own, so a fast
// path that falls to the slow level stays normal. hostpingctl calibrate learns
// which root-port pairs read at the fast level and records them here.
//
// Root-port IDs are BDF-based ("rp.<BDF>"), so a sub-class carries over
// between hosts of one hardware class (same board layout) and no further. A
// pair a host does not have is ignored there.
type SubClass struct {
	Name string
	// BandwidthBPS and LatencyNs replace the distance's values for the
	// sub-class's paths; zero means the distance's own value.
	BandwidthBPS uint64
	LatencyNs    uint64
	// RootPortPairs are the pairs whose paths belong to the sub-class, each
	// in canonical order, sorted.
	RootPortPairs []model.RootPortPair
}

// ClassName is the name a sub-class is reported under: "cross_root/fast".
func ClassName(d model.Distance, sub string) string {
	return d.Key() + "/" + sub
}

// ForPair resolves the baseline of a path that crosses pair: the values of
// the sub-class that lists pair, falling back to b's own for any value the
// sub-class leaves unset, and the sub-class's name. A zero pair, or a pair no
// sub-class lists, is b itself and "". The result carries no sub-classes.
func (b PathBaseline) ForPair(pair model.RootPortPair) (PathBaseline, string) {
	out := PathBaseline{BandwidthBPS: b.BandwidthBPS, LatencyNs: b.LatencyNs}
	if pair.IsZero() {
		return out, ""
	}
	for _, sc := range b.SubClasses {
		if !slices.Contains(sc.RootPortPairs, pair) {
			continue
		}
		if sc.BandwidthBPS > 0 {
			out.BandwidthBPS = sc.BandwidthBPS
		}
		if sc.LatencyNs > 0 {
			out.LatencyNs = sc.LatencyNs
		}
		return out, sc.Name
	}
	return out, ""
}

// validateSubClasses checks one distance's sub-classes: each has a name used
// once, and at least one pair; no pair has identical members or is listed
// twice, within one sub-class or across two. It canonicalizes and sorts the
// pairs in place.
func validateSubClasses(subs []SubClass) error {
	names := map[string]bool{}
	owner := map[model.RootPortPair]string{}
	for i := range subs {
		sc := &subs[i]
		if err := validateSubClassName(sc.Name); err != nil {
			return err
		}
		if names[sc.Name] {
			return fmt.Errorf("sub-class %q listed twice", sc.Name)
		}
		names[sc.Name] = true
		if sc.BandwidthBPS == 0 && sc.LatencyNs == 0 {
			return fmt.Errorf("sub-class %q: need bandwidth or latency", sc.Name)
		}
		if len(sc.RootPortPairs) == 0 {
			return fmt.Errorf("sub-class %q: need at least one root_port_pairs entry", sc.Name)
		}
		for j, p := range sc.RootPortPairs {
			if p[0] == "" || p[1] == "" {
				return fmt.Errorf("sub-class %q: root-port pair %d has an empty member", sc.Name, j)
			}
			if p[0] == p[1] {
				return fmt.Errorf("sub-class %q: root-port pair [%q, %q] has identical members", sc.Name, p[0], p[1])
			}
			c := model.NewRootPortPair(p[0], p[1])
			if prev, ok := owner[c]; ok {
				if prev == sc.Name {
					return fmt.Errorf("sub-class %q: root-port pair %s listed twice", sc.Name, c)
				}
				return fmt.Errorf("root-port pair %s listed in sub-classes %q and %q", c, prev, sc.Name)
			}
			owner[c] = sc.Name
			sc.RootPortPairs[j] = c
		}
		slices.SortFunc(sc.RootPortPairs, comparePairs)
	}
	return nil
}

// validateSubClassName keeps a name usable in "distance/name" and in a log
// line: lower-case letters, digits, '_' and '-'.
func validateSubClassName(name string) error {
	if name == "" {
		return fmt.Errorf("sub-class needs a name")
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return fmt.Errorf("sub-class name %q: use lower-case letters, digits, '_' and '-'", name)
		}
	}
	return nil
}

func comparePairs(a, b model.RootPortPair) int {
	if c := strings.Compare(a[0], b[0]); c != 0 {
		return c
	}
	return strings.Compare(a[1], b[1])
}

// describeSubClasses is the sub-class part of DescribeSources: each
// distance's sub-classes with the number of pairs each lists, or "".
func describeSubClasses(override map[model.Distance]PathBaseline) string {
	var parts []string
	for _, d := range model.Distances() {
		for _, sc := range override[d].SubClasses {
			parts = append(parts, fmt.Sprintf("%s (%d root-port pair(s))", ClassName(d, sc.Name), len(sc.RootPortPairs)))
		}
	}
	return strings.Join(parts, ", ")
}

// DescribeUnmatched is the start-up line listing the sub-class root-port
// pairs the baseline file names but this host's paths never cross, or ""
// when every pair is present. Those pairs are ignored: root-port IDs are
// BDF-based, so a file calibrated on a different board layout names pairs
// this host does not have.
func DescribeUnmatched(ov Overlay, present map[model.RootPortPair]bool) string {
	var missing []string
	add := func(prefix string, override map[model.Distance]PathBaseline) {
		for _, d := range model.Distances() {
			for _, sc := range override[d].SubClasses {
				for _, p := range sc.RootPortPairs {
					if !present[p] {
						missing = append(missing, prefix+ClassName(d, sc.Name)+" "+p.String())
					}
				}
			}
		}
	}
	add("", ov.Distances)
	for _, k := range ov.SectionKeys() {
		add("tables "+k+" ", ov.Tables[k])
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("baselines: %d sub-class root-port pair(s) not on this host, ignored (file calibrated on another board layout?): %s",
		len(missing), strings.Join(missing, ", "))
}

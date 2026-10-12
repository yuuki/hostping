package calibrate

import (
	"slices"
	"strings"

	"github.com/yuuki/hostping/internal/model"
)

// minClusterPaths is the fewest paths a cluster of a bimodal distance must
// hold. A lone path off the level of its siblings is an outlier, which the
// Outlier warning already reports; two levels need at least two paths each.
const minClusterPaths = 2

// Bimodal is a distance whose paths read at two clearly different levels on
// a healthy host, so that one per-distance baseline cannot serve both. The
// baseline is a median, so it sits at the level most paths read; a path of
// the other level is judged against a value that is not its own.
type Bimodal struct {
	Distance model.Distance
	// SlowBPS and FastBPS are the medians of the two clusters' per-path
	// medians.
	SlowBPS, FastBPS uint64
	// Slow and Fast are the paths of each cluster, sorted by RNIC, endpoint.
	Slow, Fast []PathRef
	// BaselineBPS is the calibrated baseline of the distance: over all its
	// paths, or over the slow cluster's when a sub-class was learned.
	BaselineBPS uint64
	// FastPairs are the root-port pairs whose every path is in the fast
	// cluster, MixedPairs those with paths in both clusters, and Unpaired
	// the fast paths that cross no root-port pair. A sub-class is learned
	// only when the latter two are empty.
	FastPairs, MixedPairs []model.RootPortPair
	Unpaired              []PathRef
	// SubClass is the name of the sub-class learned for the fast cluster,
	// and SubClassBPS its baseline; "" when the split does not follow
	// root-port pairs and the distance keeps one baseline.
	SubClass    string
	SubClassBPS uint64
	// Table is the key of the section the distance belongs to, "" for the
	// top level.
	Table string
}

// PathRef names one RNIC→endpoint path.
type PathRef struct{ RNIC, Endpoint string }

func (p PathRef) String() string { return p.RNIC + "->" + p.Endpoint }

// FastUnflagged reports whether a fast-cluster path that degrades to the
// slow level would still diagnose normal against the calibrated baseline.
// That is the blind spot of a slow-majority distance; with a fast majority
// the baseline sits at the fast level, and the slow paths read abnormal
// against it instead (they are reported as outliers).
func (b Bimodal) FastUnflagged(th float64) bool {
	return !below(b.SlowBPS, b.BaselineBPS, th)
}

type pathMedian struct {
	ref PathRef
	bps uint64
}

// splitBimodal decides whether the per-path medians of one distance form two
// levels, and if so returns the split.
//
// The rule: sort the medians ascending as v[0..n). Every split k with at
// least minClusterPaths on each side (lower v[0..k), upper v[k..n)) is a
// candidate, and the one with the largest relative gap v[k]/v[k-1] between
// its two neighbours is taken (the first one on a tie). The distance is
// bimodal when that gap exceeds th in the analyzer's idle-verdict sense:
// v[k-1] < v[k]·(1-th). Every lower path then sits more than th below every
// upper path, so the lower cluster's median is too: the slow level is what
// diagnose would call abnormal on a fast path if the fast cluster had a
// baseline of its own. Judging the gap rather than the two medians keeps a
// lone outlier out: forced into a two-path cluster, it would pull that
// cluster's median (a mean of two) away from a level its neighbour shares
// with the rest. A distance with fewer than 2·minClusterPaths paths, th ≤ 0,
// or a zero below the gap is never bimodal.
func splitBimodal(paths []pathMedian, th float64) (slow, fast []pathMedian, ok bool) {
	n := len(paths)
	if th <= 0 || n < 2*minClusterPaths {
		return nil, nil, false
	}
	v := slices.Clone(paths)
	slices.SortStableFunc(v, func(a, b pathMedian) int {
		switch {
		case a.bps < b.bps:
			return -1
		case a.bps > b.bps:
			return 1
		}
		return comparePath(a.ref, b.ref)
	})
	best, bestGap := 0, 0.0
	for k := minClusterPaths; k <= n-minClusterPaths; k++ {
		lo, hi := v[k-1].bps, v[k].bps
		if lo == 0 {
			continue
		}
		if g := float64(hi) / float64(lo); g > bestGap {
			best, bestGap = k, g
		}
	}
	if best == 0 {
		return nil, nil, false
	}
	if !below(v[best-1].bps, v[best].bps, th) {
		return nil, nil, false
	}
	return v[:best], v[best:], true
}

func medianOf(p []pathMedian) uint64 {
	v := make([]uint64, len(p))
	for i, x := range p {
		v[i] = x.bps
	}
	return Median(v)
}

func comparePath(a, b PathRef) int {
	if c := strings.Compare(a.RNIC, b.RNIC); c != 0 {
		return c
	}
	return strings.Compare(a.Endpoint, b.Endpoint)
}

// bimodal checks one distance for two levels, from each path's median
// reading across runs.
func bimodal(d model.Distance, paths map[pathKey][]uint64, base uint64, th float64) (Bimodal, bool) {
	pm := make([]pathMedian, 0, len(paths))
	for k, v := range paths {
		pm = append(pm, pathMedian{ref: PathRef{k.rnic, k.ep}, bps: Median(v)})
	}
	slow, fast, ok := splitBimodal(pm, th)
	if !ok {
		return Bimodal{}, false
	}
	refs := func(p []pathMedian) []PathRef {
		out := make([]PathRef, len(p))
		for i, x := range p {
			out[i] = x.ref
		}
		slices.SortFunc(out, comparePath)
		return out
	}
	return Bimodal{
		Distance:    d,
		SlowBPS:     medianOf(slow),
		FastBPS:     medianOf(fast),
		Slow:        refs(slow),
		Fast:        refs(fast),
		BaselineBPS: base,
	}, true
}

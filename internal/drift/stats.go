package drift

import (
	"math"
	"sort"
)

// median is the middle value of vs, the mean of the two middle values for an
// even count, and 0 for none. vs is not modified.
func median(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	// Halved before adding: the sum of two values near the largest float64
	// would overflow.
	return s[mid-1]/2 + s[mid]/2
}

// quantile is the q-quantile (0 to 1) of vs by linear interpolation between
// the two nearest order statistics, and 0 for none. vs is not modified.
func quantile(vs []float64, q float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	frac := pos - float64(lo)
	return s[lo]*(1-frac) + s[hi]*frac
}

// scatter is a robust standard deviation of vs: its interquartile range
// over the 1.349 standard deviations a normal distribution's spans.
//
// The median absolute deviation would not do here. It ignores any minority
// of the values, so a path that reads at one level on four days and at
// another on three would count as having no scatter at all, and that is the
// platform this guard is for.
func scatter(vs []float64) float64 {
	return (quantile(vs, 0.75) - quantile(vs, 0.25)) / iqrSigmas
}

// bestStep fits a single step to xs: the split that leaves the least total
// absolute deviation from the medians of its two sides, each at least
// stepSpan values long. at is the index of the first value after the split
// and rise how far the later median lies above the earlier one (negative
// when the series fell). A series too short for two sides has no step
// (0, 0).
//
// The sides are whole, not a few values around the split, so that the
// medians are steady against day-to-day scatter. The fit decides where the
// step is: every split near a real step gives about the same two medians,
// but only the right one leaves no value on the wrong side.
func bestStep(xs []float64) (at int, rise float64) {
	found := false
	var least float64
	for k := stepSpan; k+stepSpan <= len(xs); k++ {
		before, after := median(xs[:k]), median(xs[k:])
		cost := absDev(xs[:k], before) + absDev(xs[k:], after)
		if !found || cost < least {
			at, rise, least, found = k, after-before, cost, true
		}
	}
	return at, rise
}

// absDev is the total absolute deviation of xs from m.
func absDev(xs []float64, m float64) float64 {
	sum := 0.0
	for _, x := range xs {
		sum += math.Abs(x - m)
	}
	return sum
}

// mannKendallZ is the Mann-Kendall trend statistic of xs in time order, as a
// standard normal deviate: positive for a rising series, negative for a
// falling one, and 0 when there is no trend to speak of (fewer than three
// values, or all equal). Ties are corrected for in the variance.
func mannKendallZ(xs []float64) float64 {
	n := len(xs)
	if n < 3 {
		return 0
	}
	s := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			switch {
			case xs[j] > xs[i]:
				s++
			case xs[j] < xs[i]:
				s--
			}
		}
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	variance := float64(n * (n - 1) * (2*n + 5))
	for i := 0; i < n; {
		j := i
		for j < n && sorted[j] == sorted[i] {
			j++
		}
		if t := j - i; t > 1 {
			variance -= float64(t * (t - 1) * (2*t + 5))
		}
		i = j
	}
	variance /= 18
	if variance <= 0 || s == 0 {
		return 0
	}
	// The continuity correction moves S one step towards 0.
	if s > 0 {
		return float64(s-1) / math.Sqrt(variance)
	}
	return float64(s+1) / math.Sqrt(variance)
}

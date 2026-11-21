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

// scatter is how much of the movement of a path's daily values counts as
// noise, as a standard deviation. xs is the values of the window in time
// order and ref the reference, both turned so that higher is worse.
//
// Noise goes both ways. A path that scatters reads better than usual about
// as often as it reads worse, so its day-to-day scatter (dayScatter) shows
// on the better side too. A path that is degraded on some days and fine on
// others moves as much from day to day, but only to the worse side: that
// movement is the degradation, and it must not raise the bar the
// degradation has to clear. So the day-to-day scatter counts only as far as
// the better side bears it out, with upsideFactor of slack.
//
// The better side is read in two ways, and the larger counts. Against the
// reference (upsideOfReference), which sees a path that swings evenly
// around its reference between two levels. And among the path's best days
// themselves (upsideOfBestDays), which does not depend on the reference:
// that is a median of refDays values and lies off the path's true level by
// a share of the scatter, and where it lies on the better side, the first
// reading alone would take real noise for degradation.
func scatter(xs []float64, ref float64) float64 {
	return math.Min(dayScatter(xs), upsideFactor*math.Max(upsideOfReference(xs, ref), upsideOfBestDays(xs)))
}

// provenScatter is scatter without the reading among the best days. It is
// what may raise the threshold of a signal that is already drifted. There
// the window holds the degradation itself, and when most of its days are
// degraded the best days are the few fine ones against the many degraded
// ones, which upsideOfBestDays would take for scatter. Values on the better
// side of the reference cannot be degradation.
func provenScatter(xs []float64, ref float64) float64 {
	return math.Min(dayScatter(xs), upsideFactor*upsideOfReference(xs, ref))
}

// upsideOfReference is how far the daily values xs reach on the better side
// of ref, as the standard deviation of a normal scatter around ref that
// would reach as far: the distance from ref to the upsideQuantile of xs (its
// best days), over upsideSigmas. It is 0 when the best days are no better
// than ref, and for fewer than minScatterDays values. xs and ref are turned
// so that higher is worse.
func upsideOfReference(xs []float64, ref float64) float64 {
	if len(xs) < minScatterDays {
		return 0
	}
	return math.Max(0, ref-quantile(xs, upsideQuantile)) / upsideSigmas
}

// upsideOfBestDays is how far the best days of xs lie from its good days,
// as the standard deviation of a normal scatter with that spread: the
// distance from the goodQuantile of xs to its upsideQuantile, over
// goodSigmas. It is 0 for fewer than minScatterDays values.
func upsideOfBestDays(xs []float64) float64 {
	if len(xs) < minScatterDays {
		return 0
	}
	return (quantile(xs, goodQuantile) - quantile(xs, upsideQuantile)) / goodSigmas
}

// dayScatter is a robust standard deviation of the day-to-day noise of xs,
// a series in time order: the mean absolute difference of consecutive
// values, without the largest fifth of them, over the trimSigmas standard
// deviations that mean has for independent normal noise. It is 0 for fewer
// than minScatterDays values.
//
// Differences of neighbours see neither a slow trend, whose share of one
// difference is small, nor a step, which is one difference among many and
// is trimmed. So the series may hold the very drift it is the yardstick
// for. The trimmed mean is used, and not the median, because it varies less
// from one window to the next: a threshold built on an estimate that comes
// out too small now and then is what reports noise as drift.
func dayScatter(xs []float64) float64 {
	if len(xs) < minScatterDays {
		return 0
	}
	diffs := make([]float64, len(xs)-1)
	for i := range diffs {
		diffs[i] = math.Abs(xs[i+1] - xs[i])
	}
	sort.Float64s(diffs)
	kept := diffs[:len(diffs)-len(diffs)/trimShare]
	sum := 0.0
	for _, d := range kept {
		sum += d
	}
	return sum / float64(len(kept)) / trimSigmas
}

// bestStep fits a single step to xs: the split that leaves the least total
// absolute deviation from the medians of its two sides, each at least
// stepSpan values long. at is the index of the first value after the split,
// rise how far the later median lies above the earlier one (negative when
// the series fell) and cost the deviation that is left. A series too short
// for two sides has no step: ok is false.
//
// The sides are whole, not a few values around the split, so that the
// medians are steady against day-to-day scatter. The fit decides where the
// step is: every split near a real step gives about the same two medians,
// but only the right one leaves no value on the wrong side.
func bestStep(xs []float64) (at int, rise, cost float64, ok bool) {
	for k := stepSpan; k+stepSpan <= len(xs); k++ {
		before, after := median(xs[:k]), median(xs[k:])
		c := absDev(xs[:k], before) + absDev(xs[k:], after)
		if !ok || c < cost {
			at, rise, cost, ok = k, after-before, c, true
		}
	}
	return at, rise, cost, ok
}

// lineCost is the total absolute deviation of xs, read at the times ts, from
// the straight line through them that the Theil-Sen estimator fits: the
// median of the slopes between every two points, and the median intercept
// that slope leaves. Like the medians of bestStep it gives way to no single
// value.
func lineCost(ts, xs []float64) float64 {
	var slopes []float64
	for i := range xs {
		for j := i + 1; j < len(xs); j++ {
			if ts[j] != ts[i] {
				slopes = append(slopes, (xs[j]-xs[i])/(ts[j]-ts[i]))
			}
		}
	}
	slope := median(slopes)
	rest := make([]float64, len(xs))
	for i := range xs {
		rest[i] = xs[i] - slope*ts[i]
	}
	return absDev(rest, median(rest))
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

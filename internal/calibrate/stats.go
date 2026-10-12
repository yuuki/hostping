package calibrate

import (
	"math"
	"slices"
)

// Spread is the five-number summary of a set of readings.
type Spread struct {
	N                          int
	Min, P25, Median, P75, Max uint64
}

// Quantile is the q-quantile (0 ≤ q ≤ 1) of v by linear interpolation between
// the closest ranks (the R-7 / NumPy default), rounded to the nearest integer.
// It does not modify v. An empty v returns 0.
func Quantile(v []uint64, q float64) uint64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return quantileSorted(s, q)
}

func quantileSorted(s []uint64, q float64) uint64 {
	if q <= 0 {
		return s[0]
	}
	if q >= 1 {
		return s[len(s)-1]
	}
	h := q * float64(len(s)-1)
	lo := int(math.Floor(h))
	hi := int(math.Ceil(h))
	if lo == hi {
		return s[lo]
	}
	a, b := float64(s[lo]), float64(s[hi])
	return uint64(math.Round(a + (h-float64(lo))*(b-a)))
}

// Median is Quantile(v, 0.5): the middle reading, or the mean of the two
// middle readings for an even count.
func Median(v []uint64) uint64 { return Quantile(v, 0.5) }

// SpreadOf summarizes v. An empty v is the zero Spread.
func SpreadOf(v []uint64) Spread {
	if len(v) == 0 {
		return Spread{}
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return Spread{
		N:      len(s),
		Min:    s[0],
		P25:    quantileSorted(s, 0.25),
		Median: quantileSorted(s, 0.5),
		P75:    quantileSorted(s, 0.75),
		Max:    s[len(s)-1],
	}
}

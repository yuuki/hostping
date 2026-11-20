package drift

import (
	"math"
	"testing"
)

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{7}, 7},
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{1, 1, 1, 100}, 1},
		// The two middle values are halved before they are added.
		{[]float64{math.MaxFloat64, math.MaxFloat64}, math.MaxFloat64},
	}
	for _, c := range cases {
		in := append([]float64(nil), c.in...)
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
		for i := range in {
			if in[i] != c.in[i] {
				t.Fatalf("median reordered its input: %v -> %v", in, c.in)
			}
		}
	}
}

func TestScatter(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	// Seven equally spaced values: quartiles at positions 1.5 and 4.5.
	if got := scatter([]float64{1, 2, 3, 4, 5, 6, 7}); !near(got, 3/iqrSigmas) {
		t.Errorf("ladder: %v, want %v", got, 3/iqrSigmas)
	}
	// One outlier in seven moves neither quartile.
	if got := scatter([]float64{10, 10, 10, 10, 10, 10, 1}); got != 0 {
		t.Errorf("one outlier: %v, want 0", got)
	}
	// A minority level of three in seven is scatter, which a median
	// absolute deviation would call 0.
	if got := scatter([]float64{10, 9, 10, 9, 10, 9, 10}); !near(got, 1/iqrSigmas) {
		t.Errorf("two levels: %v, want %v", got, 1/iqrSigmas)
	}
	if got := scatter(nil); got != 0 {
		t.Errorf("no values: %v", got)
	}
	if got := scatter([]float64{5}); got != 0 {
		t.Errorf("one value: %v", got)
	}
}

func TestBestStep(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		at   int
		rise float64
	}{
		{"too short", []float64{1, 1, 1, 9, 9}, 0, 0},
		{"flat", []float64{5, 5, 5, 5, 5, 5, 5, 5}, 3, 0},
		{"step up", []float64{1, 1, 1, 1, 1, 9, 9, 9, 9}, 5, 8},
		{"step up at the earliest split", []float64{1, 1, 1, 9, 9, 9, 9, 9}, 3, 8},
		{"step up at the latest split", []float64{1, 1, 1, 1, 1, 9, 9, 9}, 5, 8},
		{"step down", []float64{9, 9, 9, 9, 1, 1, 1, 1}, 4, -8},
		// One value off on each side does not move the split.
		{"step with outliers", []float64{1, 7, 1, 1, 1, 9, 9, 2, 9, 9}, 5, 8},
		// A steady rise has its best split in the middle, worth half of it.
		{"ramp", []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 5, 5},
	}
	for _, c := range cases {
		at, rise := bestStep(c.in)
		if at != c.at || rise != c.rise {
			t.Errorf("%s: bestStep = (%d, %v), want (%d, %v)", c.name, at, rise, c.at, c.rise)
		}
	}
}

func TestMannKendallZ(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-3 }
	// A strictly rising series of 10: S = 45, variance 125, Z = 44/sqrt(125).
	rising := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := mannKendallZ(rising); !near(got, 3.9355) {
		t.Errorf("rising: %v, want 3.9355", got)
	}
	falling := []float64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}
	if got := mannKendallZ(falling); !near(got, -3.9355) {
		t.Errorf("falling: %v, want -3.9355", got)
	}
	// Ties shrink the variance: {1,1,2,2,3,3,4,4} has S = 24 and, with four
	// tied pairs, variance (8*7*21 - 4*2*1*9)/18 = 61.333.
	if got := mannKendallZ([]float64{1, 1, 2, 2, 3, 3, 4, 4}); !near(got, 23/math.Sqrt(61.3333)) {
		t.Errorf("ties: %v, want %v", got, 23/math.Sqrt(61.3333))
	}
	for name, xs := range map[string][]float64{
		"constant":  {5, 5, 5, 5, 5, 5, 5, 5},
		"too short": {1, 2},
		"empty":     nil,
		"balanced":  {1, 3, 2, 3, 1, 2, 2},
	} {
		if got := mannKendallZ(xs); got != 0 {
			t.Errorf("%s: %v, want 0", name, got)
		}
	}
	// A zigzag around a level is no trend.
	if got := mannKendallZ([]float64{1, 2, 1, 2, 1, 2, 1, 2, 1, 2}); math.Abs(got) >= trendZ {
		t.Errorf("zigzag: %v reaches the trend bar", got)
	}
}

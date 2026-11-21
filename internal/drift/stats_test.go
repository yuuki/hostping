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

// The quantile is read by linear interpolation between the two nearest
// order statistics, whatever the order of the values.
func TestQuantile(t *testing.T) {
	ladder := []float64{50, 10, 40, 20, 30} // 10..50
	for _, c := range []struct {
		q, want float64
	}{
		{0, 10}, {0.10, 14}, {0.25, 20}, {0.5, 30}, {0.75, 40}, {1, 50},
	} {
		if got := quantile(ladder, c.q); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("quantile(10..50, %v) = %v, want %v", c.q, got, c.want)
		}
	}
	if ladder[0] != 50 {
		t.Fatalf("quantile reordered its input: %v", ladder)
	}
	if got := quantile(nil, 0.5); got != 0 {
		t.Errorf("no values: %v", got)
	}
	if got := quantile([]float64{7}, 0.10); got != 7 {
		t.Errorf("one value: %v", got)
	}
}

func TestBestStep(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		ok   bool
		at   int
		rise float64
		cost float64
	}{
		{"too short", []float64{1, 1, 1, 9, 9}, false, 0, 0, 0},
		{"flat", []float64{5, 5, 5, 5, 5, 5, 5, 5}, true, 3, 0, 0},
		{"step up", []float64{1, 1, 1, 1, 1, 9, 9, 9, 9}, true, 5, 8, 0},
		{"step up at the earliest split", []float64{1, 1, 1, 9, 9, 9, 9, 9}, true, 3, 8, 0},
		{"step up at the latest split", []float64{1, 1, 1, 1, 1, 9, 9, 9}, true, 5, 8, 0},
		{"step down", []float64{9, 9, 9, 9, 1, 1, 1, 1}, true, 4, -8, 0},
		// One value off on each side does not move the split.
		{"step with outliers", []float64{1, 7, 1, 1, 1, 9, 9, 2, 9, 9}, true, 5, 8, 13},
		// A steady rise has its best split in the middle, worth half of it,
		// and every value but the two middle ones is off its side's median.
		{"ramp", []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, true, 5, 5, 12},
	}
	for _, c := range cases {
		at, rise, cost, ok := bestStep(c.in)
		if ok != c.ok || at != c.at || rise != c.rise || cost != c.cost {
			t.Errorf("%s: bestStep = (%d, %v, %v, %v), want (%d, %v, %v, %v)", c.name, at, rise, cost, ok, c.at, c.rise, c.cost, c.ok)
		}
	}
}

// A straight line costs a ramp nothing and a step a lot; a step costs a step
// nothing and a ramp a lot. That is what tells the two shapes apart.
func TestLineCost(t *testing.T) {
	days := func(n int) []float64 {
		ts := make([]float64, n)
		for i := range ts {
			ts[i] = float64(20500 + i)
		}
		return ts
	}
	ramp := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if got := lineCost(days(10), ramp); got != 0 {
		t.Errorf("ramp: line cost %v, want 0", got)
	}
	step := []float64{1, 1, 1, 1, 1, 9, 9, 9, 9, 9}
	_, _, stepCost, _ := bestStep(step)
	if line := lineCost(days(10), step); stepCost != 0 || line < 10 {
		t.Errorf("step: step cost %v, line cost %v, want 0 and far above it", stepCost, line)
	}
	_, _, stepCost, _ = bestStep(ramp)
	if stepCost <= lineCost(days(10), ramp) {
		t.Errorf("ramp: step cost %v is not above the line's", stepCost)
	}
	// The line follows the days, not the positions: a ramp read on every
	// third day is still a line.
	gaps := []float64{20500, 20503, 20506, 20509, 20512, 20515, 20518, 20521}
	if got := lineCost(gaps, []float64{0, 3, 6, 9, 12, 15, 18, 21}); got != 0 {
		t.Errorf("ramp with gaps: line cost %v, want 0", got)
	}
	// One outlier costs its own distance and does not tilt the line.
	bent := []float64{0, 1, 2, 30, 4, 5, 6, 7, 8, 9}
	if got := lineCost(days(10), bent); got != 27 {
		t.Errorf("ramp with an outlier: line cost %v, want 27", got)
	}
	if got := lineCost(nil, nil); got != 0 {
		t.Errorf("no values: %v", got)
	}
	if got := lineCost([]float64{1, 1, 1}, []float64{4, 5, 6}); got != 2 {
		t.Errorf("one day read three times: %v, want the deviation from the median", got)
	}
}

// The day-to-day scatter of a series sees its noise and neither a trend nor
// a step in it.
func TestDayScatter(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if got := dayScatter([]float64{1, 2, 1, 2, 1, 2, 1}); got != 0 {
		t.Errorf("fewer than %d values: %v, want 0", minScatterDays, got)
	}
	// Differences of 1 throughout: their mean is 1 with or without the
	// largest fifth, and 1 over 0.790 as a standard deviation.
	const want = 1 / 0.790
	zigzag := []float64{1, 2, 1, 2, 1, 2, 1, 2}
	if got := dayScatter(zigzag); !near(got, want) {
		t.Errorf("zigzag: %v, want %v", got, want)
	}
	var onRamp, onStep []float64
	for i, v := range zigzag {
		onRamp = append(onRamp, v+0.1*float64(i))
		if i >= 4 {
			v += 100
		}
		onStep = append(onStep, v)
	}
	if got := dayScatter(onStep); !near(got, want) {
		t.Errorf("zigzag on a step: %v, want the zigzag's %v", got, want)
	}
	// A trend adds no more than its slope to a difference: 0.1 a day here.
	if got := dayScatter(onRamp); got < 0.9*want || got > 1.1*want+1e-9 {
		t.Errorf("zigzag on a ramp: %v, want within the slope of %v", got, want)
	}
	if got := dayScatter([]float64{5, 5, 5, 5, 5, 5, 5, 5, 5}); got != 0 {
		t.Errorf("constant: %v", got)
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

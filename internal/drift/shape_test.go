package drift

import (
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// p50Days is a window of consecutive days with the given p50 latencies, a
// signal for which higher is worse.
func p50Days(xs ...float64) []Day {
	ds := make([]Day, len(xs))
	for i, x := range xs {
		ds[i] = Day{Day: 20500 + i, N: 1, Reading: Reading{BandwidthBPS: baseBW, LatP50Ns: x, LatP99Ns: x}}
	}
	return ds
}

// The shape of a drift is read at exactly the documented points: from 8
// daily values on, as a step when the best single step carries two thirds of
// the change, and as gradual when the Mann-Kendall statistic reaches the
// one-sided 1% point, 2.326.
func TestShapeBoundaries(t *testing.T) {
	p50 := signals[1]
	if p50.name != model.DriftLatencyP50 {
		t.Fatalf("signals[1] is %s", p50.name)
	}

	// Seven days are no shape, eight are.
	step := p50Days(100, 100, 100, 100, 110, 110, 110, 110)
	if got, _ := shapeOf(p50, step[:7], 10); got != model.DriftShapeUnknown {
		t.Errorf("7 daily values: %s, want unknown", got)
	}
	if got, on := shapeOf(p50, step, 10); got != model.DriftShapeStep || on != 20504 {
		t.Errorf("8 daily values: %s on day %d, want a step on the first day at the new level", got, on)
	}

	// A step of 10 is a step for a change of up to 15 against the reference:
	// two thirds of it. Past that the window shows too little of the change.
	// (It then reads as a trend: five days at one level and five at a higher
	// one have a statistic of 2.51.)
	step = p50Days(100, 100, 100, 100, 100, 110, 110, 110, 110, 110)
	if got, _ := shapeOf(p50, step, 14.9); got != model.DriftShapeStep {
		t.Errorf("a step of 10 in a change of 14.9: %s, want step", got)
	}
	if got, _ := shapeOf(p50, step, 15.2); got != model.DriftShapeGradual {
		t.Errorf("a step of 10 in a change of 15.2: %s, want gradual", got)
	}

	// Ten distinct values have a variance of 125. With 9 of the 45 pairs
	// falling the statistic is (27-1)/sqrt(125) = 2.3255, just under 2.326;
	// with 8 it is (29-1)/sqrt(125) = 2.504. The change is given as far more
	// than any step in the window, so that no step is read.
	under := p50Days(4, 3, 2, 1, 6, 5, 8, 7, 10, 9)
	over := p50Days(4, 3, 2, 1, 6, 5, 8, 7, 9, 10)
	if z := mannKendallZ([]float64{4, 3, 2, 1, 6, 5, 8, 7, 10, 9}); z < 2.3254 || z > 2.3256 {
		t.Fatalf("statistic %v, want 2.3255", z)
	}
	if got, _ := shapeOf(p50, under, 1000); got != model.DriftShapeSustained {
		t.Errorf("a statistic of 2.3255: %s, want sustained", got)
	}
	if got, _ := shapeOf(p50, over, 1000); got != model.DriftShapeGradual {
		t.Errorf("a statistic of 2.504: %s, want gradual", got)
	}
	// Eleven distinct values have a variance of 165. With 12 of the 55 pairs
	// falling the statistic is (31-1)/sqrt(165) = 2.3355, just over 2.326.
	justOver := []float64{4, 3, 2, 1, 8, 7, 6, 5, 9, 10, 11}
	if z := mannKendallZ(justOver); z < 2.3354 || z > 2.3356 {
		t.Fatalf("statistic %v, want 2.3355", z)
	}
	if got, _ := shapeOf(p50, p50Days(justOver...), 1000); got != model.DriftShapeGradual {
		t.Errorf("a statistic of 2.3355: %s, want gradual", got)
	}

	// A falling latency is no trend to the worse.
	if got, _ := shapeOf(p50, p50Days(10, 9, 8, 7, 6, 5, 4, 3, 2, 1), 1000); got != model.DriftShapeSustained {
		t.Errorf("a falling latency: %s, want sustained", got)
	}
	// For bandwidth, falling is the worse side.
	bw := signals[0]
	var falling []Day
	for i := 0; i < 10; i++ {
		falling = append(falling, Day{Day: 20500 + i, N: 1, Reading: Reading{BandwidthBPS: baseBW * (1 - 0.01*float64(i)), LatP50Ns: 1, LatP99Ns: 1}})
	}
	if got, _ := shapeOf(bw, falling, baseBW); got != model.DriftShapeGradual {
		t.Errorf("a falling bandwidth: %s, want gradual", got)
	}
}

// A step is dated to the first day at the new level, from three days at it
// on.
func TestStepIsDatedFromThreeDays(t *testing.T) {
	p50 := signals[1]
	three := p50Days(100, 100, 100, 100, 100, 100, 100, 110, 110, 110)
	if got, on := shapeOf(p50, three, 10); got != model.DriftShapeStep || on != 20507 {
		t.Errorf("three days at the new level: %s on day %d, want a step on day 20507", got, on)
	}
	early := p50Days(100, 100, 100, 110, 110, 110, 110, 110, 110, 110)
	if got, on := shapeOf(p50, early, 10); got != model.DriftShapeStep || on != 20503 {
		t.Errorf("three days at the old level: %s on day %d, want a step on day 20503", got, on)
	}
}

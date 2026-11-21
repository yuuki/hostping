package drift

import (
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// A path that scatters to both sides of its level needs a larger change than
// --drift-th: three times its day-to-day scatter. The threshold in force is
// part of the path's state, so that a change above --drift-th that is not
// reported can be explained.
func TestScatterRaisesTheThreshold(t *testing.T) {
	// At its level, 6% above it, at its level, 6% below it, and again: a
	// day-to-day difference of 6% around a reference at the level.
	swing := func(i int) float64 { return 1 + 0.06*float64([]int{0, 1, 0, -1}[i%4]) }
	s := newSeries(t)
	s.days(60, swing)
	if s.last.Status != model.DriftStable || s.flips != 0 {
		t.Fatalf("status %s after %d flips, want stable throughout", s.last.Status, s.flips)
	}
	// The mean absolute difference, 6%, over 0.790 is a scatter of 7.6%.
	want := 3 * (0.06 / 0.790)
	if th := threshold(t, s.last, model.DriftBandwidth); th < want*0.999 || th > want*1.001 {
		t.Fatalf("threshold %v, want three times the day-to-day scatter, %v", th, want)
	}
	// The lower level alone, for good: 6% below the reference, and inside
	// what the path has shown of itself.
	s.days(7, func(int) float64 { return 0.94 })
	if s.last.Status != model.DriftStable {
		t.Fatalf("at its lower level for a week: %s, want stable", s.last.Status)
	}
	if c := change(t, s.last, model.DriftBandwidth); c < 0.059 || c > 0.061 {
		t.Fatalf("change %v, want the 6%% still reported", c)
	}
}

// A reference week that happened to be quiet does not make later scatter a
// drift: a path that then reads 6% low and 6% high in turn shows its scatter
// on the better side of the reference as well, and the 6% of its lower level
// are inside it.
func TestScatterAfterAQuietReference(t *testing.T) {
	// From day 8 on the path reads 6% low and 6% high in turn, low on four
	// days of every seven: the recent median is 6% below the reference.
	swing := func(i int) float64 {
		if i%7%2 == 0 {
			return 0.94
		}
		return 1.06
	}
	s := newSeries(t)
	s.days(7, flat)
	s.days(90, swing)
	if c := change(t, s.last, model.DriftBandwidth); c < 0.059 || c > 0.061 {
		t.Fatalf("change %v, want the 6%% of the lower level", c)
	}
	if s.last.Status != model.DriftStable || s.flips != 0 || len(s.logs) != 0 {
		t.Fatalf("status %s after %d flips, want stable throughout: the path scatters by more than it moved (%q)", s.last.Status, s.flips, s.logs)
	}
	// The same level without the scatter is a drift, from the first verdict
	// on.
	q := newSeries(t)
	q.days(7, flat)
	q.days(90, func(int) float64 { return 0.94 })
	if q.last.Status != model.DriftDrifted || q.flips != 0 {
		t.Fatalf("6%% down on a quiet path: %s after %d flips", q.last.Status, q.flips)
	}
}

// A device that is degraded on some days and fine on others moves a lot from
// day to day, but only ever to the worse side of its reference. That
// movement is the degradation. It must not count as scatter, raise the
// threshold and report the path as recovered while it reads as low as when
// it was reported.
func TestIntermittentDegradationIsNotScatter(t *testing.T) {
	g := &gauss{state: 7}
	s := newSeries(t)
	first := -1
	for d := 0; d < 150; d++ {
		level := 1.0
		if d >= 30 && d%3 != 0 {
			level = 0.85 // low on two days out of three
		}
		got := s.day(baseBW*level*(1+0.005*g.normal()), baseP50, baseP99)
		if got.Status == model.DriftDrifted && first < 0 {
			first = d
		}
		if first >= 0 && got.Status != model.DriftDrifted {
			t.Fatalf("day %d: %s at a change of %.1f%%, drifted since day %d: %q", d, got.Status, change(t, got, model.DriftBandwidth)*100, first, s.logs)
		}
	}
	if first < 33 || first > 40 {
		t.Fatalf("first drifted on day %d, want within days of the onset on day 30", first)
	}
	if s.logged("recovered") || s.logged("no longer counted") {
		t.Fatalf("the verdict was withdrawn: %q", s.logs)
	}
	f := s.finding(model.DriftBandwidth)
	if f.Threshold != 0.05 || f.Change < 0.14 || f.WorseDays < 4 || f.RecentDays != 7 {
		t.Fatalf("finding = %+v, want the 5%% threshold it drifted at, a change of 15%% and most recent days worse", f)
	}
	if th := threshold(t, s.last, model.DriftBandwidth); th != 0.05 {
		t.Fatalf("threshold in the path's state = %v, want 0.05", th)
	}
}

// The same on random days, over many paths: a path that reads 15% low on
// more than half of its days is reported on nearly all of them and is not
// logged as recovered again and again.
func TestIntermittentDegradationOverManyPaths(t *testing.T) {
	const paths, days = 100, 150
	for _, c := range []struct {
		share      float64 // share of days that read low
		minDrifted float64 // least share of the days 60..149 judged drifted
		maxLines   float64 // most "recovered" lines per path
	}{
		{0.55, 0.80, 2.5},
		{0.65, 0.93, 1.0},
		{0.75, 0.98, 0.4},
		{1.00, 1.00, 0},
	} {
		drifted, total, lines := 0, 0, 0
		for p := 0; p < paths; p++ {
			g := &gauss{state: uint64(p + 1)}
			s := newSeries(t)
			for d := 0; d < days; d++ {
				level := 1.0
				if d >= 30 && g.uniform() < c.share {
					level = 0.85
				}
				got := s.day(baseBW*level*(1+0.005*g.normal()), baseP50, baseP99)
				if d >= 60 {
					total++
					if got.Status == model.DriftDrifted {
						drifted++
					}
				}
			}
			for _, l := range s.logs {
				if strings.Contains(l, "recovered") || strings.Contains(l, "no longer counted") {
					lines++
				}
			}
		}
		share, perPath := float64(drifted)/float64(total), float64(lines)/paths
		if share < c.minDrifted || perPath > c.maxLines {
			t.Errorf("low on %.0f%% of days: drifted on %.0f%% of the days after the first month, %.1f withdrawn verdicts per path; want at least %.0f%% and at most %.1f",
				c.share*100, share*100, perPath, c.minDrifted*100, c.maxLines)
		}
	}
}

// Low days among the seven reference days are not scatter either. Two of
// them leave the reference where the other five put it and the threshold at
// --drift-th, so a lasting loss beyond it is reported.
func TestLowReferenceDaysDoNotRaiseTheThreshold(t *testing.T) {
	for _, dip := range []float64{0.97, 0.95, 0.90, 0.85} {
		g := &gauss{state: 3}
		s := newSeries(t)
		for d := 0; d < 7; d++ {
			level := 1.0
			if d == 2 || d == 5 {
				level = dip
			}
			s.day(baseBW*level*(1+0.002*g.normal()), baseP50, baseP99)
		}
		s.days(60, func(int) float64 { return 1 + 0.002*g.normal() })
		if th := threshold(t, s.last, model.DriftBandwidth); th != 0.05 {
			t.Errorf("two reference days at %.0f%%: threshold %v two months later, want 0.05", dip*100, th)
		}
		if ref := s.tr.File().Paths[gpuPath.Key].Ref.BandwidthBPS; ref < baseBW*0.995 || ref > baseBW*1.005 {
			t.Errorf("two reference days at %.0f%%: reference %v, want the level of the other five", dip*100, ref)
		}
		s.days(40, func(int) float64 { return 0.92 * (1 + 0.002*g.normal()) })
		if s.last.Status != model.DriftDrifted {
			t.Errorf("two reference days at %.0f%%: a lasting loss of 8%% is %s with threshold %v, want drifted", dip*100, s.last.Status, threshold(t, s.last, model.DriftBandwidth))
		}
	}
}

// The same holds while the low reference days are still in the window: the
// first verdict is not held up by them.
func TestLowReferenceDaysInTheWindow(t *testing.T) {
	s := newSeries(t)
	s.days(7, func(i int) float64 {
		if i == 2 || i == 5 {
			return 0.90
		}
		return 1
	})
	s.days(8, func(int) float64 { return 0.92 })
	if s.last.Status != model.DriftDrifted || threshold(t, s.last, model.DriftBandwidth) != 0.05 {
		t.Fatalf("8%% down right after a reference week with two low days: %s with threshold %v", s.last.Status, threshold(t, s.last, model.DriftBandwidth))
	}
}

// A path that read at a lower level on a minority of its reference days and
// reads there on every day now is reported. The lower level is not scatter:
// treating it as such is what hid a device that throttles on some days. See
// docs/design.md for what that costs on platforms with two healthy levels.
func TestLowerLevelOfTheReferenceIsNotScatter(t *testing.T) {
	s := newSeries(t)
	s.days(7, func(i int) float64 { return 1 - 0.08*float64(i%2) }) // 4 days fast, 3 slow
	s.days(30, func(int) float64 { return 0.92 })
	if s.last.Status != model.DriftDrifted || s.flips != 0 {
		t.Fatalf("status %s after %d flips, want drifted from the first verdict on", s.last.Status, s.flips)
	}
	if f := s.finding(model.DriftBandwidth); f.Threshold != 0.05 || f.Change < 0.079 || f.Change > 0.081 {
		t.Fatalf("finding = %+v, want 8%% against a threshold of 5%%", f)
	}
}

// Scatter that the path only shows once it is drifted raises its threshold
// when it shows on the better side of the reference too, and only then.
func TestThresholdOfADriftedSignal(t *testing.T) {
	// Drifted at 8% down, quiet. Then the path starts to swing by 10%
	// around its reference: the days it reads low are within its scatter.
	s := newSeries(t)
	s.days(7, flat)
	s.days(10, func(int) float64 { return 0.92 })
	if s.last.Status != model.DriftDrifted {
		t.Fatalf("8%% down: %s", s.last.Status)
	}
	s.days(40, func(i int) float64 { return 1 + 0.10*float64(1-2*(i%2)) })
	if s.last.Status != model.DriftStable {
		t.Fatalf("swinging 10%% around the reference: %s, want the verdict withdrawn", s.last.Status)
	}
	// Drifted at 8% down, then 20% down on every other day: more movement,
	// all of it below the reference. The threshold stays.
	q := newSeries(t)
	q.days(7, flat)
	q.days(10, func(int) float64 { return 0.92 })
	q.days(60, func(i int) float64 { return 1 - 0.20*float64(i%2) })
	if q.last.Status != model.DriftDrifted || q.flips != 0 || threshold(t, q.last, model.DriftBandwidth) != 0.05 {
		t.Fatalf("20%% down on every other day: %s after %d flips with threshold %v", q.last.Status, q.flips, threshold(t, q.last, model.DriftBandwidth))
	}
}

// What the scatter of a window is made of, on the values themselves.
func TestScatterEstimates(t *testing.T) {
	// Higher is worse here, as scatter is given its values.
	swing := []float64{-1, 1, -1, 1, -1, 1, -1, 1, -1, 1}
	oneSided := []float64{0, 5, 0, 5, 0, 5, 0, 5, 0, 5}
	if got, want := dayScatter(swing), 2/0.790; got < want*0.999 || got > want*1.001 {
		t.Errorf("day-to-day scatter of a swing of 2: %v, want %v", got, want)
	}
	// Half of the days lie 1 on the better side of the reference: a normal
	// scatter whose best tenth reaches that far has a standard deviation of
	// 1/1.2816, and three times that is how much of the day-to-day scatter
	// counts.
	bestSide := 3 * (1 / 1.2816)
	if got := scatter(swing, 0); got < bestSide*0.999 || got > bestSide*1.001 {
		t.Errorf("scatter of a swing around the reference: %v, want %v", got, bestSide)
	}
	// A swing whose day-to-day differences are smaller than its reach counts
	// in full.
	slow := []float64{-1, -1, -1, 0, 1, 1, 1, 0, -1, -1, -1, 0, 1, 1, 1, 0}
	if got := scatter(slow, 0); got != dayScatter(slow) || got == 0 {
		t.Errorf("scatter of a slow swing: %v, want its day-to-day scatter %v", got, dayScatter(slow))
	}
	if got := scatter(oneSided, 0); got != 0 {
		t.Errorf("scatter of days that are at the reference or worse: %v, want 0", got)
	}
	if got := dayScatter(oneSided); got < 5 {
		t.Errorf("day-to-day scatter of those days: %v, want it large: that is why it must not count alone", got)
	}
	// The same days against a reference that lies on their worse side are
	// a swing around it.
	if got := scatter(oneSided, 2.5); got == 0 {
		t.Errorf("scatter of days on both sides of the reference: %v", got)
	}
	// A reference that lies on the better side of noisy days hides their
	// better side from it. Their best days still lie apart from their good
	// days, and that counts.
	noisy := []float64{0.2, -1.3, 0.9, -0.4, 1.6, -0.8, 0.1, -1.9, 0.6, -0.2, 1.1, -0.6}
	if up := upsideOfReference(noisy, -3); up != 0 {
		t.Errorf("upside against a reference better than every day: %v, want 0", up)
	}
	if got := scatter(noisy, -3); got == 0 || got > dayScatter(noisy) {
		t.Errorf("scatter of noisy days below their reference: %v, want it counted, up to %v", got, dayScatter(noisy))
	}
	// For a signal that is drifted, only the better side of the reference
	// counts.
	if got := provenScatter(noisy, -3); got != 0 {
		t.Errorf("proven scatter below the reference: %v, want 0", got)
	}
	if got := provenScatter(swing, 0); got < bestSide*0.999 || got > bestSide*1.001 {
		t.Errorf("proven scatter of a swing around the reference: %v, want %v", got, bestSide)
	}
	// The best days read against their good days: the best tenth of 0..10
	// lies at 1 and the best quarter at 2.5, 1.5 apart, over 0.607.
	ladder := []float64{5, 0, 9, 2, 7, 4, 10, 1, 6, 3, 8}
	if got, want := upsideOfBestDays(ladder), 1.5/0.607; got < want*0.999 || got > want*1.001 {
		t.Errorf("upside among the best days of 0..10: %v, want %v", got, want)
	}
	// Fewer than 8 values are no window.
	if got := scatter(swing[:7], 0); got != 0 {
		t.Errorf("scatter of 7 values: %v, want 0", got)
	}
	if got := scatter(swing[:8], 0); got == 0 {
		t.Errorf("scatter of 8 values: %v", got)
	}
}

// One bad day in a quiet window is not scatter: the fifth of the largest
// differences is left out.
func TestDayScatterLeavesOutTheLargestFifth(t *testing.T) {
	xs := make([]float64, 31)
	for i := range xs {
		xs[i] = 100
	}
	xs[10], xs[20], xs[25] = 50, 50, 50 // three bad days: six differences of 50 among 30
	if got := dayScatter(xs); got != 0 {
		t.Errorf("three bad days in 31: %v, want 0", got)
	}
	xs[5] = 50 // a fourth: eight differences, two more than the fifth left out
	if got, want := dayScatter(xs), (2*50.0/24)/0.790; got < want*0.999 || got > want*1.001 {
		t.Errorf("four bad days in 31: %v, want %v", got, want)
	}
}

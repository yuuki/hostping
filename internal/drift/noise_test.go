package drift

import (
	"math"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

// gauss is a deterministic source of standard normal values (SplitMix64 and
// the Box-Muller transform), so that the tests below read the same series
// on every run and every Go release.
type gauss struct{ state uint64 }

func (g *gauss) uniform() float64 {
	g.state += 0x9e3779b97f4a7c15
	z := g.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return (float64(z>>11) + 0.5) / (1 << 53)
}

func (g *gauss) normal() float64 {
	return math.Sqrt(-2*math.Log(g.uniform())) * math.Cos(2*math.Pi*g.uniform())
}

// noisyPaths runs paths independent paths for days days, one reading a day:
// level(day) times a normal scatter of sigma. It returns, per path, the day
// (from 0) it was first drifted on, -1 for never, how often its verdict
// changed, and its bandwidth finding after the last day, if any.
func noisyPaths(paths, days int, sigma float64, level func(day int) float64) (first, flips []int, last []*model.DriftFinding) {
	for p := 0; p < paths; p++ {
		g := &gauss{state: uint64(p + 1)}
		tr := New(testTh, []PathIdent{gpuPath}, nil, File{})
		now := start
		firstDay, n, was := -1, 0, false
		var res Result
		for d := 0; d < days; d++ {
			bw := baseBW * level(d) * (1 + sigma*g.normal())
			res = tr.Observe(now, []Sample{{RNIC: "rnic0", Endpoint: "gpu0", Reading: Reading{BandwidthBPS: bw, LatP50Ns: baseP50, LatP99Ns: baseP99}}}, Software{})
			now = now.Add(24 * time.Hour)
			st := res.Paths[0].Status
			if st == model.DriftUnknown {
				continue
			}
			is := st == model.DriftDrifted
			if is && firstDay < 0 {
				firstDay = d
			}
			if is != was {
				n++
			}
			was = is
		}
		var f *model.DriftFinding
		for i := range res.Summary.Findings {
			if res.Summary.Findings[i].Signal == model.DriftBandwidth {
				f = &res.Summary.Findings[i]
			}
		}
		first, flips, last = append(first, firstDay), append(flips, n), append(last, f)
	}
	return first, flips, last
}

func count(xs []int, ok func(int) bool) int {
	n := 0
	for _, x := range xs {
		if ok(x) {
			n++
		}
	}
	return n
}

// Noise alone is not drift. A path whose daily medians scatter by up to 2%
// around one level is not reported in a year, and at 5% the bar that
// follows the scatter keeps it to a few paths in a hundred.
func TestNoiseAloneIsNotDrift(t *testing.T) {
	const paths = 150
	for _, c := range []struct {
		sigma float64
		most  int // most paths that may ever be drifted
	}{{0.005, 0}, {0.01, 0}, {0.02, 0}, {0.05, 6}} {
		first, flips, _ := noisyPaths(paths, 365, c.sigma, flat)
		ever := count(first, func(d int) bool { return d >= 0 })
		if ever > c.most {
			t.Errorf("scatter %.1f%%: %d of %d paths drifted in a year, want at most %d", c.sigma*100, ever, paths, c.most)
		}
		if n := count(flips, func(n int) bool { return n > 2 }); n > 1 {
			t.Errorf("scatter %.1f%%: %d paths changed their verdict more than twice", c.sigma*100, n)
		}
	}
}

// A decay of 0.3% a day is found through a day-to-day scatter of 1%, about
// three weeks after it began, and read as gradual, whether the window still
// holds the days before it began or not.
func TestGradualDriftInNoise(t *testing.T) {
	decay := func(d int) float64 {
		if d < 20 {
			return 1
		}
		return 1 - 0.003*float64(d-20)
	}
	for _, days := range []int{45, 60} {
		first, flips, last := noisyPaths(200, days, 0.01, decay)
		// 25 days in, a few paths are not found yet.
		if n := count(first, func(d int) bool { return d >= 20+10 && d < 20+32 }); n < 185 {
			t.Errorf("after %d days: %d of 200 paths were first drifted 10 to 32 days into the decay, want nearly all", days, n)
		}
		if n := count(flips, func(n int) bool { return n > 1 }); n > 10 {
			t.Errorf("after %d days: %d paths changed their verdict more than once on the way down", days, n)
		}
		gradual := 0
		for _, f := range last {
			if f != nil && f.Shape == model.DriftShapeGradual {
				gradual++
			}
		}
		if gradual < 180 {
			t.Errorf("after %d days: %d of 200 findings are gradual, want nearly all", days, gradual)
		}
	}
}

// A drop of 8% from one day to the next is found within a week through a
// scatter of 1%, read as a step and dated to the day.
func TestStepInNoise(t *testing.T) {
	step := func(d int) float64 {
		if d < 40 {
			return 1
		}
		return 0.92
	}
	first, _, last := noisyPaths(200, 55, 0.01, step)
	if n := count(first, func(d int) bool { return d >= 40+3 && d <= 40+7 }); n < 198 {
		t.Errorf("%d of 200 paths were first drifted 3 to 7 days after the step, want nearly all", n)
	}
	onTheDay := 0
	for _, f := range last {
		if f != nil && f.Shape == model.DriftShapeStep && f.StepOn == dateOf(dayOf(start)+40) {
			onTheDay++
		}
	}
	if onTheDay < 195 {
		t.Errorf("%d of 200 findings are a step dated to the day, want nearly all", onTheDay)
	}
	// Two months later the step has left the window: sustained.
	_, _, last = noisyPaths(200, 100, 0.01, step)
	sustained := 0
	for _, f := range last {
		if f != nil && f.Shape == model.DriftShapeSustained {
			sustained++
		}
	}
	if sustained < 190 {
		t.Errorf("%d of 200 findings are sustained once the step is older than the window, want nearly all", sustained)
	}
}

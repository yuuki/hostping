package drift

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

const (
	testTh  = 0.05
	baseBW  = 200e9
	baseP50 = 1400.0
	baseP99 = 1600.0
)

// start is noon of an arbitrary UTC day, so that a clock step of a few hours
// stays inside the day.
var start = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

var gpuPath = PathIdent{RNIC: "rnic0", Endpoint: "gpu0", Key: "0000:17:00.0->0000:18:00.0/same_switch", EndpointSensor: true}
var memPath = PathIdent{RNIC: "rnic0", Endpoint: "mem0", Key: "0000:17:00.0->mem0of2/local_mem"}

// series drives a tracker over one path, one observed idle run per call, and
// keeps what the path's state did.
type series struct {
	t   *testing.T
	tr  *Tracker
	p   PathIdent
	now time.Time
	// last is the path's state after the latest run, and res that run's
	// whole result.
	last model.PathDrift
	res  Result
	// flips counts the changes between stable and drifted.
	flips int
	logs  []string
}

func newSeries(t *testing.T) *series {
	t.Helper()
	return &series{t: t, tr: New(testTh, []PathIdent{gpuPath}, []string{"0000:17:00.0"}, File{}), p: gpuPath, now: start}
}

// observe applies one run with the given readings (none when r is nil) at
// the series' clock, without moving it.
func (s *series) observe(r *Reading, sw Software) model.PathDrift {
	s.t.Helper()
	var samples []Sample
	if r != nil {
		samples = []Sample{{RNIC: s.p.RNIC, Endpoint: s.p.Endpoint, Reading: *r}}
	}
	s.res = s.tr.Observe(s.now, samples, sw)
	s.logs = append(s.logs, s.res.Logs...)
	if len(s.res.Paths) != 1 {
		s.t.Fatalf("result names %d paths, want 1", len(s.res.Paths))
	}
	got := s.res.Paths[0]
	judged := func(p model.PathDrift) bool { return p.Status != model.DriftUnknown }
	if judged(s.last) && judged(got) && s.last.Status != got.Status {
		s.flips++
	}
	if judged(got) || !judged(s.last) {
		s.last = got
	}
	s.checkFile()
	return got
}

// day is one run with the readings and then the next day.
func (s *series) day(bw, p50, p99 float64) model.PathDrift {
	s.t.Helper()
	got := s.observe(&Reading{BandwidthBPS: bw, LatP50Ns: p50, LatP99Ns: p99}, Software{})
	s.now = s.now.Add(24 * time.Hour)
	return got
}

// days runs n days whose bandwidth is f(i) times baseBW, i counting from 0.
func (s *series) days(n int, f func(i int) float64) model.PathDrift {
	s.t.Helper()
	var got model.PathDrift
	for i := 0; i < n; i++ {
		got = s.day(baseBW*f(i), baseP50, baseP99)
	}
	return got
}

// checkFile holds the tracker to its own file format: whatever it has done,
// what it would save must load again.
func (s *series) checkFile() {
	s.t.Helper()
	b, err := Marshal(s.tr.File())
	if err != nil {
		s.t.Fatalf("history does not marshal: %v", err)
	}
	if _, err := Parse(b); err != nil {
		s.t.Fatalf("the tracker wrote a history it would reject: %v\n%s", err, b)
	}
}

func (s *series) finding(signal model.DriftSignal) model.DriftFinding {
	s.t.Helper()
	for _, f := range s.res.Summary.Findings {
		if f.Signal == signal {
			return f
		}
	}
	s.t.Fatalf("no %s finding in %+v", signal, s.res.Summary.Findings)
	return model.DriftFinding{}
}

func flat(int) float64 { return 1 }

// noise is a deterministic scatter of about ±amp around 1 that neither
// trends nor repeats with the week.
func noise(amp float64) func(int) float64 {
	return func(i int) float64 {
		x := uint32(i+1) * 2654435761
		return 1 + amp*(float64(x>>8)/float64(1<<24)*2-1)
	}
}

func change(t *testing.T, p model.PathDrift, signal model.DriftSignal) float64 {
	t.Helper()
	for _, c := range p.Changes {
		if c.Signal == signal {
			return c.Change
		}
	}
	t.Fatalf("no %s change in %+v", signal, p)
	return 0
}

// A path is unknown, with the reason, until its reference and then enough
// recent days exist: a daily value counts once its day is over, so the first
// verdict comes with the run of the 11th day.
func TestNoVerdictBeforeEnoughHistory(t *testing.T) {
	s := newSeries(t)
	for i := 1; i <= 12; i++ {
		got := s.day(baseBW, baseP50, baseP99)
		want, why := model.DriftUnknown, model.DriftCollectingReference
		switch {
		case i >= refDays+minRecentDays+1:
			want, why = model.DriftStable, model.DriftReasonNone
		case i > refDays:
			why = model.DriftFewRecentDays
		}
		if got.Status != want || got.Reason != why {
			t.Fatalf("day %d: %s (%s), want %s (%s)", i, got.Status, got.Reason, want, why)
		}
		if (got.Status == model.DriftUnknown) != (len(got.Changes) == 0) {
			t.Fatalf("day %d: an unknown path must carry no change, a judged one all of them: %+v", i, got)
		}
	}
	sum := s.res.Summary
	if sum.Stable != 1 || sum.Drifted != 0 || sum.Unknown != 0 || len(sum.UnknownReasons) != 0 || len(sum.Findings) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

// Many runs on one day are one daily value: a host that has run idle for a
// single day, however often, has no reference.
func TestRunsOfOneDayAreOneDay(t *testing.T) {
	s := newSeries(t)
	for i := 0; i < 500; i++ {
		got := s.observe(&Reading{BandwidthBPS: baseBW, LatP50Ns: baseP50, LatP99Ns: baseP99}, Software{})
		if got.Reason != model.DriftCollectingReference {
			t.Fatalf("run %d: %s (%s)", i, got.Status, got.Reason)
		}
		s.now = s.now.Add(time.Minute)
	}
	h := s.tr.File().Paths[gpuPath.Key]
	if h.Open == nil || h.Open.Seen != 500 || len(h.Open.Samples) > openCap || len(h.Days) != 0 {
		t.Fatalf("open day = %+v, days = %d", h.Open, len(h.Days))
	}
}

// Day-to-day scatter well inside the threshold never becomes a verdict.
func TestNoiseIsStable(t *testing.T) {
	s := newSeries(t)
	s.days(200, noise(0.01))
	if s.last.Status != model.DriftStable || s.flips != 0 {
		t.Fatalf("status %s after %d flips, want stable throughout", s.last.Status, s.flips)
	}
}

// A slow monotonic decay is reported once it has cost the threshold, as a
// gradual drift, and the verdict does not come and go on the way down.
func TestSlowDriftIsGradual(t *testing.T) {
	s := newSeries(t)
	n := noise(0.004)
	first := 0
	for i := 0; i < 60; i++ {
		got := s.day(baseBW*(1-0.003*float64(i))*n(i), baseP50, baseP99)
		if got.Status == model.DriftDrifted && first == 0 {
			first = i + 1
		}
	}
	if s.last.Status != model.DriftDrifted || s.flips != 1 {
		t.Fatalf("status %s after %d flips, want drifted after exactly one", s.last.Status, s.flips)
	}
	// 0.3% a day against the median of the first week (day 4) reaches 5%
	// about 17 days later, seen through a recent median that lags 4 days.
	if first < 20 || first > 30 {
		t.Fatalf("first drifted on day %d, want about day 25", first)
	}
	f := s.finding(model.DriftBandwidth)
	if f.Shape != model.DriftShapeGradual || f.StepOn != "" {
		t.Fatalf("shape %s (step on %q), want gradual", f.Shape, f.StepOn)
	}
	if f.Change < 0.10 || f.Change > 0.20 || f.Recent >= f.Reference {
		t.Fatalf("change %.3f (%.3g against %.3g), want a drop of 10-20%%", f.Change, f.Recent, f.Reference)
	}
	if f.ReferenceFrom != "2026-03-01" || f.ReferenceTo != "2026-03-07" {
		t.Fatalf("reference %s..%s, want the first week", f.ReferenceFrom, f.ReferenceTo)
	}
	if len(s.res.Summary.Findings) != 1 {
		t.Fatalf("a bandwidth drift must not flag the latencies: %+v", s.res.Summary.Findings)
	}
	if !strings.Contains(strings.Join(s.logs, "\n"), "drifted: rnic0->gpu0 bandwidth") {
		t.Fatalf("no log line for the drift: %q", s.logs)
	}
}

// A drop that happens from one day to the next is a step, dated to the first
// day at the new level.
func TestStepChange(t *testing.T) {
	s := newSeries(t)
	n := noise(0.004)
	for i := 0; i < 30; i++ {
		level := 1.0
		if i >= 20 {
			level = 0.92
		}
		s.day(baseBW*level*n(i), baseP50, baseP99)
	}
	if s.last.Status != model.DriftDrifted || s.flips != 1 {
		t.Fatalf("status %s after %d flips", s.last.Status, s.flips)
	}
	f := s.finding(model.DriftBandwidth)
	if f.Shape != model.DriftShapeStep || f.StepOn != "2026-03-21" {
		t.Fatalf("shape %s on %q, want a step on 2026-03-21 (day 21)", f.Shape, f.StepOn)
	}
	// The verdict needs most of the recent days at the new level: with three
	// of them after the step, the median is the new level.
	if f.Since != "2026-03-23" && f.Since != "2026-03-24" && f.Since != "2026-03-25" {
		t.Fatalf("since %s, want within a few days of the step", f.Since)
	}
}

// Once the step has left the window, the path still reads below its
// reference, with neither a trend nor a step to show for it.
func TestOldChangeIsSustained(t *testing.T) {
	s := newSeries(t)
	n := noise(0.004)
	for i := 0; i < 80; i++ {
		level := 1.0
		if i >= 20 {
			level = 0.92
		}
		s.day(baseBW*level*n(i), baseP50, baseP99)
	}
	if f := s.finding(model.DriftBandwidth); f.Shape != model.DriftShapeSustained || f.Since > "2026-03-25" {
		t.Fatalf("shape %s since %s, want sustained since the step", f.Shape, f.Since)
	}
	if s.flips != 1 {
		t.Fatalf("%d flips, want 1", s.flips)
	}
}

// A path that returns to its reference level is stable again, and says so.
func TestRecovery(t *testing.T) {
	s := newSeries(t)
	for i := 0; i < 60; i++ {
		level := 1.0
		if i >= 15 && i < 35 {
			level = 0.90
		}
		s.day(baseBW*level, baseP50, baseP99)
	}
	if s.last.Status != model.DriftStable || s.flips != 2 {
		t.Fatalf("status %s after %d flips, want drifted and back", s.last.Status, s.flips)
	}
	if h := s.tr.File().Paths[gpuPath.Key]; len(h.Drifted) != 0 {
		t.Fatalf("verdict memory not cleared: %v", h.Drifted)
	}
	if !strings.Contains(strings.Join(s.logs, "\n"), "rnic0->gpu0 bandwidth recovered") {
		t.Fatalf("no log line for the recovery: %q", s.logs)
	}
}

// Between half the threshold and the threshold a verdict keeps what it was:
// a path that hovers there neither enters nor, once in, leaves.
func TestHysteresis(t *testing.T) {
	hover := func(i int) float64 { return 1 - 0.04 - 0.008*float64(i%2) } // 4.0% / 4.8% down

	never := newSeries(t)
	never.days(12, flat)
	never.days(40, hover)
	if never.last.Status != model.DriftStable || never.flips != 0 {
		t.Fatalf("below the threshold: %s after %d flips", never.last.Status, never.flips)
	}

	held := newSeries(t)
	held.days(12, flat)
	held.days(8, func(int) float64 { return 0.93 })
	if held.last.Status != model.DriftDrifted {
		t.Fatalf("7%% down: %s", held.last.Status)
	}
	held.days(40, hover)
	if held.last.Status != model.DriftDrifted || held.flips != 1 {
		t.Fatalf("back inside the band: %s after %d flips, want the verdict held", held.last.Status, held.flips)
	}
	held.days(10, func(int) float64 { return 0.98 })
	if held.last.Status != model.DriftStable || held.flips != 2 {
		t.Fatalf("below half the threshold: %s after %d flips", held.last.Status, held.flips)
	}
}

// Single bad days, and single bad runs inside a day, move no median.
func TestOutliersDoNotCount(t *testing.T) {
	s := newSeries(t)
	for i := 0; i < 60; i++ {
		level := 1.0
		if i%6 == 5 {
			level = 0.5 // every sixth day reads half
		}
		for run := 0; run < 5; run++ {
			bw := baseBW * level
			if run == 2 {
				bw = baseBW * 0.3 // one run in five is far off
			}
			s.observe(&Reading{BandwidthBPS: bw, LatP50Ns: baseP50, LatP99Ns: baseP99}, Software{})
			s.now = s.now.Add(time.Hour)
		}
		s.now = s.now.Add(19 * time.Hour)
	}
	if s.last.Status != model.DriftStable || s.flips != 0 {
		t.Fatalf("status %s after %d flips, want stable throughout", s.last.Status, s.flips)
	}
	if c := change(t, s.last, model.DriftBandwidth); c != 0 {
		t.Fatalf("bandwidth change %v, want 0: neither a day nor a run in the minority moves a median", c)
	}
}

// A path whose reference days already scatter needs a larger change than the
// threshold: three times their scatter.
func TestNoiseGuard(t *testing.T) {
	// The first week spans 0.94 to 1.06 around 1: an interquartile range of
	// 6%, a scatter of about 4.4%, so the bar is about 13% instead of 5%.
	ladder := []float64{1.02, 0.94, 1.06, 0.98, 1.00, 0.96, 1.04}
	s := newSeries(t)
	s.days(refDays, func(i int) float64 { return ladder[i] })
	s.days(20, func(int) float64 { return 0.90 })
	if s.last.Status != model.DriftStable {
		t.Fatalf("10%% down on a path that scatters 4%% a day: %s, want stable", s.last.Status)
	}
	if c := change(t, s.last, model.DriftBandwidth); c < 0.099 || c > 0.101 {
		t.Fatalf("change %v, want the 10%% still reported", c)
	}
	s.days(20, func(int) float64 { return 0.78 })
	if s.last.Status != model.DriftDrifted {
		t.Fatalf("22%% down: %s, want drifted", s.last.Status)
	}
}

// A path that reads at two levels from day to day, as some platforms'
// memory paths do, is not drifting when the lower level takes over for a
// while: the minority level of the reference days is part of its scatter.
func TestTwoLevelPathIsNotDrift(t *testing.T) {
	s := newSeries(t)
	s.days(refDays, func(i int) float64 { return 1 - 0.08*float64(i%2) }) // 4 days fast, 3 slow
	s.days(30, func(int) float64 { return 0.92 })
	if s.last.Status != model.DriftStable || s.flips != 0 {
		t.Fatalf("status %s after %d flips, want stable: the slow level was in the reference", s.last.Status, s.flips)
	}
}

// A reference week that happened to be quiet does not make later scatter a
// drift: the bar also follows the day-to-day scatter of the window, which
// is read from the differences of neighbouring days and so holds for a path
// that is drifting as well.
func TestScatterOfTheWindowRaisesTheBar(t *testing.T) {
	// From day 8 on the path reads 6% low and 6% high in turn, low on four
	// days of every seven: the recent median is 6% below a reference whose
	// own scatter is 0.
	jitter := func(i int) float64 {
		if i%7%2 == 0 {
			return 0.94
		}
		return 1.06
	}
	s := newSeries(t)
	s.days(refDays, flat)
	// The window needs its 8 days, and the quiet reference days in it hold
	// the estimate down for a while: the first weeks may report the path.
	s.days(30, jitter)
	if ref := s.tr.File().Paths[gpuPath.Key].Ref; ref.BandwidthSpread != 0 {
		t.Fatalf("reference scatter %v, want 0", ref.BandwidthSpread)
	}
	// Once the window is the path's scatter, it is not reported again.
	s.flips = 0
	s.days(60, jitter)
	if c := change(t, s.last, model.DriftBandwidth); c < 0.059 || c > 0.061 {
		t.Fatalf("change %v, want the 6%% of the lower level", c)
	}
	if s.last.Status != model.DriftStable || s.flips != 0 {
		t.Fatalf("status %s after %d flips, want stable: the path scatters by more than it moved", s.last.Status, s.flips)
	}
	// The same level without the scatter is a drift.
	q := newSeries(t)
	q.days(refDays, flat)
	q.days(40, func(int) float64 { return 0.94 })
	if q.last.Status != model.DriftDrifted {
		t.Fatalf("6%% down on a quiet path: %s", q.last.Status)
	}
}

// Latency is judged per signal and in the other direction: a p99 that creeps
// up is reported as such while bandwidth and p50 stay stable.
func TestLatencyP99Creep(t *testing.T) {
	s := newSeries(t)
	for i := 0; i < 50; i++ {
		s.day(baseBW, baseP50, baseP99*(1+0.004*float64(i)))
	}
	if s.last.Status != model.DriftDrifted {
		t.Fatalf("status %s", s.last.Status)
	}
	if len(s.res.Summary.Findings) != 1 {
		t.Fatalf("findings = %+v, want the p99 only", s.res.Summary.Findings)
	}
	f := s.finding(model.DriftLatencyP99)
	if f.Shape != model.DriftShapeGradual || f.Recent <= f.Reference || f.Change <= testTh {
		t.Fatalf("finding = %+v", f)
	}
	if c := change(t, s.last, model.DriftBandwidth); c != 0 {
		t.Fatalf("bandwidth change %v, want 0", c)
	}
}

// A path that reads better than its reference is stable, with a negative
// change; the reference is not moved.
func TestImprovementIsStable(t *testing.T) {
	s := newSeries(t)
	s.days(12, flat)
	s.days(20, func(int) float64 { return 1.10 })
	if s.last.Status != model.DriftStable {
		t.Fatalf("status %s", s.last.Status)
	}
	if c := change(t, s.last, model.DriftBandwidth); c > -0.09 || c < -0.11 {
		t.Fatalf("change %v, want about -0.10", c)
	}
	if ref := s.tr.File().Paths[gpuPath.Key].Ref; ref.BandwidthBPS != baseBW {
		t.Fatalf("reference moved to %v", ref.BandwidthBPS)
	}
}

// A host that is off for weeks has no recent days when it returns: unknown,
// not the verdict from before, until three days are read again. The verdict
// memory survives the gap.
func TestGap(t *testing.T) {
	s := newSeries(t)
	s.days(12, flat)
	s.days(10, func(int) float64 { return 0.93 })
	if s.last.Status != model.DriftDrifted {
		t.Fatalf("before the gap: %s", s.last.Status)
	}
	s.now = s.now.Add(21 * 24 * time.Hour)
	for i := 0; i < minRecentDays; i++ {
		// 3.5% down: inside the band, so the verdict follows the memory.
		if got := s.day(baseBW*0.965, baseP50, baseP99); got.Status != model.DriftUnknown || got.Reason != model.DriftFewRecentDays {
			t.Fatalf("day %d after the gap: %s (%s), want unknown (few_recent_days)", i+1, got.Status, got.Reason)
		}
	}
	if got := s.day(baseBW*0.965, baseP50, baseP99); got.Status != model.DriftDrifted {
		t.Fatalf("after the gap, inside the band: %s, want the verdict from before held", got.Status)
	}
	if f := s.finding(model.DriftBandwidth); f.Since != "2026-03-17" || f.RecentFrom != "2026-04-13" {
		t.Fatalf("since %s, recent from %s", f.Since, f.RecentFrom)
	}
	// The same gap on a path that was stable resolves the band the other way.
	q := newSeries(t)
	q.days(22, flat)
	q.now = q.now.Add(21 * 24 * time.Hour)
	q.days(minRecentDays, func(int) float64 { return 0.965 })
	if got := q.day(baseBW*0.965, baseP50, baseP99); got.Status != model.DriftStable {
		t.Fatalf("stable before the gap, inside the band after it: %s", got.Status)
	}
}

// A path the runs stop reading (it turned abnormal, its probe is skipped)
// is unknown once its recent days are gone, and keeps its reference.
func TestPathWithoutReadings(t *testing.T) {
	s := newSeries(t)
	s.days(20, flat)
	for i := 0; i < recentSpan+2; i++ {
		s.observe(nil, Software{})
		s.now = s.now.Add(24 * time.Hour)
	}
	got := s.observe(nil, Software{})
	if got.Status != model.DriftUnknown || got.Reason != model.DriftFewRecentDays {
		t.Fatalf("%s (%s), want unknown (few_recent_days)", got.Status, got.Reason)
	}
	if s.tr.File().Paths[gpuPath.Key].Ref == nil {
		t.Fatal("the reference was lost")
	}
}

// The clock stepping back: nothing is recorded under a day the history is
// already past, and the paths say why they are unknown. When the clock is
// past the history again, tracking goes on where it was.
func TestClockStepsBackAndReturns(t *testing.T) {
	s := newSeries(t)
	s.days(20, flat)
	before, _ := Marshal(s.tr.File())
	good := s.now
	s.now = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) // an RTC that lost its battery
	got := s.observe(&Reading{BandwidthBPS: baseBW / 2, LatP50Ns: baseP50, LatP99Ns: baseP99}, Software{Kernel: "6.8.0-1"})
	if got.Status != model.DriftUnknown || got.Reason != model.DriftClockBehind {
		t.Fatalf("%s (%s), want unknown (clock_behind)", got.Status, got.Reason)
	}
	if sum := s.res.Summary; sum.Unknown != 1 || len(sum.UnknownReasons) != 1 || sum.UnknownReasons[0].Reason != model.DriftClockBehind {
		t.Fatalf("summary = %+v", sum)
	}
	if after, _ := Marshal(s.tr.File()); string(after) != string(before) {
		t.Fatalf("a run under a clock behind the history changed it:\n%s\n%s", before, after)
	}
	if !strings.Contains(strings.Join(s.logs, "\n"), "the clock (2000-01-01) is behind the newest day of the history (2026-03-20)") {
		t.Fatalf("no log line: %q", s.logs)
	}
	s.now = good
	if got := s.day(baseBW, baseP50, baseP99); got.Status != model.DriftStable {
		t.Fatalf("after the clock returned: %s (%s)", got.Status, got.Reason)
	}
}

// A clock that ran ahead for a while leaves days in the history that the
// corrected clock never reaches. After two days behind them they are
// dropped. The reference and the verdict memory are from before the jump
// and are kept; the daily values from before it left the window when the
// clock jumped, as in a real gap.
func TestClockRanAhead(t *testing.T) {
	s := newSeries(t)
	s.days(20, flat) // 2026-03-01 .. 03-20
	good := s.now    // 2026-03-21
	s.now = good.AddDate(1, 0, 0)
	s.days(2, func(int) float64 { return 0.5 }) // two days dated a year ahead
	s.now = good
	for i := 0; i < healDays; i++ {
		if got := s.day(baseBW, baseP50, baseP99); got.Reason != model.DriftClockBehind {
			t.Fatalf("day %d behind: %s (%s)", i+1, got.Status, got.Reason)
		}
	}
	if got := s.day(baseBW, baseP50, baseP99); got.Reason != model.DriftFewRecentDays {
		t.Fatalf("after the heal: %s (%s), want unknown (few_recent_days)", got.Status, got.Reason)
	}
	h := s.tr.File().Paths[gpuPath.Key]
	if len(h.Days) != 0 || h.Open == nil || h.Open.Day != dayOf(good)+healDays {
		t.Fatalf("days = %+v, open = %+v: want the days dated ahead gone and today open", h.Days, h.Open)
	}
	if h.Ref == nil || h.Ref.BandwidthBPS != baseBW || h.Ref.To != dayOf(start)+refDays-1 {
		t.Fatalf("reference = %+v, want the first week's", h.Ref)
	}
	if !strings.Contains(strings.Join(s.logs, "\n"), "dropped 2 daily summaries dated after 2026-03-23") {
		t.Fatalf("no log line for the heal: %q", s.logs)
	}
	s.days(minRecentDays, flat)
	if s.last.Status != model.DriftStable || s.res.Paths[0].Status != model.DriftStable {
		t.Fatalf("three days later: %s (%s)", s.res.Paths[0].Status, s.res.Paths[0].Reason)
	}
}

// A reference formed under a clock that ran ahead cannot be dated: its path
// starts over.
func TestClockRanAheadDuringReference(t *testing.T) {
	s := newSeries(t)
	good := s.now
	s.now = good.AddDate(1, 0, 0)
	s.days(12, flat)
	s.now = good
	for i := 0; i <= healDays; i++ {
		s.day(baseBW, baseP50, baseP99)
	}
	h := s.tr.File().Paths[gpuPath.Key]
	if h.Ref != nil || len(h.Days) != 0 || h.Open == nil || h.FirstSeen > h.LastSeen {
		t.Fatalf("history = %+v, want a fresh start", h)
	}
	if got := s.res.Paths[0]; got.Status != model.DriftUnknown || got.Reason != model.DriftCollectingReference {
		t.Fatalf("%s (%s)", got.Status, got.Reason)
	}
}

// A clock that jumps ahead and stays there is a gap.
func TestClockJumpsAhead(t *testing.T) {
	s := newSeries(t)
	s.days(20, flat)
	s.now = s.now.AddDate(3, 0, 0)
	if got := s.day(baseBW, baseP50, baseP99); got.Reason != model.DriftFewRecentDays {
		t.Fatalf("%s (%s), want unknown (few_recent_days)", got.Status, got.Reason)
	}
	s.days(minRecentDays, flat)
	if s.last.Status != model.DriftStable {
		t.Fatalf("%s (%s)", s.last.Status, s.last.Reason)
	}
}

// The clock stepping back onto a day that is already closed adds nothing to
// it and does not open it a second time.
func TestClockStepsBackOntoClosedDay(t *testing.T) {
	s := newSeries(t)
	s.days(3, flat)
	// A run without a clean reading closes day 3 and opens nothing.
	s.observe(nil, Software{})
	s.now = s.now.Add(-24 * time.Hour)
	s.observe(&Reading{BandwidthBPS: baseBW / 2, LatP50Ns: baseP50, LatP99Ns: baseP99}, Software{})
	h := s.tr.File().Paths[gpuPath.Key]
	if h.Open != nil || len(h.Days) != 3 || h.Days[2].BandwidthBPS != baseBW {
		t.Fatalf("open = %+v, days = %+v", h.Open, h.Days)
	}
}

// A year of runs every five minutes keeps a bounded history: at most
// windowDays daily values and openCap readings of the day.
func TestHistoryIsBounded(t *testing.T) {
	tr := New(testTh, []PathIdent{gpuPath}, nil, File{})
	now := start
	tC := 50.0
	sample := []Sample{{RNIC: "rnic0", Endpoint: "gpu0", Reading: Reading{BandwidthBPS: baseBW, LatP50Ns: baseP50, LatP99Ns: baseP99, TempRNIC: &tC, TempEndpoint: &tC}}}
	largest := 0
	for day := 0; day < 400; day++ {
		for run := 0; run < 288; run++ {
			tr.Observe(now, sample, Software{})
			now = now.Add(5 * time.Minute)
		}
		h := tr.File().Paths[gpuPath.Key]
		if len(h.Days) > windowDays || len(h.Open.Samples) > openCap {
			t.Fatalf("day %d: %d days, %d open readings", day, len(h.Days), len(h.Open.Samples))
		}
		b, err := Marshal(tr.File())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(b); err != nil {
			t.Fatalf("day %d: the tracker wrote a history it would reject: %v", day, err)
		}
		largest = max(largest, len(b))
	}
	if largest > 8<<10 {
		t.Fatalf("one path's history grew to %d bytes", largest)
	}
	h := tr.File().Paths[gpuPath.Key]
	if d := h.Days[len(h.Days)-1]; d.N != 288 || len(h.Days) != windowDays-1 {
		t.Fatalf("%d days, the last with n = %d: want every run of a day counted", len(h.Days), d.N)
	}
	t.Logf("largest history of one path: %d bytes", largest)
}

// The kept readings of a busy day are spread over the whole day, not its
// first openCap runs.
func TestThinningKeepsTheWholeDay(t *testing.T) {
	h := &PathHistory{}
	for i := 0; i < 1000; i++ {
		h.add(100, Reading{BandwidthBPS: float64(i + 1), LatP50Ns: 1, LatP99Ns: 1})
	}
	o := h.Open
	if o.Seen != 1000 || len(o.Samples) < openCap/2 || len(o.Samples) > openCap {
		t.Fatalf("seen %d, kept %d", o.Seen, len(o.Samples))
	}
	for i, r := range o.Samples {
		if want := float64(i*o.Stride + 1); r.BandwidthBPS != want {
			t.Fatalf("kept[%d] = %v, want reading %v (every %dth)", i, r.BandwidthBPS, want, o.Stride)
		}
	}
	if last := o.Samples[len(o.Samples)-1].BandwidthBPS; last <= float64(1000-o.Stride) {
		t.Fatalf("last kept reading is %v of 1000 at stride %d", last, o.Stride)
	}
	// The median of the kept readings is that of the day.
	if d := o.close(); d.BandwidthBPS < 400 || d.BandwidthBPS > 600 || d.N != 1000 {
		t.Fatalf("daily value %v of n %d", d.BandwidthBPS, d.N)
	}
}

func temps(rnic, ep float64) Reading {
	r := Reading{BandwidthBPS: baseBW, LatP50Ns: baseP50, LatP99Ns: baseP99}
	if rnic != 0 {
		r.TempRNIC = &rnic
	}
	if ep != 0 {
		r.TempEndpoint = &ep
	}
	return r
}

// The temperature of the path's devices is compared between the reference
// days and the recent days, and only what was read on both sides counts.
func TestTemperature(t *testing.T) {
	cases := []struct {
		name string
		p    PathIdent
		// ref and recent are the RNIC and endpoint temperature (0: unread)
		// of the reference days and of the days after them.
		ref, recent [2]float64
		want        model.DriftTempStatus
		reason      string
		devices     int
	}{
		{"gpu hotter", gpuPath, [2]float64{55, 60}, [2]float64{56, 71}, model.DriftTempRose, "", 2},
		{"both steady", gpuPath, [2]float64{55, 60}, [2]float64{57, 63}, model.DriftTempSteady, "", 2},
		{"rnic hotter, gpu unread", gpuPath, [2]float64{55, 0}, [2]float64{62, 0}, model.DriftTempRose, "", 1},
		{"rnic steady, gpu unread", gpuPath, [2]float64{55, 0}, [2]float64{56, 0}, model.DriftTempUnknown, "gpu0: no_sensor", 1},
		{"sensor appeared later", gpuPath, [2]float64{0, 60}, [2]float64{70, 61}, model.DriftTempUnknown, "rnic0: no_reference_reading", 1},
		{"sensor went away", gpuPath, [2]float64{55, 60}, [2]float64{0, 61}, model.DriftTempUnknown, "rnic0: no_recent_reading", 1},
		{"nothing read", gpuPath, [2]float64{}, [2]float64{}, model.DriftTempUnknown, "rnic0: no_sensor", 0},
		{"memory path, rnic steady", memPath, [2]float64{55, 0}, [2]float64{56, 0}, model.DriftTempSteady, "", 1},
		{"memory path, rnic hotter", memPath, [2]float64{55, 0}, [2]float64{61, 0}, model.DriftTempRose, "", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSeries(t)
			s.p = c.p
			s.tr = New(testTh, []PathIdent{c.p}, nil, File{})
			for i := 0; i < 20; i++ {
				r := temps(c.ref[0], c.ref[1])
				if i >= refDays {
					r = temps(c.recent[0], c.recent[1])
					r.BandwidthBPS = baseBW * 0.9
				}
				s.observe(&r, Software{})
				s.now = s.now.Add(24 * time.Hour)
			}
			got := s.finding(model.DriftBandwidth).Temperature
			if got.Status != c.want || got.Reason != c.reason || len(got.Devices) != c.devices {
				t.Fatalf("temperature = %+v, want %s (%q) over %d devices", got, c.want, c.reason, c.devices)
			}
		})
	}
}

// A kernel or firmware change is recorded once, on the day it is first
// seen, and quoted by the findings whose reference is older. A value that
// was not read is not a change.
func TestSoftwareChanges(t *testing.T) {
	s := newSeries(t)
	sw := Software{Kernel: "6.8.0-45-generic", Firmware: map[string]string{"0000:17:00.0": "28.39.1002", "0000:99:00.0": "1.0"}}
	run := func(level float64, sw Software) {
		s.observe(&Reading{BandwidthBPS: baseBW * level, LatP50Ns: baseP50, LatP99Ns: baseP99}, sw)
		s.now = s.now.Add(24 * time.Hour)
	}
	for i := 0; i < 15; i++ {
		run(1, sw)
	}
	run(1, Software{}) // unread: not a change
	if ev := s.tr.File().Events; len(ev) != 0 {
		t.Fatalf("events before any change: %+v", ev)
	}
	next := Software{Kernel: "6.8.0-48-generic\n", Firmware: map[string]string{"0000:17:00.0": "28.41.1000"}}
	for i := 0; i < 10; i++ {
		run(0.92, next)
	}
	f := s.finding(model.DriftBandwidth)
	want := []model.DriftSoftwareChange{
		{On: "2026-03-17", Component: "kernel", From: "6.8.0-45-generic", To: "6.8.0-48-generic"},
		{On: "2026-03-17", Component: "firmware 0000:17:00.0", From: "28.39.1002", To: "28.41.1000"},
	}
	if fmt.Sprint(f.SoftwareChanges) != fmt.Sprint(want) {
		t.Fatalf("software changes = %+v, want %+v", f.SoftwareChanges, want)
	}
	if f.Shape != model.DriftShapeStep || f.StepOn != "2026-03-17" {
		t.Fatalf("shape %s on %s, want the step on the day of the change", f.Shape, f.StepOn)
	}
	if fw := s.tr.File().Software.Firmware; len(fw) != 1 {
		t.Fatalf("firmware of an RNIC the topology does not have was kept: %v", fw)
	}
	line := FormatFinding(f)
	for _, sub := range []string{"rnic0->gpu0 bandwidth 184.00 Gb/s against its reference 200.00 Gb/s (-8.0%", "shape step on 2026-03-17", "temperature unknown (rnic0: no_sensor)", "2026-03-17 kernel 6.8.0-45-generic -> 6.8.0-48-generic"} {
		if !strings.Contains(line, sub) {
			t.Errorf("log line %q lacks %q", line, sub)
		}
	}
}

// The software changes kept are bounded, newest last, and a version string
// is cut to what the file holds.
func TestSoftwareEventsAreBounded(t *testing.T) {
	s := newSeries(t)
	for i := 0; i < 3*maxEvents; i++ {
		s.observe(nil, Software{Kernel: fmt.Sprintf("6.8.0-%d-%s", i, strings.Repeat("é", 100))})
		s.now = s.now.Add(24 * time.Hour)
	}
	ev := s.tr.File().Events
	if len(ev) != maxEvents || !strings.HasPrefix(ev[len(ev)-1].To, fmt.Sprintf("6.8.0-%d-", 3*maxEvents-1)) {
		t.Fatalf("%d events, last %+v", len(ev), ev[len(ev)-1])
	}
	if k := s.tr.File().Software.Kernel; len(k) > maxText || !strings.HasSuffix(k, "é") {
		t.Fatalf("kernel %q is %d bytes", k, len(k))
	}
}

// reload is f as a daemon that starts later reads it: a copy that shares
// nothing with the tracker that wrote it.
func reload(t *testing.T, f File) File {
	t.Helper()
	b, err := Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A path the topology no longer has keeps its history for staleDays, in
// case the device comes back, and is dropped after that.
func TestStalePathsExpire(t *testing.T) {
	two := New(testTh, []PathIdent{gpuPath, memPath}, nil, File{})
	now := start
	reading := Reading{BandwidthBPS: baseBW, LatP50Ns: baseP50, LatP99Ns: baseP99}
	for i := 0; i < 12; i++ {
		two.Observe(now, []Sample{{RNIC: "rnic0", Endpoint: "gpu0", Reading: reading}, {RNIC: "rnic0", Endpoint: "mem0", Reading: reading}}, Software{})
		now = now.Add(24 * time.Hour)
	}
	// The GPU is gone: the next daemon tracks the memory path only.
	one := New(testTh, []PathIdent{memPath}, nil, reload(t, two.File()))
	for i := 0; i < staleDays; i++ {
		res := one.Observe(now, []Sample{{RNIC: "rnic0", Endpoint: "mem0", Reading: reading}}, Software{})
		if len(res.Paths) != 1 {
			t.Fatalf("result names %d paths", len(res.Paths))
		}
		now = now.Add(24 * time.Hour)
	}
	if one.File().Paths[gpuPath.Key] == nil {
		t.Fatalf("history dropped after %d days, before staleDays were over", staleDays)
	}
	// It comes back in time: its reference is still there.
	back := New(testTh, []PathIdent{gpuPath, memPath}, nil, reload(t, one.File()))
	res := back.Observe(now, nil, Software{})
	if res.Paths[0].Reason != model.DriftFewRecentDays {
		t.Fatalf("returned path: %s (%s), want its reference in use", res.Paths[0].Status, res.Paths[0].Reason)
	}
	// It stays away: dropped, with a log line.
	res = one.Observe(now.Add(48*time.Hour), nil, Software{})
	if one.File().Paths[gpuPath.Key] != nil {
		t.Fatal("stale history kept past staleDays")
	}
	if len(res.Logs) != 1 || !strings.Contains(res.Logs[0], "dropped the history of 1 paths") {
		t.Fatalf("logs = %q", res.Logs)
	}
}

// A full history makes room by dropping the stale path seen longest ago,
// and leaves a path untracked, saying so, when every held path is live.
func TestFullHistory(t *testing.T) {
	f := File{Paths: map[string]*PathHistory{}}
	for i := 0; i < MaxPaths; i++ {
		f.Paths[fmt.Sprintf("stale-%04d", i)] = &PathHistory{FirstSeen: 1, LastSeen: dayOf(start) - staleDays + i/30}
	}
	live := []PathIdent{gpuPath, memPath}
	tr := New(testTh, live, nil, f)
	res := tr.Observe(start, nil, Software{})
	if len(tr.File().Paths) != MaxPaths {
		t.Fatalf("%d paths, want the bound %d", len(tr.File().Paths), MaxPaths)
	}
	for _, gone := range []string{"stale-0000", "stale-0001"} {
		if tr.File().Paths[gone] != nil {
			t.Fatalf("%s kept: the oldest stale paths must make room", gone)
		}
	}
	if tr.File().Paths[gpuPath.Key] == nil || tr.File().Paths[memPath.Key] == nil || res.Summary.Unknown != 2 {
		t.Fatalf("live paths not tracked: %+v", res.Summary)
	}

	var many []PathIdent
	for i := 0; i <= MaxPaths; i++ {
		many = append(many, PathIdent{RNIC: fmt.Sprintf("rnic%d", i), Endpoint: "mem0", Key: fmt.Sprintf("live-%04d", i)})
	}
	tr = New(testTh, many, nil, File{})
	res = tr.Observe(start, nil, Software{})
	lastPath := res.Paths[len(res.Paths)-1]
	if len(tr.File().Paths) != MaxPaths || lastPath.Status != model.DriftUnknown || lastPath.Reason != model.DriftNotTracked {
		t.Fatalf("%d paths tracked, last path %s (%s)", len(tr.File().Paths), lastPath.Status, lastPath.Reason)
	}
	if got := res.Summary.UnknownReasons; len(got) != 2 || got[0].Reason != model.DriftCollectingReference || got[0].Paths != MaxPaths || got[1].Reason != model.DriftNotTracked || got[1].Paths != 1 {
		t.Fatalf("unknown reasons = %+v", got)
	}
}

// A threshold change moves the bar, not the history.
func TestThresholdIsTheTrackers(t *testing.T) {
	s := newSeries(t)
	s.days(12, flat)
	s.days(10, func(int) float64 { return 0.93 })
	if s.last.Status != model.DriftDrifted {
		t.Fatalf("7%% down at a 5%% threshold: %s", s.last.Status)
	}
	loose := New(0.20, []PathIdent{gpuPath}, nil, reload(t, s.tr.File()))
	res := loose.Observe(s.now, nil, Software{})
	if res.Paths[0].Status != model.DriftStable {
		t.Fatalf("7%% down at a 20%% threshold: %s", res.Paths[0].Status)
	}
}

// A change that reaches every path at once gives a report its worst
// findings and the count of the rest, not one finding per path and signal.
func TestFindingsAreBounded(t *testing.T) {
	var paths []PathIdent
	for i := 0; i < 50; i++ {
		paths = append(paths, PathIdent{RNIC: "rnic0", Endpoint: fmt.Sprintf("gpu%d", i), Key: fmt.Sprintf("path-%02d", i)})
	}
	tr := New(testTh, paths, nil, File{})
	now := start
	var res Result
	for day := 0; day < 25; day++ {
		var samples []Sample
		for i, p := range paths {
			level := 1.0
			if day >= 12 {
				level = 0.90 - 0.001*float64(i) // the later the path, the larger its drop
			}
			samples = append(samples, Sample{RNIC: p.RNIC, Endpoint: p.Endpoint, Reading: Reading{BandwidthBPS: baseBW * level, LatP50Ns: baseP50, LatP99Ns: baseP99}})
		}
		res = tr.Observe(now, samples, Software{})
		now = now.Add(24 * time.Hour)
	}
	sum := res.Summary
	if sum.Drifted != 50 || len(sum.Findings) != maxFindings || sum.FindingsOmitted != 50-maxFindings {
		t.Fatalf("%d drifted, %d findings, %d omitted", sum.Drifted, len(sum.Findings), sum.FindingsOmitted)
	}
	if sum.Findings[0].Endpoint != "gpu49" || sum.Findings[maxFindings-1].Endpoint != fmt.Sprintf("gpu%d", 50-maxFindings) {
		t.Fatalf("findings run from %s to %s, want the largest changes, worst first", sum.Findings[0].Endpoint, sum.Findings[maxFindings-1].Endpoint)
	}
	// The metrics still name every path.
	if len(res.Paths) != 50 || res.Paths[0].Status != model.DriftDrifted {
		t.Fatalf("%d path states, the first %s", len(res.Paths), res.Paths[0].Status)
	}
}

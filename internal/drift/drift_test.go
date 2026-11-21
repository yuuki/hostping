package drift

import (
	"encoding/json"
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

// threshold is the change at which the signal of p is judged drifted.
func threshold(t *testing.T, p model.PathDrift, signal model.DriftSignal) float64 {
	t.Helper()
	for _, c := range p.Changes {
		if c.Signal == signal {
			return c.Threshold
		}
	}
	t.Fatalf("no %s threshold in %+v", signal, p)
	return 0
}

// logged reports whether a line of s.logs holds sub.
func (s *series) logged(sub string) bool {
	return strings.Contains(strings.Join(s.logs, "\n"), sub)
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
		t.Fatalf("the tracker wrote a history it would reject: %v", err)
	}
	return out
}

// restart replaces the series' tracker by one a restarted daemon builds over
// the saved history.
func (s *series) restart() {
	s.t.Helper()
	s.tr = New(testTh, []PathIdent{s.p}, []string{"0000:17:00.0"}, reload(s.t, s.tr.File()))
}

// A path is unknown, with the reason, until its reference and then enough
// recent days exist: a daily value counts once its day is over, so the first
// verdict comes with the run of the 11th day. The numbers are the documented
// ones, spelled out: 7 days of reference, 3 recent days.
func TestNoVerdictBeforeEnoughHistory(t *testing.T) {
	s := newSeries(t)
	for i := 1; i <= 12; i++ {
		got := s.day(baseBW, baseP50, baseP99)
		want, why := model.DriftUnknown, model.DriftCollectingReference
		switch {
		case i >= 11:
			want, why = model.DriftStable, model.DriftReasonNone
		case i > 7:
			why = model.DriftFewRecentDays
		}
		if got.Status != want || got.Reason != why {
			t.Fatalf("day %d: %s (%s), want %s (%s)", i, got.Status, got.Reason, want, why)
		}
		if (got.Status == model.DriftUnknown) != (len(got.Changes) == 0) {
			t.Fatalf("day %d: an unknown path must carry no change, a judged one all of them: %+v", i, got)
		}
	}
	if th := threshold(t, s.last, model.DriftBandwidth); th != 0.05 {
		t.Fatalf("threshold of a quiet path = %v, want --drift-th", th)
	}
	sum := s.res.Summary
	if sum.Stable != 1 || sum.Drifted != 0 || sum.Unknown != 0 || len(sum.UnknownReasons) != 0 || len(sum.Findings) != 0 || len(sum.SoftwareChanges) != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	ref := s.tr.File().Paths[gpuPath.Key].Ref
	if ref.From != dayOf(start) || ref.To != dayOf(start)+6 {
		t.Fatalf("reference spans days %d..%d, want the first 7", ref.From-dayOf(start), ref.To-dayOf(start))
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
	if h.Open == nil || h.Open.Seen != 500 || len(h.Open.Samples) > 16 || len(h.Days) != 0 {
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
	if f.Threshold != 0.05 || f.WorseDays != 7 || f.RecentDays != 7 {
		t.Fatalf("threshold %v, %d of %d recent days worse: want --drift-th and every day", f.Threshold, f.WorseDays, f.RecentDays)
	}
	if len(s.res.Summary.Findings) != 1 {
		t.Fatalf("a bandwidth drift must not flag the latencies: %+v", s.res.Summary.Findings)
	}
	if !s.logged("drifted: rnic0->gpu0 bandwidth") {
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
	// The verdict needs most of the recent days at the new level: four of
	// seven, so it comes with the run of the fifth day after the step.
	if f.Since != "2026-03-25" {
		t.Fatalf("since %s, want the fifth day at the new level", f.Since)
	}
}

// Once the step is older than the 30 daily values a path keeps, the path
// still reads below its reference, with neither a trend nor a step to show
// for it.
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
	if f := s.finding(model.DriftBandwidth); f.Shape != model.DriftShapeSustained || f.Since != "2026-03-25" {
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
		t.Fatalf("verdict not cleared: %v", h.Drifted)
	}
	if !s.logged("rnic0->gpu0 bandwidth recovered: 200.00 Gb/s against its reference 200.00 Gb/s") {
		t.Fatalf("no log line for the recovery: %q", s.logs)
	}
}

// The verdict changes at exactly the documented points. A signal is drifted
// when the median of its recent days has moved by the threshold, 5% here, and
// not a little before. It stays drifted while two or more of its recent days
// still read worse than the reference by half the threshold, 2.5%, and stops
// when one does or none.
func TestVerdictBoundaries(t *testing.T) {
	const (
		atThreshold   = 0.95  // exactly 5% down
		belowIt       = 0.951 // 4.9% down
		atHalf        = 0.975 // exactly 2.5% down
		belowHalf     = 0.976
		wellRecovered = 1.0
	)
	enters := func(level float64) model.DriftStatus {
		s := newSeries(t)
		s.days(7, flat)
		s.days(10, func(int) float64 { return level })
		return s.last.Status
	}
	if got := enters(atThreshold); got != model.DriftDrifted {
		t.Errorf("a change of exactly the threshold: %s, want drifted", got)
	}
	if got := enters(belowIt); got != model.DriftStable {
		t.Errorf("a change just below the threshold: %s, want stable", got)
	}

	// Drifted at 8% down, then the last seven days are given one by one.
	leaves := func(week [7]float64) model.DriftStatus {
		s := newSeries(t)
		s.days(7, flat)
		s.days(10, func(int) float64 { return 0.92 })
		if s.last.Status != model.DriftDrifted {
			t.Fatalf("8%% down: %s", s.last.Status)
		}
		s.days(7, func(i int) float64 { return week[i] })
		// One more run closes the seventh day.
		s.observe(nil, Software{})
		return s.res.Paths[0].Status
	}
	r := wellRecovered
	for _, c := range []struct {
		name string
		week [7]float64
		want model.DriftStatus
	}{
		{"no day worse", [7]float64{r, r, r, r, r, r, r}, model.DriftStable},
		{"one day still low", [7]float64{r, r, r, 0.92, r, r, r}, model.DriftStable},
		{"two days still low", [7]float64{r, 0.92, r, r, 0.92, r, r}, model.DriftDrifted},
		{"two days at exactly half the threshold", [7]float64{r, atHalf, r, r, atHalf, r, r}, model.DriftDrifted},
		{"two days just inside half the threshold", [7]float64{r, belowHalf, r, r, belowHalf, r, r}, model.DriftStable},
		{"every day in the band between half and the whole", [7]float64{0.96, 0.96, 0.96, 0.96, 0.96, 0.96, 0.96}, model.DriftDrifted},
	} {
		if got := leaves(c.week); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
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
	// The finding says why a signal that is below the threshold is still
	// reported.
	if f := held.finding(model.DriftBandwidth); f.Change >= f.Threshold || f.WorseDays != 7 || f.RecentDays != 7 {
		t.Fatalf("finding = %+v, want a change below the threshold with every recent day worse by half of it", f)
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
		if i%8 == 7 {
			level = 0.5 // every eighth day reads half
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
	// Days that only ever read worse are not scatter either: the threshold
	// stays where --drift-th puts it.
	if th := threshold(t, s.last, model.DriftBandwidth); th != 0.05 {
		t.Fatalf("threshold %v, want --drift-th", th)
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
// is remembered through the gap, and so are the daily values from before it.
func TestGap(t *testing.T) {
	s := newSeries(t)
	s.days(12, flat)
	s.days(10, func(int) float64 { return 0.93 })
	if s.last.Status != model.DriftDrifted {
		t.Fatalf("before the gap: %s", s.last.Status)
	}
	s.now = s.now.Add(21 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		// 3.5% down: inside the band, so the verdict follows what it was.
		if got := s.day(baseBW*0.965, baseP50, baseP99); got.Status != model.DriftUnknown || got.Reason != model.DriftFewRecentDays {
			t.Fatalf("day %d after the gap: %s (%s), want unknown (few_recent_days)", i+1, got.Status, got.Reason)
		}
	}
	if !s.logged("is 22 days past the last run the history saw (2026-03-22)") {
		t.Fatalf("no log line for the gap: %q", s.logs)
	}
	if got := s.day(baseBW*0.965, baseP50, baseP99); got.Status != model.DriftDrifted {
		t.Fatalf("after the gap, inside the band: %s, want the verdict from before held", got.Status)
	}
	if f := s.finding(model.DriftBandwidth); f.Since != "2026-03-17" || f.RecentFrom != "2026-04-13" || f.RecentDays != 3 {
		t.Fatalf("since %s, recent from %s over %d days", f.Since, f.RecentFrom, f.RecentDays)
	}
	if n := len(s.tr.File().Paths[gpuPath.Key].Days); n != 25 {
		t.Fatalf("%d daily values, want the 22 from before the gap and the 3 since", n)
	}
	// The same gap on a path that was stable resolves the band the other way.
	q := newSeries(t)
	q.days(22, flat)
	q.now = q.now.Add(21 * 24 * time.Hour)
	q.days(3, func(int) float64 { return 0.965 })
	if got := q.day(baseBW*0.965, baseP50, baseP99); got.Status != model.DriftStable {
		t.Fatalf("stable before the gap, inside the band after it: %s", got.Status)
	}
}

// Only days of the last 14 calendar days are recent. A daily value that is
// 13 days old still counts, and one that is 14 days old does not.
func TestRecentDaysAreTheLastFourteen(t *testing.T) {
	judged := func(gap int) model.PathDrift {
		s := newSeries(t)
		s.days(10, flat) // 7 of reference and 3 more: days 0..9
		// The run that judges them comes gap days after the last of them.
		s.now = start.Add(time.Duration(9+gap) * 24 * time.Hour)
		return s.observe(nil, Software{})
	}
	// Days 7, 8 and 9 are recent as long as day 7 is: up to 13 days back
	// from the day of the run.
	if got := judged(11); got.Status != model.DriftStable {
		t.Errorf("the oldest of three days is 13 days old: %s (%s), want it judged", got.Status, got.Reason)
	}
	if got := judged(12); got.Status != model.DriftUnknown || got.Reason != model.DriftFewRecentDays {
		t.Errorf("the oldest of three days is 14 days old: %s (%s), want unknown (few_recent_days)", got.Status, got.Reason)
	}
}

// Three recent days are enough for a verdict and two are not.
func TestThreeRecentDaysAreNeeded(t *testing.T) {
	s := newSeries(t)
	s.days(7, flat)
	s.days(2, flat)
	if got := s.day(baseBW, baseP50, baseP99); got.Reason != model.DriftFewRecentDays {
		t.Fatalf("two days after the reference: %s (%s)", got.Status, got.Reason)
	}
	if got := s.day(baseBW, baseP50, baseP99); got.Status != model.DriftStable {
		t.Fatalf("three days after the reference: %s (%s)", got.Status, got.Reason)
	}
}

// A path the runs stop reading (it turned abnormal, its probe is skipped)
// is unknown once its recent days are gone, and keeps its reference.
func TestPathWithoutReadings(t *testing.T) {
	s := newSeries(t)
	s.days(20, flat)
	for i := 0; i < 16; i++ {
		s.observe(nil, Software{})
		s.now = s.now.Add(24 * time.Hour)
	}
	got := s.observe(nil, Software{})
	if got.Status != model.DriftUnknown || got.Reason != model.DriftFewRecentDays {
		t.Fatalf("%s (%s), want unknown (few_recent_days)", got.Status, got.Reason)
	}
	if h := s.tr.File().Paths[gpuPath.Key]; h.Ref == nil || len(h.Days) != 20 {
		t.Fatalf("reference %v with %d daily values, want both kept", h.Ref, len(h.Days))
	}
}

// A year of runs every five minutes keeps a bounded history: the last 30
// daily values and at most 16 readings of the day.
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
		if len(h.Days) > 30 || len(h.Open.Samples) > 16 {
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
	// The runs start at noon, so the last one falls on the 401st UTC day,
	// which is still open: the 30 days before it are kept.
	if d := h.Days[len(h.Days)-1]; d.N != 288 || len(h.Days) != 30 || h.Days[0].Day != dayOf(start)+400-30 {
		t.Fatalf("%d days from day %d, the last with n = %d: want the last 30, every run of a day counted", len(h.Days), h.Days[0].Day-dayOf(start), d.N)
	}
	t.Logf("largest history of one path: %d bytes", largest)
}

// The window is the last 30 days that have a value, whatever their age: a
// path read once a week keeps 30 weeks.
func TestWindowCountsDaysNotTheCalendar(t *testing.T) {
	s := newSeries(t)
	for i := 0; i < 40; i++ {
		s.observe(&Reading{BandwidthBPS: baseBW, LatP50Ns: baseP50, LatP99Ns: baseP99}, Software{})
		s.now = s.now.Add(7 * 24 * time.Hour)
	}
	h := s.tr.File().Paths[gpuPath.Key]
	if len(h.Days) != 30 || h.Days[0].Day != dayOf(start)+7*9 || h.Days[29].Day != dayOf(start)+7*38 {
		t.Fatalf("%d daily values from day %d to day %d, want the last 30 of 39 closed ones", len(h.Days), h.Days[0].Day-dayOf(start), h.Days[len(h.Days)-1].Day-dayOf(start))
	}
	if h.Ref == nil || h.Ref.From != dayOf(start) || h.Ref.To != dayOf(start)+7*6 {
		t.Fatalf("reference = %+v, want the first 7 values, a week apart", h.Ref)
	}
}

// The kept readings of a busy day are spread over the whole day, not its
// first 16 runs.
func TestThinningKeepsTheWholeDay(t *testing.T) {
	h := &PathHistory{}
	for i := 0; i < 1000; i++ {
		h.add(100, Reading{BandwidthBPS: float64(i + 1), LatP50Ns: 1, LatP99Ns: 1})
	}
	o := h.Open
	if o.Seen != 1000 || len(o.Samples) < 8 || len(o.Samples) > 16 {
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

// A path the topology no longer has keeps its history for 90 days with a
// run, in case the device comes back, and is dropped after that. Only days
// with a run count: a clock that reads months ahead for one run is one day.
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
	for i := 0; i < 90; i++ {
		// Several runs a day are one day.
		for run := 0; run < 3; run++ {
			res := one.Observe(now.Add(time.Duration(run)*time.Hour), []Sample{{RNIC: "rnic0", Endpoint: "mem0", Reading: reading}}, Software{})
			if len(res.Paths) != 1 {
				t.Fatalf("result names %d paths", len(res.Paths))
			}
		}
		now = now.Add(24 * time.Hour)
	}
	if h := one.File().Paths[gpuPath.Key]; h == nil || h.Absent != 90 {
		t.Fatalf("history after 90 days without the path: %+v, want it kept", h)
	}
	// It comes back in time: its reference is still there, and the count of
	// days starts over.
	back := New(testTh, []PathIdent{gpuPath, memPath}, nil, reload(t, one.File()))
	res := back.Observe(now, nil, Software{})
	if res.Paths[0].Reason != model.DriftFewRecentDays || back.File().Paths[gpuPath.Key].Absent != 0 {
		t.Fatalf("returned path: %s (%s), absent %d: want its reference in use", res.Paths[0].Status, res.Paths[0].Reason, back.File().Paths[gpuPath.Key].Absent)
	}
	// It stays away for a 91st day: dropped, with a log line.
	res = one.Observe(now, nil, Software{})
	if one.File().Paths[gpuPath.Key] != nil {
		t.Fatal("stale history kept past 90 days")
	}
	if len(res.Logs) != 1 || !strings.Contains(res.Logs[0], "dropped the history of 1 paths the topology has not had on 90 days with a run") {
		t.Fatalf("logs = %q", res.Logs)
	}
}

// A full history makes room by dropping the path the topology has lacked for
// the longest, and leaves a path untracked, saying so, when every held path
// is one the topology has.
func TestFullHistory(t *testing.T) {
	f := File{Paths: map[string]*PathHistory{}}
	for i := 0; i < MaxPaths; i++ {
		f.Paths[fmt.Sprintf("stale-%04d", i)] = &PathHistory{Absent: 60 - i/40}
	}
	live := []PathIdent{gpuPath, memPath}
	tr := New(testTh, live, nil, f)
	res := tr.Observe(start, nil, Software{})
	if len(tr.File().Paths) != 2048 {
		t.Fatalf("%d paths, want the bound 2048", len(tr.File().Paths))
	}
	for _, gone := range []string{"stale-0000", "stale-0001"} {
		if tr.File().Paths[gone] != nil {
			t.Fatalf("%s kept: the paths lacked for the longest must make room", gone)
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

// A threshold change moves the bar, not the history. A signal that drifted
// at a lower --drift-th and is inside the new one stops being reported, and
// the log does not call that a recovery.
func TestThresholdIsTheTrackers(t *testing.T) {
	s := newSeries(t)
	s.days(12, flat)
	s.days(10, func(int) float64 { return 0.93 })
	if s.last.Status != model.DriftDrifted {
		t.Fatalf("7%% down at a 5%% threshold: %s", s.last.Status)
	}
	loose := New(0.20, []PathIdent{gpuPath}, nil, reload(t, s.tr.File()))
	res := loose.Observe(s.now, nil, Software{})
	if res.Paths[0].Status != model.DriftStable || res.Paths[0].Changes[0].Threshold != 0.20 {
		t.Fatalf("7%% down at a 20%% threshold: %s with threshold %v", res.Paths[0].Status, res.Paths[0].Changes[0].Threshold)
	}
	if len(res.Logs) != 1 || strings.Contains(res.Logs[0], "recovered") ||
		!strings.Contains(res.Logs[0], "is no longer counted as drifted, with the threshold now at 20.0%") {
		t.Fatalf("logs = %q, want a line that does not call it a recovery", res.Logs)
	}
	// A lower threshold does not let a drifted signal go more easily than
	// the one it drifted at.
	tight := New(0.02, []PathIdent{gpuPath}, nil, reload(t, s.tr.File()))
	res = tight.Observe(s.now, nil, Software{})
	if res.Paths[0].Status != model.DriftDrifted || res.Paths[0].Changes[0].Threshold != 0.05 {
		t.Fatalf("at a 2%% threshold: %s with threshold %v, want drifted at the 5%% it drifted at", res.Paths[0].Status, res.Paths[0].Changes[0].Threshold)
	}
}

// A change that reaches every path at once gives a report its worst
// findings and the count of the rest, not one finding per path and signal,
// and names the software change behind it once, not once per finding. The
// section stays a few kilobytes.
func TestDriftSectionIsBounded(t *testing.T) {
	var paths []PathIdent
	var rnics []string
	for r := 0; r < 8; r++ {
		rnics = append(rnics, fmt.Sprintf("0000:%02x:00.0", 0x17+r))
	}
	for i := 0; i < 40; i++ {
		paths = append(paths, PathIdent{RNIC: fmt.Sprintf("rnic%d", i%8), Endpoint: fmt.Sprintf("gpu%d", i/8), Key: fmt.Sprintf("path-%02d", i), EndpointSensor: true})
	}
	tr := New(testTh, paths, rnics, File{})
	now := start
	var res Result
	for day := 0; day < 40; day++ {
		level, kernel, fw := 1.0, "6.8.0-45-generic", "28.39.1002"
		if day >= 15 {
			fw = "28.40.1000"
		}
		if day >= 18 {
			fw = "28.41.1000"
		}
		var samples []Sample
		tR, tE := 55.0, 61.0
		for i, p := range paths {
			l := level
			if day >= 20 {
				kernel = "6.8.0-48-generic"
				l = 0.93 - 0.0005*float64(i) // the later the path, the larger its drop
			}
			samples = append(samples, Sample{RNIC: p.RNIC, Endpoint: p.Endpoint, Reading: Reading{BandwidthBPS: baseBW * l, LatP50Ns: baseP50 / l, LatP99Ns: baseP99 / l, TempRNIC: &tR, TempEndpoint: &tE}})
		}
		firmware := map[string]string{}
		for _, k := range rnics {
			firmware[k] = fw
		}
		res = tr.Observe(now, samples, Software{Kernel: kernel, Hostping: "0.2.6", Firmware: firmware})
		now = now.Add(24 * time.Hour)
	}
	sum := res.Summary
	if sum.Drifted != 40 || len(sum.Findings) != 32 || sum.FindingsOmitted != 3*40-32 {
		t.Fatalf("%d drifted, %d findings, %d omitted: want every path, the 32 largest of 120 findings", sum.Drifted, len(sum.Findings), sum.FindingsOmitted)
	}
	for i := 1; i < len(sum.Findings); i++ {
		if sum.Findings[i].Change > sum.Findings[i-1].Change {
			t.Fatalf("finding %d has a larger change than the one before it", i)
		}
	}
	if sum.Findings[0].Endpoint != "gpu4" || sum.Findings[0].RNIC != "rnic7" {
		t.Fatalf("first finding is %s->%s, want the path with the largest drop", sum.Findings[0].RNIC, sum.Findings[0].Endpoint)
	}
	// Two firmware updates of eight RNICs and a kernel update: three
	// entries, once for the section.
	want := []model.DriftSoftwareChange{
		{On: "2026-03-16", Component: "firmware", From: "28.39.1002", To: "28.40.1000", Devices: 8},
		{On: "2026-03-19", Component: "firmware", From: "28.40.1000", To: "28.41.1000", Devices: 8},
		{On: "2026-03-21", Component: "kernel", From: "6.8.0-45-generic", To: "6.8.0-48-generic"},
	}
	if fmt.Sprint(sum.SoftwareChanges) != fmt.Sprint(want) || sum.SoftwareChangesOmitted != 0 {
		t.Fatalf("software changes = %+v (%d omitted), want %+v", sum.SoftwareChanges, sum.SoftwareChangesOmitted, want)
	}
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 20<<10 {
		t.Fatalf("the drift section is %d bytes", len(b))
	}
	t.Logf("drift section with 32 findings: %d bytes", len(b))
	// The metrics still name every path.
	if len(res.Paths) != 40 || res.Paths[0].Status != model.DriftDrifted {
		t.Fatalf("%d path states, the first %s", len(res.Paths), res.Paths[0].Status)
	}
}

package drift

import (
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

func reading(level float64) *Reading {
	return &Reading{BandwidthBPS: baseBW * level, LatP50Ns: baseP50, LatP99Ns: baseP99}
}

// pathsJSON is the paths of a history as they would be saved, to compare
// two states of one history.
func pathsJSON(t *testing.T, f File) string {
	t.Helper()
	b, err := Marshal(File{Version: Version, Paths: f.Paths})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The clock stepping back behind a history of many days (an RTC that lost
// its battery): the history is the longer witness. Nothing is recorded
// under a day it is already past, and the paths say why they are unknown.
// When the clock is past the history again, tracking goes on where it was.
func TestClockStepsBackAndReturns(t *testing.T) {
	s := newSeries(t)
	s.days(20, flat)
	before := pathsJSON(t, s.tr.File())
	good := s.now
	s.now = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	got := s.observe(reading(0.5), Software{Kernel: "6.8.0-1"})
	if got.Status != model.DriftUnknown || got.Reason != model.DriftClockBehind {
		t.Fatalf("%s (%s), want unknown (clock_behind)", got.Status, got.Reason)
	}
	if sum := s.res.Summary; sum.Unknown != 1 || len(sum.UnknownReasons) != 1 || sum.UnknownReasons[0].Reason != model.DriftClockBehind {
		t.Fatalf("summary = %+v", sum)
	}
	if after := pathsJSON(t, s.tr.File()); after != before {
		t.Fatalf("a run under a clock behind the history changed it:\n%s\n%s", before, after)
	}
	if !s.logged("the clock (2000-01-01) is behind the newest day of the history (2026-03-20)") {
		t.Fatalf("no log line: %q", s.logs)
	}
	s.now = good
	if got := s.day(baseBW, baseP50, baseP99); got.Status != model.DriftStable {
		t.Fatalf("after the clock returned: %s (%s)", got.Status, got.Reason)
	}
	if s.tr.File().BehindSince != nil {
		t.Fatal("the day the clock was first seen behind was kept after it returned")
	}
}

// One idle run under a clock that reads ahead (a boot with a wrong RTC,
// before time synchronization) costs that run and nothing else. The daily
// values from before it stay, a path that is still collecting its reference
// keeps what it has, the history of a path the topology lacks is not
// expired, and once the clock is right the run's readings are dropped at
// once, with a log line: they span less than 2 days, so the clock that took
// them is the one that was wrong.
func TestOneRunUnderAClockAhead(t *testing.T) {
	for _, ahead := range []int{40, 100, 400} {
		tr := New(testTh, []PathIdent{gpuPath, memPath}, nil, File{})
		now := start
		samples := func(mem bool) []Sample {
			out := []Sample{{RNIC: "rnic0", Endpoint: "gpu0", Reading: *reading(1)}}
			if mem {
				out = append(out, Sample{RNIC: "rnic0", Endpoint: "mem0", Reading: *reading(1)})
			}
			return out
		}
		// The GPU path for 20 days, the memory path for the last 6 of them.
		for i := 0; i < 20; i++ {
			tr.Observe(now, samples(i >= 14), Software{})
			now = now.Add(24 * time.Hour)
		}
		// A third path is gone from the topology since yesterday.
		tr.File().Paths["gone"] = &PathHistory{Days: []Day{{Day: dayOf(now) - 1, N: 1, Reading: *reading(1)}}}

		res := tr.Observe(now.AddDate(0, 0, ahead), samples(true), Software{})
		g, m := tr.File().Paths[gpuPath.Key], tr.File().Paths[memPath.Key]
		if len(g.Days) != 20 || g.Ref == nil {
			t.Fatalf("%d days ahead: the GPU path has %d daily values, want the 20 from before", ahead, len(g.Days))
		}
		if len(m.Days) != 6 || m.Ref != nil {
			t.Fatalf("%d days ahead: the memory path has %d daily values, want the 6 it had collected", ahead, len(m.Days))
		}
		if tr.File().Paths["gone"] == nil {
			t.Fatalf("%d days ahead: one run expired the history of a path the topology lacks", ahead)
		}
		if len(res.Logs) != 1 || !containsAll(res.Logs[0], "days past the last run the history saw", "or the clock reads ahead") {
			t.Fatalf("%d days ahead: logs = %q, want one line about the jump", ahead, res.Logs)
		}

		// The clock is right again on the next run.
		res = tr.Observe(now, samples(true), Software{})
		if res.Paths[0].Status != model.DriftStable {
			t.Fatalf("%d days ahead: after the correction the GPU path is %s (%s), want it judged at once", ahead, res.Paths[0].Status, res.Paths[0].Reason)
		}
		if res.Paths[1].Reason != model.DriftCollectingReference {
			t.Fatalf("%d days ahead: after the correction the memory path is %s (%s)", ahead, res.Paths[1].Status, res.Paths[1].Reason)
		}
		if len(res.Logs) != 1 || !containsAll(res.Logs[0], "dropped 2 daily summaries dated after "+dateOf(dayOf(now)), "they span fewer than 2 days") {
			t.Fatalf("%d days ahead: logs after the correction = %q", ahead, res.Logs)
		}
		g, m = tr.File().Paths[gpuPath.Key], tr.File().Paths[memPath.Key]
		if len(g.Days) != 20 || len(m.Days) != 6 || g.Open == nil || g.Open.Day != dayOf(now) || m.Open == nil {
			t.Fatalf("%d days ahead: after the correction: %d and %d daily values, open %+v", ahead, len(g.Days), len(m.Days), g.Open)
		}
		// The memory path completes its reference with the next day, as if
		// nothing had happened.
		res = tr.Observe(now.Add(24*time.Hour), samples(true), Software{})
		if tr.File().Paths[memPath.Key].Ref == nil {
			t.Fatalf("%d days ahead: the memory path did not form its reference from its 7 days", ahead)
		}
		if len(res.Logs) != 0 {
			t.Fatalf("%d days ahead: logs a day later = %q", ahead, res.Logs)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// A clock that read ahead for two days or more has left a history of its
// own, and which of the two clocks was wrong is not known. Nothing is
// recorded for 2 days, and then what is dated after the clock is dropped.
// The days from before the jump are kept. The first day behind is kept in
// the history, so that a daemon that is restarted every day waits 2 days
// too, and not for ever.
func TestClockRanAheadForDays(t *testing.T) {
	for _, restartDaily := range []bool{false, true} {
		s := newSeries(t)
		s.days(20, flat) // 2026-03-01 .. 03-20
		good := s.now    // 2026-03-21
		s.now = good.AddDate(1, 0, 0)
		s.days(3, func(int) float64 { return 0.5 }) // three days dated a year ahead
		s.now = good
		for i := 0; i < 2; i++ {
			if restartDaily {
				s.restart()
			}
			// Several runs a day do not shorten the wait.
			for run := 0; run < 3; run++ {
				got := s.observe(reading(1), Software{})
				if got.Reason != model.DriftClockBehind {
					t.Fatalf("restart %v, day %d behind: %s (%s)", restartDaily, i+1, got.Status, got.Reason)
				}
				s.now = s.now.Add(time.Hour)
			}
			s.now = s.now.Add(21 * time.Hour)
		}
		if since := s.tr.File().BehindSince; since == nil || *since != dayOf(good) {
			t.Fatalf("restart %v: first day behind = %v, want it kept in the history", restartDaily, since)
		}
		if restartDaily {
			s.restart()
		}
		if got := s.day(baseBW, baseP50, baseP99); got.Status != model.DriftStable {
			t.Fatalf("restart %v, third day: %s (%s), want the history from before the jump in use", restartDaily, got.Status, got.Reason)
		}
		h := s.tr.File().Paths[gpuPath.Key]
		if n := len(h.Days); n != 20 || h.Days[n-1].Day != dayOf(good)-1 || h.Open == nil || h.Open.Day != dayOf(good)+2 {
			t.Fatalf("restart %v: %d daily values, open %+v: want the 20 from before the jump and today open", restartDaily, n, h.Open)
		}
		if h.Ref == nil || h.Ref.BandwidthBPS != baseBW || h.Ref.To != dayOf(start)+6 {
			t.Fatalf("restart %v: reference = %+v, want the first week's", restartDaily, h.Ref)
		}
		if !s.logged("dropped 3 daily summaries dated after 2026-03-23, and the reference of 0 paths that reaches past it: the clock has stayed behind them for 2 days") {
			t.Fatalf("restart %v: no log line for the drop: %q", restartDaily, s.logs)
		}
		if s.tr.File().BehindSince != nil {
			t.Fatalf("restart %v: the first day behind was kept after the drop", restartDaily)
		}
	}
}

// What is dropped is what is dated after the clock. A daily value of the
// very day the clock reads is kept, and so is a reference that ends on it.
// A reference that reaches past it cannot be told apart from the days that
// are dropped, and its path starts over.
func TestWhatAClockBehindDrops(t *testing.T) {
	day := func(d int) Day { return Day{Day: d, N: 1, Reading: *reading(1)} }
	week := func(from int) []Day {
		var ds []Day
		for d := from; d < from+7; d++ {
			ds = append(ds, day(d))
		}
		return ds
	}
	f := File{Paths: map[string]*PathHistory{
		"ends today":    {Probe: "p", Ref: &Reference{From: 94, To: 100, Reading: *reading(1)}, Days: week(94)},
		"ends tomorrow": {Probe: "p", Absent: 3, Ref: &Reference{From: 95, To: 101, Reading: *reading(1)}, Days: week(95), Drifted: map[model.DriftSignal]Verdict{model.DriftBandwidth: {Since: 105, Threshold: 0.05}}},
		"no reference":  {Days: []Day{day(99), day(100), day(101)}, Open: &OpenDay{Day: 102, Seen: 1, Stride: 1, Samples: []Reading{*reading(1)}}},
		"verdict":       {Ref: &Reference{From: 80, To: 86, Reading: *reading(1)}, Days: []Day{day(99), day(104)}, Drifted: map[model.DriftSignal]Verdict{model.DriftBandwidth: {Since: 104, Threshold: 0.07}}},
	}, Events: []Event{{Day: 100, Component: "kernel", From: "a", To: "b"}, {Day: 101, Component: "kernel", From: "b", To: "c"}}, LastDay: 104}
	days, refs := f.dropAfter(100)
	if days != 7+1+1+1 || refs != 1 {
		t.Fatalf("dropped %d daily values and %d references, want 10 and 1", days, refs)
	}
	if h := f.Paths["ends today"]; h.Ref == nil || len(h.Days) != 7 {
		t.Errorf("a reference that ends on the day of the clock: %+v with %d days, want both kept", h.Ref, len(h.Days))
	}
	if h := f.Paths["ends tomorrow"]; h.Ref != nil || len(h.Days) != 0 || len(h.Drifted) != 0 || h.Probe != "p" || h.Absent != 3 {
		t.Errorf("a reference that reaches past the day of the clock: %+v, want the path started over", h)
	}
	if h := f.Paths["no reference"]; len(h.Days) != 2 || h.Days[1].Day != 100 || h.Open != nil {
		t.Errorf("days = %+v, open = %+v: want the day of the clock kept and the days after it gone", h.Days, h.Open)
	}
	if h := f.Paths["verdict"]; len(h.Days) != 1 || h.Drifted[model.DriftBandwidth] != (Verdict{Since: 100, Threshold: 0.07}) {
		t.Errorf("days = %+v, verdict = %+v: want the verdict kept, dated no later than the clock", h.Days, h.Drifted)
	}
	if len(f.Events) != 1 || f.Events[0].Day != 100 || f.LastDay != 100 {
		t.Errorf("events = %+v, last day %d", f.Events, f.LastDay)
	}
	if got := f.daysAfter(100); got != 0 {
		t.Errorf("%d days still dated after the clock", got)
	}
}

// Readings that span fewer than 2 days after the clock are dropped at once,
// and 2 days or more make the clock wait.
func TestHowManyDaysAheadMakeAHistory(t *testing.T) {
	for _, c := range []struct {
		ahead int // days recorded under the clock that read ahead
		waits bool
	}{{1, false}, {2, true}, {3, true}} {
		s := newSeries(t)
		s.days(20, flat)
		good := s.now
		s.now = good.AddDate(0, 6, 0)
		s.days(c.ahead, flat)
		s.now = good
		got := s.observe(reading(1), Software{})
		if waits := got.Reason == model.DriftClockBehind; waits != c.waits {
			t.Errorf("%d days recorded ahead: %s (%s), want the clock to wait: %v", c.ahead, got.Status, got.Reason, c.waits)
		}
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
	for i := 0; i < 3; i++ {
		s.day(baseBW, baseP50, baseP99)
	}
	h := s.tr.File().Paths[gpuPath.Key]
	if h.Ref != nil || len(h.Days) != 0 || h.Open == nil {
		t.Fatalf("history = %+v, want a fresh start", h)
	}
	if got := s.res.Paths[0]; got.Status != model.DriftUnknown || got.Reason != model.DriftCollectingReference {
		t.Fatalf("%s (%s)", got.Status, got.Reason)
	}
	if !s.logged("and the reference of 1 paths that reaches past it") {
		t.Fatalf("no log line: %q", s.logs)
	}
}

// A clock that jumps ahead and stays there is a gap: the verdict waits for
// three new days, and nothing from before is lost.
func TestClockJumpsAhead(t *testing.T) {
	s := newSeries(t)
	s.days(20, flat)
	s.now = s.now.AddDate(3, 0, 0)
	if got := s.day(baseBW, baseP50, baseP99); got.Reason != model.DriftFewRecentDays {
		t.Fatalf("%s (%s), want unknown (few_recent_days)", got.Status, got.Reason)
	}
	s.days(3, flat)
	if s.last.Status != model.DriftStable {
		t.Fatalf("%s (%s)", s.last.Status, s.last.Reason)
	}
	if n := len(s.tr.File().Paths[gpuPath.Key].Days); n != 23 {
		t.Fatalf("%d daily values, want the 20 from before the jump and 3 since", n)
	}
}

// The gap is logged when the clock is more than 14 days past the last run,
// and not for a host that was off for two weeks.
func TestGapIsLoggedPastFourteenDays(t *testing.T) {
	for _, c := range []struct {
		gap  int
		logs bool
	}{{14, false}, {15, true}} {
		s := newSeries(t)
		s.days(3, flat) // the last run is on day 2
		s.now = start.Add(time.Duration(2+c.gap) * 24 * time.Hour)
		s.observe(nil, Software{})
		if got := s.logged("days past the last run the history saw"); got != c.logs {
			t.Errorf("a run %d days after the last: logged %v, want %v: %q", c.gap, got, c.logs, s.logs)
		}
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
	s.observe(reading(0.5), Software{})
	h := s.tr.File().Paths[gpuPath.Key]
	if h.Open != nil || len(h.Days) != 3 || h.Days[2].BandwidthBPS != baseBW {
		t.Fatalf("open = %+v, days = %+v", h.Open, h.Days)
	}
}

// A clock that steps further back while it is behind starts the wait over
// from the day it now reads: the two days are counted on the clock that
// holds, not on one that has moved again.
func TestClockStepsFurtherBackWhileBehind(t *testing.T) {
	s := newSeries(t)
	s.days(30, flat)
	at := func(day int) time.Time { return start.Add(time.Duration(day) * 24 * time.Hour) }

	s.now = at(10)
	if got := s.observe(reading(1), Software{}); got.Reason != model.DriftClockBehind {
		t.Fatalf("clock on day 10: %s (%s)", got.Status, got.Reason)
	}
	if since := s.tr.File().BehindSince; since == nil || *since != dayOf(start)+10 {
		t.Fatalf("first seen behind on %v, want day 10", since)
	}
	// Five days further back: behind since then.
	s.now = at(5)
	if got := s.observe(reading(1), Software{}); got.Reason != model.DriftClockBehind {
		t.Fatalf("clock on day 5: %s (%s)", got.Status, got.Reason)
	}
	if since := s.tr.File().BehindSince; since == nil || *since != dayOf(start)+5 {
		t.Fatalf("first seen behind on %v, want day 5 after the second step", since)
	}
	s.now = at(6)
	if got := s.observe(reading(1), Software{}); got.Reason != model.DriftClockBehind {
		t.Fatalf("one day after the second step: %s (%s), want the wait still on", got.Status, got.Reason)
	}
	// Two days after the second step the clock has held: what is dated
	// after it goes. Counted from the first step, it would not be due before
	// day 12.
	s.now = at(7)
	if got := s.observe(reading(1), Software{}); got.Reason == model.DriftClockBehind {
		t.Fatalf("two days after the second step: still %s (%s)", got.Status, got.Reason)
	}
	if !s.logged("the clock has stayed behind them for 2 days") {
		t.Fatalf("no log line for the drop: %q", s.logs)
	}
	if h := s.tr.File().Paths[gpuPath.Key]; len(h.Days) != 8 || h.Days[7].Day != dayOf(start)+7 || h.Open != nil {
		t.Fatalf("history after the drop: %d days, open %+v: want days 0 to 7 kept", len(h.Days), h.Open)
	}
}

// What is dated on the day of the clock itself is not dated after it. The
// open day of a path the topology lacks since that day stays when another
// path's later readings are dropped.
func TestClockBehindKeepsTheDayItReads(t *testing.T) {
	tr := New(testTh, []PathIdent{gpuPath, memPath}, nil, File{})
	both := []Sample{{RNIC: "rnic0", Endpoint: "gpu0", Reading: *reading(1)}, {RNIC: "rnic0", Endpoint: "mem0", Reading: *reading(1)}}
	now := start
	for i := 0; i < 10; i++ {
		tr.Observe(now, both, Software{})
		now = now.Add(24 * time.Hour)
	}
	// Day 10: both paths are read, twice. Then the daemon restarts without
	// the memory path, and one run a day ahead reads the GPU path.
	tr.Observe(now, both, Software{})
	tr.Observe(now, both, Software{})
	tr = New(testTh, []PathIdent{gpuPath}, nil, reload(t, tr.File()))
	tr.Observe(now.Add(24*time.Hour), both[:1], Software{})
	if g := tr.File().Paths[gpuPath.Key]; g.Open == nil || g.Open.Day != dayOf(now)+1 || len(g.Days) != 11 {
		t.Fatalf("GPU path before the correction: %d days, open %+v", len(g.Days), g.Open)
	}
	res := tr.Observe(now, nil, Software{})
	if len(res.Logs) != 1 || !containsAll(res.Logs[0], "dropped 1 daily summaries dated after "+dateOf(dayOf(now))) {
		t.Fatalf("logs = %q", res.Logs)
	}
	g, m := tr.File().Paths[gpuPath.Key], tr.File().Paths[memPath.Key]
	if len(g.Days) != 11 || g.Days[10].Day != dayOf(now) || g.Open != nil {
		t.Fatalf("GPU path: %d days, open %+v: want day 10 kept as closed and the day after it gone", len(g.Days), g.Open)
	}
	if m == nil || m.Open == nil || m.Open.Day != dayOf(now) || m.Open.Seen != 2 || len(m.Days) != 10 {
		t.Fatalf("memory path: %+v: want its open day 10 untouched", m)
	}
}

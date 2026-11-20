// Package drift tracks how each path's idle readings move over months
// (--drift-tracking): hardware that ages, runs hotter or regresses after an
// update loses a few percent long before it falls below the distance baseline
// that the analyzer judges a run against.
//
// The history is one bounded summary per path in DIR/drift.json: the path's
// reference (its first refDays days with a clean idle reading), one median
// per day for the last windowDays days, and the thinned readings of the
// current day. A path is judged by comparing the median of its recent days
// with its own reference, never with another path or with the baseline, and
// only once both sides hold enough days: until then it is unknown, with a
// reason. docs/design.md ("Drift against the host's own history") gives the
// reason behind each constant below.
package drift

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yuuki/hostping/internal/model"
)

const (
	// refDays is how many days with a clean idle reading form a path's
	// reference.
	refDays = 7
	// recentDays is the most days whose median is compared with the
	// reference; minRecentDays is the fewest, and recentSpan how many
	// calendar days back they may lie.
	recentDays    = 7
	minRecentDays = 3
	recentSpan    = 14
	// windowDays is how many calendar days of daily values a path keeps,
	// and so the most it holds: the shape of a drift is read from them.
	windowDays = 30
	// minShapeDays is the fewest daily values a shape is read from.
	minShapeDays = 8
	// openCap bounds the readings kept of the current day. A day that
	// offers more is thinned to every 2nd, 4th, ... reading.
	openCap   = 16
	maxStride = 1 << 20
	maxSeen   = 1 << 30
	// noiseK scales the reference days' scatter into the least change a
	// signal must show: a path whose daily medians already scatter by more
	// than DriftTh/noiseK needs a larger change than DriftTh.
	noiseK = 3
	// iqrSigmas is the interquartile range of a normal distribution in
	// standard deviations.
	iqrSigmas = 1.349
	// stepShare is the share of a signal's change against its reference
	// that the best single step through the window must account for, for
	// the change to be a step. A steady decline over the whole window gives
	// the best step half of the decline at most. stepSpan is the fewest
	// daily values on either side of a step.
	stepShare = 2.0 / 3
	stepSpan  = 3
	// trendZ is the one-sided 1% point of the normal distribution, which
	// the Mann-Kendall statistic must reach for a trend.
	trendZ = 2.326
	// thermalRiseC is how much hotter, in degrees Celsius, a device must
	// read in the recent days than in the reference days to count as risen.
	thermalRiseC = 5
	// staleDays is how long the history of a path the topology no longer
	// has is kept, in case the device comes back.
	staleDays = 90
	// healDays is how many days the clock must stay behind the newest day
	// of the history before the days after it are dropped as recorded under
	// a wrong clock.
	healDays = 2
	// MaxPaths bounds the paths of one history, stale ones included.
	MaxPaths = 2048
	// maxFindings bounds the findings one report carries.
	maxFindings = 32
	// maxEvents bounds the software changes kept; maxText the bytes of a
	// version string and maxKey those of a path or firmware key.
	maxEvents = 16
	maxText   = 64
	maxKey    = 256
	// maxDay bounds a day number (days since 1970-01-01 UTC), so that day
	// arithmetic cannot overflow whatever the clock or the file says.
	maxDay = 1 << 22
	// maxValue bounds a bandwidth (bit/s) or latency (ns) in the history.
	maxValue = 1e18
	// minTempC and maxTempC bound a stored temperature.
	minTempC = -100
	maxTempC = 1000
)

// Reading is one clean idle reading of a path, or the median of several.
type Reading struct {
	BandwidthBPS float64 `json:"bw"`
	LatP50Ns     float64 `json:"p50"`
	LatP99Ns     float64 `json:"p99"`
	// TempRNIC and TempEndpoint are the temperature of the path's RNIC and
	// of its GPU endpoint, in degrees Celsius; nil when it was not read.
	TempRNIC     *float64 `json:"t_rnic,omitempty"`
	TempEndpoint *float64 `json:"t_ep,omitempty"`
}

// Day is the median of one UTC day's clean idle readings of a path.
type Day struct {
	// Day counts days since 1970-01-01 UTC.
	Day int `json:"day"`
	// N is how many clean idle runs read the path that day.
	N int `json:"n"`
	Reading
}

// OpenDay holds the readings of the day that is still running.
type OpenDay struct {
	Day int `json:"day"`
	// Seen counts the clean readings offered so far, kept or not.
	Seen int `json:"seen"`
	// Stride is the thinning in force: every Stride-th reading is kept.
	Stride  int       `json:"stride"`
	Samples []Reading `json:"samples"`
}

// Reference is the level a path is compared with: the median of its first
// refDays daily values, with their scatter (see scatter) per signal.
type Reference struct {
	// From and To are the first and last day behind the reference.
	From int `json:"from"`
	To   int `json:"to"`
	Reading
	BandwidthSpread float64 `json:"bw_spread"`
	LatP50Spread    float64 `json:"p50_spread"`
	LatP99Spread    float64 `json:"p99_spread"`
}

// PathHistory is everything the history keeps of one path.
type PathHistory struct {
	// FirstSeen and LastSeen are the first and last day the path was part
	// of the topology of an observed run.
	FirstSeen int        `json:"first_seen"`
	LastSeen  int        `json:"last_seen"`
	Ref       *Reference `json:"reference,omitempty"`
	// Days are the closed days, oldest first.
	Days []Day    `json:"days,omitempty"`
	Open *OpenDay `json:"open,omitempty"`
	// Drifted holds, per signal judged drifted, the day it was first judged
	// so. It is the memory of the verdict's hysteresis.
	Drifted map[model.DriftSignal]int `json:"drifted,omitempty"`
}

// Software is the software a drift may follow a change of: the kernel
// release and each RNIC's firmware version, keyed like the RNIC half of a
// path key. An empty value was not read.
type Software struct {
	Kernel   string            `json:"kernel,omitempty"`
	Firmware map[string]string `json:"firmware,omitempty"`
}

// Event is one recorded software change.
type Event struct {
	Day       int    `json:"day"`
	Component string `json:"component"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// signal is one tracked reading: where it lives in a Reading and a
// Reference, and which direction is worse.
type signal struct {
	name   model.DriftSignal
	value  func(Reading) float64
	spread func(Reference) float64
	// higherIsWorse is false for bandwidth.
	higherIsWorse bool
}

var signals = []signal{
	{model.DriftBandwidth, func(r Reading) float64 { return r.BandwidthBPS }, func(r Reference) float64 { return r.BandwidthSpread }, false},
	{model.DriftLatencyP50, func(r Reading) float64 { return r.LatP50Ns }, func(r Reference) float64 { return r.LatP50Spread }, true},
	{model.DriftLatencyP99, func(r Reading) float64 { return r.LatP99Ns }, func(r Reference) float64 { return r.LatP99Spread }, true},
}

// PathIdent names one path of the running topology to the tracker.
type PathIdent struct {
	// RNIC and Endpoint are the IDs the report and the metrics carry.
	RNIC     string
	Endpoint string
	// Key identifies the path in the history across restarts and
	// renumbering (see PathKey).
	Key string
	// EndpointSensor is set when the endpoint is a device that can carry a
	// temperature sensor of its own (a GPU). A memory endpoint has none
	// that could be attributed to it.
	EndpointSensor bool
}

// Sample is one clean idle reading of the path RNIC -> Endpoint.
type Sample struct {
	RNIC     string
	Endpoint string
	Reading
}

// Result is what one observed idle run did to the history.
type Result struct {
	// Paths is every tracked path's state, in the order the tracker was
	// given them.
	Paths []model.PathDrift
	// Summary is the report's drift section.
	Summary model.DriftSummary
	// Logs are the lines worth telling the operator: a signal that drifted
	// or recovered, a software change, stale paths that expired, a clock
	// that is behind the history.
	Logs []string
}

// Tracker applies idle runs to one host's drift history.
type Tracker struct {
	th    float64
	paths []PathIdent
	// rnicKeys is the RNIC half of every path key, the firmware map's keys.
	rnicKeys map[string]bool
	inGraph  map[string]bool
	f        File
	// behind is set while the clock reads earlier than the newest day of
	// the history, and behindSince is the first day it was seen so.
	behind      bool
	behindSince int
}

// New returns a tracker over f for the paths of the running topology. th is
// the drift threshold (--drift-th). Firmware versions of RNICs the topology no
// longer has are dropped from f.
func New(th float64, paths []PathIdent, rnicKeys []string, f File) *Tracker {
	t := &Tracker{th: th, paths: paths, rnicKeys: map[string]bool{}, inGraph: map[string]bool{}, f: f}
	if t.f.Paths == nil {
		t.f.Paths = map[string]*PathHistory{}
	}
	t.f.Version = Version
	for _, k := range rnicKeys {
		t.rnicKeys[k] = true
	}
	for _, p := range paths {
		t.inGraph[p.Key] = true
	}
	for k := range t.f.Software.Firmware {
		if !t.rnicKeys[k] {
			delete(t.f.Software.Firmware, k)
		}
	}
	return t
}

// File is the history as it stands, for the store to write.
func (t *Tracker) File() File { return t.f }

// dayOf is the UTC day number of now, held within [0, maxDay].
func dayOf(now time.Time) int {
	d := now.Unix() / 86400
	if now.Unix() < 0 {
		return 0
	}
	if d > maxDay {
		return maxDay
	}
	return int(d)
}

// dateOf spells a day number as YYYY-MM-DD.
func dateOf(day int) string {
	return time.Unix(int64(day)*86400, 0).UTC().Format("2006-01-02")
}

// Observe applies one confirmed idle run to the history: the clean readings
// in samples, taken at now, and the software read with them (the zero value
// when it was not read). Paths the run did not read cleanly get no reading
// but are still judged. It returns every path's state.
func (t *Tracker) Observe(now time.Time, samples []Sample, sw Software) Result {
	day := dayOf(now)
	var res Result
	if newest := t.f.newestDay(); day < newest {
		if !t.behind || day < t.behindSince {
			t.behind, t.behindSince = true, day
			res.Logs = append(res.Logs, fmt.Sprintf("the clock (%s) is behind the newest day of the history (%s): nothing is recorded until it is past it, and after %d days behind the days after it are dropped",
				dateOf(day), dateOf(newest), healDays))
		}
		if day-t.behindSince < healDays {
			for _, p := range t.paths {
				res.Paths = append(res.Paths, model.PathDrift{RNIC: p.RNIC, Endpoint: p.Endpoint, Reason: model.DriftClockBehind})
			}
			res.Summary = summarize(res.Paths, nil)
			return res
		}
		days, reset := t.f.dropAfter(day)
		res.Logs = append(res.Logs, fmt.Sprintf("the clock has stayed behind the history for %d days: dropped %d daily summaries dated after %s as recorded under a wrong clock, and the reference of %d paths",
			healDays, days, dateOf(day), reset))
	}
	t.behind = false

	t.noteSoftware(day, sw, &res)
	bySample := make(map[[2]string]Reading, len(samples))
	for _, s := range samples {
		bySample[[2]string{s.RNIC, s.Endpoint}] = s.Reading
	}
	var findings []model.DriftFinding
	for _, p := range t.paths {
		h := t.entry(p.Key, day)
		if h == nil {
			res.Paths = append(res.Paths, model.PathDrift{RNIC: p.RNIC, Endpoint: p.Endpoint, Reason: model.DriftNotTracked})
			continue
		}
		h.LastSeen = day
		h.FirstSeen = min(h.FirstSeen, day)
		h.roll(day)
		if r, ok := bySample[[2]string{p.RNIC, p.Endpoint}]; ok {
			h.add(day, r)
		}
		pd, fs, logs := t.judge(p, h, day)
		res.Paths = append(res.Paths, pd)
		findings = append(findings, fs...)
		res.Logs = append(res.Logs, logs...)
	}
	if n := t.expire(day); n > 0 {
		res.Logs = append(res.Logs, fmt.Sprintf("dropped the history of %d paths the topology has not had for more than %d days", n, staleDays))
	}
	res.Summary = summarize(res.Paths, findings)
	return res
}

// summarize counts paths per status and unknown paths per reason, and puts
// the findings worst first. A report holds at most maxFindings of them: a
// change that reaches every path at once (a kernel update) would otherwise
// add a finding per path and signal to every report from then on.
func summarize(paths []model.PathDrift, findings []model.DriftFinding) model.DriftSummary {
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Change > findings[j].Change })
	s := model.DriftSummary{Findings: findings}
	if len(findings) > maxFindings {
		s.Findings, s.FindingsOmitted = findings[:maxFindings], len(findings)-maxFindings
	}
	why := map[model.DriftReason]int{}
	for _, p := range paths {
		switch p.Status {
		case model.DriftStable:
			s.Stable++
		case model.DriftDrifted:
			s.Drifted++
		default:
			s.Unknown++
			why[p.Reason]++
		}
	}
	reasons := make([]model.DriftReason, 0, len(why))
	for r := range why {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
	for _, r := range reasons {
		s.UnknownReasons = append(s.UnknownReasons, model.DriftUnknownCount{Reason: r, Paths: why[r]})
	}
	return s
}

// entry is the history of the path key, created on first sight. It is nil
// when the history is full of paths the topology still has.
func (t *Tracker) entry(key string, day int) *PathHistory {
	if h := t.f.Paths[key]; h != nil {
		return h
	}
	if len(t.f.Paths) >= MaxPaths && !t.evictStale() {
		return nil
	}
	h := &PathHistory{FirstSeen: day, LastSeen: day}
	t.f.Paths[key] = h
	return h
}

// evictStale drops the path the topology no longer has that was seen longest
// ago (the smallest key among equals), and reports whether there was one.
func (t *Tracker) evictStale() bool {
	victim := ""
	for k, h := range t.f.Paths {
		if t.inGraph[k] {
			continue
		}
		if v := t.f.Paths[victim]; victim == "" || h.LastSeen < v.LastSeen || (h.LastSeen == v.LastSeen && k < victim) {
			victim = k
		}
	}
	if victim == "" {
		return false
	}
	delete(t.f.Paths, victim)
	return true
}

// expire drops the paths the topology no longer has once they have not been
// seen for staleDays, and returns how many.
func (t *Tracker) expire(day int) int {
	n := 0
	for k, h := range t.f.Paths {
		if !t.inGraph[k] && day-h.LastSeen > staleDays {
			delete(t.f.Paths, k)
			n++
		}
	}
	return n
}

// noteSoftware records what changed between the stored software and sw. A
// value that was not read (empty) changes nothing: unknown is not a change.
func (t *Tracker) noteSoftware(day int, sw Software, res *Result) {
	change := func(component, from, to string) string {
		to = clip(to)
		if to == "" || to == from {
			return from
		}
		if from != "" {
			t.f.Events = append(t.f.Events, Event{Day: day, Component: component, From: from, To: to})
			if len(t.f.Events) > maxEvents {
				t.f.Events = t.f.Events[len(t.f.Events)-maxEvents:]
			}
			res.Logs = append(res.Logs, fmt.Sprintf("%s changed from %s to %s", component, from, to))
		}
		return to
	}
	t.f.Software.Kernel = change("kernel", t.f.Software.Kernel, sw.Kernel)
	keys := make([]string, 0, len(sw.Firmware))
	for k := range sw.Firmware {
		if t.rnicKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := change("firmware "+k, t.f.Software.Firmware[k], sw.Firmware[k])
		if v == "" {
			continue
		}
		if t.f.Software.Firmware == nil {
			t.f.Software.Firmware = map[string]string{}
		}
		if _, known := t.f.Software.Firmware[k]; known || len(t.f.Software.Firmware) < MaxPaths {
			t.f.Software.Firmware[k] = v
		}
	}
}

// clip makes s storable: valid UTF-8, one line, at most maxText bytes.
func clip(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "\uFFFD"))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	for len(s) > maxText {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// roll moves the history to day: a finished open day is closed into a daily
// value, daily values that left the window are dropped, and the reference is
// formed once refDays daily values stand in the window.
func (h *PathHistory) roll(day int) {
	if h.Open != nil && h.Open.Day < day {
		h.Days = append(h.Days, h.Open.close())
		h.Open = nil
	}
	keep := h.Days[:0]
	for _, d := range h.Days {
		if d.Day > day-windowDays {
			keep = append(keep, d)
		}
	}
	h.Days = keep
	if len(h.Days) > windowDays {
		h.Days = h.Days[len(h.Days)-windowDays:]
	}
	if h.Ref == nil && len(h.Days) >= refDays {
		ref := newReference(h.Days[:refDays])
		h.Ref = &ref
	}
}

// add offers one clean reading of day to the open day. Every Stride-th
// reading is kept. When openCap are held, every other one is dropped and the
// stride doubles, so the kept readings stay evenly spread over the day. A
// reading the history could not hold, or one of a day that is already
// closed (the clock stepped back onto it), is not taken.
func (h *PathHistory) add(day int, r Reading) {
	if checkReading(r) != nil {
		return
	}
	if n := len(h.Days); n > 0 && h.Days[n-1].Day >= day {
		return
	}
	if h.Open == nil {
		h.Open = &OpenDay{Day: day, Stride: 1}
	}
	o := h.Open
	if o.Seen%o.Stride == 0 && len(o.Samples) < openCap {
		o.Samples = append(o.Samples, r)
		if len(o.Samples) == openCap && o.Stride < maxStride {
			k := 0
			for i := 0; i < len(o.Samples); i += 2 {
				o.Samples[k] = o.Samples[i]
				k++
			}
			o.Samples = o.Samples[:k]
			o.Stride *= 2
		}
	}
	if o.Seen < maxSeen {
		o.Seen++
	}
}

// close is the open day as a daily value: the median of each signal over the
// kept readings, and of each temperature over the readings that have one.
func (o *OpenDay) close() Day {
	return Day{Day: o.Day, N: o.Seen, Reading: medianReading(o.Samples, 1)}
}

// medianReading is the per-field median of rs. A temperature is present when
// at least minTemps of rs carry it.
func medianReading(rs []Reading, minTemps int) Reading {
	col := func(f func(Reading) float64) float64 {
		vs := make([]float64, len(rs))
		for i, r := range rs {
			vs[i] = f(r)
		}
		return median(vs)
	}
	temp := func(f func(Reading) *float64) *float64 {
		var vs []float64
		for _, r := range rs {
			if p := f(r); p != nil {
				vs = append(vs, *p)
			}
		}
		if len(vs) == 0 || len(vs) < minTemps {
			return nil
		}
		m := median(vs)
		return &m
	}
	return Reading{
		BandwidthBPS: col(func(r Reading) float64 { return r.BandwidthBPS }),
		LatP50Ns:     col(func(r Reading) float64 { return r.LatP50Ns }),
		LatP99Ns:     col(func(r Reading) float64 { return r.LatP99Ns }),
		TempRNIC:     temp(func(r Reading) *float64 { return r.TempRNIC }),
		TempEndpoint: temp(func(r Reading) *float64 { return r.TempEndpoint }),
	}
}

// readings is the Reading of every day of ds.
func readings(ds []Day) []Reading {
	out := make([]Reading, len(ds))
	for i, d := range ds {
		out[i] = d.Reading
	}
	return out
}

// newReference is the reference formed by the daily values ds: each signal's
// median and scatter, and each temperature's median when most days have one.
func newReference(ds []Day) Reference {
	rs := readings(ds)
	ref := Reference{From: ds[0].Day, To: ds[len(ds)-1].Day, Reading: medianReading(rs, len(ds)/2+1)}
	spread := func(f func(Reading) float64) float64 {
		vs := make([]float64, len(rs))
		for i, r := range rs {
			vs[i] = f(r)
		}
		return scatter(vs)
	}
	ref.BandwidthSpread = spread(func(r Reading) float64 { return r.BandwidthBPS })
	ref.LatP50Spread = spread(func(r Reading) float64 { return r.LatP50Ns })
	ref.LatP99Spread = spread(func(r Reading) float64 { return r.LatP99Ns })
	return ref
}

// recent is the daily values a path is judged on at day: the last recentDays
// days after the reference that lie within recentSpan calendar days.
func (h *PathHistory) recent(day int) []Day {
	end := len(h.Days)
	start := end
	for start > 0 && end-start < recentDays {
		d := h.Days[start-1]
		if d.Day <= h.Ref.To || d.Day <= day-recentSpan {
			break
		}
		start--
	}
	return h.Days[start:end]
}

// judge is the path's state at day and the findings of its drifted signals.
// It updates the verdict memory in h and returns a log line per signal that
// drifted or recovered.
func (t *Tracker) judge(p PathIdent, h *PathHistory, day int) (model.PathDrift, []model.DriftFinding, []string) {
	pd := model.PathDrift{RNIC: p.RNIC, Endpoint: p.Endpoint}
	if h.Ref == nil {
		pd.Reason = model.DriftCollectingReference
		return pd, nil, nil
	}
	recent := h.recent(day)
	if len(recent) < minRecentDays {
		pd.Reason = model.DriftFewRecentDays
		return pd, nil, nil
	}
	pd.Status = model.DriftStable
	cur := medianReading(readings(recent), minRecentDays)
	var findings []model.DriftFinding
	var logs []string
	for _, s := range signals {
		ref, now := s.value(h.Ref.Reading), s.value(cur)
		change := (now - ref) / ref
		if !s.higherIsWorse {
			change = -change
		}
		pd.Changes = append(pd.Changes, model.DriftChange{Signal: s.name, Change: change})
		enter := math.Max(t.th, noiseK*s.spread(*h.Ref)/ref)
		since, was := h.Drifted[s.name]
		if !(change >= enter || (was && change >= enter/2)) {
			if was {
				delete(h.Drifted, s.name)
				logs = append(logs, fmt.Sprintf("%s->%s %s recovered: %s against its reference %s (%+.1f%%), drifted since %s",
					p.RNIC, p.Endpoint, s.name, formatValue(s, now), formatValue(s, ref), signed(s, change)*100, dateOf(since)))
			}
			continue
		}
		if !was {
			since = day
			if h.Drifted == nil {
				h.Drifted = map[model.DriftSignal]int{}
			}
			h.Drifted[s.name] = day
		}
		pd.Status = model.DriftDrifted
		f := model.DriftFinding{
			RNIC: p.RNIC, Endpoint: p.Endpoint, Signal: s.name,
			Change: change, Reference: ref, Recent: now,
			ReferenceFrom: dateOf(h.Ref.From), ReferenceTo: dateOf(h.Ref.To),
			RecentFrom: dateOf(recent[0].Day), RecentTo: dateOf(recent[len(recent)-1].Day),
			Since:           dateOf(since),
			Temperature:     temperature(p, h.Ref.Reading, cur),
			SoftwareChanges: t.softwareSince(h.Ref.From),
		}
		var stepDay int
		f.Shape, stepDay = shapeOf(s, h.Days, math.Abs(now-ref))
		if f.Shape == model.DriftShapeStep {
			f.StepOn = dateOf(stepDay)
		}
		findings = append(findings, f)
		if !was {
			logs = append(logs, "drifted: "+FormatFinding(f))
		}
	}
	return pd, findings, logs
}

// signed is change as a change of the reading itself: negative for a
// bandwidth that fell, positive for a latency that rose.
func signed(s signal, change float64) float64 {
	if s.higherIsWorse {
		return change
	}
	return -change
}

// formatValue spells a reading of s with its unit.
func formatValue(s signal, v float64) string {
	if s.name == model.DriftBandwidth {
		return fmt.Sprintf("%.2f Gb/s", v/1e9)
	}
	return fmt.Sprintf("%.0f ns", v)
}

// FormatFinding is the one-line form of f for the log.
func FormatFinding(f model.DriftFinding) string {
	s := signals[0]
	for _, c := range signals {
		if c.name == f.Signal {
			s = c
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s->%s %s %s against its reference %s (%+.1f%%; reference %s..%s, recent %s..%s), shape %s",
		f.RNIC, f.Endpoint, f.Signal, formatValue(s, f.Recent), formatValue(s, f.Reference), signed(s, f.Change)*100,
		f.ReferenceFrom, f.ReferenceTo, f.RecentFrom, f.RecentTo, f.Shape)
	if f.StepOn != "" {
		fmt.Fprintf(&b, " on %s", f.StepOn)
	}
	fmt.Fprintf(&b, ", temperature %s", f.Temperature.Status)
	if f.Temperature.Reason != "" {
		fmt.Fprintf(&b, " (%s)", f.Temperature.Reason)
	}
	for _, d := range f.Temperature.Devices {
		fmt.Fprintf(&b, " %s %.0f->%.0f C", d.Device, d.ReferenceC, d.RecentC)
	}
	for _, c := range f.SoftwareChanges {
		fmt.Fprintf(&b, "; %s %s %s -> %s", c.On, c.Component, c.From, c.To)
	}
	return b.String()
}

// softwareSince is the recorded software changes from day from on.
func (t *Tracker) softwareSince(from int) []model.DriftSoftwareChange {
	var out []model.DriftSoftwareChange
	for _, e := range t.f.Events {
		if e.Day >= from {
			out = append(out, model.DriftSoftwareChange{On: dateOf(e.Day), Component: e.Component, From: e.From, To: e.To})
		}
	}
	return out
}

// temperature compares the temperature of the path's devices in the
// reference days (ref) with that in the recent days (cur). Only a device read
// on both sides is compared, and the verdict is steady only when every device
// of the path that can have a sensor was.
func temperature(p PathIdent, ref, cur Reading) model.DriftTemperature {
	type device struct {
		id       string
		ref, cur *float64
	}
	devs := []device{{p.RNIC, ref.TempRNIC, cur.TempRNIC}}
	if p.EndpointSensor {
		devs = append(devs, device{p.Endpoint, ref.TempEndpoint, cur.TempEndpoint})
	}
	out := model.DriftTemperature{Status: model.DriftTempSteady}
	missing := ""
	for _, d := range devs {
		switch {
		case d.ref != nil && d.cur != nil:
			out.Devices = append(out.Devices, model.DriftDeviceTemp{Device: d.id, ReferenceC: *d.ref, RecentC: *d.cur})
			if *d.cur-*d.ref >= thermalRiseC {
				out.Status = model.DriftTempRose
			}
		case missing != "":
		case d.ref == nil && d.cur == nil:
			missing = d.id + ": " + model.DriftTempNoSensor
		case d.ref == nil:
			missing = d.id + ": " + model.DriftTempNoReference
		default:
			missing = d.id + ": " + model.DriftTempNoRecent
		}
	}
	if out.Status != model.DriftTempRose && missing != "" {
		out.Status, out.Reason = model.DriftTempUnknown, missing
	}
	return out
}

// shapeOf reads how the signal s moved over the daily values ds, given the
// size of its change against the reference (in the signal's unit). It
// returns the first day at the new level for a step.
func shapeOf(s signal, ds []Day, change float64) (model.DriftShape, int) {
	if len(ds) < minShapeDays {
		return model.DriftShapeUnknown, 0
	}
	// worse holds the daily values with "higher is worse" for every signal.
	worse := make([]float64, len(ds))
	for i, d := range ds {
		worse[i] = s.value(d.Reading)
		if !s.higherIsWorse {
			worse[i] = -worse[i]
		}
	}
	if at, rise := bestStep(worse); rise >= stepShare*change {
		return model.DriftShapeStep, ds[at].Day
	}
	if mannKendallZ(worse) >= trendZ {
		return model.DriftShapeGradual, 0
	}
	return model.DriftShapeSustained, 0
}

// newestDay is the latest day the history holds a reading of; 0 when it
// holds none.
func (f *File) newestDay() int {
	newest := 0
	for _, h := range f.Paths {
		if n := len(h.Days); n > 0 && h.Days[n-1].Day > newest {
			newest = h.Days[n-1].Day
		}
		if h.Open != nil && h.Open.Day > newest {
			newest = h.Open.Day
		}
	}
	return newest
}

// dropAfter removes what the history recorded under a clock that ran ahead
// of day: the daily values of day and later (day itself starts over), open
// days and software events after it, and the whole history of a path whose
// reference reaches past day. It returns how many daily values and
// references went.
func (f *File) dropAfter(day int) (days, refs int) {
	for _, h := range f.Paths {
		if h.Ref != nil && h.Ref.To > day {
			days += len(h.Days)
			refs++
			*h = PathHistory{FirstSeen: min(h.FirstSeen, day), LastSeen: min(h.LastSeen, day)}
			continue
		}
		keep := h.Days[:0]
		for _, d := range h.Days {
			if d.Day < day {
				keep = append(keep, d)
			}
		}
		days += len(h.Days) - len(keep)
		h.Days = keep
		if h.Open != nil && h.Open.Day > day {
			h.Open = nil
			days++
		}
		h.FirstSeen, h.LastSeen = min(h.FirstSeen, day), min(h.LastSeen, day)
		for s, since := range h.Drifted {
			h.Drifted[s] = min(since, day)
		}
	}
	keep := f.Events[:0]
	for _, e := range f.Events {
		if e.Day <= day {
			keep = append(keep, e)
		}
	}
	f.Events = keep
	return days, refs
}

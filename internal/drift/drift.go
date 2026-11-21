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
	// windowDays is how many daily values a path keeps: the shape of a
	// drift and the scatter of the path are read from them. The window is
	// counted in days that have a value, not in calendar days, so that a
	// clock that reads ahead for one run cannot age it out.
	windowDays = 30
	// minShapeDays is the fewest daily values a shape is read from.
	minShapeDays = 8
	// openCap bounds the readings kept of the current day. A day that
	// offers more is thinned to every 2nd, 4th, ... reading.
	openCap   = 16
	maxStride = 1 << 20
	maxSeen   = 1 << 30
	// noiseK scales a path's day-to-day scatter into the least change a
	// signal must show: a path whose daily medians scatter by more than the
	// drift threshold over noiseK needs a larger change than the threshold.
	noiseK = 3
	// trimShare is the share of the largest day-to-day differences left out
	// of the scatter, as its inverse (a fifth), and trimSigmas the mean of
	// the rest for independent normal values, in standard deviations of
	// one.
	trimShare  = 5
	trimSigmas = 0.790
	// minScatterDays is the fewest daily values the scatter of the window
	// is read from.
	minScatterDays = 8
	// upsideQuantile marks a path's best days (its best tenth) and
	// goodQuantile its good days (its best quarter). upsideSigmas is how
	// far the first lies below the mean of a normal distribution, in
	// standard deviations, and goodSigmas how far it lies below the second.
	// upsideFactor is how far beyond what the better side shows the
	// day-to-day scatter still counts as scatter.
	upsideQuantile = 0.10
	goodQuantile   = 0.25
	upsideSigmas   = 1.2816
	goodSigmas     = 0.607
	upsideFactor   = 3
	// stepShare is the share of a signal's change against its reference
	// that the best single step through the window must account for, for
	// the change to be a step. stepSpan is the fewest daily values on
	// either side of a step.
	stepShare = 2.0 / 3
	stepSpan  = 3
	// trendZ is the one-sided 1% point of the normal distribution, which
	// the Mann-Kendall statistic must reach for a trend.
	trendZ = 2.326
	// thermalRiseC is how much hotter, in degrees Celsius, a device must
	// read in the recent days than in the reference days to count as risen.
	thermalRiseC = 5
	// staleDays is on how many days with an observed run the topology may
	// lack a path before its history is dropped. Until then it is kept, in
	// case the device comes back.
	staleDays = 90
	// healDays is how many days a clock must hold before it is trusted over
	// the dates of the history: readings dated after the clock are dropped
	// at once when they span fewer days than this, and otherwise once the
	// clock has stayed behind them for this many days.
	healDays = 2
	// MaxPaths bounds the paths of one history, stale ones included.
	MaxPaths = 2048
	// maxFindings bounds the findings one report carries, and maxChanges
	// the software changes.
	maxFindings = 32
	maxChanges  = 16
	// maxEventsPer bounds the software changes the history keeps of one
	// component (the kernel, hostping, one RNIC's firmware), so that many
	// changes of one cannot push out the last change of another.
	maxEventsPer = 4
	// maxText bounds the bytes of a version string and maxKey those of a
	// path key, a firmware key or a probe setting.
	maxText = 64
	maxKey  = 256
	// maxDay bounds a day number (days since 1970-01-01 UTC), so that day
	// arithmetic cannot overflow whatever the clock or the file says.
	maxDay = 1 << 22
	// minValue and maxValue bound a bandwidth (bit/s) or latency (ns) in
	// the history: no probe reads less than 1 bit/s or 1 ns, and a ratio of
	// two values inside the bounds is a finite number.
	minValue = 1
	maxValue = 1e18
	// maxThreshold bounds a stored threshold: noiseK times a scatter of at
	// most maxValue over a reference of at least minValue.
	maxThreshold = 1e19
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
// refDays daily values.
type Reference struct {
	// From and To are the first and last day behind the reference.
	From int `json:"from"`
	To   int `json:"to"`
	Reading
}

// Verdict is the memory of a signal that is judged drifted.
type Verdict struct {
	// Since is the day the signal was first judged drifted.
	Since int `json:"since"`
	// Threshold is the change it had to reach then. The signal stays
	// drifted by this threshold, not by what the days since make of the
	// path's scatter: a degradation that comes and goes from day to day
	// would otherwise raise the bar it has to clear and report itself as
	// recovered.
	Threshold float64 `json:"threshold"`
}

// PathHistory is everything the history keeps of one path.
type PathHistory struct {
	// Probe is the probe settings the readings were taken with (see
	// Probe). Readings taken with others are not comparable, so a path
	// whose settings changed starts over.
	Probe string `json:"probe"`
	// Absent counts the days with an observed run on which the topology did
	// not have the path; 0 while it has it.
	Absent int        `json:"absent,omitempty"`
	Ref    *Reference `json:"reference,omitempty"`
	// Days are the closed days, oldest first.
	Days []Day    `json:"days,omitempty"`
	Open *OpenDay `json:"open,omitempty"`
	// Drifted holds the verdict of every signal judged drifted.
	Drifted map[model.DriftSignal]Verdict `json:"drifted,omitempty"`
}

// Software is the software a drift may follow a change of: the kernel
// release, the hostping build that takes the readings, and each RNIC's
// firmware version, keyed like the RNIC half of a path key. An empty value
// was not read.
type Software struct {
	Kernel   string            `json:"kernel,omitempty"`
	Hostping string            `json:"hostping,omitempty"`
	Firmware map[string]string `json:"firmware,omitempty"`
}

// Event is one recorded software change.
type Event struct {
	Day int `json:"day"`
	// Component is model.DriftComponentKernel, DriftComponentHostping or
	// DriftComponentFirmware, and Device the RNIC key of a firmware change.
	Component string `json:"component"`
	Device    string `json:"device,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// signal is one tracked reading: where it lives in a Reading, and which
// direction is worse.
type signal struct {
	name  model.DriftSignal
	value func(Reading) float64
	// higherIsWorse is false for bandwidth.
	higherIsWorse bool
}

var signals = []signal{
	{model.DriftBandwidth, func(r Reading) float64 { return r.BandwidthBPS }, false},
	{model.DriftLatencyP50, func(r Reading) float64 { return r.LatP50Ns }, true},
	{model.DriftLatencyP99, func(r Reading) float64 { return r.LatP99Ns }, true},
}

// worse turns a value of s into one for which higher is worse, whatever the
// signal: a bandwidth is negated.
func (s signal) worse(v float64) float64 {
	if s.higherIsWorse {
		return v
	}
	return -v
}

// change is how far v lies on the worse side of ref, as a fraction of ref.
func (s signal) change(ref, v float64) float64 {
	return (s.worse(v) - s.worse(ref)) / ref
}

// PathIdent names one path of the running topology to the tracker.
type PathIdent struct {
	// RNIC and Endpoint are the IDs the report and the metrics carry.
	RNIC     string
	Endpoint string
	// Key identifies the path in the history across restarts and
	// renumbering (see Paths).
	Key string
	// Probe is the probe settings the path is read with (Probe.For).
	Probe string
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
	// or recovered, a software change, paths that started over or expired,
	// a clock that does not agree with the history.
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
}

// New returns a tracker over f for the paths of the running topology. th is
// the drift threshold (--drift-th). The tracker owns f from here on.
// Firmware versions of RNICs the topology no longer has are dropped from it.
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
	if t.clockBehind(day, &res) {
		for _, p := range t.paths {
			res.Paths = append(res.Paths, model.PathDrift{RNIC: p.RNIC, Endpoint: p.Endpoint, Reason: model.DriftClockBehind})
		}
		res.Summary = summarize(res.Paths, nil, nil)
		return res
	}
	// A new day is counted against the paths the topology lacks. The clock
	// alone does not count: one run under a clock that reads months ahead
	// is one day.
	newDay := day != t.f.LastDay
	if gap := day - t.f.LastDay; t.f.LastDay != 0 && gap > recentSpan {
		res.Logs = append(res.Logs, fmt.Sprintf("the clock (%s) is %d days past the last run the history saw (%s): the host was off or busy, or the clock reads ahead; if it does, what is recorded under it is dropped once it is corrected",
			dateOf(day), gap, dateOf(t.f.LastDay)))
	}
	t.f.LastDay = day

	t.noteSoftware(day, sw, &res)
	bySample := make(map[[2]string]Reading, len(samples))
	for _, s := range samples {
		bySample[[2]string{s.RNIC, s.Endpoint}] = s.Reading
	}
	var findings []model.DriftFinding
	restarted := map[string]int{}
	oldest := -1 // the first day of the oldest reference behind a finding
	for _, p := range t.paths {
		h := t.entry(p)
		if h == nil {
			res.Paths = append(res.Paths, model.PathDrift{RNIC: p.RNIC, Endpoint: p.Endpoint, Reason: model.DriftNotTracked})
			continue
		}
		if h.Probe != p.Probe {
			restarted[h.Probe+" -> "+p.Probe]++
			*h = PathHistory{Probe: p.Probe}
		}
		h.Absent = 0
		h.roll(day)
		if r, ok := bySample[[2]string{p.RNIC, p.Endpoint}]; ok {
			h.add(day, r)
		}
		pd, fs, logs := t.judge(p, h, day)
		res.Paths = append(res.Paths, pd)
		findings = append(findings, fs...)
		res.Logs = append(res.Logs, logs...)
		if len(fs) > 0 && (oldest < 0 || h.Ref.From < oldest) {
			oldest = h.Ref.From
		}
	}
	for _, change := range sortedKeys(restarted) {
		res.Logs = append(res.Logs, fmt.Sprintf("%d paths start a new history: their readings were taken with other probe settings (%s) and are not comparable", restarted[change], change))
	}
	if n := t.expire(newDay); n > 0 {
		res.Logs = append(res.Logs, fmt.Sprintf("dropped the history of %d paths the topology has not had on %d days with a run", n, staleDays))
	}
	var changes []model.DriftSoftwareChange
	if oldest >= 0 {
		changes = t.softwareSince(oldest)
	}
	res.Summary = summarize(res.Paths, findings, changes)
	return res
}

// clockBehind reports whether the clock, at day, reads earlier than what
// the history holds and nothing may be recorded under it. It decides which
// of the two is wrong by which has held for longer.
//
// Readings dated after day that span fewer than healDays days were taken
// under a clock that read ahead for a short while (a boot before time
// synchronization): they are dropped at once. More than that is a history,
// and the clock is the suspect (an RTC that lost its battery): nothing is
// recorded until the clock is past it, or has stayed behind it for
// healDays days, when what is dated after it is dropped after all. The day
// it was first seen behind is kept in the history, so that the wait does
// not start over with every restart of the daemon.
func (t *Tracker) clockBehind(day int, res *Result) bool {
	newest := t.f.newestDay()
	if day >= newest {
		t.f.BehindSince = nil
		return false
	}
	reason := fmt.Sprintf("they span fewer than %d days, so the clock read ahead when they were taken", healDays)
	if t.f.daysAfter(day) >= healDays {
		if t.f.BehindSince == nil || day < *t.f.BehindSince {
			since := day
			t.f.BehindSince = &since
			res.Logs = append(res.Logs, fmt.Sprintf("the clock (%s) is behind the newest day of the history (%s): nothing is recorded until it is past it, and after %d days behind the days after it are dropped",
				dateOf(day), dateOf(newest), healDays))
		}
		if day-*t.f.BehindSince < healDays {
			return true
		}
		reason = fmt.Sprintf("the clock has stayed behind them for %d days", healDays)
	}
	days, reset := t.f.dropAfter(day)
	res.Logs = append(res.Logs, fmt.Sprintf("dropped %d daily summaries dated after %s, and the reference of %d paths that reaches past it: %s",
		days, dateOf(day), reset, reason))
	t.f.BehindSince = nil
	return false
}

// sortedKeys is the keys of m in order.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// summarize counts paths per status and unknown paths per reason, puts the
// findings worst first and attaches the software changes. A report holds at
// most maxFindings findings and maxChanges changes (the newest): a change
// that reaches every path at once (a kernel update) would otherwise add a
// finding per path and signal to every report from then on.
func summarize(paths []model.PathDrift, findings []model.DriftFinding, changes []model.DriftSoftwareChange) model.DriftSummary {
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Change > findings[j].Change })
	s := model.DriftSummary{Findings: findings}
	if len(findings) > maxFindings {
		s.Findings, s.FindingsOmitted = findings[:maxFindings], len(findings)-maxFindings
	}
	if len(findings) > 0 {
		s.SoftwareChanges = changes
		if n := len(changes) - maxChanges; n > 0 {
			s.SoftwareChanges, s.SoftwareChangesOmitted = changes[n:], n
		}
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

// entry is the history of the path p, created on first sight. It is nil
// when the history is full of paths the topology still has.
func (t *Tracker) entry(p PathIdent) *PathHistory {
	if h := t.f.Paths[p.Key]; h != nil {
		return h
	}
	if len(t.f.Paths) >= MaxPaths && !t.evictStale() {
		return nil
	}
	h := &PathHistory{Probe: p.Probe}
	t.f.Paths[p.Key] = h
	return h
}

// evictStale drops the path the topology has lacked for the most days (the
// smallest key among equals), and reports whether there was one.
func (t *Tracker) evictStale() bool {
	victim := ""
	for k, h := range t.f.Paths {
		if t.inGraph[k] {
			continue
		}
		if v := t.f.Paths[victim]; victim == "" || h.Absent > v.Absent || (h.Absent == v.Absent && k < victim) {
			victim = k
		}
	}
	if victim == "" {
		return false
	}
	delete(t.f.Paths, victim)
	return true
}

// expire counts a new day against every path the topology lacks, drops
// those it has lacked for more than staleDays such days, and returns how
// many went.
func (t *Tracker) expire(newDay bool) int {
	n := 0
	for k, h := range t.f.Paths {
		if t.inGraph[k] {
			continue
		}
		if newDay && h.Absent < maxDay {
			h.Absent++
		}
		if h.Absent > staleDays {
			delete(t.f.Paths, k)
			n++
		}
	}
	return n
}

// noteSoftware records what changed between the stored software and sw. A
// value that was not read (empty) changes nothing: unknown is not a change.
func (t *Tracker) noteSoftware(day int, sw Software, res *Result) {
	change := func(component, device, from, to string) string {
		to = clip(to)
		if to == "" || to == from {
			return from
		}
		if from != "" {
			t.f.addEvent(Event{Day: day, Component: component, Device: device, From: from, To: to})
			what := component
			if device != "" {
				what += " of " + device
			}
			res.Logs = append(res.Logs, fmt.Sprintf("%s changed from %s to %s", what, from, to))
		}
		return to
	}
	t.f.Software.Kernel = change(model.DriftComponentKernel, "", t.f.Software.Kernel, sw.Kernel)
	t.f.Software.Hostping = change(model.DriftComponentHostping, "", t.f.Software.Hostping, sw.Hostping)
	keys := make([]string, 0, len(sw.Firmware))
	for k := range sw.Firmware {
		if t.rnicKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := change(model.DriftComponentFirmware, k, t.f.Software.Firmware[k], sw.Firmware[k])
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

// addEvent records e and keeps the last maxEventsPer changes of its
// component (of its RNIC, for firmware), so that a host with many RNICs and
// a few firmware updates still holds its last kernel change.
func (f *File) addEvent(e Event) {
	f.Events = append(f.Events, e)
	n := 0
	for _, o := range f.Events {
		if o.Component == e.Component && o.Device == e.Device {
			n++
		}
	}
	for i := 0; n > maxEventsPer && i < len(f.Events); i++ {
		if o := f.Events[i]; o.Component == e.Component && o.Device == e.Device {
			f.Events = append(f.Events[:i], f.Events[i+1:]...)
			n--
			i--
		}
	}
}

// softwareSince is the recorded software changes from day from on, oldest
// first. Firmware changes of several RNICs from one version to another on
// one day are one entry that counts the RNICs.
func (t *Tracker) softwareSince(from int) []model.DriftSoftwareChange {
	var out []model.DriftSoftwareChange
	for _, e := range t.f.Events {
		if e.Day < from {
			continue
		}
		c := model.DriftSoftwareChange{On: dateOf(e.Day), Component: e.Component, From: e.From, To: e.To}
		if e.Component != model.DriftComponentFirmware {
			out = append(out, c)
			continue
		}
		c.Devices = 1
		merged := false
		for i := range out {
			if o := &out[i]; o.Component == c.Component && o.On == c.On && o.From == c.From && o.To == c.To {
				o.Devices++
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].On < out[j].On })
	return out
}

// clip makes s storable: valid UTF-8, one line, at most maxText bytes.
func clip(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, "�"))
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
// value, the oldest daily values beyond windowDays are dropped, and the
// reference is formed once refDays daily values exist.
//
// Nothing is dropped for its age. A clock that reads ahead for one run
// would otherwise age the whole window out, and a host that was off for a
// month would lose the days its next verdict is to be compared with.
func (h *PathHistory) roll(day int) {
	if h.Open != nil && h.Open.Day < day {
		h.Days = append(h.Days, h.Open.close())
		h.Open = nil
	}
	if h.Ref == nil && len(h.Days) >= refDays {
		ref := newReference(h.Days[:refDays])
		h.Ref = &ref
	}
	if n := len(h.Days) - windowDays; n > 0 {
		h.Days = append(h.Days[:0], h.Days[n:]...)
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
// median, and each temperature's median when most of the days have one.
func newReference(ds []Day) Reference {
	return Reference{From: ds[0].Day, To: ds[len(ds)-1].Day, Reading: medianReading(readings(ds), len(ds)/2+1)}
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

// threshold is the change the signal s of the path has to reach to be judged
// drifted: th, or noiseK times the scatter of its window when that is more.
// For a signal that is drifted (proven set), only scatter that shows on the
// better side of the reference counts (see provenScatter).
func (h *PathHistory) threshold(s signal, th float64, proven bool) float64 {
	ref := s.value(h.Ref.Reading)
	xs := make([]float64, len(h.Days))
	for i, d := range h.Days {
		xs[i] = s.worse(s.value(d.Reading))
	}
	noise := scatter(xs, s.worse(ref))
	if proven {
		noise = provenScatter(xs, s.worse(ref))
	}
	return math.Max(th, noiseK*noise/ref)
}

// judge is the path's state at day and the findings of its drifted signals.
// It updates the verdicts in h and returns a log line per signal that
// drifted or stopped being drifted.
//
// A signal becomes drifted when the median of its recent days has moved by
// its threshold. It stops when at most one of the recent days still reads
// worse than the reference by half the threshold it drifted at. The two
// sides differ on purpose. A level that sits near the threshold must not
// change its verdict with every day's noise, and a device that is degraded
// on some days and fine on others must not be reported as recovered on the
// days the fine ones are in the majority.
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
		change := s.change(ref, now)
		v, was := h.Drifted[s.name]
		bar := h.threshold(s, t.th, was)
		if was {
			// The threshold it drifted at. It rises when the operator raises
			// --drift-th past it, or when the path has since shown, on the
			// better side of its reference, that it scatters by more.
			bar = math.Max(bar, v.Threshold)
		}
		pd.Changes = append(pd.Changes, model.DriftChange{Signal: s.name, Change: change, Threshold: bar})
		worseDays := 0
		for _, d := range recent {
			if s.change(ref, s.value(d.Reading)) >= bar/2 {
				worseDays++
			}
		}
		switch {
		case !was && change < bar:
			continue
		case was && worseDays <= 1:
			delete(h.Drifted, s.name)
			how := "recovered"
			if change >= v.Threshold/2 {
				// The path has not moved back: the threshold has moved up.
				how = fmt.Sprintf("is no longer counted as drifted, with the threshold now at %.1f%% (--drift-th was raised, or the path has shown that it scatters)", bar*100)
			}
			logs = append(logs, fmt.Sprintf("%s->%s %s %s: %s against its reference %s (%+.1f%%), drifted since %s",
				p.RNIC, p.Endpoint, s.name, how, formatValue(s, now), formatValue(s, ref), signed(s, change)*100, dateOf(v.Since)))
			continue
		case !was:
			v = Verdict{Since: day, Threshold: bar}
			if h.Drifted == nil {
				h.Drifted = map[model.DriftSignal]Verdict{}
			}
			h.Drifted[s.name] = v
		}
		pd.Status = model.DriftDrifted
		f := model.DriftFinding{
			RNIC: p.RNIC, Endpoint: p.Endpoint, Signal: s.name,
			Change: change, Threshold: bar, WorseDays: worseDays, RecentDays: len(recent),
			Reference: ref, Recent: now,
			ReferenceFrom: dateOf(h.Ref.From), ReferenceTo: dateOf(h.Ref.To),
			RecentFrom: dateOf(recent[0].Day), RecentTo: dateOf(recent[len(recent)-1].Day),
			Since:       dateOf(v.Since),
			Temperature: temperature(p, h.Ref.Reading, cur),
		}
		var stepDay int
		f.Shape, stepDay = shapeOf(s, h.Days, math.Abs(now-ref))
		if f.Shape == model.DriftShapeStep {
			f.StepOn = dateOf(stepDay)
		}
		findings = append(findings, f)
		if !was {
			logs = append(logs, "drifted: "+FormatFinding(f)+formatChanges(t.softwareSince(h.Ref.From)))
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
	fmt.Fprintf(&b, "%s->%s %s %s against its reference %s (%+.1f%%, threshold %.1f%%; reference %s..%s, recent %s..%s, %d of %d recent days worse by half the threshold), shape %s",
		f.RNIC, f.Endpoint, f.Signal, formatValue(s, f.Recent), formatValue(s, f.Reference), signed(s, f.Change)*100, f.Threshold*100,
		f.ReferenceFrom, f.ReferenceTo, f.RecentFrom, f.RecentTo, f.WorseDays, f.RecentDays, f.Shape)
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
	return b.String()
}

// formatChanges spells software changes for the log, after a finding.
func formatChanges(cs []model.DriftSoftwareChange) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "; %s %s %s -> %s", c.On, c.Component, c.From, c.To)
		if c.Devices > 0 {
			fmt.Fprintf(&b, " (%d RNICs)", c.Devices)
		}
	}
	return b.String()
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
	// worse holds the daily values with "higher is worse" for every signal,
	// and times the days they are of.
	worse, times := make([]float64, len(ds)), make([]float64, len(ds))
	for i, d := range ds {
		worse[i], times[i] = s.worse(s.value(d.Reading)), float64(d.Day)
	}
	// A step must carry most of the change and describe the window better
	// than a straight line does. The second test is what keeps a decline
	// that began inside the window from reading as a step in its middle.
	if at, rise, cost, ok := bestStep(worse); ok && rise >= stepShare*change && cost < lineCost(times, worse) {
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

// daysAfter is how many distinct days after day the history holds a reading
// of.
func (f *File) daysAfter(day int) int {
	after := map[int]bool{}
	for _, h := range f.Paths {
		for i := len(h.Days) - 1; i >= 0 && h.Days[i].Day > day; i-- {
			after[h.Days[i].Day] = true
		}
		if h.Open != nil && h.Open.Day > day {
			after[h.Open.Day] = true
		}
	}
	return len(after)
}

// dropAfter removes what the history dates after day: daily values, open
// days and software events, and the whole history of a path whose reference
// reaches past day, since its days cannot be told apart any more. What is
// dated day itself stays. It returns how many daily values and references
// went.
func (f *File) dropAfter(day int) (days, refs int) {
	for _, h := range f.Paths {
		if h.Ref != nil && h.Ref.To > day {
			days += len(h.Days)
			if h.Open != nil {
				days++
			}
			refs++
			*h = PathHistory{Probe: h.Probe, Absent: h.Absent}
			continue
		}
		keep := h.Days[:0]
		for _, d := range h.Days {
			if d.Day <= day {
				keep = append(keep, d)
			}
		}
		days += len(h.Days) - len(keep)
		h.Days = keep
		if h.Open != nil && h.Open.Day > day {
			h.Open = nil
			days++
		}
		for s, v := range h.Drifted {
			v.Since = min(v.Since, day)
			h.Drifted[s] = v
		}
	}
	keep := f.Events[:0]
	for _, e := range f.Events {
		if e.Day <= day {
			keep = append(keep, e)
		}
	}
	f.Events = keep
	f.LastDay = min(f.LastDay, day)
	return days, refs
}

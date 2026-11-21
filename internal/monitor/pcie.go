package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/topology"
)

// Passive PCIe link monitoring (--pcie-interval): what the kernel already
// knows about the host's PCIe links, read without sending a probe.
//
// Two things are read per link. The negotiated speed and width say whether
// the link trained below what both of its ends can do; the analyzer's
// down-train rules judge that, on every sample instead of only when an
// engine run happens to start. The AER statistics of the functions at the
// two ends of the link (pci.ReadLinkErrors) count the errors that report on
// the link itself; this file turns those cumulative counts into a rate per
// link and compares it with what the link's bit error ratio allows.
//
// A counter is evidence only as a difference between two consecutive samples
// of the same function. The first sample of a function, the first one after
// it could not be read, and one whose counters went down (the device was
// removed and enumerated again, which starts its counters from zero) are
// baselines: they yield no difference, and the window they fall in is not
// measured for that function. The totals a function had counted before the
// daemon started are never looked at.

// PCIeSampler is an optional Sampler extension: one passive reading of every
// PCIe link the sampler watches. The monitor calls it at most once per
// --pcie-interval.
type PCIeSampler interface {
	PCIeLinks(ctx context.Context) []PCIeLinkSample
}

// PCIeLinkSample is one passive reading of one PCIe link.
type PCIeLinkSample struct {
	// Link is the topology link ID.
	Link string
	// Device is the RNIC or endpoint ID of a device link (pcie.<device>),
	// "" for the link below a root port or between switches.
	Device string
	// Config is the link as its lower end reads it (pci.ReadTraining), valid
	// when ConfigOK. The negotiated speed and width size the error budget;
	// a device link's is also judged for a down-train.
	Config   model.PCIeConfig
	ConfigOK bool
	// Ends are the functions whose AER statistics stand for the link, the
	// port above it first.
	Ends []PCIeEndSample
}

// PCIeEndSample is the AER link-error counts of one function at an end of a
// link, or why they could not be read.
type PCIeEndSample struct {
	BDF string
	// Port marks the port above the link.
	Port   bool
	Errors pci.LinkErrors
	Err    error
}

// pcieWindow is the least time a link's errors are counted over before they
// are judged: the minute its budget is stated per. A sample interval longer
// than this makes every sample a window.
const pcieWindow = time.Minute

// pcieProbeRefs is how many unloaded RNICs outside the affected subtree a
// pcie_link run also sweeps, when it sweeps an unloaded RNIC: the references
// the paired bandwidth recheck reads a baseline verdict against
// (analyzer.BWRecheckMaxRefs).
const pcieProbeRefs = 2

// pcieMaxJudgedGTs is the fastest link speed whose correctable errors are
// held to a bit-error budget. Up to 32 GT/s a link signals two levels (NRZ)
// and a bit error is rare by specification; from 64 GT/s on it signals four
// (PAM4), bit errors are expected and corrected by a forward error correction
// the link layer builds in, and what the AER counters of such a link count in
// normal operation is not established here. Its rate is not judged.
const pcieMaxJudgedGTs = 32

// PCIeErrorBudget is the link errors per minute one direction of a PCIe link
// may show at bit error ratio ber: ber × bits per second × 60, for the speed
// and width the link trained to. It is 0, no budget, when ber is 0, the
// negotiated link is unread, or the link runs faster than 32 GT/s
// (pcieMaxJudgedGTs). An end's errors are counted by its largest row, and
// every error of one row stands for at least one bit error, so a count over
// the budget is a ratio over ber.
func PCIeErrorBudget(ber float64, c model.PCIeConfig) float64 {
	gts := pci.GTs(c.Speed)
	if !(ber > 0) || gts <= 0 || gts > pcieMaxJudgedGTs || c.Width <= 0 {
		return 0
	}
	return ber * gts * 1e9 * float64(c.Width) * 60
}

// pcieTooFast returns the speed of a link read as c (ok when it read) when it
// is faster than its correctable errors are judged at, "" otherwise.
func pcieTooFast(c model.PCIeConfig, ok bool) string {
	if ok && pci.GTs(c.Speed) > pcieMaxJudgedGTs {
		return c.Speed
	}
	return ""
}

// pcieNoBudget says why a window has no budget at a bit error ratio above 0:
// fast is the speed of a sample of it that was too fast to be judged, "" when
// the negotiated link did not read.
func pcieNoBudget(fast string) string {
	if fast != "" {
		return fmt.Sprintf("trained at %s: a link faster than %d GT/s corrects bit errors as it runs, so its correctable errors are held to no budget and only its uncorrectable ones are judged", fast, pcieMaxJudgedGTs)
	}
	return "negotiated link unread, so its link-error rate has no budget to be judged against"
}

// pcieWatch is the passive state of every watched link across samples.
type pcieWatch struct {
	ber   float64
	logf  func(format string, args ...any)
	links map[string]*pcieLinkTrack
}

func newPCIeWatch(ber float64, logf func(string, ...any)) *pcieWatch {
	return &pcieWatch{ber: ber, logf: logf, links: map[string]*pcieLinkTrack{}}
}

// pcieLinkTrack is one link's state: its ends, the window being counted and
// the evidence the closed windows left.
type pcieLinkTrack struct {
	ends map[string]*pcieEndTrack
	// cfg is the previous sample's link; cfgOK whether it was read.
	cfg   model.PCIeConfig
	cfgOK bool
	// winStart is when the window being counted began, and winBudget the
	// largest budget a sample of it gave: a link that changes speed inside
	// a window, as an idle GPU does, is held to the faster one.
	winStart  time.Time
	winBudget float64
	// winFast is the speed of a sample of the window that was faster than
	// correctable errors are judged at (pcieTooFast), "" without one. Such
	// a window has no budget, whatever its other samples read.
	winFast string
	// recent is the last model.PCIeRecentWindows judged windows, the oldest
	// first. A window that is unknown or has no budget empties it.
	recent []pcieWindowCount
	// hot is set once the errors of recent exceed what the budget allows
	// them by more than chance explains (pcieWindowSummary.beyondChance),
	// and sustained once they also came often enough to be no burst or two
	// (repeated). Both stay set until the errors of recent are within what
	// the budget allows them: a link is not accused, and not cleared, on
	// the noise of a count near its budget.
	hot, sustained bool
	// uncorrectable counts down the measured windows an uncorrectable link
	// error still stands for.
	uncorrectable int
	// finding is the link's standing finding, nil without one.
	finding *model.PCIeLinkFinding
	// unjudged is why the link's rate had no budget when that was last
	// logged, "" while it has one, so each reason is logged once.
	unjudged string
	// sinceLog counts the judged windows since the link's finding was last
	// logged, so a suspect that keeps going over the budget is logged again
	// once per model.PCIeRecentWindows.
	sinceLog int
}

// pcieWindowCount is one judged window of a link: what its worse end
// counted, and what the budget allowed a window of its length.
type pcieWindowCount struct {
	errors  uint64
	allowed float64
	minutes float64
	over    bool
}

// pcieEndTrack is one function's counters across samples.
type pcieEndTrack struct {
	port bool
	// prev is the previous sample's counts, valid when have.
	prev pci.LinkErrors
	have bool
	// since is when the function was last baselined: it has been
	// differenced on every sample after that.
	since time.Time
	// cor and unc are the row deltas of the window being counted.
	cor, unc map[string]uint64
	// read is whether the function has ever been read; unread whether it
	// read before and does not now. Both only shape the log.
	read, unread bool
}

// observe folds one sample taken at now into the state and returns what the
// sample added and what stands after it.
func (w *pcieWatch) observe(now time.Time, samples []PCIeLinkSample) model.PCIePassive {
	out := model.PCIePassive{Sampled: true}
	for _, s := range samples {
		t := w.links[s.Link]
		if t == nil {
			t = &pcieLinkTrack{ends: map[string]*pcieEndTrack{}, winStart: now}
			w.links[s.Link] = t
		}
		// A link is judged for a down-train only on a speed and width that
		// read the same twice: one read can catch it while it retrains.
		if s.Device != "" && s.ConfigOK && t.cfgOK && s.Config.Speed == t.cfg.Speed && s.Config.Width == t.cfg.Width {
			if out.Configs == nil {
				out.Configs = map[string]model.PCIeConfig{}
			}
			out.Configs[s.Device] = s.Config
		}
		t.cfg, t.cfgOK = s.Config, s.ConfigOK
		budget := 0.0
		if s.ConfigOK {
			budget = PCIeErrorBudget(w.ber, s.Config)
		}

		var cor, unc uint64
		measured := false
		for _, e := range s.Ends {
			dc, du, ok := w.difference(now, s.Link, t, e)
			if !ok {
				continue
			}
			measured = true
			cor, unc = cor+dc, unc+du
		}
		if measured {
			out.Errors = append(out.Errors, model.PCIeLinkErrors{Link: s.Link, Correctable: cor, Uncorrectable: unc, BudgetPerMinute: budget})
		}
		t.winBudget = math.Max(t.winBudget, budget)
		fast := pcieTooFast(s.Config, s.ConfigOK)
		if fast != "" {
			t.winFast = fast
		}
		if now.Sub(t.winStart) >= pcieWindow {
			w.closeWindow(now, s, t)
			// This sample ends one window and starts the next.
			t.winBudget, t.winFast = budget, fast
		}
		if t.finding != nil {
			out.Findings = append(out.Findings, *t.finding)
		}
	}
	return out
}

// difference takes end e's counts against its previous sample and adds the
// deltas to the window of its link. It returns the correctable and
// uncorrectable link errors counted since that sample; ok is false when the
// sample is a baseline or could not be read.
func (w *pcieWatch) difference(now time.Time, link string, t *pcieLinkTrack, e PCIeEndSample) (cor, unc uint64, ok bool) {
	et := t.ends[e.BDF]
	if et == nil {
		et = &pcieEndTrack{port: e.Port}
		t.ends[e.BDF] = et
	}
	if e.Err != nil {
		et.have = false
		if !et.unread && et.read {
			et.unread = true
			w.logf("hostping: pcie: %s: AER counters of %s could not be read (%v); its errors are unknown until they read again", link, e.BDF, e.Err)
		}
		return 0, 0, false
	}
	if et.unread {
		et.unread = false
		w.logf("hostping: pcie: %s: AER counters of %s read again; re-baselined", link, e.BDF)
	}
	et.read = true
	prev, had := et.prev, et.have
	et.prev, et.have = e.Errors, true
	if !had {
		et.rebaseline(now)
		return 0, 0, false
	}
	dCor, okCor := rowDeltas(prev.Correctable, e.Errors.Correctable)
	dUnc, okUnc := rowDeltas(prev.Uncorrectable, e.Errors.Uncorrectable)
	if !okCor || !okUnc {
		// A kernel counter only grows while its device exists. One that
		// went down belongs to a device that was removed and enumerated
		// again: the interval says nothing, not even zero.
		et.rebaseline(now)
		w.logf("hostping: pcie: %s: AER counters of %s went down (the device was enumerated again); re-baselined", link, e.BDF)
		return 0, 0, false
	}
	for row, d := range dCor {
		et.cor[row] += d
		cor += d
	}
	for row, d := range dUnc {
		et.unc[row] += d
		unc += d
	}
	return cor, unc, true
}

// rebaseline starts the function's differencing over at now.
func (et *pcieEndTrack) rebaseline(now time.Time) {
	et.since = now
	et.cor, et.unc = map[string]uint64{}, map[string]uint64{}
}

// rowDeltas is how far each row of cur moved from prev, for the rows that
// moved. ok is false when the two samples do not hold the same rows or a row
// went down: they cannot be differenced.
func rowDeltas(prev, cur map[string]uint64) (map[string]uint64, bool) {
	if len(prev) != len(cur) {
		return nil, false
	}
	out := map[string]uint64{}
	for row, v := range cur {
		p, ok := prev[row]
		if !ok || v < p {
			return nil, false
		}
		if v > p {
			out[row] = v - p
		}
	}
	return out, true
}

// closeWindow judges the window of link s that ends at now and starts the
// next one. An end counts for the window only when it was differenced on
// every sample of it; a window no end counts for is unknown.
func (w *pcieWatch) closeWindow(now time.Time, s PCIeLinkSample, t *pcieLinkTrack) {
	elapsed := now.Sub(t.winStart).Seconds()
	budget := t.winBudget
	if t.winFast != "" {
		budget = 0
	}
	var ends []model.PCIeLinkEnd
	var worst uint64
	uncorrectable := false
	for _, e := range s.Ends {
		et := t.ends[e.BDF]
		if et == nil || !et.have {
			continue
		}
		whole := !et.since.After(t.winStart)
		cor, unc := et.cor, et.unc
		et.cor, et.unc = map[string]uint64{}, map[string]uint64{}
		if !whole {
			continue
		}
		// The kernel adds one to every row whose status bit an error report
		// carries, and one bit error can show in more than one (a corrupted
		// TLP as Receiver Error and Bad TLP, a lost ACK as Bad DLLP and a
		// replay timeout). The rows are therefore not added: the largest
		// one is the least number of errors the end saw.
		worst = max(worst, maxRow(cor))
		uncorrectable = uncorrectable || sumRows(unc) > 0
		end := model.PCIeLinkEnd{BDF: e.BDF, Port: e.Port}
		if len(cor) > 0 {
			end.Correctable = cor
		}
		if len(unc) > 0 {
			end.Uncorrectable = unc
		}
		ends = append(ends, end)
	}
	t.winStart = now
	before := model.PCIeLinkState("")
	if t.finding != nil {
		before = t.finding.State
	}
	if len(ends) == 0 {
		// Unknown is not within budget: what the windows before showed ends
		// here, and so does a finding, whose evidence is no longer current.
		t.recent, t.hot, t.sustained, t.uncorrectable, t.finding = nil, false, false, 0, nil
		if before != "" {
			w.logf("hostping: pcie: %s: its AER counters were not read throughout the last window; the %s finding is dropped, not cleared", s.Link, before)
		}
		return
	}

	rate := float64(worst) * 60 / elapsed
	ended := ""
	if budget <= 0 {
		// No negotiated link, one faster than is judged, or --pcie-ber 0:
		// the rate is not judged, so nothing shows the link over its budget
		// now. Like an unknown window, that ends what the windows before
		// showed and a suspect or sustained finding; it is not a window
		// within the budget.
		t.recent, t.hot, t.sustained = nil, false, false
		ended = "its link-error rate can no longer be judged"
		if w.ber > 0 {
			if why := pcieNoBudget(t.winFast); why != t.unjudged {
				t.unjudged = why
				w.logf("hostping: pcie: %s: %s", s.Link, why)
			}
		}
	} else {
		t.unjudged = ""
		allowed := math.Ceil(budget)
		t.recent = append(t.recent, pcieWindowCount{errors: worst, allowed: allowed * elapsed / 60, minutes: elapsed / 60, over: rate > allowed})
		if len(t.recent) > model.PCIeRecentWindows {
			t.recent = t.recent[len(t.recent)-model.PCIeRecentWindows:]
		}
	}
	sum := summarizeWindows(t.recent)
	switch {
	case !sum.exceeds():
		// The windows looked back over hold no more errors than the budget
		// allows them: whatever some of them counted, the link runs within
		// its budget, and a finding ends.
		t.hot, t.sustained = false, false
		if len(t.recent) > 0 {
			ended = fmt.Sprintf("the link errors of its last %d windows are within what the budget allows them", len(t.recent))
		}
	case sum.beyondChance():
		t.hot = true
		if sum.repeated() {
			t.sustained = true
		}
	}
	if uncorrectable {
		t.uncorrectable = model.PCIeSustainWindows
	} else if t.uncorrectable > 0 {
		t.uncorrectable--
	}

	state := model.PCIeLinkState("")
	switch {
	case t.uncorrectable > 0:
		state = model.PCIeLinkUncorrectable
	case t.sustained:
		state = model.PCIeLinkSustained
	case t.hot:
		state = model.PCIeLinkSuspect
	}
	if state == "" {
		t.finding = nil
	} else {
		t.finding = &model.PCIeLinkFinding{
			Link: s.Link, State: state,
			ErrorsPerMinute: rate, BudgetPerMinute: budget,
			RecentWindows: len(t.recent), OverBudgetWindows: sum.over, OverBudgetRuns: sum.runs, OverBudgetLongestRun: sum.longest,
			RecentErrorsPerMinute: sum.rate(), RecentBudgetPerMinute: sum.allowedRate(),
			WindowSeconds: elapsed, Ends: ends,
		}
	}
	if state != before {
		if before == model.PCIeLinkUncorrectable {
			// Whatever became of the rate: this finding stood on the
			// uncorrectable error alone.
			ended = fmt.Sprintf("no uncorrectable link error in the last %d windows", model.PCIeSustainWindows)
		}
		w.logState(s.Link, before, t.finding, ended)
		t.sinceLog = 0
		return
	}
	// A suspect names no cause, so the log is all an operator sees of a
	// link that stays one: bursts too far apart for a verdict, or a rate
	// that hovers at the budget. It is logged again when it goes over the
	// budget once more, at most once per the windows it looks back over.
	t.sinceLog++
	if state == model.PCIeLinkSuspect && t.sinceLog >= model.PCIeRecentWindows && len(t.recent) > 0 && t.recent[len(t.recent)-1].over {
		w.logf("hostping: pcie: %s: %s (%s): still a hint, no verdict", s.Link, state, t.finding.Evidence())
		t.sinceLog = 0
	}
}

// pcieWindowSummary is what the judged windows a link is looked back over
// amount to.
type pcieWindowSummary struct {
	// over is how many of them were over the budget; runs how many separate
	// times that was, windows in a row counting as one; longest the most
	// windows in a row.
	over, runs, longest int
	// errors is what the worse end counted across them, allowed what the
	// budget allowed them, and minutes how long they were.
	errors  uint64
	allowed float64
	minutes float64
}

func summarizeWindows(recent []pcieWindowCount) pcieWindowSummary {
	var sum pcieWindowSummary
	run := 0
	for _, c := range recent {
		sum.errors += c.errors
		sum.allowed += c.allowed
		sum.minutes += c.minutes
		if !c.over {
			run = 0
			continue
		}
		if run == 0 {
			sum.runs++
		}
		run++
		sum.over++
		sum.longest = max(sum.longest, run)
	}
	return sum
}

// pcieExcessSigmas is how far the errors of the windows looked back over must
// exceed what the budget allows them before a finding begins, in standard
// deviations of that allowance. Bit errors arrive at random, so a link that
// runs at its budget counts a Poisson number around the allowance, with the
// square root of the allowance as its standard deviation, and is over it in
// about half of all stretches. At three standard deviations a link at or
// below its budget begins a finding about once in a thousand independent
// looks, however short the stretch looked at: a few windows need a much
// larger relative excess than thirty.
const pcieExcessSigmas = 3

// rate is the errors per minute across the windows, and allowedRate what the
// budget allows them per minute: the rounded-up budgets of the windows,
// weighted by their length. Both are 0 without a window.
func (sum pcieWindowSummary) rate() float64 {
	if sum.minutes <= 0 {
		return 0
	}
	return float64(sum.errors) / sum.minutes
}

func (sum pcieWindowSummary) allowedRate() float64 {
	if sum.minutes <= 0 {
		return 0
	}
	return sum.allowed / sum.minutes
}

// exceeds reports whether the windows together hold more errors than the
// budget allows them. A finding stands only while they do, and its evidence
// prints the two rates compared here, so the two cannot disagree.
func (sum pcieWindowSummary) exceeds() bool {
	return sum.rate() > sum.allowedRate()
}

// beyondChance reports whether the windows hold more errors than a link
// running at its budget would count by chance (pcieExcessSigmas). A finding
// begins only then.
func (sum pcieWindowSummary) beyondChance() bool {
	return sum.exceeds() && float64(sum.errors) > sum.allowed+pcieExcessSigmas*math.Sqrt(sum.allowed)
}

// repeated reports whether the windows over the budget are more than a burst
// or two. One burst of errors, however large, is over the budget in two
// windows in a row at most, so it takes model.PCIeSustainWindows windows in a
// row, a link that errs steadily, or as many separate times over the budget,
// a link that errs in bursts.
func (sum pcieWindowSummary) repeated() bool {
	return sum.longest >= model.PCIeSustainWindows || sum.runs >= model.PCIeSustainWindows
}

// logState logs one change of a link's finding; ended says why one that
// stood before is gone.
func (w *pcieWatch) logState(link string, before model.PCIeLinkState, f *model.PCIeLinkFinding, ended string) {
	if f == nil {
		w.logf("hostping: pcie: %s: %s; the %s finding ends", link, ended, before)
		return
	}
	verdict := "a hint, no verdict yet"
	if f.State.Proven() {
		verdict = "reported as " + model.CausePCIeLinkErrors
	}
	w.logf("hostping: pcie: %s: %s (%s): %s", link, f.State, f.Evidence(), verdict)
}

func sumRows(m map[string]uint64) uint64 {
	var n uint64
	for _, v := range m {
		n += v
	}
	return n
}

func maxRow(m map[string]uint64) uint64 {
	var n uint64
	for _, v := range m {
		n = max(n, v)
	}
	return n
}

// pollPCIe takes a passive sample when one is due at now, the poll's time,
// and returns the passive reading this poll hands on: the state after the
// last sample, with what the samples since the last delivered poll added. It
// is nil when the passive reading is off, the sampler has none, or the poll
// was canceled.
func (m *Monitor) pollPCIe(ctx context.Context, now time.Time) *model.PCIePassive {
	ps, ok := m.Sampler.(PCIeSampler)
	if !ok || m.Cfg.PCIeInterval <= 0 || ctx.Err() != nil {
		return nil
	}
	if m.pcie == nil {
		m.pcie = newPCIeWatch(m.Cfg.PCIeBER, m.logf)
	}
	if m.pcieLast.IsZero() || now.Sub(m.pcieLast) >= m.Cfg.PCIeInterval {
		// The counters are differenced over the time between their own
		// reads, which the RNIC reads of this poll came before.
		at := m.Now()
		samples := ps.PCIeLinks(ctx)
		if ctx.Err() != nil {
			// A read cut short is no reading: nothing is differenced.
			return nil
		}
		m.pcieLast = now
		got := m.pcie.observe(at, samples)
		m.pciePending = mergePCIeErrors(m.pciePending, got.Errors)
		m.pcieState = got
		m.pcieSampled = true
	}
	out := m.pcieState
	out.Sampled, out.Errors = m.pcieSampled, m.pciePending
	return &out
}

// deliveredPCIe marks the passive reading of the poll that just returned as
// handed on, so its error deltas are not handed on again.
func (m *Monitor) deliveredPCIe() {
	m.pciePending, m.pcieSampled = nil, false
}

// failedPoll is the Decision of a poll that returns an error: the passive
// reading and nothing else. A canceled poll hands on nothing and keeps what
// the samples added for the next one.
func (m *Monitor) failedPoll(ctx context.Context, passive *model.PCIePassive) Decision {
	if passive == nil || ctx.Err() != nil {
		return Decision{}
	}
	m.deliveredPCIe()
	return Decision{PCIe: passive}
}

// mergePCIeErrors adds the per-link deltas of add to pending, in link order
// of first appearance.
func mergePCIeErrors(pending, add []model.PCIeLinkErrors) []model.PCIeLinkErrors {
	for _, a := range add {
		found := false
		for i := range pending {
			if pending[i].Link == a.Link {
				pending[i].Correctable += a.Correctable
				pending[i].Uncorrectable += a.Uncorrectable
				pending[i].BudgetPerMinute = a.BudgetPerMinute
				found = true
				break
			}
		}
		if !found {
			pending = append(pending, a)
		}
	}
	return pending
}

// pcieProbePlan is the links a pcie_link run would answer and the RNICs it
// would sweep for each.
type pcieProbePlan struct {
	links []string
	rnics map[string][]string
}

// planPCIeProbe maps the links PCIeNeed names to the RNICs whose busy-mode
// probe set crosses them. A link no RNIC probes in busy mode (a GPU that is
// affinitive to none) cannot be answered by a busy run and is left out.
func (m *Monitor) planPCIeProbe() pcieProbePlan {
	plan := pcieProbePlan{rnics: map[string][]string{}}
	if m.PCIeNeed == nil || m.Cfg.PCIeProbeInterval <= 0 {
		return plan
	}
	for _, link := range m.PCIeNeed() {
		if _, dup := plan.rnics[link]; dup {
			continue
		}
		if rnics := m.Graph.RNICsProbing(link, model.ModeBusy); len(rnics) > 0 {
			plan.links = append(plan.links, link)
			plan.rnics[link] = rnics
		}
	}
	return plan
}

// sweep is the set of RNICs a pcie_link run for links sweeps: every RNIC
// whose probes cross one of them, plus, when one of those is not loaded, up
// to pcieProbeRefs other unloaded RNICs in graph order. An unloaded RNIC is
// judged against the idle baseline, and that verdict is only confirmed by
// the paired bandwidth recheck, which reads it next to the same class on
// other unloaded RNICs; without them in the sweep the recheck has nothing to
// read against.
func (p pcieProbePlan) sweep(g topology.Graph, links []string, busy map[string]bool) map[string]bool {
	out := map[string]bool{}
	needRefs := false
	for _, link := range links {
		for _, id := range p.rnics[link] {
			out[id] = true
			needRefs = needRefs || !busy[id]
		}
	}
	if !needRefs {
		return out
	}
	refs := 0
	for _, n := range g.RNICs {
		if refs == pcieProbeRefs {
			break
		}
		if !out[n.ID] && !busy[n.ID] {
			out[n.ID] = true
			refs++
		}
	}
	return out
}

// PCIeLinkSpec is one PCIe link the live sampler reads passively: where its
// negotiated link is read and which functions' AER statistics stand for it.
type PCIeLinkSpec struct {
	// Link is the topology link ID; Device the RNIC or endpoint ID of a
	// device link.
	Link, Device string
	// Lower is the function at the lower end of the link, whose link
	// attributes describe it: the device, or the upstream port of a switch.
	Lower string
	// Ends are the functions with AER statistics at the two ends: the port
	// above the link, then the functions of the device below it.
	Ends []PCIeEndSpec
}

// PCIeEndSpec is one function whose AER statistics stand for a link.
type PCIeEndSpec struct {
	BDF  string
	Port bool
}

// PCIeLinkSpecs lists the PCIe links of g that can be read passively under
// sysroot (normally /sys), and what could not be: one note per function
// without AER statistics and per ancestry whose links cannot be told.
//
// Every RNIC and GPU gives its own link (pcie.<device>): the port it hangs
// off and the functions of its device. The links above it on its ancestry
// (pci.AncestryLinks) are filed under the topology link of their lower end:
// the root-port link rp.<bdf> when its lower end is a switch upstream port,
// and that switch's sw.<bdf> for a link between two switches. A link that
// joins a root port and the device itself is the device's link only, so one
// physical link is never reported under two IDs by one device. The functions
// of one device share its link, so two RNICs, or an RNIC and a GPU, that are
// functions of one device each name it.
//
// A function without AER statistics is not an end: its link is still read
// for the negotiated link, and counted from the other end when that has
// them.
func PCIeLinkSpecs(sysroot string, g topology.Graph) (specs []PCIeLinkSpec, notes []string) {
	seen := map[string]bool{}
	noted := map[string]bool{}
	note := func(format string, args ...any) {
		n := fmt.Sprintf(format, args...)
		if !noted[n] {
			noted[n] = true
			notes = append(notes, n)
		}
	}
	ends := func(link, port, lower string) []PCIeEndSpec {
		var out []PCIeEndSpec
		var absent []string
		add := func(bdf string, isPort bool) {
			_, err := pci.ReadLinkErrors(sysroot, bdf)
			if errors.Is(err, pci.ErrAERAbsent) {
				absent = append(absent, bdf)
				return
			}
			if err != nil {
				// Kept as an end and read again on every sample: unknown
				// until it reads, and said so here because the first failure
				// of a function that never read is not logged again.
				note("%s: AER statistics at %s do not read yet: %v", link, bdf, err)
			}
			out = append(out, PCIeEndSpec{BDF: bdf, Port: isPort})
		}
		if port != "" {
			add(port, true)
		}
		for _, fn := range pci.DeviceFunctions(sysroot, lower) {
			add(fn, false)
		}
		if len(absent) > 0 {
			// One line per link: a card with virtual functions has many
			// functions without the capability.
			note("%s: no AER statistics at %s (no AER capability, or a kernel without them)", link, strings.Join(absent, ", "))
		}
		return out
	}
	device := func(id, bdf string) {
		bdf = pci.CanonicalBDF(bdf)
		if bdf == "" {
			return
		}
		link := model.PCIeLinkID(id)
		if _, ok := g.Link(link); !ok || seen[link] {
			return
		}
		seen[link] = true
		specs = append(specs, PCIeLinkSpec{Link: link, Device: id, Lower: bdf, Ends: ends(link, pci.LinkPort(sysroot, bdf), bdf)})
		chain, err := pci.AncestryLinks(sysroot, bdf)
		if err != nil {
			note("%s: the links above it are not read: %v", link, err)
			return
		}
		for i, l := range chain[:len(chain)-1] {
			up := "sw." + l.Lower
			if i == 0 {
				up = "rp." + l.Port
			}
			if _, ok := g.Link(up); !ok || seen[up] {
				continue
			}
			seen[up] = true
			if e := ends(up, l.Port, l.Lower); len(e) > 0 {
				specs = append(specs, PCIeLinkSpec{Link: up, Lower: l.Lower, Ends: e})
			}
		}
	}
	for _, n := range g.RNICs {
		device(n.ID, n.PCIeBDF)
	}
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			device(e.ID, e.PCIeBDF)
		}
	}
	sort.SliceStable(notes, func(i, j int) bool { return notes[i] < notes[j] })
	return specs, notes
}

// PCIeLinks reads every link of s.PCIe once: its negotiated link and the AER
// link-error counts of its ends.
func (s SysfsSampler) PCIeLinks(ctx context.Context) []PCIeLinkSample {
	root := s.sysroot()
	out := make([]PCIeLinkSample, 0, len(s.PCIe))
	for _, spec := range s.PCIe {
		if ctx.Err() != nil {
			return out
		}
		smp := PCIeLinkSample{Link: spec.Link, Device: spec.Device}
		if c, err := pci.ReadTraining(root, spec.Lower); err == nil {
			smp.Config, smp.ConfigOK = c, true
		}
		for _, e := range spec.Ends {
			le, err := pci.ReadLinkErrors(root, e.BDF)
			smp.Ends = append(smp.Ends, PCIeEndSample{BDF: e.BDF, Port: e.Port, Errors: le, Err: err})
		}
		out = append(out, smp)
	}
	return out
}

package agent

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// passiveWatch turns the hardware monitor's passive PCIe reading
// (--pcie-interval, monitor.Decision.PCIe) into reports that need no probe.
//
// An engine run already reports what the passive reading shows: it reads
// config space as it starts and is handed the standing link-error findings.
// But on a host that stays loaded without PFC pauses or drops no run starts
// for hours, and the verdict would wait for the host to go idle. So the
// causes the passive reading proves on its own (analyzer.PassiveCauses) are
// also reported between runs, in a report with trigger and mode "passive"
// that holds no path and no link state:
//
//   - at once when a cause appears that no report has named within the last
//     --idle-interval, and
//   - again once --idle-interval has passed since a report last named a
//     standing cause, whether that was a passive report or an engine run's.
//     That is the pace at which an idle host repeats a standing cause, and it
//     is what lets a rule that wants a code in two reports see a standing
//     passive cause on a host that never goes idle.
//
// So every standing passive cause is named by some report at least once per
// --idle-interval, and a passive report is only written when no engine run
// did it.
//
// An uncorrectable link error is an event, not a condition: its finding
// stands for a few windows and may be gone before the cause is due again,
// and one report alone raises no alert that wants a code in two. Such a
// cause is therefore kept after its finding ended until passiveEventReports
// reports have named it, and is named in the next passive report that is
// due. A link that trained below its capability, or whose error rate ended,
// is not repeated once the passive reading no longer shows it: that would
// state a condition that is over.
type passiveWatch struct {
	g     topology.Graph
	every time.Duration
	logf  func(format string, args ...any)

	// latest is the monitor's last passive reading, nil before the first.
	latest *model.PCIePassive
	// causes is what latest proves on its own.
	causes []model.RootCause
	// owed is the causes of an uncorrectable link error whose finding ended
	// before passiveEventReports reports had named them, by passiveKey, as
	// they stood last.
	owed map[string]model.RootCause
	// events counts, for each cause in causes or owed that stands or stood
	// on an uncorrectable link error, by passiveKey, the reports that have
	// named it since it appeared.
	events map[string]int
	// named is when a report last named each passive cause, by passiveKey.
	// Like owed and events it holds at most one entry per PCIe link and
	// code.
	named map[string]time.Time
}

func newPassiveWatch(cfg config.Config, g topology.Graph, logf func(string, ...any)) *passiveWatch {
	if cfg.PCIeInterval <= 0 || cfg.Once {
		return nil
	}
	// Never faster than the minute a link's errors are judged over: an
	// --idle-interval of 0 must not turn a standing cause into a report per
	// poll.
	return &passiveWatch{g: g, every: max(cfg.IdleInterval, passiveMinPace), logf: logf,
		owed: map[string]model.RootCause{}, events: map[string]int{}, named: map[string]time.Time{}}
}

// passiveMinPace is the shortest time between two reports of one standing
// passive cause, whatever --idle-interval says.
const passiveMinPace = time.Minute

// passiveEventReports is how many reports name an uncorrectable link error
// at the least: as many as the packaged alerting rules want of a code before
// they raise an alert (contrib/prometheus/hostping-rules.yml).
const passiveEventReports = 2

// owedEvidence ends the evidence of a cause that is named after its finding
// ended.
const owedEvidence = "; no uncorrectable link error was counted since and the finding has ended: it is repeated because every uncorrectable link error is named by two reports"

// passiveKey is the identity of a passive cause: its code and the link it
// names. The confidence and the endpoints of a cause differ between a passive
// report and an engine run's, which may have measured a symptom.
func passiveKey(c model.RootCause) string {
	return c.Code + "\x00" + strings.Join(c.Links, ",")
}

// observe takes the passive reading of one poll. The causes only change on
// the poll that took a new sample.
func (p *passiveWatch) observe(pcie *model.PCIePassive) {
	if p == nil || pcie == nil {
		return
	}
	p.latest = pcie
	if !pcie.Sampled {
		return
	}
	before := p.causes
	p.causes = analyzer.PassiveCauses(p.g, *pcie)
	uncorrectable := map[string]bool{}
	for _, f := range pcie.Findings {
		uncorrectable[f.Link] = f.State == model.PCIeLinkUncorrectable
	}
	stands := map[string]bool{}
	for _, c := range p.causes {
		k := passiveKey(c)
		stands[k] = true
		// A cause that stands again is no longer owed: the reports it had
		// still count.
		delete(p.owed, k)
		if _, known := p.events[k]; !known && c.Code == model.CausePCIeLinkErrors && len(c.Links) == 1 && uncorrectable[c.Links[0]] {
			p.events[k] = 0
		}
	}
	for _, c := range before {
		k := passiveKey(c)
		if stands[k] {
			continue
		}
		link := strings.Join(c.Links, ",")
		if n, event := p.events[k]; event && n < passiveEventReports {
			c.Evidence += owedEvidence
			p.owed[k] = c
			p.logf("hostping: pcie: %s: the finding ended after %d of the %d reports that name every uncorrectable link error; the cause stays in the passive reports until the rest are written", link, n, passiveEventReports)
			continue
		}
		delete(p.events, k)
		if c.Code != model.CausePCIeLinkErrors {
			// The monitor logs the end of a link-error finding itself.
			p.logf("hostping: pcie: %s: no longer reads as trained below its capability; the passive %s finding ends", link, c.Code)
		}
	}
}

// findings is the standing link-error findings an engine run is handed.
func (p *passiveWatch) findings() []model.PCIeLinkFinding {
	if p == nil || p.latest == nil {
		return nil
	}
	return p.latest.Findings
}

// need names the PCIe links whose passive finding asks for a probe
// (monitor.Monitor.PCIeNeed): the link of every cause the passive reading
// proves, and of every suspect.
func (p *passiveWatch) need() []string {
	if p == nil || p.latest == nil {
		return nil
	}
	var out []string
	add := func(link string) {
		if !slices.Contains(out, link) {
			out = append(out, link)
		}
	}
	for _, c := range p.causes {
		for _, l := range c.Links {
			add(l)
		}
	}
	for _, f := range p.latest.Findings {
		add(f.Link)
	}
	return out
}

// reportable is the causes a passive report names now: what the passive
// reading proves, then the causes still owed a report, by key.
func (p *passiveWatch) reportable() []model.RootCause {
	out := slices.Clone(p.causes)
	for _, k := range slices.Sorted(maps.Keys(p.owed)) {
		out = append(out, p.owed[k])
	}
	return out
}

// due returns the passive report to make at now, if one is due: a cause
// stands, or is owed a report, that no report has named within the last
// --idle-interval. The report names every such cause, not only that one.
func (p *passiveWatch) due(now time.Time, hostIdle bool) (model.Report, bool) {
	if p == nil {
		return model.Report{}, false
	}
	causes := p.reportable()
	stale := false
	for _, c := range causes {
		if at, ok := p.named[passiveKey(c)]; !ok || now.Sub(at) >= p.every {
			stale = true
			break
		}
	}
	if !stale {
		return model.Report{}, false
	}
	host, _ := os.Hostname()
	return model.Report{
		Trigger:   model.TriggerPassive,
		Mode:      model.ModePassive,
		Host:      host,
		Started:   now,
		Paths:     []model.PathMeasurement{},
		Links:     []model.LinkState{},
		Causes:    causes,
		HostIdle:  hostIdle,
		PCIeLinks: p.findings(),
	}, true
}

// passiveCodes are the cause codes a passive report can hold.
var passiveCodes = []string{model.CauseRNICPCIeFailure, model.CauseGPUPCIeFailure, model.CausePCIeLinkErrors}

// emitted records that rep, a passive report or an engine run's, was written
// at now: every cause in it that a passive report could hold counts as named
// then. An engine run that names a link under the same code, from the same
// config space or the findings it was handed, puts the passive report of
// that cause off by one --idle-interval; one that does not, does not. A
// cause that was owed a report is dropped once enough have named it.
func (p *passiveWatch) emitted(now time.Time, rep model.Report) {
	if p == nil {
		return
	}
	for _, c := range rep.Causes {
		if !slices.Contains(passiveCodes, c.Code) || len(c.Links) == 0 {
			continue
		}
		k := passiveKey(c)
		p.named[k] = now
		n, event := p.events[k]
		if !event {
			continue
		}
		p.events[k] = n + 1
		if _, owed := p.owed[k]; owed && n+1 >= passiveEventReports {
			delete(p.owed, k)
			delete(p.events, k)
		}
	}
}

// livePCIeLinks is the PCIe links the live sampler reads passively, or nil
// when --pcie-interval is off. It says what it reads and what it cannot.
func livePCIeLinks(g topology.Graph, cfg config.Config, logf func(string, ...any)) []monitor.PCIeLinkSpec {
	if cfg.PCIeInterval <= 0 || cfg.Once {
		return nil
	}
	specs, notes := monitor.PCIeLinkSpecs("/sys", g)
	for _, n := range notes {
		logf("hostping: pcie: %s", n)
	}
	counted := 0
	for _, s := range specs {
		if len(s.Ends) > 0 {
			counted++
		}
	}
	logf("hostping: passive PCIe reading every %v: %d links, %d of them with AER counters (link errors beyond a bit error ratio of %g are reported as %s)",
		cfg.PCIeInterval, len(specs), counted, cfg.PCIeBER, model.CausePCIeLinkErrors)
	return specs
}

// simSlotSpeed and simSlotWidth are the slots of the simulated host: Fig.2
// puts every device behind a Gen4 x16 hop.
const (
	simSlotSpeed = "16.0 GT/s PCIe"
	simSlotWidth = 16
)

// simPCIeLinks is the simulated host's passive PCIe reading: every RNIC and
// GPU link as the world's PCIe configuration has it, with the AER counters
// the aer fault advances. now is the clock the counters advance on. The
// world states a device's own maximum only; the port maximum the down-train
// rule also needs is the simulated slot's.
func simPCIeLinks(w *sim.World, g topology.Graph, now func() time.Time) func(context.Context) []monitor.PCIeLinkSample {
	start := now()
	return func(context.Context) []monitor.PCIeLinkSample {
		elapsed := now().Sub(start)
		cfgs := w.PCIeConfigs()
		var out []monitor.PCIeLinkSample
		add := func(id, bdf string) {
			link := model.PCIeLinkID(id)
			s := monitor.PCIeLinkSample{Link: link, Device: id}
			s.Config, s.ConfigOK = cfgs[id]
			if s.ConfigOK && s.Config.PortMaxSpeed == "" && s.Config.PortMaxWidth == 0 {
				s.Config.PortMaxSpeed, s.Config.PortMaxWidth = simSlotSpeed, simSlotWidth
			}
			s.Ends = []monitor.PCIeEndSample{{BDF: bdf, Errors: pci.LinkErrors{
				Correctable: map[string]uint64{
					pci.AERRxErr: 0, pci.AERBadTLP: w.AERErrors(link, elapsed), pci.AERBadDLLP: 0, pci.AERRollover: 0, pci.AERTimeout: 0,
				},
				Uncorrectable: map[string]uint64{pci.AERDLP: 0, pci.AERSDES: 0},
			}}}
			out = append(out, s)
		}
		for _, n := range g.RNICs {
			add(n.ID, n.PCIeBDF)
		}
		for _, e := range g.Endpoints {
			if e.Kind == model.EndpointGPU {
				add(e.ID, e.PCIeBDF)
			}
		}
		return out
	}
}

package agent

import (
	"context"
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
//   - at once when a cause appears that the last report of them did not
//     name, and
//   - again once --idle-interval has passed since a report last named all of
//     them, whether that was a passive report or an engine run's. That is the
//     pace at which an idle host repeats a standing cause, and it is what
//     lets a rule that wants a code in two reports see a standing passive
//     cause on a host that never goes idle.
//
// A cause that goes away is logged, not reported.
type passiveWatch struct {
	g     topology.Graph
	every time.Duration
	logf  func(format string, args ...any)

	// latest is the monitor's last passive reading, nil before the first.
	latest *model.PCIePassive
	// causes is what latest proves on its own.
	causes []model.RootCause
	// reported is the keys of the causes the last report of them named, and
	// at when that report was made.
	reported map[string]bool
	at       time.Time
}

func newPassiveWatch(cfg config.Config, g topology.Graph, logf func(string, ...any)) *passiveWatch {
	if cfg.PCIeInterval <= 0 || cfg.Once {
		return nil
	}
	return &passiveWatch{g: g, every: cfg.IdleInterval, logf: logf, reported: map[string]bool{}}
}

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
	before := len(p.causes)
	p.causes = analyzer.PassiveCauses(p.g, *pcie)
	if before > 0 && len(p.causes) == 0 {
		p.logf("hostping: passive PCIe findings ended: no link is down-trained or counting link errors beyond its budget now")
		p.reported = map[string]bool{}
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

// due returns the passive report to make at now, if one is due.
func (p *passiveWatch) due(now time.Time, hostIdle bool) (model.Report, bool) {
	if p == nil || len(p.causes) == 0 {
		return model.Report{}, false
	}
	fresh := false
	for _, c := range p.causes {
		if !p.reported[passiveKey(c)] {
			fresh = true
			break
		}
	}
	if !fresh && now.Sub(p.at) < p.every {
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
		Causes:    slices.Clone(p.causes),
		HostIdle:  hostIdle,
		PCIeLinks: p.findings(),
	}, true
}

// emitted records that rep, a passive report or an engine run's, was written
// at now. It counts as a report of the standing passive causes only when it
// names every one of them: an engine run whose config-space reading differs,
// or that started before a finding stood, does not put the next passive
// report off.
func (p *passiveWatch) emitted(now time.Time, rep model.Report) {
	if p == nil || len(p.causes) == 0 {
		return
	}
	named := map[string]bool{}
	for _, c := range rep.Causes {
		named[passiveKey(c)] = true
	}
	for _, c := range p.causes {
		if !named[passiveKey(c)] {
			return
		}
	}
	p.reported = map[string]bool{}
	for _, c := range p.causes {
		p.reported[passiveKey(c)] = true
	}
	p.at = now
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

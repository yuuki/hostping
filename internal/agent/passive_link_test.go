package agent

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/telemetry"
	"github.com/yuuki/hostping/internal/topology"
)

// The functions of the scripted link pcie.rnic0: a root port and the RNIC
// below it.
const (
	linkPort = "0000:16:00.0"
	linkDev  = "0000:17:00.0"
)

// linkDaemon is the daemon loop over the Fig.2 host with a scripted passive
// reading of one Gen4 x16 link, pcie.rnic0, whose RNIC can leave the bus.
// The host is loaded on every RNIC, or idle. The test steps the clock the
// whole loop reads.
type linkDaemon struct {
	*passiveDaemon
	// port is the cumulative AER counts of the root port. The RNIC's own
	// stay at zero while it is on the bus.
	port pci.LinkErrors
	// gone takes rnic0 off the bus: its RDMA counters, its link attributes
	// and its AER statistics cannot be read.
	gone bool
}

func zeroLinkErrors() pci.LinkErrors {
	return pci.LinkErrors{
		Correctable:   map[string]uint64{pci.AERRxErr: 0, pci.AERBadTLP: 0, pci.AERBadDLLP: 0, pci.AERRollover: 0, pci.AERTimeout: 0},
		Uncorrectable: map[string]uint64{pci.AERDLP: 0, pci.AERSDES: 0},
	}
}

func cloneLinkErrors(e pci.LinkErrors) pci.LinkErrors {
	out := zeroLinkErrors()
	for k, v := range e.Correctable {
		out.Correctable[k] = v
	}
	for k, v := range e.Uncorrectable {
		out.Uncorrectable[k] = v
	}
	return out
}

// goneRNIC is the static sampler with an RNIC that can leave the bus: its
// counters then fail to read, as the live sampler's do for a device whose
// RDMA device and netdev are gone.
type goneRNIC struct {
	monitor.StaticSampler
	gone *bool
}

func (s goneRNIC) RNIC(ctx context.Context, n model.RNIC) (model.RNICCounters, error) {
	if *s.gone && n.ID == "rnic0" {
		return model.RNICCounters{}, fmt.Errorf("no RNIC counters readable for %s", n.Device)
	}
	return s.StaticSampler.RNIC(ctx, n)
}

func newLinkDaemon(t *testing.T, cfg config.Config, loaded bool) *linkDaemon {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	ld := &linkDaemon{port: zeroLinkErrors()}
	pd := &passiveDaemon{t: t, clk: time.Unix(1_700_000_000, 0), counters: map[string]model.RNICCounters{}, world: w}
	ld.passiveDaemon = pd
	now := func() time.Time { return pd.clk }
	logf := func(f string, a ...any) { pd.logs = append(pd.logs, fmt.Sprintf(f, a...)) }
	gpus := idleGPUs(g, nil)
	if loaded {
		pd.busy = []string{"rnic0", "rnic1", "rnic2", "rnic3"}
		for i := range gpus {
			gpus[i].Idle, gpus[i].Util = false, 90
		}
	}
	link := model.PCIeConfig{BDF: linkDev, Speed: simSlotSpeed, Width: simSlotWidth, MaxSpeed: simSlotSpeed, MaxWidth: simSlotWidth, PortMaxSpeed: simSlotSpeed, PortMaxWidth: simSlotWidth}
	static := monitor.StaticSampler{Counters: pd.counters, GPU: gpus, PCIe: func(context.Context) []monitor.PCIeLinkSample {
		s := monitor.PCIeLinkSample{Link: "pcie.rnic0", Device: "rnic0", Config: link, ConfigOK: !ld.gone}
		s.Ends = []monitor.PCIeEndSample{{BDF: linkPort, Port: true, Errors: cloneLinkErrors(ld.port)}}
		if ld.gone {
			s.Ends = append(s.Ends, monitor.PCIeEndSample{BDF: linkDev, Err: fmt.Errorf("%w: %s: gone", pci.ErrAERAbsent, linkDev)})
		} else {
			s.Ends = append(s.Ends, monitor.PCIeEndSample{BDF: linkDev, Errors: zeroLinkErrors()})
		}
		return []monitor.PCIeLinkSample{s}
	}}
	s := goneRNIC{StaticSampler: static, gone: &ld.gone}
	mon := monitor.New(cfg, g, s)
	mon.Logf = logf
	mon.Now = now
	eng := engine.New(cfg, g, sim.Prober{World: w})
	eng.Logf = logf
	pd.reader = sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(pd.reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tel, err := telemetry.New(provider)
	if err != nil {
		t.Fatal(err)
	}
	pd.out = bytes.Buffer{}
	pd.daemon = &daemon{
		rig:     &rig{g: g, eng: eng, world: w, sampler: s, mon: mon, logf: logf},
		tel:     tel,
		live:    telemetry.NewLiveness(cfg.MetricInterval, now),
		rp:      &reporter{format: config.ReportFormatJSON, stdout: &pd.out, now: now, logf: logf},
		passive: newPassiveWatch(cfg, g, logf),
		now:     now,
	}
	mon.PCIeNeed = pd.passive.need
	return ld
}

// run steps the loop through d, one second a tick, and returns the reports
// written meanwhile with the second each was written in.
func (ld *linkDaemon) run(d time.Duration) []timedReport {
	ld.t.Helper()
	var out []timedReport
	for i := 0; i < int(d/time.Second); i++ {
		ld.step()
		for _, rep := range ld.reports() {
			out = append(out, timedReport{Report: rep, at: ld.clk})
		}
	}
	return out
}

type timedReport struct {
	model.Report
	at time.Time
}

// logged returns the log lines that contain sub.
func (pd *passiveDaemon) logged(sub string) []string {
	var out []string
	for _, l := range pd.logs {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// An RNIC's link goes down by surprise: its root port counts a Surprise Down
// and the RNIC leaves the bus, so every monitor poll fails on its counters
// from then on. That is when the passive reading matters most, and it does
// not stand on the RNIC: the verdict is reported while the polls fail, and a
// second time like every uncorrectable link error.
func TestDaemonReportsASurpriseDownWhileThePollsFail(t *testing.T) {
	ld := newLinkDaemon(t, passiveConfig(), true)
	if got := ld.run(2 * time.Minute); len(got) != 0 {
		t.Fatalf("reports %+v on a quiet link", got)
	}
	ld.port.Uncorrectable[pci.AERSDES]++
	ld.gone = true
	got := ld.run(90 * time.Second)
	if len(ld.logged("monitor: no RNIC counters readable")) < 80 {
		t.Fatalf("the polls did not fail: %q", ld.logs)
	}
	if len(got) != 1 || got[0].Trigger != model.TriggerPassive {
		t.Fatalf("reports %+v within 90 s of the Surprise Down, want the passive report", got)
	}
	c, ok := causeOf(got[0].Report, model.CausePCIeLinkErrors)
	if !ok || !slices.Equal(c.Links, []string{"pcie.rnic0"}) || !strings.Contains(c.Evidence, linkPort+" (port): uncorrectable SDES=1") {
		t.Fatalf("causes %+v, want pcie_link_errors on pcie.rnic0 with the port's Surprise Down", got[0].Causes)
	}
	if got[0].HostIdle {
		t.Fatal("the report of a poll that failed calls the host idle")
	}
	ms := ld.metrics()
	if n, _ := ms.sum("hostping.diagnosis.count", "code", model.CausePCIeLinkErrors, "confidence", "medium"); n != 1 {
		t.Fatalf("hostping.diagnosis.count{pcie_link_errors} = %d, want 1", n)
	}
	if n, _ := ms.sum("hostping.pcie.link.errors", "link", "pcie.rnic0", "severity", telemetry.PCIeUncorrectable); n != 1 {
		t.Fatalf("hostping.pcie.link.errors{pcie.rnic0,uncorrectable} = %d, want the 1 the port counted", n)
	}

	// The RNIC stays away. The finding ends three windows later, and the
	// error is still named a second time.
	later := ld.run(20 * time.Minute)
	if len(later) != 1 || later[0].at.Sub(got[0].at) != passiveConfig().IdleInterval {
		t.Fatalf("reports %+v in the next 20 minutes of failing polls, want the second one --idle-interval after the first", later)
	}
	if c, ok := causeOf(later[0].Report, model.CausePCIeLinkErrors); !ok || !strings.HasSuffix(c.Evidence, owedEvidence) || len(later[0].PCIeLinks) != 0 {
		t.Fatalf("second report %+v, want the error named once more after its finding ended", later[0].Report)
	}
	if n, _ := ld.metrics().sum("hostping.diagnosis.count", "code", model.CausePCIeLinkErrors); n != 2 {
		t.Fatalf("hostping.diagnosis.count{pcie_link_errors} = %d, want 2", n)
	}
	if lines := ld.logged("the uncorrectable finding ends"); len(lines) != 1 || !strings.Contains(lines[0], "no uncorrectable link error in the last 3 windows") {
		t.Fatalf("end of the finding logged as %q", lines)
	}
}

// A link that errs only now and then, as one that errs under traffic does:
// its port counts 1200 Bad TLPs, 75 times the budget, in one minute of every
// three and nothing in between, so it is never over the budget three windows
// in a row. The third burst makes the verdict, and it is reported like any
// standing cause from then on, without coming and going with the bursts.
func TestDaemonReportsALinkThatErrsInBursts(t *testing.T) {
	ld := newLinkDaemon(t, passiveConfig(), true)
	ld.run(time.Minute)
	start := ld.clk
	var got []timedReport
	for minute := 0; minute < 60; minute++ {
		for s := 0; s < 60; s++ {
			if minute%3 == 0 && s%5 == 0 {
				ld.port.Correctable[pci.AERBadTLP] += 100
			}
			got = append(got, ld.run(time.Second)...)
		}
	}
	if len(got) == 0 {
		t.Fatalf("no report in an hour of bursts; log %q", ld.logged("pcie:"))
	}
	// Each burst can fall across two windows, so the third is over by the
	// ninth minute at the latest.
	if first := got[0].at.Sub(start); first < 6*time.Minute || first > 9*time.Minute {
		t.Fatalf("first report after %v, want it with the third burst", first)
	}
	for i, rep := range got {
		c, ok := causeOf(rep.Report, model.CausePCIeLinkErrors)
		if rep.Trigger != model.TriggerPassive || !ok || len(rep.Causes) != 1 || c.Confidence != model.ConfidenceMedium {
			t.Fatalf("report %d: %+v, want a passive report of pcie_link_errors [medium]", i, rep.Report)
		}
		if len(rep.PCIeLinks) != 1 || rep.PCIeLinks[0].State != model.PCIeLinkSustained || rep.PCIeLinks[0].OverBudgetLongestRun > 2 {
			t.Fatalf("report %d: findings %+v, want pcie.rnic0 sustained on windows that were never three in a row", i, rep.PCIeLinks)
		}
		if i > 0 {
			if gap := rep.at.Sub(got[i-1].at); gap != passiveConfig().IdleInterval {
				t.Fatalf("report %d comes %v after the one before, want one per --idle-interval", i, gap)
			}
		}
	}
	if len(got) < 10 {
		t.Fatalf("%d reports in an hour, want one per --idle-interval from the verdict on", len(got))
	}
	if !strings.Contains(got[0].Causes[0].Evidence, "separate times, at most ") {
		t.Fatalf("evidence %q does not say what the verdict stands on", got[0].Causes[0].Evidence)
	}
	if n, _ := ld.metrics().sum("hostping.diagnosis.count", "code", model.CausePCIeLinkErrors); n != int64(len(got)) {
		t.Fatalf("hostping.diagnosis.count{pcie_link_errors} = %d for %d reports", n, len(got))
	}
	if lines := ld.logged("pcie: pcie.rnic0"); len(lines) != 2 || !strings.Contains(lines[0], "suspect") || !strings.Contains(lines[1], "sustained") {
		t.Fatalf("log of the link %q, want the hint and the verdict, once each", lines)
	}
}

// One uncorrectable link error on a loaded host is named by two reports,
// --idle-interval apart, which is what the packaged alerting rules need to
// open a ticket (contrib/prometheus/hostping-rules.test.yml holds the same
// two increments). The finding itself stands three windows, less than the
// default --idle-interval, and how long a window is must not decide whether
// the second report comes.
func TestDaemonNamesOneUncorrectableErrorInTwoReports(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pcieInterval time.Duration
	}{
		{"default flags and a 10 s passive reading", 10 * time.Second},
		{"windows of three minutes", 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.PCIeInterval = tc.pcieInterval
			ld := newLinkDaemon(t, cfg, true)
			ld.run(2 * tc.pcieInterval)
			ld.port.Uncorrectable[pci.AERSDES]++
			got := ld.run(40 * time.Minute)
			if len(got) != 2 {
				t.Fatalf("%d reports in 40 minutes, want 2; log %q", len(got), ld.logged("pcie:"))
			}
			for i, rep := range got {
				c, ok := causeOf(rep.Report, model.CausePCIeLinkErrors)
				if rep.Trigger != model.TriggerPassive || !ok || len(rep.Causes) != 1 || c.Confidence != model.ConfidenceMedium || c.Summary != "PCIe link reported an uncorrectable link error" {
					t.Fatalf("report %d: %+v", i, rep.Report)
				}
			}
			if gap := got[1].at.Sub(got[0].at); gap != cfg.IdleInterval {
				t.Fatalf("second report %v after the first, want --idle-interval (%v)", gap, cfg.IdleInterval)
			}
			if n, _ := ld.metrics().sum("hostping.diagnosis.count", "code", model.CausePCIeLinkErrors, "confidence", "medium"); n != 2 {
				t.Fatalf("hostping.diagnosis.count{pcie_link_errors,medium} = %d, want 2", n)
			}
		})
	}
}

// The passive report is put off while an idle-mode report is held: the run
// behind that report was handed the same findings, so the report names the
// same cause once the next clean poll confirms it, and a passive report a
// poll ahead of it would say the cause twice.
func TestDaemonHoldsThePassiveReportBehindAnIdleReport(t *testing.T) {
	cfg := passiveConfig()
	cfg.PCIeInterval = time.Second
	// The second idle run starts on the poll that closes the first window,
	// the one the Surprise Down is counted in.
	cfg.IdleInterval = 59 * time.Second
	ld := newLinkDaemon(t, cfg, false)
	first := ld.run(30 * time.Second)
	if len(first) != 1 || first[0].Trigger != model.TriggerIdle || len(first[0].Causes) != 0 {
		t.Fatalf("reports %+v in the first 30 s of a healthy idle host, want its clean idle report", first)
	}
	ld.port.Uncorrectable[pci.AERSDES]++
	got := ld.run(45 * time.Second)
	if len(got) != 1 {
		t.Fatalf("%d reports around the second idle run, want the idle report alone: %+v", len(got), got)
	}
	c, ok := causeOf(got[0].Report, model.CausePCIeLinkErrors)
	if got[0].Trigger != model.TriggerIdle || !ok || !strings.HasSuffix(c.Evidence, "the same run names no other cause at the link (counters only)") {
		t.Fatalf("report %+v, want the idle report with the uncorrectable link error it was handed", got[0].Report)
	}
}

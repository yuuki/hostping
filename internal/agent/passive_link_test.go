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

// linkDaemon is the daemon loop over the Fig.2 host, loaded on every RNIC,
// with a scripted passive reading of one Gen4 x16 link, pcie.rnic0, whose
// RNIC can leave the bus. The test steps the clock the whole loop reads.
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

func newLinkDaemon(t *testing.T, cfg config.Config) *linkDaemon {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	ld := &linkDaemon{port: zeroLinkErrors()}
	pd := &passiveDaemon{t: t, clk: time.Unix(1_700_000_000, 0), counters: map[string]model.RNICCounters{},
		busy: []string{"rnic0", "rnic1", "rnic2", "rnic3"}, world: w}
	ld.passiveDaemon = pd
	now := func() time.Time { return pd.clk }
	logf := func(f string, a ...any) { pd.logs = append(pd.logs, fmt.Sprintf(f, a...)) }
	gpus := idleGPUs(g, nil)
	for i := range gpus {
		gpus[i].Idle, gpus[i].Util = false, 90
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
// not stand on the RNIC: the verdict is reported while the polls fail.
func TestDaemonReportsASurpriseDownWhileThePollsFail(t *testing.T) {
	ld := newLinkDaemon(t, passiveConfig())
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
}

// A link that errs only now and then, as one that errs under traffic does:
// its port counts 1200 Bad TLPs, 75 times the budget, in one minute of every
// three and nothing in between, so it is never over the budget three windows
// in a row. The third burst makes the verdict, and it is reported like any
// standing cause from then on, without coming and going with the bursts.
func TestDaemonReportsALinkThatErrsInBursts(t *testing.T) {
	ld := newLinkDaemon(t, passiveConfig())
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

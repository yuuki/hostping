package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/telemetry"
	"github.com/yuuki/hostping/internal/topology"
)

// passiveDaemon is the daemon loop over the simulated Fig.2 host with the
// passive PCIe reading on, on a clock the test steps: the monitor, the
// simulated AER counters and the passive report pace all read it.
type passiveDaemon struct {
	*daemon
	t        *testing.T
	clk      time.Time
	counters map[string]model.RNICCounters
	// busy is the RNICs that carry 90 % of line rate on every step.
	busy   []string
	tx     uint64
	out    bytes.Buffer
	logs   []string
	reader *sdkmetric.ManualReader
	world  *sim.World
}

func newPassiveDaemon(t *testing.T, cfg config.Config, fault string, busy ...string) *passiveDaemon {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault(fault); err != nil {
		t.Fatal(err)
	}
	pd := &passiveDaemon{t: t, clk: time.Unix(1_700_000_000, 0), counters: map[string]model.RNICCounters{}, busy: busy, world: w}
	now := func() time.Time { return pd.clk }
	logf := func(f string, a ...any) { pd.logs = append(pd.logs, fmt.Sprintf(f, a...)) }

	gpus := idleGPUs(g, nil)
	if len(busy) > 0 {
		// A loaded host: its GPUs compute.
		for i := range gpus {
			gpus[i].Idle, gpus[i].Util = false, 90
		}
	}
	s := monitor.StaticSampler{Counters: pd.counters, GPU: gpus, PCIe: simPCIeLinks(w, g, now)}
	mon := monitor.New(cfg, g, s)
	mon.Logf = logf
	mon.SetClock(now)
	eng := engine.New(cfg, g, sim.Prober{World: w})
	eng.Logf = logf

	pd.reader = sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(pd.reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tel, err := telemetry.New(provider)
	if err != nil {
		t.Fatal(err)
	}
	pd.daemon = &daemon{
		rig:     &rig{g: g, eng: eng, world: w, sampler: s, mon: mon, logf: logf},
		tel:     tel,
		live:    telemetry.NewLiveness(cfg.MetricInterval, now),
		rp:      &reporter{format: config.ReportFormatJSON, stdout: &pd.out, now: now, logf: logf},
		passive: newPassiveWatch(cfg, g, logf),
		now:     now,
	}
	mon.PCIeNeed = pd.passive.need
	return pd
}

// step advances the clock by one second of load on the busy RNICs and runs
// one tick of the loop.
func (pd *passiveDaemon) step() {
	pd.t.Helper()
	pd.clk = pd.clk.Add(time.Second)
	pd.tx += uint64(0.9 * 200e9 / 8)
	for _, id := range pd.busy {
		pd.counters[id] = model.RNICCounters{TxBytes: pd.tx}
	}
	if !pd.tick(context.Background()) {
		pd.t.Fatal("tick stopped the loop without a shutdown")
	}
}

// reports decodes the reports written since the last call.
func (pd *passiveDaemon) reports() []model.Report {
	pd.t.Helper()
	var out []model.Report
	for _, line := range strings.Split(strings.TrimSpace(pd.out.String()), "\n") {
		if line == "" {
			continue
		}
		// The statuses marshal as words and have no decoder, so only what
		// the tests read is taken from the line.
		var raw struct {
			Trigger model.Trigger `json:"trigger"`
			Mode    model.RunMode `json:"mode"`
			Paths   []struct {
				RNIC     string `json:"rnic"`
				Endpoint string `json:"endpoint"`
			} `json:"paths"`
			Causes []model.RootCause `json:"causes"`
			Links  []struct {
				ID string `json:"id"`
			} `json:"links"`
			HostIdle  bool                    `json:"host_idle"`
			PCIeLinks []model.PCIeLinkFinding `json:"pcie_links"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			pd.t.Fatalf("report of %d bytes does not decode: %v", len(line), err)
		}
		rep := model.Report{Trigger: raw.Trigger, Mode: raw.Mode, Causes: raw.Causes, HostIdle: raw.HostIdle, PCIeLinks: raw.PCIeLinks}
		for _, m := range raw.Paths {
			rep.Paths = append(rep.Paths, model.PathMeasurement{RNIC: m.RNIC, Endpoint: m.Endpoint})
		}
		for _, l := range raw.Links {
			rep.Links = append(rep.Links, model.LinkState{ID: l.ID})
		}
		// The raw line shows the shape the consumers of a passive report
		// rely on.
		if raw.Trigger == model.TriggerPassive && (!strings.Contains(line, `"paths":[]`) || !strings.Contains(line, `"links":[]`)) {
			pd.t.Fatalf("passive report does not spell its paths and links as empty lists: %.200s", line)
		}
		out = append(out, rep)
	}
	pd.out.Reset()
	return out
}

func (pd *passiveDaemon) metrics() metrics {
	pd.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := pd.reader.Collect(context.Background(), &rm); err != nil {
		pd.t.Fatal(err)
	}
	out := metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// sum is a counter's value over the series whose attributes include want
// (key=value pairs); ok is false when no series does.
func (ms metrics) sum(name string, want ...string) (total int64, ok bool) {
	m, found := ms[name]
	if !found {
		return 0, false
	}
	for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
		match := true
		for i := 0; i+1 < len(want); i += 2 {
			if v, has := dp.Attributes.Value(attribute.Key(want[i])); !has || v.AsString() != want[i+1] {
				match = false
			}
		}
		if match {
			total += dp.Value
			ok = true
		}
	}
	return total, ok
}

func passiveConfig() config.Config {
	cfg := config.Defaults()
	cfg.IdleInterval = 5 * time.Minute
	cfg.PCIeInterval = 10 * time.Second
	return cfg
}

// The case the passive reading exists for: a host that stays loaded without
// PFC pauses or drops starts no run, and a GPU link there begins to count
// link errors far over its budget. No probe is sent; the verdict is reported
// when the counters prove it, and again every --idle-interval while it
// stands.
func TestDaemonPassiveReportOnALoadedHost(t *testing.T) {
	pd := newPassiveDaemon(t, passiveConfig(), "aer:pcie.gpu3", "rnic0", "rnic1", "rnic2", "rnic3")

	// Two windows over the budget: a suspect, logged, not reported.
	for i := 0; i < 150; i++ {
		pd.step()
	}
	if got := pd.reports(); len(got) != 0 {
		t.Fatalf("reports %+v before the third window, want none: a suspect is a hint", got)
	}
	if !slices.ContainsFunc(pd.logs, func(l string) bool { return strings.Contains(l, "pcie.gpu3: suspect") }) {
		t.Fatalf("the suspect was not logged: %q", pd.logs)
	}

	// The third window closes at the sample 180 s after the first.
	for i := 0; i < 40; i++ {
		pd.step()
	}
	got := pd.reports()
	if len(got) != 1 {
		t.Fatalf("%d reports after the third window, want the one passive report", len(got))
	}
	rep := got[0]
	if rep.Trigger != model.TriggerPassive || rep.Mode != model.ModePassive || len(rep.Paths) != 0 || len(rep.Links) != 0 || rep.HostIdle {
		t.Fatalf("report %+v, want a passive one with no path and no link state on a host that is not idle", rep)
	}
	if len(rep.Causes) != 1 {
		t.Fatalf("causes %+v, want pcie_link_errors only", rep.Causes)
	}
	c := rep.Causes[0]
	if c.Code != model.CausePCIeLinkErrors || c.Confidence != model.ConfidenceMedium || !slices.Equal(c.Links, []string{"pcie.gpu3"}) || !slices.Equal(c.Endpoints, []string{"gpu3"}) {
		t.Fatalf("cause %+v, want pcie_link_errors [medium] on pcie.gpu3", c)
	}
	if !strings.Contains(c.Evidence, "1000 link errors/min against a budget of 16/min, over it in 3 consecutive windows") || !strings.HasSuffix(c.Evidence, "read without a probe (counters only)") {
		t.Fatalf("evidence %q", c.Evidence)
	}
	if len(rep.PCIeLinks) != 1 || rep.PCIeLinks[0].State != model.PCIeLinkSustained {
		t.Fatalf("report findings %+v", rep.PCIeLinks)
	}
	if !slices.ContainsFunc(pd.logs, func(l string) bool {
		return strings.HasPrefix(l, "hostping diagnosis [medium] pcie_link_errors")
	}) {
		t.Fatalf("no diagnosis log line: %q", pd.logs)
	}

	// It is a verdict without an engine run: the diagnosis is counted, and
	// nothing that describes a run is written.
	ms := pd.metrics()
	if n, _ := ms.sum("hostping.diagnosis.count", "code", model.CausePCIeLinkErrors, "confidence", "medium"); n != 1 {
		t.Fatalf("hostping.diagnosis.count{pcie_link_errors,medium} = %d, want 1", n)
	}
	if n, ok := ms.sum("hostping.engine.run.count"); ok {
		t.Fatalf("hostping.engine.run.count = %d for a report no run is behind", n)
	}
	for _, name := range []string{"hostping.path.status_code", "hostping.link.status_code", "hostping.host.idle", "hostping.engine.last_success_timestamp", "hostping.engine.last_run_timestamp"} {
		if _, ok := ms[name]; ok {
			t.Fatalf("%s written by a passive report", name)
		}
	}
	// The counters behind it: 1000 a minute on pcie.gpu3 since the first
	// sample, none on a quiet link, which still has its series at 0.
	if n, _ := ms.sum("hostping.pcie.link.errors", "link", "pcie.gpu3", "severity", telemetry.PCIeCorrectable); n < 3000 || n > 3400 {
		t.Fatalf("hostping.pcie.link.errors{pcie.gpu3} = %d, want about 3000 after three minutes", n)
	}
	if n, ok := ms.sum("hostping.pcie.link.errors", "link", "pcie.gpu0", "severity", telemetry.PCIeCorrectable); !ok || n != 0 {
		t.Fatalf("hostping.pcie.link.errors{pcie.gpu0} = %d (exported %v), want a series at 0", n, ok)
	}
	if n, ok := ms.sum("hostping.pcie.link.errors", "link", "pcie.gpu3", "severity", telemetry.PCIeUncorrectable); !ok || n != 0 {
		t.Fatalf("uncorrectable errors on pcie.gpu3 = %d (exported %v), want a series at 0", n, ok)
	}

	// While it stands it is reported once per --idle-interval, the pace at
	// which an idle host would repeat it, so a rule that wants a code in two
	// reports can see it on a host that never goes idle.
	for i := 0; i < 290; i++ {
		pd.step()
	}
	if got := pd.reports(); len(got) != 0 {
		t.Fatalf("%d more reports within one idle interval of the first, want none", len(got))
	}
	for i := 0; i < 20; i++ {
		pd.step()
	}
	if got := pd.reports(); len(got) != 1 || got[0].Trigger != model.TriggerPassive {
		t.Fatalf("reports %+v one idle interval after the first, want the passive report again", got)
	}
	if n, _ := pd.metrics().sum("hostping.diagnosis.count", "code", model.CausePCIeLinkErrors); n != 2 {
		t.Fatalf("hostping.diagnosis.count{pcie_link_errors} = %d, want 2", n)
	}
}

// A link error rate within the budget is no finding however long it lasts:
// nothing is reported and nothing is logged about the link.
func TestDaemonPassiveWithinBudgetReportsNothing(t *testing.T) {
	pd := newPassiveDaemon(t, passiveConfig(), "aer:pcie.gpu3=12", "rnic0")
	for i := 0; i < 900; i++ {
		pd.step()
	}
	if got := pd.reports(); len(got) != 0 {
		t.Fatalf("reports %+v for 12 link errors a minute on a link allowed 16", got)
	}
	if slices.ContainsFunc(pd.logs, func(l string) bool { return strings.Contains(l, "pcie.gpu3") }) {
		t.Fatalf("pcie.gpu3 logged: %q", pd.logs)
	}
	if n, _ := pd.metrics().sum("hostping.pcie.link.errors", "link", "pcie.gpu3", "severity", telemetry.PCIeCorrectable); n < 170 || n > 180 {
		t.Fatalf("hostping.pcie.link.errors{pcie.gpu3} = %d, want the errors counted all the same (about 178)", n)
	}
}

// A link that trained below its capability is reported once it read the same
// on two passive samples, by the down-train rule an engine run applies and
// at the confidence that rule gives config space alone.
func TestDaemonPassiveDownTrainOnALoadedHost(t *testing.T) {
	pd := newPassiveDaemon(t, passiveConfig(), "rnic_pcie:rnic2", "rnic0")
	for i := 0; i < 9; i++ {
		pd.step()
	}
	if got := pd.reports(); len(got) != 0 {
		t.Fatalf("reports %+v after one passive sample, want none: one read can catch a link retraining", got)
	}
	for i := 0; i < 5; i++ {
		pd.step()
	}
	got := pd.reports()
	if len(got) != 1 || got[0].Trigger != model.TriggerPassive || len(got[0].Causes) != 1 {
		t.Fatalf("reports %+v after two samples, want one passive report with one cause", got)
	}
	c := got[0].Causes[0]
	if c.Code != model.CauseRNICPCIeFailure || c.Confidence != model.ConfidenceMedium || !slices.Equal(c.RNICs, []string{"rnic2"}) {
		t.Fatalf("cause %+v, want rnic_pcie_failure [medium] on rnic2", c)
	}
	if want := "PCIe 16.0 GT/s PCIe x4 (max 16.0 GT/s PCIe x16, port max 16.0 GT/s PCIe x16) trained below its width capability; 63 Gb/s cannot carry the 200 Gb/s line rate; read without a probe on two consecutive samples (config space only)"; c.Evidence != want {
		t.Fatalf("evidence %q, want %q", c.Evidence, want)
	}
	if len(got[0].PCIeLinks) != 0 {
		t.Fatalf("report findings %+v for a link with no link errors", got[0].PCIeLinks)
	}
}

// On an idle host the idle run reads the same config space and is handed the
// same findings, so its report names the passive causes and no passive
// report repeats them inside the interval.
func TestDaemonPassiveCausesRideTheIdleRun(t *testing.T) {
	cfg := passiveConfig()
	pd := newPassiveDaemon(t, cfg, "aer:pcie.gpu3")
	var all []model.Report
	for i := 0; i < 600; i++ {
		pd.step()
		all = append(all, pd.reports()...)
	}
	var passive, runs, runsNaming int
	for _, rep := range all {
		named := slices.ContainsFunc(rep.Causes, func(c model.RootCause) bool { return c.Code == model.CausePCIeLinkErrors })
		switch rep.Trigger {
		case model.TriggerPassive:
			passive++
		case model.TriggerIdle:
			runs++
			if named {
				runsNaming++
				if len(rep.PCIeLinks) != 1 || len(rep.Paths) == 0 {
					t.Fatalf("idle report %+v, want its paths and the finding", rep)
				}
			}
		default:
			t.Fatalf("report with trigger %q on an idle host", rep.Trigger)
		}
	}
	// Idle runs start at t=1 s, 5 min and 10 min; the finding is proven from
	// t=3 min on, so the second run is the first to name it. The passive
	// report at t=3 min is the only one: the run two minutes later counts as
	// the next report of the cause.
	if runs < 2 || runsNaming < 1 {
		t.Fatalf("%d idle reports, %d naming pcie_link_errors; want at least 2 and 1", runs, runsNaming)
	}
	if passive != 1 {
		t.Fatalf("%d passive reports, want 1: the idle runs carry the cause from then on", passive)
	}
}

// With --pcie-probe-interval the suspect alone starts a run on a loaded
// host, of the one RNIC whose busy probes cross the link.
func TestDaemonPCIeLinkRunOnALoadedHost(t *testing.T) {
	cfg := passiveConfig()
	cfg.PCIeProbeInterval = 10 * time.Minute
	pd := newPassiveDaemon(t, cfg, "aer:pcie.gpu3;gpu_pcie:gpu3", "rnic0", "rnic1")
	// The passive reading names gpu3's narrow link after two samples; the
	// pcie_link run follows on the next ready poll.
	var all []model.Report
	for i := 0; i < 30; i++ {
		pd.step()
		all = append(all, pd.reports()...)
	}
	var run *model.Report
	for i := range all {
		if all[i].Trigger == model.TriggerPCIeLink {
			if run != nil {
				t.Fatalf("two pcie_link runs within %v: %+v", cfg.PCIeProbeInterval, all)
			}
			run = &all[i]
		}
	}
	if run == nil {
		t.Fatalf("no pcie_link run; reports %+v", all)
	}
	if run.Mode != model.ModeBusy {
		t.Fatalf("pcie_link run in mode %q, want busy", run.Mode)
	}
	for _, m := range run.Paths {
		if m.RNIC != "rnic1" {
			t.Fatalf("the run probed %s->%s; only rnic1's busy probes cross pcie.gpu3", m.RNIC, m.Endpoint)
		}
	}
	if len(run.Paths) != 4 {
		t.Fatalf("%d paths, want rnic1's busy probe set of 4", len(run.Paths))
	}
	// The run measured the bottleneck the config space named: high.
	c, ok := causeOf(*run, model.CauseGPUPCIeFailure)
	if !ok || c.Confidence != model.ConfidenceHigh || !slices.Equal(c.Endpoints, []string{"gpu3"}) {
		t.Fatalf("causes %+v, want gpu_pcie_failure [high] on gpu3", run.Causes)
	}

	ms := pd.metrics()
	if n, _ := ms.sum("hostping.engine.run.count", "trigger", string(model.TriggerPCIeLink), "mode", string(model.ModeBusy)); n != 1 {
		t.Fatalf("hostping.engine.run.count{pcie_link,busy} = %d, want 1", n)
	}
	// Only the links the run tested have a status series: the rest keep
	// whatever the last run that tested them left, here nothing.
	links := map[string]bool{}
	for _, dp := range ms["hostping.link.status_code"].Data.(metricdata.Gauge[int64]).DataPoints {
		v, _ := dp.Attributes.Value("link")
		links[v.AsString()] = true
	}
	if !links["pcie.gpu3"] || !links["pcie.rnic1"] {
		t.Fatalf("link status series %v lack the hops of the abnormal path rnic1->gpu3", links)
	}
	for _, id := range []string{"pcie.rnic0", "pcie.gpu0", "pcie.gpu7", "rp.3", "mem.0", "upi.0"} {
		if links[id] {
			t.Fatalf("link status written for %s, which the run did not test", id)
		}
	}
}

func causeOf(rep model.Report, code string) (model.RootCause, bool) {
	for _, c := range rep.Causes {
		if c.Code == code {
			return c, true
		}
	}
	return model.RootCause{}, false
}

// passiveWatch on its own: when a report is due, and which reports count.
func TestPassiveWatchPace(t *testing.T) {
	g := topology.Fig2()
	cfg := passiveConfig()
	var logs []string
	p := newPassiveWatch(cfg, g, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	t0 := time.Unix(1_700_000_000, 0)
	finding := func(link string, state model.PCIeLinkState) model.PCIeLinkFinding {
		return model.PCIeLinkFinding{Link: link, State: state, ErrorsPerMinute: 100, BudgetPerMinute: 15.36, OverBudgetWindows: 3, WindowSeconds: 60}
	}
	sample := func(fs ...model.PCIeLinkFinding) {
		p.observe(&model.PCIePassive{Sampled: true, Findings: fs})
	}

	if _, ok := p.due(t0, false); ok {
		t.Fatal("a report is due before any reading")
	}
	sample(finding("pcie.gpu1", model.PCIeLinkSuspect))
	if _, ok := p.due(t0, false); ok {
		t.Fatal("a report is due for a suspect")
	}
	if got := p.need(); !slices.Equal(got, []string{"pcie.gpu1"}) {
		t.Fatalf("need %v, want the suspect's link: a hint may start a probe", got)
	}

	sample(finding("pcie.gpu1", model.PCIeLinkSustained))
	rep, ok := p.due(t0, true)
	if !ok || len(rep.Causes) != 1 || rep.Trigger != model.TriggerPassive || !rep.HostIdle || rep.Paths == nil || rep.Links == nil {
		t.Fatalf("report %+v (due %v), want the passive report at once", rep, ok)
	}
	p.emitted(t0, rep)
	// A poll between samples repeats the state and changes nothing.
	p.observe(&model.PCIePassive{Findings: []model.PCIeLinkFinding{finding("pcie.gpu1", model.PCIeLinkSustained)}})
	if _, ok := p.due(t0.Add(cfg.IdleInterval-time.Second), false); ok {
		t.Fatal("a second report inside the idle interval")
	}
	if _, ok := p.due(t0.Add(cfg.IdleInterval), false); !ok {
		t.Fatal("no report one idle interval after the last")
	}

	// An engine run that names the cause counts as its report; one that
	// does not, does not.
	t1 := t0.Add(4 * time.Minute)
	p.emitted(t1, model.Report{Trigger: model.TriggerIdle, Causes: []model.RootCause{{Code: model.CauseGPUPCIeFailure, Links: []string{"pcie.gpu1"}}}})
	if _, ok := p.due(t0.Add(cfg.IdleInterval), false); !ok {
		t.Fatal("a run that named another cause put the passive report off")
	}
	p.emitted(t1, model.Report{Trigger: model.TriggerIdle, Causes: []model.RootCause{{Code: model.CausePCIeLinkErrors, Links: []string{"pcie.gpu1"}, Confidence: model.ConfidenceHigh}}})
	if _, ok := p.due(t1.Add(cfg.IdleInterval-time.Second), false); ok {
		t.Fatal("a passive report inside the idle interval of the run that named the cause")
	}

	// A second cause appears: reported at once, with the first.
	sample(finding("pcie.gpu1", model.PCIeLinkSustained), finding("pcie.rnic2", model.PCIeLinkUncorrectable))
	rep, ok = p.due(t1.Add(time.Second), false)
	if !ok || len(rep.Causes) != 2 {
		t.Fatalf("report %+v (due %v), want both causes at once", rep, ok)
	}
	p.emitted(t1.Add(time.Second), rep)
	if got := p.need(); !slices.Equal(got, []string{"pcie.gpu1", "pcie.rnic2"}) {
		t.Fatalf("need %v", got)
	}

	// One goes away: nothing to report. Both go away: logged, and a cause
	// that comes back later is new again.
	sample(finding("pcie.gpu1", model.PCIeLinkSustained))
	if _, ok := p.due(t1.Add(2*time.Second), false); ok {
		t.Fatal("a report because a cause went away")
	}
	sample()
	if _, ok := p.due(t1.Add(time.Hour), false); ok {
		t.Fatal("a report with no cause standing")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "passive PCIe findings ended") {
		t.Fatalf("logs %q, want the end of the findings once", logs)
	}
	sample(finding("pcie.gpu1", model.PCIeLinkSustained))
	if _, ok := p.due(t1.Add(time.Hour+time.Second), false); !ok {
		t.Fatal("a cause that came back is not reported")
	}
}

// Without --pcie-interval, and in a one-shot run, there is no passive watch,
// and every method of a nil one is a no-op.
func TestPassiveWatchOff(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	if p := newPassiveWatch(cfg, g, nolog); p != nil {
		t.Fatalf("passive watch %+v without --pcie-interval", p)
	}
	cfg.PCIeInterval = time.Second
	cfg.Once = true
	p := newPassiveWatch(cfg, g, nolog)
	if p != nil {
		t.Fatalf("passive watch %+v in a one-shot run", p)
	}
	p.observe(&model.PCIePassive{Sampled: true})
	p.emitted(time.Now(), model.Report{})
	if _, ok := p.due(time.Now(), true); ok || p.need() != nil || p.findings() != nil {
		t.Fatal("a nil passive watch reports something")
	}
}

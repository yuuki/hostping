package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/yuuki/hostping/internal/buildinfo"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/drift"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/telemetry"
	"github.com/yuuki/hostping/internal/topology"
)

// driftStart is noon of the UTC day the drift tests begin on.
var driftStart = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// driftDaemon is the daemon loop over the simulated Fig.2 host with
// --drift-tracking: the simulator's links can be slowed between runs, and
// the drift history sees one confirmed idle run per call of day, on a clock
// the test moves a day at a time.
type driftDaemon struct {
	*testDaemon
	world *sim.World
	dir   string
	// out receives the reports as the daemon writes them, one JSON line each.
	out  *bytes.Buffer
	now  time.Time
	logs *logLines
}

// driftBuild names the hostping build of the test daemons.
const driftBuild = "0.2.6 (abc1234)"

func driftCfg(dir string) config.Config {
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.IdleInterval = time.Hour
	cfg.StateDir = dir
	cfg.DriftTracking = true
	return cfg
}

// startDriftDaemon starts a daemon over dir, as a restart of whichever
// daemon wrote the history there before. at is the drift clock it starts on.
func startDriftDaemon(t *testing.T, dir string, at time.Time) *driftDaemon {
	t.Helper()
	return startDriftDaemonWith(t, driftCfg(dir), driftBuild, at)
}

// startDriftDaemonWith is startDriftDaemon with the configuration and the
// hostping build given.
func startDriftDaemonWith(t *testing.T, cfg config.Config, build string, at time.Time) *driftDaemon {
	t.Helper()
	dir := cfg.StateDir
	g := topology.Fig2()
	w := sim.New(g)
	eng := engine.New(cfg, g, sim.Prober{World: w})
	eng.Logf = func(string, ...any) {}
	td := &testDaemon{counters: map[string]model.RNICCounters{}, clk: time.Unix(1_700_000_000, 0)}
	s := flakySampler{StaticSampler: monitor.StaticSampler{Counters: td.counters, GPU: idleGPUs(g, nil)}, fail: &td.failPoll}
	mon := monitor.New(cfg, g, s)
	mon.Logf = nolog
	td.reader = sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(td.reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tel, err := telemetry.New(provider)
	if err != nil {
		t.Fatal(err)
	}
	dd := &driftDaemon{testDaemon: td, world: w, dir: dir, out: &bytes.Buffer{}, now: at, logs: &logLines{}}
	dt, err := newDriftTracker(cfg, g, build, func() time.Time { return dd.now }, dd.logs.logf)
	if err != nil {
		t.Fatal(err)
	}
	td.daemon = &daemon{
		rig:   &rig{g: g, eng: eng, sampler: s, mon: mon, logf: nolog},
		tel:   tel,
		live:  telemetry.NewLiveness(cfg.MetricInterval, func() time.Time { return td.clk }),
		rp:    &reporter{format: config.ReportFormatJSON, stdout: dd.out, now: time.Now, logf: nolog},
		drift: dt,
	}
	return dd
}

// driftReport is what the tests read back from a report line.
type driftReport struct {
	Trigger string `json:"trigger"`
	Paths   []struct {
		RNIC     string `json:"rnic"`
		Endpoint string `json:"endpoint"`
		Status   string `json:"status"`
	} `json:"paths"`
	Causes []struct {
		Code string `json:"code"`
	} `json:"causes"`
	Drift *struct {
		Stable         int `json:"stable"`
		Drifted        int `json:"drifted"`
		Unknown        int `json:"unknown"`
		UnknownReasons []struct {
			Reason string `json:"reason"`
			Paths  int    `json:"paths"`
		} `json:"unknown_reasons"`
		Findings []struct {
			RNIC        string  `json:"rnic"`
			Endpoint    string  `json:"endpoint"`
			Signal      string  `json:"signal"`
			Shape       string  `json:"shape"`
			Change      float64 `json:"change"`
			Threshold   float64 `json:"threshold"`
			WorseDays   int     `json:"worse_days"`
			RecentDays  int     `json:"recent_days"`
			Since       string  `json:"since"`
			Temperature struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"temperature"`
		} `json:"findings"`
		SoftwareChanges []struct {
			On        string `json:"on"`
			Component string `json:"component"`
			From      string `json:"from"`
			To        string `json:"to"`
			Devices   int    `json:"devices"`
		} `json:"software_changes"`
	} `json:"drift"`
}

// run ticks the loop through one confirmed idle-trigger run and returns its
// report as it was written. The drift clock is not moved.
func (dd *driftDaemon) run(t *testing.T) driftReport {
	t.Helper()
	dd.out.Reset()
	dd.mon.ResumeIdle(time.Time{}) // the next idle poll starts a run
	for i := 0; i < 8 && dd.out.Len() == 0; i++ {
		dd.tick(t)
	}
	var rep driftReport
	if err := json.Unmarshal(dd.out.Bytes(), &rep); err != nil {
		t.Fatalf("no report after 8 polls, or not one JSON line: %v\n%s", err, dd.out)
	}
	if rep.Trigger != string(model.TriggerIdle) {
		t.Fatalf("trigger %q, want an idle run", rep.Trigger)
	}
	return rep
}

// day is one confirmed idle run and then the next day.
func (dd *driftDaemon) day(t *testing.T) driftReport {
	t.Helper()
	rep := dd.run(t)
	dd.now = dd.now.Add(24 * time.Hour)
	return rep
}

// history is the drift history as it is on disk.
func (dd *driftDaemon) history(t *testing.T) drift.File {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dd.dir, drift.FileName))
	if err != nil {
		t.Fatal(err)
	}
	f, err := drift.Parse(b)
	if err != nil {
		t.Fatalf("the daemon wrote a history it would reject: %v", err)
	}
	return f
}

// pathGauge is an int gauge's value for one path, and whether it has the
// series.
func (ms metrics) pathGauge(t *testing.T, name, rnic, endpoint string) (int64, bool) {
	t.Helper()
	m, ok := ms[name]
	if !ok {
		return 0, false
	}
	want := attribute.NewSet(attribute.String("rnic", rnic), attribute.String("endpoint", endpoint))
	for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value, true
		}
	}
	return 0, false
}

func (ms metrics) driftChange(t *testing.T, rnic, endpoint, signal string) (float64, bool) {
	t.Helper()
	m, ok := ms["hostping.path.drift.change_ratio"]
	if !ok {
		return 0, false
	}
	want := attribute.NewSet(attribute.String("rnic", rnic), attribute.String("endpoint", endpoint), attribute.String("signal", signal))
	for _, dp := range m.Data.(metricdata.Gauge[float64]).DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value, true
		}
	}
	return 0, false
}

// slowGPU3 caps the PCIe link of gpu3 at frac of what its fastest path
// (rnic1->gpu3, under the same switch) reads when healthy. The other RNICs
// reach gpu3 across a root port or a socket and read below that anyway, so
// one path slows and nothing else.
func (dd *driftDaemon) slowGPU3(healthy uint64, frac float64) {
	dd.world.LinkBW["pcie.gpu3"] = uint64(float64(healthy) * frac)
}

// The daemon loop end to end over the simulator: a path that loses a few
// tenths of a percent a day is reported as drifted, in the report, the
// metrics and the log, while every existing verdict of the run stays what it
// was; the verdict survives a restart; and it clears when the path recovers.
func TestDaemonDriftEndToEnd(t *testing.T) {
	dir := t.TempDir()
	dd := startDriftDaemon(t, dir, driftStart)
	if !dd.logs.has("drift tracking starts a new history") {
		t.Fatalf("no start line: %q", *dd.logs)
	}
	g := topology.Fig2()
	paths := len(g.Paths)

	// Unknown, with the reason, until the reference and three recent days
	// exist.
	rep := dd.day(t)
	if rep.Drift == nil || rep.Drift.Unknown != paths || rep.Drift.Stable+rep.Drift.Drifted != 0 ||
		len(rep.Drift.UnknownReasons) != 1 || rep.Drift.UnknownReasons[0].Reason != "collecting_reference" || rep.Drift.UnknownReasons[0].Paths != paths {
		t.Fatalf("first run: drift = %+v, want every path unknown (collecting_reference)", rep.Drift)
	}
	ms := dd.collect(t)
	if v, ok := ms.pathGauge(t, "hostping.path.drift.status_code", "rnic1", "gpu3"); !ok || v != 2 {
		t.Fatalf("status code = %d (exported %v), want 2 (unknown)", v, ok)
	}
	if v, ok := ms.pathGauge(t, "hostping.path.drift.reason_code", "rnic1", "gpu3"); !ok || v != 1 {
		t.Fatalf("reason code = %d (exported %v), want 1 (collecting_reference)", v, ok)
	}
	if _, ok := ms.driftChange(t, "rnic1", "gpu3", "bandwidth"); ok {
		t.Fatal("a change is exported for a path that is not judged yet")
	}
	for i := 2; i <= 11; i++ {
		rep = dd.day(t)
	}
	if rep.Drift.Stable != paths || rep.Drift.Unknown != 0 || len(rep.Drift.Findings) != 0 {
		t.Fatalf("day 11: drift = %+v, want every path stable", rep.Drift)
	}

	// rnic1->gpu3 loses 0.4% a day.
	healthy := dd.world.PathBW(mustPath(t, g, "rnic1", "gpu3"))
	first := 0
	for i := 1; i <= 30; i++ {
		dd.slowGPU3(healthy, 1-0.004*float64(i))
		rep = dd.day(t)
		if rep.Drift.Drifted > 0 && first == 0 {
			first = i
		}
		// Nothing but the drift section changes: the path is within its
		// baseline, so no path is abnormal and no cause is named.
		if len(rep.Causes) != 0 {
			t.Fatalf("day %d of the decay: causes = %+v, want none", i, rep.Causes)
		}
		for _, p := range rep.Paths {
			if p.Status == "abnormal" {
				t.Fatalf("day %d of the decay: %s->%s is abnormal", i, p.RNIC, p.Endpoint)
			}
		}
	}
	if first < 14 || first > 24 {
		t.Fatalf("first reported on day %d of the decay, want once the recent days read 5%% down", first)
	}
	if rep.Drift.Drifted != 1 || rep.Drift.Stable != paths-1 || len(rep.Drift.Findings) != 1 {
		t.Fatalf("drift = %+v, want one drifted path", rep.Drift)
	}
	f := rep.Drift.Findings[0]
	if f.RNIC != "rnic1" || f.Endpoint != "gpu3" || f.Signal != "bandwidth" || f.Shape != "gradual" || f.Change < 0.05 || f.Change > 0.12 {
		t.Fatalf("finding = %+v", f)
	}
	// The simulated host has no sensor: unknown, with the reason, not steady.
	if f.Temperature.Status != "unknown" || f.Temperature.Reason != "rnic1: no_sensor" {
		t.Fatalf("temperature = %+v", f.Temperature)
	}
	if !dd.logs.has("hostping: drift: drifted: rnic1->gpu3 bandwidth") {
		t.Fatalf("no log line for the drift: %q", *dd.logs)
	}
	ms = dd.collect(t)
	if v, _ := ms.pathGauge(t, "hostping.path.drift.status_code", "rnic1", "gpu3"); v != 3 {
		t.Fatalf("status code of the drifted path = %d, want 3", v)
	}
	if v, _ := ms.pathGauge(t, "hostping.path.drift.status_code", "rnic1", "gpu2"); v != 1 {
		t.Fatalf("status code of its neighbour = %d, want 1 (stable)", v)
	}
	if v, _ := ms.pathGauge(t, "hostping.path.drift.reason_code", "rnic1", "gpu3"); v != 0 {
		t.Fatalf("reason code of a judged path = %d, want 0", v)
	}
	if c, ok := ms.driftChange(t, "rnic1", "gpu3", "bandwidth"); !ok || c != f.Change {
		t.Fatalf("change gauge = %v (exported %v), want the finding's %v", c, ok, f.Change)
	}
	if c, ok := ms.driftChange(t, "rnic1", "gpu3", "latency_p99"); !ok || c != 0 {
		t.Fatalf("latency p99 change = %v (exported %v), want 0", c, ok)
	}

	// A restart on the same directory carries the history on: the first run
	// reports the drift, without the weeks it took to find it.
	dd.drift.shutdown()
	st, err := os.Stat(filepath.Join(dir, drift.FileName))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("drift history: %v, mode %v", err, st.Mode().Perm())
	}
	again := startDriftDaemon(t, dir, dd.now)
	if !again.logs.has("restored: 40 of this topology's 40 paths (40 with a reference, 1 judged drifted)") {
		t.Fatalf("restart did not restore: %q", *again.logs)
	}
	again.slowGPU3(healthy, 0.88)
	rep = again.day(t)
	if rep.Drift.Drifted != 1 || rep.Drift.Findings[0].Since != f.Since {
		t.Fatalf("after the restart: drift = %+v, want the verdict from before it (since %s)", rep.Drift, f.Since)
	}
	if again.logs.has("drifted: rnic1->gpu3") {
		t.Fatalf("a verdict carried over a restart was logged as new: %q", *again.logs)
	}

	// The link is back at full speed: the verdict clears once the recent
	// days read it.
	delete(again.world.LinkBW, "pcie.gpu3")
	again.world.LinkBW["pcie.gpu3"] = healthy * 2
	for i := 0; i < 8; i++ {
		rep = again.day(t)
	}
	if rep.Drift.Drifted != 0 || rep.Drift.Stable != paths {
		t.Fatalf("after the recovery: drift = %+v", rep.Drift)
	}
	if !again.logs.has("rnic1->gpu3 bandwidth recovered") {
		t.Fatalf("no log line for the recovery: %q", *again.logs)
	}
	if v, _ := again.collect(t).pathGauge(t, "hostping.path.drift.status_code", "rnic1", "gpu3"); v != 1 {
		t.Fatalf("status code after the recovery = %d, want 1", v)
	}
}

func mustPath(t *testing.T, g topology.Graph, rnic, endpoint string) model.Path {
	t.Helper()
	p, ok := g.Path(rnic, endpoint)
	if !ok {
		t.Fatalf("no path %s->%s", rnic, endpoint)
	}
	return p
}

// A run the idle hold discards never reaches the history: its readings were
// taken on a host that turned busy.
func TestDiscardedRunIsNotTracked(t *testing.T) {
	dd := startDriftDaemon(t, t.TempDir(), driftStart)
	reads := 0
	dd.drift.temps = func(context.Context) map[string]float64 {
		reads++
		return map[string]float64{"rnic0": 55}
	}
	dd.day(t)
	seen := func() int { return dd.history(t).Paths["0000:17:00.0->mem0of2/local_mem"].Open.Seen }
	if seen() != 1 || reads != 1 {
		t.Fatalf("after one confirmed run: %d readings in the history, %d temperature reads", seen(), reads)
	}
	// The next run is held, and the host is busy on the clean poll after it.
	dd.mon.ResumeIdle(time.Time{})
	dd.tick(t) // the run
	if reads != 2 || dd.drift.held == nil {
		t.Fatalf("temperatures are read when the run ends: %d reads, held %v", reads, dd.drift.held)
	}
	dd.tick(t) // re-baseline
	dd.counters["rnic0"] = model.RNICCounters{TxBytes: 1 << 40}
	dd.out.Reset()
	dd.tick(t)
	if dd.out.Len() != 0 {
		t.Fatalf("a discarded run was reported: %s", dd.out)
	}
	if dd.drift.held != nil {
		t.Fatal("the temperatures of a discarded run were kept for the next one")
	}
	dd.drift.shutdown()
	if seen() != 1 {
		t.Fatalf("the discarded run added a reading: %d", seen())
	}
	if _, _, busy, _, _ := dd.collect(t).outcomes(t); busy != 1 {
		t.Fatalf("run.discarded host_busy = %d, want 1", busy)
	}
}

// Only a confirmed idle-trigger run is applied. A busy-mode run has no idle
// readings, and an idle-mode run a mutual alarm started is not gated by
// --idle-gate-command, so neither gets a drift section or moves the history.
func TestOnlyIdleTriggerRunsAreTracked(t *testing.T) {
	dd := startDriftDaemon(t, t.TempDir(), driftStart)
	idle := dd.day(t)
	if idle.Drift == nil {
		t.Fatal("an idle-trigger run has no drift section")
	}
	before, _ := drift.Marshal(dd.drift.tr.File())
	for _, rep := range []model.Report{
		{Trigger: model.TriggerPFC, Mode: model.ModeBusy},
		{Trigger: model.TriggerPeriodic, Mode: model.ModeBusy},
		{Trigger: model.TriggerMutualTimeout, Mode: model.ModeIdle, HostIdle: true},
		{Trigger: model.TriggerManual, Mode: model.ModeIdle, HostIdle: true},
	} {
		if paths := dd.drift.observe(&rep); paths != nil || rep.Drift != nil {
			t.Errorf("trigger %s mode %s: %d paths, drift %+v, want neither", rep.Trigger, rep.Mode, len(paths), rep.Drift)
		}
	}
	if after, _ := drift.Marshal(dd.drift.tr.File()); !bytes.Equal(before, after) {
		t.Fatal("a run that is not an idle-trigger run changed the history")
	}
}

// Without --drift-tracking, and in a one-shot run, there is no tracker, and
// every call on it is a no-op: no file, no drift section, no series.
func TestDriftTrackingIsOptIn(t *testing.T) {
	dir := t.TempDir()
	g := topology.Fig2()
	off := driftCfg(dir)
	off.DriftTracking = false
	once := driftCfg(dir)
	once.Once = true
	for name, cfg := range map[string]config.Config{"flag off": off, "one-shot": once} {
		dt, err := newDriftTracker(cfg, g, driftBuild, time.Now, nolog)
		if err != nil || dt != nil {
			t.Fatalf("%s: tracker %v, err %v, want none", name, dt, err)
		}
		rep := model.Report{Trigger: model.TriggerIdle, Mode: model.ModeIdle, HostIdle: true}
		dt.runEnded(context.Background(), rep)
		dt.discarded()
		if paths := dt.observe(&rep); paths != nil || rep.Drift != nil {
			t.Fatalf("%s: a nil tracker observed a run", name)
		}
		dt.shutdown()
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("the state directory holds %v", ents)
	}
	// The daemon loop without a tracker reports and exports as before.
	td := newTestDaemon(t, nil)
	var out bytes.Buffer
	td.rp.stdout = &out
	td.idleRun(t)
	td.tick(t)
	td.tick(t)
	if out.Len() == 0 || bytes.Contains(out.Bytes(), []byte(`"drift"`)) {
		t.Fatalf("report without --drift-tracking: %s", out.Bytes())
	}
	ms := td.collect(t)
	for _, name := range []string{"hostping.path.drift.status_code", "hostping.path.drift.reason_code", "hostping.path.drift.change_ratio", "hostping.path.drift.threshold_ratio"} {
		if _, ok := ms[name]; ok {
			t.Errorf("%s exported without --drift-tracking", name)
		}
	}
	// Tracking without somewhere to keep the history fails the start.
	noDir := driftCfg("")
	if _, err := newDriftTracker(noDir, g, driftBuild, time.Now, nolog); err == nil || !strings.Contains(err.Error(), "--drift-tracking needs --state-dir") {
		t.Fatalf("--drift-tracking without --state-dir: %v", err)
	}
	if _, err := newRig(noDir, nolog); err == nil || !strings.Contains(err.Error(), "--drift-tracking needs --state-dir") {
		t.Fatalf("newRig with --drift-tracking and no --state-dir: %v", err)
	}
}

// A history a newer build wrote is left untouched, and the daemon says on
// every run, in the report and the metrics, that nothing is tracked: an
// absent verdict must not read as "no drift".
func TestBlockedHistoryIsNotReplaced(t *testing.T) {
	dir := t.TempDir()
	newer := []byte(`{"version": 2, "paths": {}, "added_later": true}`)
	path := filepath.Join(dir, drift.FileName)
	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	dd := startDriftDaemon(t, dir, driftStart)
	if !dd.logs.has("drift tracking is off") || !dd.logs.has("schema version 2") {
		t.Fatalf("no log line: %q", *dd.logs)
	}
	for i := 0; i < 3; i++ {
		rep := dd.day(t)
		if rep.Drift == nil || rep.Drift.Unknown != 40 || len(rep.Drift.UnknownReasons) != 1 || rep.Drift.UnknownReasons[0].Reason != "history_blocked" {
			t.Fatalf("drift = %+v, want every path unknown (history_blocked)", rep.Drift)
		}
	}
	if v, ok := dd.collect(t).pathGauge(t, "hostping.path.drift.reason_code", "rnic0", "mem0"); !ok || v != 5 {
		t.Fatalf("reason code = %d (exported %v), want 5 (history_blocked)", v, ok)
	}
	dd.drift.shutdown()
	if got, _ := os.ReadFile(path); !bytes.Equal(got, newer) {
		t.Fatalf("the newer build's history was changed: %s", got)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("the state directory holds %v, want the untouched history only", ents)
	}
}

// A damaged history is kept aside for the operator, and tracking starts
// over instead of staying off until someone looks.
func TestDamagedHistoryIsSetAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, drift.FileName)
	if err := os.WriteFile(path, []byte(`{"version": 1, "paths": {"k": {"first_`), 0o600); err != nil {
		t.Fatal(err)
	}
	dd := startDriftDaemon(t, dir, driftStart)
	if !dd.logs.has("was not usable") || !dd.logs.has(path+".rejected") {
		t.Fatalf("no log line naming where the file went: %q", *dd.logs)
	}
	if got, err := os.ReadFile(path + ".rejected"); err != nil || !strings.HasPrefix(string(got), `{"version": 1, "paths": {"k"`) {
		t.Fatalf("set-aside file: %q, %v", got, err)
	}
	if rep := dd.day(t); rep.Drift == nil || rep.Drift.UnknownReasons[0].Reason != "collecting_reference" {
		t.Fatalf("drift = %+v, want a new history collecting its reference", rep.Drift)
	}
	if len(dd.history(t).Paths) != 40 {
		t.Fatalf("new history has %d paths", len(dd.history(t).Paths))
	}
}

// The device temperatures read at the end of each run reach the history,
// and a drift that comes with a hotter device says so.
func TestDriftWithTemperatures(t *testing.T) {
	dd := startDriftDaemon(t, t.TempDir(), driftStart)
	g := topology.Fig2()
	healthy := dd.world.PathBW(mustPath(t, g, "rnic1", "gpu3"))
	gpuTemp := 60.0
	dd.drift.temps = func(context.Context) map[string]float64 {
		return map[string]float64{"rnic1": 55, "gpu3": gpuTemp}
	}
	kernel := "6.8.0-45-generic"
	dd.drift.software = func() (string, map[string]string) {
		return kernel, map[string]string{"rnic1": "28.39.1002", "rnic9": "1.0"}
	}
	for i := 0; i < 12; i++ {
		dd.day(t)
	}
	// gpu3 runs 9 degrees hotter after a kernel update, and its path 8% slower.
	gpuTemp, kernel = 69, "6.8.0-48-generic"
	dd.slowGPU3(healthy, 0.92)
	var rep driftReport
	for i := 0; i < 10; i++ {
		rep = dd.day(t)
	}
	if rep.Drift.Drifted != 1 || len(rep.Drift.Findings) != 1 {
		t.Fatalf("drift = %+v", rep.Drift)
	}
	f := rep.Drift.Findings[0]
	if f.Temperature.Status != "rose" || f.Shape != "step" {
		t.Fatalf("finding = %+v, want a step with a temperature that rose", f)
	}
	// The change is named once for the section, not once per finding.
	if sw := rep.Drift.SoftwareChanges; len(sw) != 1 || sw[0].Component != "kernel" || sw[0].From != "6.8.0-45-generic" || sw[0].To != "6.8.0-48-generic" || sw[0].On != "2026-03-13" || sw[0].Devices != 0 {
		t.Fatalf("software changes = %+v", sw)
	}
	if !dd.logs.has("hostping: drift: kernel changed from 6.8.0-45-generic to 6.8.0-48-generic") {
		t.Fatalf("no log line for the kernel change: %q", *dd.logs)
	}
	h := dd.history(t)
	if fw := h.Software.Firmware; len(fw) != 1 || fw["0000:37:00.0"] != "28.39.1002" {
		t.Fatalf("firmware = %v, want rnic1's under its PCI address only", fw)
	}
	ref := h.Paths["0000:37:00.0->0000:39:00.0/same_switch"].Ref
	if ref == nil || ref.TempRNIC == nil || *ref.TempRNIC != 55 || ref.TempEndpoint == nil || *ref.TempEndpoint != 60 {
		t.Fatalf("reference temperatures = %+v", ref)
	}
	// A memory path of the same RNIC keeps the RNIC's temperature and none
	// for the memory.
	if m := h.Paths["0000:37:00.0->mem0of2/local_mem"].Ref; m == nil || m.TempRNIC == nil || m.TempEndpoint != nil {
		t.Fatalf("memory path reference temperatures = %+v", m)
	}
}

// Within a day the history is written at most every driftSaveEvery, at once
// when a day rolls over, and on shutdown; a run always reaches the file
// before the daemon stops.
func TestDriftHistorySaves(t *testing.T) {
	dd := startDriftDaemon(t, t.TempDir(), driftStart)
	savedAt := func() time.Time { return dd.history(t).SavedAt }
	seen := func() int { return dd.history(t).Paths["0000:17:00.0->mem0of2/local_mem"].Open.Seen }
	dd.run(t)
	if !savedAt().Equal(driftStart) || seen() != 1 {
		t.Fatalf("first run: saved at %v with %d readings", savedAt(), seen())
	}
	dd.now = driftStart.Add(10 * time.Minute)
	dd.run(t)
	dd.now = driftStart.Add(20 * time.Minute)
	dd.run(t)
	if !savedAt().Equal(driftStart) || seen() != 1 {
		t.Fatalf("within %v: saved at %v with %d readings, want the first save still", driftSaveEvery, savedAt(), seen())
	}
	dd.now = driftStart.Add(driftSaveEvery)
	dd.run(t)
	if !savedAt().Equal(dd.now) || seen() != 4 {
		t.Fatalf("after %v: saved at %v with %d readings", driftSaveEvery, savedAt(), seen())
	}
	dd.now = dd.now.Add(time.Minute)
	dd.run(t)
	dd.drift.shutdown()
	if !savedAt().Equal(dd.now) || seen() != 5 {
		t.Fatalf("after shutdown: saved at %v with %d readings", savedAt(), seen())
	}
	// Nothing new: a second shutdown writes nothing.
	dd.now = dd.now.Add(time.Minute)
	dd.drift.shutdown()
	if savedAt().Equal(dd.now) {
		t.Fatal("a history with nothing new was written again")
	}
	// The next day closes the first one: written at once.
	dd.now = driftStart.Add(24 * time.Hour)
	dd.run(t)
	if h := dd.history(t); !h.SavedAt.Equal(dd.now) || len(h.Paths["0000:17:00.0->mem0of2/local_mem"].Days) != 1 {
		t.Fatalf("after the day rolled over: saved at %v", h.SavedAt)
	}
}

// A history that cannot be written is logged and tried again; the daemon
// keeps running and reporting.
func TestDriftHistorySaveFailure(t *testing.T) {
	dir := t.TempDir()
	dd := startDriftDaemon(t, dir, driftStart)
	// A directory where the history goes: the rename over it fails.
	if err := os.MkdirAll(filepath.Join(dir, drift.FileName, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	rep := dd.run(t)
	if rep.Drift == nil || !dd.logs.has("not saved") {
		t.Fatalf("drift = %+v, logs = %q", rep.Drift, *dd.logs)
	}
	if err := os.RemoveAll(filepath.Join(dir, drift.FileName)); err != nil {
		t.Fatal(err)
	}
	dd.drift.shutdown()
	if dd.history(t).Paths["0000:17:00.0->mem0of2/local_mem"].Open.Seen != 1 {
		t.Fatal("the run was lost when the save was retried")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("the state directory holds %v", ents)
	}
}

// The tracker reads the live host only where there is one: under --sim
// there are no sensors, no kernel and no firmware to read, and the hostping
// build is all the software it knows.
func TestDriftLiveReaders(t *testing.T) {
	g := topology.Fig2()
	simulated, err := newDriftTracker(driftCfg(t.TempDir()), g, driftBuild, time.Now, nolog)
	if err != nil {
		t.Fatal(err)
	}
	if simulated.temps != nil || simulated.software != nil {
		t.Fatal("a tracker over the simulator has the readers of a live host")
	}
	if sw := simulated.readSoftware(); sw.Kernel != "" || sw.Firmware != nil || sw.Hostping != driftBuild {
		t.Fatalf("software under --sim = %+v, want the hostping build only", sw)
	}
	// The readers themselves are not called here: they would read the test
	// host's own /sys and fork its nvidia-smi. internal/monitor tests them on
	// fixture trees.
	cfg := driftCfg(t.TempDir())
	cfg.Sim = false
	live, err := newDriftTracker(cfg, g, driftBuild, time.Now, nolog)
	if err != nil {
		t.Fatal(err)
	}
	if live.temps == nil || live.software == nil {
		t.Fatal("a tracker on a live host has no readers")
	}
	live.software = func() (string, map[string]string) {
		return "6.8.0-45-generic", map[string]string{"rnic1": "28.39.1002", "rnic9": "1.0"}
	}
	sw := live.readSoftware()
	if sw.Kernel != "6.8.0-45-generic" || sw.Hostping != driftBuild || len(sw.Firmware) != 1 || sw.Firmware["0000:37:00.0"] != "28.39.1002" {
		t.Fatalf("software on a live host = %+v, want the firmware under the RNIC's PCI address", sw)
	}
}

// A build is named by its version and commit, and a build that knows neither
// is not named: running one must not read as a software change.
func TestBuildName(t *testing.T) {
	for _, c := range []struct {
		b    buildinfo.Info
		want string
	}{
		{buildinfo.Info{Version: "0.2.6", Commit: "abc1234"}, "0.2.6 (abc1234)"},
		{buildinfo.Info{Version: "devel", Commit: "abc1234-dirty"}, "devel (abc1234-dirty)"},
		{buildinfo.Info{Version: "0.2.6", Commit: "unknown"}, "0.2.6 (unknown)"},
		{buildinfo.Info{Version: "devel", Commit: "unknown"}, ""},
		{buildinfo.Info{}, ""},
	} {
		if got := buildName(c.b); got != c.want {
			t.Errorf("buildName(%+v) = %q, want %q", c.b, got, c.want)
		}
	}
}

// A device that throttles on most days and is fine on the others is reported
// through the whole daemon loop for as long as it does so. Every run stays
// clean (no path is abnormal, no cause is named), so the drift section is all
// that says it. The day-to-day movement between the two levels is the
// degradation itself: it must not raise the threshold and have the path
// logged as recovered and exported as stable while it reads 15% low.
func TestDaemonReportsIntermittentDegradation(t *testing.T) {
	dd := startDriftDaemon(t, t.TempDir(), driftStart)
	g := topology.Fig2()
	healthy := dd.world.PathBW(mustPath(t, g, "rnic1", "gpu3"))
	first := -1
	for d := 0; d < 120; d++ {
		if d >= 30 && d%3 != 0 {
			dd.slowGPU3(healthy, 0.85) // low on two days out of three
		} else {
			dd.slowGPU3(healthy, 2) // no cap
		}
		rep := dd.day(t)
		for _, p := range rep.Paths {
			if p.Status == "abnormal" {
				t.Fatalf("day %d: %s->%s abnormal", d, p.RNIC, p.Endpoint)
			}
		}
		code, _ := dd.collect(t).pathGauge(t, "hostping.path.drift.status_code", "rnic1", "gpu3")
		if code == 3 && first < 0 {
			first = d
		}
		if first >= 0 && (code != 3 || rep.Drift.Drifted != 1) {
			t.Fatalf("day %d: status code %d, %d drifted paths in the report, drifted since day %d: %q", d, code, rep.Drift.Drifted, first, *dd.logs)
		}
	}
	if first < 33 || first > 40 {
		t.Fatalf("first reported on day %d, want within days of the onset on day 30", first)
	}
	if dd.logs.has("recovered") || dd.logs.has("no longer counted as drifted") {
		t.Fatalf("the verdict was withdrawn while the path still throttles: %q", *dd.logs)
	}
	ms := dd.collect(t)
	if c, ok := ms.driftChange(t, "rnic1", "gpu3", "bandwidth"); !ok || c < 0.14 || c > 0.16 {
		t.Fatalf("change gauge = %v (exported %v), want the 15%% of the low days", c, ok)
	}
	if th, ok := ms.driftThreshold(t, "rnic1", "gpu3", "bandwidth"); !ok || th != 0.05 {
		t.Fatalf("threshold gauge = %v (exported %v), want the 5%% the path drifted at", th, ok)
	}
}

func (ms metrics) driftThreshold(t *testing.T, rnic, endpoint, signal string) (float64, bool) {
	t.Helper()
	m, ok := ms["hostping.path.drift.threshold_ratio"]
	if !ok {
		return 0, false
	}
	want := attribute.NewSet(attribute.String("rnic", rnic), attribute.String("endpoint", endpoint), attribute.String("signal", signal))
	for _, dp := range m.Data.(metricdata.Gauge[float64]).DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value, true
		}
	}
	return 0, false
}

// A daemon restarted with other probe settings takes other readings from the
// same hardware. They are not compared with a reference taken with the old
// settings: the paths start a new history, the log says why, and no path is
// reported as drifted hardware.
func TestProbeSettingsChangeStartsANewHistory(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*config.Config)
		// memoryOnly is set when the setting applies to memory paths only.
		memoryOnly bool
		log        string
	}{
		// The 32 GPU paths and the 8 memory paths have a line each: the
		// settings of a memory path include the warm-up.
		{"--small-msg", func(c *config.Config) { c.SmallMsgBytes = 4096 }, false,
			"32 paths start a new history: their readings were taken with other probe settings (small_msg=1 large_msg=262144 iters=512 -> small_msg=4096 large_msg=262144 iters=512) and are not comparable"},
		{"--large-msg", func(c *config.Config) { c.LargeMsgBytes = 64 << 10 }, false,
			"(small_msg=1 large_msg=262144 iters=512 -> small_msg=1 large_msg=65536 iters=512)"},
		{"--iters", func(c *config.Config) { c.Iters = 64 }, false,
			"(small_msg=1 large_msg=262144 iters=512 mem_warmup=0s -> small_msg=1 large_msg=262144 iters=64 mem_warmup=0s)"},
		{"--mem-warmup", func(c *config.Config) { c.MemWarmup = 200 * time.Millisecond }, true,
			"8 paths start a new history: their readings were taken with other probe settings (small_msg=1 large_msg=262144 iters=512 mem_warmup=0s -> small_msg=1 large_msg=262144 iters=512 mem_warmup=200ms) and are not comparable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dd := startDriftDaemon(t, dir, driftStart)
			var rep driftReport
			for i := 0; i < 12; i++ {
				rep = dd.day(t)
			}
			if rep.Drift.Stable != 40 {
				t.Fatalf("before the change: %+v, want every path stable", rep.Drift)
			}
			dd.drift.shutdown()

			cfg := driftCfg(dir)
			c.set(&cfg)
			again := startDriftDaemonWith(t, cfg, driftBuild, dd.now)
			restarted := 40
			if c.memoryOnly {
				restarted = 8
			}
			rep = again.day(t)
			if !again.logs.has(c.log) {
				t.Fatalf("no log line for the paths that start over: %q", *again.logs)
			}
			if rep.Drift.Drifted != 0 || rep.Drift.Stable != 40-restarted || rep.Drift.Unknown != restarted ||
				rep.Drift.UnknownReasons[0].Reason != "collecting_reference" {
				t.Fatalf("first run with the new settings: %+v, want %d paths collecting a new reference and none drifted", rep.Drift, restarted)
			}
			for i := 0; i < 11; i++ {
				rep = again.day(t)
				if rep.Drift.Drifted != 0 {
					t.Fatalf("day %d with the new settings: %d paths reported as drifted: %q", i+2, rep.Drift.Drifted, *again.logs)
				}
			}
			if rep.Drift.Stable != 40 {
				t.Fatalf("12 days with the new settings: %+v, want every path stable against its new reference", rep.Drift)
			}
			// The same settings again start nothing over.
			again.drift.shutdown()
			same := startDriftDaemonWith(t, cfg, driftBuild, again.now)
			if rep = same.day(t); rep.Drift.Stable != 40 || same.logs.has("start a new history") {
				t.Fatalf("restart with the same settings: %+v, %q", rep.Drift, *same.logs)
			}
		})
	}
}

// Another hostping build may read the same hardware differently. The history
// records the build like the kernel and the firmware, and a drift that
// follows an upgrade names it.
func TestHostpingUpgradeIsNamed(t *testing.T) {
	dir := t.TempDir()
	dd := startDriftDaemon(t, dir, driftStart)
	g := topology.Fig2()
	healthy := dd.world.PathBW(mustPath(t, g, "rnic1", "gpu3"))
	for i := 0; i < 12; i++ {
		dd.day(t)
	}
	dd.drift.shutdown()
	if got := dd.history(t).Software.Hostping; got != driftBuild {
		t.Fatalf("recorded build %q, want %q", got, driftBuild)
	}
	upgraded := startDriftDaemonWith(t, driftCfg(dir), "0.3.0 (def5678)", dd.now)
	upgraded.slowGPU3(healthy, 0.92)
	var rep driftReport
	for i := 0; i < 8; i++ {
		rep = upgraded.day(t)
	}
	if !upgraded.logs.has("hostping: drift: hostping changed from 0.2.6 (abc1234) to 0.3.0 (def5678)") {
		t.Fatalf("no log line for the upgrade: %q", *upgraded.logs)
	}
	if rep.Drift.Drifted != 1 || len(rep.Drift.SoftwareChanges) != 1 {
		t.Fatalf("drift = %+v, want one drifted path and the upgrade named", rep.Drift)
	}
	if sw := rep.Drift.SoftwareChanges[0]; sw.Component != "hostping" || sw.From != driftBuild || sw.To != "0.3.0 (def5678)" || sw.On != "2026-03-13" {
		t.Fatalf("software change = %+v", sw)
	}
	// A build that cannot name itself records no change.
	upgraded.drift.shutdown()
	unnamed := startDriftDaemonWith(t, driftCfg(dir), buildName(buildinfo.Info{Version: buildinfo.UnknownVersion, Commit: buildinfo.UnknownCommit}), upgraded.now)
	unnamed.day(t)
	unnamed.drift.shutdown()
	if h := unnamed.history(t); len(h.Events) != 1 || h.Software.Hostping != "0.3.0 (def5678)" {
		t.Fatalf("after a run of an unnamed build: %d events, build %q", len(h.Events), h.Software.Hostping)
	}
}

// A history with a value that no probe reads (a hand edit, a damaged byte)
// is not a history: a change against a reference of 1e-320 ns is not a
// number a report can carry, and encoding it would lose the whole report of
// every idle run. The file is set aside like any other unusable one, and the
// reports keep coming.
func TestHistoryWithImpossibleValueIsSetAside(t *testing.T) {
	dir := t.TempDir()
	dd := startDriftDaemon(t, dir, driftStart)
	for i := 0; i < 12; i++ {
		dd.day(t)
	}
	dd.drift.shutdown()
	path := filepath.Join(dir, drift.FileName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A positive latency far below anything a probe reads.
	edited := editFirstNumber(t, b, `"p50":`, "1e-320")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	again := startDriftDaemon(t, dir, dd.now)
	if !again.logs.has("was not usable") || !again.logs.has("is not a reading between 1 and 1e+18") {
		t.Fatalf("the edited history was not set aside: %q", *again.logs)
	}
	rep := again.day(t)
	if rep.Drift == nil || rep.Drift.Unknown != 40 || len(rep.Paths) != 40 {
		t.Fatalf("report after the restart: %d paths, drift %+v: want the whole report, with a new history", len(rep.Paths), rep.Drift)
	}
	if kept, err := os.ReadFile(path + ".rejected"); err != nil || !bytes.Equal(kept, edited) {
		t.Fatalf("the edited history was not kept for the operator: %v", err)
	}
}

// editFirstNumber replaces the JSON number that follows the first key in b.
func editFirstNumber(t *testing.T, b []byte, key, value string) []byte {
	t.Helper()
	i := bytes.Index(b, []byte(key))
	if i < 0 {
		t.Fatalf("no %s in the history", key)
	}
	start := i + len(key)
	end := start
	for end < len(b) && b[end] != ',' && b[end] != '}' {
		end++
	}
	out := append([]byte(nil), b[:start]...)
	out = append(out, value...)
	return append(out, b[end:]...)
}

// A drift section that JSON cannot carry is left out, with a log line, and
// the report of the run is written without it.
func TestUnencodableDriftSectionIsLeftOut(t *testing.T) {
	dd := startDriftDaemon(t, t.TempDir(), driftStart)
	rep := model.Report{Trigger: model.TriggerIdle, Mode: model.ModeIdle}
	dd.drift.attach(&rep, model.DriftSummary{Drifted: 1, Findings: []model.DriftFinding{{RNIC: "rnic0", Endpoint: "gpu0", Change: math.Inf(1)}}})
	if rep.Drift != nil {
		t.Fatalf("a section with an infinite change was attached: %+v", rep.Drift)
	}
	if !dd.logs.has("the drift section is left out of this report: it cannot be encoded") {
		t.Fatalf("no log line: %q", *dd.logs)
	}
	if _, err := json.Marshal(rep); err != nil {
		t.Fatalf("the report does not encode without the section: %v", err)
	}
	dd.drift.attach(&rep, model.DriftSummary{Stable: 40})
	if rep.Drift == nil || rep.Drift.Stable != 40 {
		t.Fatalf("a sound section was not attached: %+v", rep.Drift)
	}
}

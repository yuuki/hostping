package agent

import (
	"context"
	"errors"
	"io"
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

// failingProber fails every loopback probe with an error the engine does not
// skip, so the whole run fails.
type failingProber struct{ sim.Prober }

func (failingProber) Loopback(context.Context, model.RNIC, model.Endpoint, uint32, uint32) (model.PathMeasurement, error) {
	return model.PathMeasurement{}, errors.New("loopback: device wedged")
}

// flakySampler fails every RNIC counter read while *fail is set.
type flakySampler struct {
	monitor.StaticSampler
	fail *bool
}

func (s flakySampler) RNIC(ctx context.Context, n model.RNIC) (model.RNICCounters, error) {
	if *s.fail {
		return model.RNICCounters{}, errors.New("sysfs counters unreadable")
	}
	return s.StaticSampler.RNIC(ctx, n)
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

// testDaemon is the daemon loop over the simulated Fig.2 host, with a sampler
// the test steps by hand and its metrics on a manual reader.
type testDaemon struct {
	*daemon
	counters map[string]model.RNICCounters
	failPoll bool
	reader   *sdkmetric.ManualReader
	clk      time.Time
}

func newTestDaemon(t *testing.T, prober engine.Prober) *testDaemon {
	t.Helper()
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	g := topology.Fig2()
	if prober == nil {
		prober = sim.Prober{World: sim.New(g)}
	}
	eng := engine.New(cfg, g, prober)
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
	td.daemon = &daemon{
		rig:  &rig{g: g, eng: eng, sampler: s, mon: mon, logf: nolog},
		tel:  tel,
		live: telemetry.NewLiveness(cfg.MetricInterval, func() time.Time { return td.clk }),
		rp:   &reporter{format: config.ReportFormatJSON, stdout: io.Discard, now: time.Now, logf: nolog},
	}
	return td
}

func (td *testDaemon) tick(t *testing.T) {
	t.Helper()
	if !td.daemon.tick(context.Background()) {
		t.Fatal("tick stopped the loop without a shutdown")
	}
}

// idleRun warms the monitor up and takes the idle trigger: the run's report
// is then held for confirmation.
func (td *testDaemon) idleRun(t *testing.T) {
	t.Helper()
	td.tick(t) // warmup
	td.tick(t) // idle: the run starts
}

type metrics map[string]metricdata.Metrics

func (td *testDaemon) collect(t *testing.T) metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := td.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// count is a counter's value for one attribute (key=value, or "" for none).
func (ms metrics) count(t *testing.T, name, key, value string) int64 {
	t.Helper()
	m, ok := ms[name]
	if !ok {
		t.Fatalf("%s not exported", name)
	}
	want := attribute.NewSet()
	if key != "" {
		want = attribute.NewSet(attribute.String(key, value))
	}
	for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
		if dp.Attributes.Equals(&want) {
			return dp.Value
		}
	}
	t.Fatalf("%s has no series %s=%s", name, key, value)
	return 0
}

// stamp is a timestamp gauge's value, and whether it was exported at all.
func (ms metrics) stamp(name string) (float64, bool) {
	m, ok := ms[name]
	if !ok {
		return 0, false
	}
	dps := m.Data.(metricdata.Gauge[float64]).DataPoints
	if len(dps) != 1 {
		return 0, false
	}
	return dps[0].Value, true
}

// idle is hostping.monitor.host_idle.
func (ms metrics) idle(t *testing.T) int64 {
	t.Helper()
	m, ok := ms["hostping.monitor.host_idle"]
	if !ok {
		t.Fatal("hostping.monitor.host_idle not exported")
	}
	dps := m.Data.(metricdata.Gauge[int64]).DataPoints
	if len(dps) != 1 {
		t.Fatalf("hostping.monitor.host_idle has %d series, want 1", len(dps))
	}
	return dps[0].Value
}

func (ms metrics) outcomes(t *testing.T) (failedEngine, failedReport, busy, unknown, pollErrors int64) {
	t.Helper()
	return ms.count(t, "hostping.engine.run.failed", "stage", telemetry.FailedEngine),
		ms.count(t, "hostping.engine.run.failed", "stage", telemetry.FailedReport),
		ms.count(t, "hostping.engine.run.discarded", "reason", telemetry.DiscardHostBusy),
		ms.count(t, "hostping.engine.run.discarded", "reason", telemetry.DiscardIdleUnknown),
		ms.count(t, "hostping.monitor.poll.errors", "", "")
}

func recent(v float64) bool {
	now := float64(time.Now().UnixNano()) / 1e9
	return v > now-60 && v <= now+1
}

func TestDaemonRecordsConfirmedRun(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.idleRun(t)
	ms := td.collect(t)
	if v, ok := ms.stamp("hostping.engine.last_run_timestamp"); !ok || !recent(v) {
		t.Fatalf("last_run after the idle run = %v (exported %v)", v, ok)
	}
	if _, ok := ms.stamp("hostping.engine.last_success_timestamp"); ok {
		t.Fatal("a held idle-mode run is not a success until it is confirmed")
	}
	td.tick(t) // re-baseline
	td.tick(t) // clean and idle: confirmed
	ms = td.collect(t)
	if v, ok := ms.stamp("hostping.engine.last_success_timestamp"); !ok || !recent(v) {
		t.Fatalf("last_success after the confirmed run = %v (exported %v)", v, ok)
	}
	if v, ok := ms.stamp("hostping.monitor.last_poll_timestamp"); !ok || !recent(v) {
		t.Fatalf("last_poll = %v (exported %v)", v, ok)
	}
	if v := ms.idle(t); v != 1 {
		t.Fatalf("hostping.monitor.host_idle = %d on an idle host, want 1", v)
	}
	if fe, fr, b, u, pe := ms.outcomes(t); fe+fr+b+u+pe != 0 {
		t.Fatalf("healthy run counted failed engine=%d report=%d discarded busy=%d unknown=%d poll errors=%d", fe, fr, b, u, pe)
	}
}

func TestDaemonRecordsEngineFailure(t *testing.T) {
	td := newTestDaemon(t, failingProber{})
	td.idleRun(t)
	ms := td.collect(t)
	if fe, fr, _, _, _ := ms.outcomes(t); fe != 1 || fr != 0 {
		t.Fatalf("run.failed engine=%d report=%d, want 1 and 0", fe, fr)
	}
	if _, ok := ms.stamp("hostping.engine.last_run_timestamp"); !ok {
		t.Fatal("a failed run still ended: last_run must be set")
	}
	if _, ok := ms.stamp("hostping.engine.last_success_timestamp"); ok {
		t.Fatal("a failed run is not a success")
	}
}

func TestDaemonRecordsReportFailure(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.rp.stdout = failWriter{}
	td.idleRun(t)
	td.tick(t)
	td.tick(t)
	ms := td.collect(t)
	if fe, fr, _, _, _ := ms.outcomes(t); fe != 0 || fr != 1 {
		t.Fatalf("run.failed engine=%d report=%d, want 0 and 1", fe, fr)
	}
	if _, ok := ms.stamp("hostping.engine.last_success_timestamp"); ok {
		t.Fatal("a report that was not written is not a success")
	}
}

func TestDaemonRecordsDiscardedRun(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.idleRun(t)
	td.tick(t) // re-baseline
	// A workload comes back: rnic0 moves far above Thp_low by the clean poll.
	td.counters["rnic0"] = model.RNICCounters{TxBytes: 1 << 40}
	td.tick(t)
	ms := td.collect(t)
	if _, _, b, u, _ := ms.outcomes(t); b != 1 || u != 0 {
		t.Fatalf("run.discarded host_busy=%d idle_unknown=%d, want 1 and 0", b, u)
	}
	if v := ms.idle(t); v != 0 {
		t.Fatalf("hostping.monitor.host_idle = %d on a busy host, want 0", v)
	}
	if _, ok := ms.stamp("hostping.engine.last_success_timestamp"); ok {
		t.Fatal("a discarded run is not a success")
	}
}

func TestDiscardReason(t *testing.T) {
	if r := discardReason(monitor.Decision{Ready: true, UnreadCounters: []string{"rnic3"}}); r != telemetry.DiscardIdleUnknown {
		t.Errorf("unclassified host: %s", r)
	}
	if r := discardReason(busyAfterRun); r != telemetry.DiscardHostBusy {
		t.Errorf("busy host: %s", r)
	}
}

// A poll that fails is counted and still proves the loop is alive.
func TestDaemonRecordsPollError(t *testing.T) {
	td := newTestDaemon(t, nil)
	td.clk = td.clk.Add(time.Hour) // long past the start-up grace
	if td.live.Check() == nil {
		t.Fatal("no poll for an hour must be a stall")
	}
	td.failPoll = true
	td.tick(t)
	td.tick(t)
	if err := td.live.Check(); err != nil {
		t.Fatalf("a poll that returned an error still ticks the loop: %v", err)
	}
	ms := td.collect(t)
	if _, _, _, _, pe := ms.outcomes(t); pe != 2 {
		t.Fatalf("poll.errors = %d, want 2", pe)
	}
	if _, ok := ms.stamp("hostping.monitor.last_poll_timestamp"); ok {
		t.Fatal("last_poll must stay unset until a poll succeeds")
	}
}

// Shutdown cuts the poll short: the loop stops and nothing is counted.
func TestDaemonTickStopsOnShutdown(t *testing.T) {
	td := newTestDaemon(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if td.daemon.tick(ctx) {
		t.Fatal("a canceled tick must stop the loop")
	}
	if _, _, _, _, pe := td.collect(t).outcomes(t); pe != 0 {
		t.Fatalf("shutdown counted %d poll errors", pe)
	}
}

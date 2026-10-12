package telemetry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeClock is a hand-stepped clock for Liveness.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) step(d time.Duration)  { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock             { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }
func healthz(h http.Handler) (int, string) { return serve(h, "/healthz") }
func serve(h http.Handler, path string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestLivenessMaxAge(t *testing.T) {
	for _, c := range []struct {
		interval, want time.Duration
	}{
		{time.Second, LivenessFloor},
		{3 * time.Second, LivenessFloor},
		{10 * time.Second, 100 * time.Second},
	} {
		if got := NewLiveness(c.interval, nil).MaxAge(); got != c.want {
			t.Errorf("--metric-interval %v: max age %v, want %v", c.interval, got, c.want)
		}
	}
}

// /healthz is 200 from start-up (the grace period counts from the tracker's
// creation), stays 200 while polls keep completing, and turns 503 with a
// reason once the loop stops polling for longer than the limit.
func TestHealthzStalledPoll(t *testing.T) {
	clk := newFakeClock()
	live := NewLiveness(time.Second, clk.now)
	srv := newMetricsServer(live)

	clk.step(LivenessFloor) // start-up: no poll yet, still within the grace
	if code, body := healthz(srv.Handler); code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("start-up grace: %d %q, want 200 ok", code, body)
	}
	clk.step(time.Second)
	if code, body := healthz(srv.Handler); code != http.StatusServiceUnavailable || !strings.Contains(body, "no monitor poll completed for 31s") {
		t.Fatalf("no poll since start-up: %d %q, want 503 naming the stall", code, body)
	}
	live.Polled()
	if code, _ := healthz(srv.Handler); code != http.StatusOK {
		t.Fatalf("after a poll: %d, want 200", code)
	}
	for i := 0; i < 100; i++ {
		clk.step(time.Second)
		live.Polled()
	}
	if code, _ := healthz(srv.Handler); code != http.StatusOK {
		t.Fatalf("polling every second: %d, want 200", code)
	}
	clk.step(LivenessFloor + time.Second)
	code, body := healthz(srv.Handler)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "limit 30s") {
		t.Fatalf("stalled loop: %d %q, want 503", code, body)
	}
}

// The loop polls nothing while it runs the engine. A run is not a stall until
// it exceeds LivenessRunBudget, and its end counts as a tick.
func TestHealthzEngineRun(t *testing.T) {
	clk := newFakeClock()
	live := NewLiveness(time.Second, clk.now)
	live.Polled()
	live.RunStarted()
	clk.step(10 * time.Minute)
	if err := live.Check(); err != nil {
		t.Fatalf("10-minute run: %v", err)
	}
	live.RunEnded()
	clk.step(time.Second)
	if err := live.Check(); err != nil {
		t.Fatalf("just after the run: %v", err)
	}
	live.RunStarted()
	clk.step(LivenessRunBudget + time.Second)
	if err := live.Check(); err == nil || !strings.Contains(err.Error(), "engine run in progress") {
		t.Fatalf("run past its budget: %v", err)
	}
}

// A one-shot run has no loop: /healthz is 200 without a tracker.
func TestHealthzWithoutLiveness(t *testing.T) {
	if code, _ := healthz(newMetricsServer(nil).Handler); code != http.StatusOK {
		t.Fatalf("no tracker: %d, want 200", code)
	}
}

func counterInt(t *testing.T, ms map[string]metricdata.Metrics, name string) map[string]int64 {
	t.Helper()
	m, ok := ms[name]
	if !ok {
		t.Fatalf("metric %s not recorded", name)
	}
	s, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %s is %T, want Sum[int64]", name, m.Data)
	}
	out := map[string]int64{}
	for _, dp := range s.DataPoints {
		out[attrKey(dp.Attributes)] = dp.Value
	}
	return out
}

func stageKey(s string) string  { return attrKey(attribute.NewSet(attribute.String("stage", s))) }
func reasonKey(s string) string { return attrKey(attribute.NewSet(attribute.String("reason", s))) }

// The outcome counters are 0 from the first collection; the timestamps are
// absent until their first event.
func TestLivenessMetricsBeforeAnyEvent(t *testing.T) {
	_, reader := newTestTelemetry(t)
	ms := collect(t, reader)
	failed := counterInt(t, ms, "hostping.engine.run.failed")
	if len(failed) != 2 || failed[stageKey(FailedEngine)] != 0 || failed[stageKey(FailedReport)] != 0 {
		t.Errorf("run.failed = %v, want engine and report at 0", failed)
	}
	disc := counterInt(t, ms, "hostping.engine.run.discarded")
	if len(disc) != 2 || disc[reasonKey(DiscardHostBusy)] != 0 || disc[reasonKey(DiscardIdleUnknown)] != 0 {
		t.Errorf("run.discarded = %v, want host_busy and idle_unknown at 0", disc)
	}
	if pe := counterInt(t, ms, "hostping.monitor.poll.errors"); len(pe) != 1 || pe[""] != 0 {
		t.Errorf("poll.errors = %v, want one series at 0", pe)
	}
	for _, name := range []string{"hostping.engine.last_run_timestamp", "hostping.engine.last_success_timestamp", "hostping.monitor.last_poll_timestamp", "hostping.monitor.host_idle"} {
		if _, ok := ms[name]; ok {
			t.Errorf("%s exported before its first event", name)
		}
	}
}

func TestLivenessMetricsRecorded(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 500_000_000)
	tel.RecordPoll(ctx, at, nil)
	tel.RecordPoll(ctx, at.Add(time.Second), errors.New("sysfs read"))
	tel.RecordRunEnd(ctx, at.Add(2*time.Second))
	tel.RecordRunFailed(ctx, FailedEngine)
	tel.RecordRunFailed(ctx, FailedReport)
	tel.RecordRunFailed(ctx, FailedReport)
	tel.RecordSuccess(ctx, at.Add(3*time.Second))
	tel.RecordDiscarded(ctx, DiscardIdleUnknown)
	tel.RecordMonitorIdle(ctx, true)
	tel.RecordMonitorIdle(ctx, false)
	ms := collect(t, reader)
	if got := gaugeInt(t, ms, "hostping.monitor.host_idle"); len(got) != 1 || got[""] != 0 {
		t.Errorf("monitor.host_idle = %v, want the last poll's 0", got)
	}

	for name, want := range map[string]float64{
		"hostping.monitor.last_poll_timestamp":   1_700_000_000.5, // the failed poll does not move it
		"hostping.engine.last_run_timestamp":     1_700_000_002.5,
		"hostping.engine.last_success_timestamp": 1_700_000_003.5,
	} {
		if got := gaugeFloat(t, ms, name); got[""] != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if got := counterInt(t, ms, "hostping.monitor.poll.errors"); got[""] != 1 {
		t.Errorf("poll.errors = %v, want 1", got)
	}
	if got := counterInt(t, ms, "hostping.engine.run.failed"); got[stageKey(FailedEngine)] != 1 || got[stageKey(FailedReport)] != 2 {
		t.Errorf("run.failed = %v, want engine=1 report=2", got)
	}
	if got := counterInt(t, ms, "hostping.engine.run.discarded"); got[reasonKey(DiscardIdleUnknown)] != 1 || got[reasonKey(DiscardHostBusy)] != 0 {
		t.Errorf("run.discarded = %v, want idle_unknown=1 host_busy=0", got)
	}

	var nilTel *Telemetry // a one-shot run without exporters
	nilTel.RecordPoll(ctx, at, nil)
	nilTel.RecordRunEnd(ctx, at)
	nilTel.RecordRunFailed(ctx, FailedEngine)
	nilTel.RecordSuccess(ctx, at)
	nilTel.RecordDiscarded(ctx, DiscardHostBusy)
	nilTel.RecordMonitorIdle(ctx, true)
}

// The names operators write alerts against, as the Prometheus exporter
// renders them. docs/operations.md quotes these.
func TestLivenessPrometheusNames(t *testing.T) {
	reg := promclient.NewRegistry()
	exp, err := prometheus.New(prometheus.WithRegisterer(reg))
	if err != nil {
		t.Fatal(err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tel, err := New(provider)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	tel.RecordPoll(ctx, now, nil)
	tel.RecordRunEnd(ctx, now)
	tel.RecordSuccess(ctx, now)
	tel.RecordMonitorIdle(ctx, true)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for i, mf := range mfs {
		got[mf.GetName()] = i
	}
	for name, typ := range map[string]string{
		"hostping_engine_last_run_timestamp_seconds":     "GAUGE",
		"hostping_engine_last_success_timestamp_seconds": "GAUGE",
		"hostping_monitor_last_poll_timestamp_seconds":   "GAUGE",
		"hostping_engine_run_failed_total":               "COUNTER",
		"hostping_engine_run_discarded_total":            "COUNTER",
		"hostping_monitor_poll_errors_total":             "COUNTER",
		"hostping_monitor_host_idle":                     "GAUGE",
	} {
		i, ok := got[name]
		if !ok {
			var names []string
			for n := range got {
				names = append(names, n)
			}
			t.Errorf("%s not exported; got %s", name, strings.Join(names, ", "))
			continue
		}
		if mt := mfs[i].GetType().String(); mt != typ {
			t.Errorf("%s is %s, want %s", name, mt, typ)
		}
	}
	if v := mfs[got["hostping_engine_last_success_timestamp_seconds"]].GetMetric()[0].GetGauge().GetValue(); v < float64(now.Unix())-1 || v > float64(now.Unix())+1 {
		t.Errorf("last success = %v, want about %d (unix seconds)", v, now.Unix())
	}
	labels := func(name, key string) []string {
		var out []string
		for _, m := range mfs[got[name]].GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == key {
					out = append(out, l.GetValue())
				}
			}
		}
		return out
	}
	if l := strings.Join(labels("hostping_engine_run_failed_total", "stage"), ","); l != "engine,report" {
		t.Errorf("run_failed stages = %s, want engine,report", l)
	}
	if l := strings.Join(labels("hostping_engine_run_discarded_total", "reason"), ","); l != "host_busy,idle_unknown" {
		t.Errorf("run_discarded reasons = %s, want host_busy,idle_unknown", l)
	}
}

package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/yuuki/hostping/internal/model"
)

// newTestTelemetry wires the instruments to a manual reader so a test can
// collect them without the Prometheus listener or an OTLP exporter.
func newTestTelemetry(t *testing.T) (*Telemetry, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel := &Telemetry{provider: provider, meter: provider.Meter("github.com/yuuki/hostping")}
	if err := tel.initInstruments(); err != nil {
		t.Fatalf("initInstruments: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return tel, reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func attrKey(set attribute.Set) string {
	return set.Encoded(attribute.DefaultEncoder())
}

func gaugeInt(t *testing.T, ms map[string]metricdata.Metrics, name string) map[string]int64 {
	t.Helper()
	m, ok := ms[name]
	if !ok {
		t.Fatalf("metric %s not recorded", name)
	}
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("metric %s is %T, want Gauge[int64]", name, m.Data)
	}
	out := map[string]int64{}
	for _, dp := range g.DataPoints {
		out[attrKey(dp.Attributes)] = dp.Value
	}
	return out
}

func gaugeFloat(t *testing.T, ms map[string]metricdata.Metrics, name string) map[string]float64 {
	t.Helper()
	m, ok := ms[name]
	if !ok {
		t.Fatalf("metric %s not recorded", name)
	}
	g, ok := m.Data.(metricdata.Gauge[float64])
	if !ok {
		t.Fatalf("metric %s is %T, want Gauge[float64]", name, m.Data)
	}
	out := map[string]float64{}
	for _, dp := range g.DataPoints {
		out[attrKey(dp.Attributes)] = dp.Value
	}
	return out
}

func sumInt(t *testing.T, ms map[string]metricdata.Metrics, name string) map[string]int64 {
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

func sampleReport() model.Report {
	return model.Report{
		Trigger:  model.TriggerManual,
		Mode:     model.ModeIdle,
		HostIdle: true,
		Paths: []model.PathMeasurement{{
			RNIC:          "rnic0",
			Endpoint:      "mem0",
			SmallLatNsP50: 2000,
			BandwidthBPS:  200e9,
			Status:        model.PathAbnormal,
		}},
		Links: []model.LinkState{{
			ID:            "rp.0",
			Status:        model.LinkAbnormal,
			AbnormalCount: 3,
			Utilization:   ptr(0.5),
		}},
		Mutual: []model.MutualMeasurement{{
			Src: "rnic0", Dst: "rnic1", Connected: true, RTTnsP50: 4000, LossRatio: 0.25, Sent: 64, Recv: 48,
		}},
		Causes: []model.RootCause{{Code: model.CauseRNICPCIeFailure, Confidence: "high"}},
	}
}

func TestRecordReportInstruments(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordReport(context.Background(), sampleReport())
	ms := collect(t, reader)

	pathKey := "endpoint=mem0,rnic=rnic0"
	if got := gaugeFloat(t, ms, "hostping.loopback.latency")[pathKey]; got != 2.0 {
		t.Errorf("loopback.latency = %v, want 2 (us)", got)
	}
	if got := gaugeFloat(t, ms, "hostping.loopback.bandwidth")[pathKey]; got != 200e9 {
		t.Errorf("loopback.bandwidth = %v, want 2e11", got)
	}
	if got := gaugeInt(t, ms, "hostping.path.abnormal")[pathKey]; got != 1 {
		t.Errorf("path.abnormal = %d, want 1", got)
	}
	if got := gaugeInt(t, ms, "hostping.path.status")["endpoint=mem0,reason=none,rnic=rnic0,status=abnormal"]; got != 1 {
		t.Errorf("path.status[abnormal] = %d, want 1", got)
	}
	if got := gaugeInt(t, ms, "hostping.link.abnormal_count")["link=rp.0"]; got != 3 {
		t.Errorf("link.abnormal_count = %d, want 3", got)
	}
	if got := gaugeFloat(t, ms, "hostping.bus.utilization")["link=rp.0"]; got != 0.5 {
		t.Errorf("bus.utilization = %v, want 0.5", got)
	}
	mutualKey := "dst=rnic1,src=rnic0"
	if got := gaugeFloat(t, ms, "hostping.mutual.rtt")[mutualKey]; got != 4.0 {
		t.Errorf("mutual.rtt = %v, want 4 (us)", got)
	}
	if got := gaugeFloat(t, ms, "hostping.mutual.loss_ratio")[mutualKey]; got != 0.25 {
		t.Errorf("mutual.loss_ratio = %v, want 0.25", got)
	}
	if got := gaugeFloat(t, ms, "hostping.mutual.timeout_ratio")[mutualKey]; got != 0.25 {
		t.Errorf("mutual.timeout_ratio = %v, want 0.25", got)
	}
	if got := gaugeInt(t, ms, "hostping.host.idle")[""]; got != 1 {
		t.Errorf("host.idle = %d, want 1", got)
	}
	if got := sumInt(t, ms, "hostping.engine.run.count")["mode=idle,trigger=manual"]; got != 1 {
		t.Errorf("engine.run.count = %d, want 1", got)
	}
	if got := sumInt(t, ms, "hostping.diagnosis.count")["code=rnic_pcie_failure,confidence=high"]; got != 1 {
		t.Errorf("diagnosis.count = %d, want 1", got)
	}
}

// A path/link gauge must not keep a stale series when a status changes: the
// status lives in hostping.link.status, which is rewritten for every status on
// every report.
func TestRecordReportNoStaleStatusSeries(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordReport(ctx, model.Report{Links: []model.LinkState{
		{ID: "rp.0", Status: model.LinkAbnormal, AbnormalCount: 3, Utilization: ptr(0.9)},
	}})
	tel.RecordReport(ctx, model.Report{Links: []model.LinkState{
		{ID: "rp.0", Status: model.LinkNormal, AbnormalCount: 0, Utilization: ptr(0.1)},
	}})
	ms := collect(t, reader)

	abn := gaugeInt(t, ms, "hostping.link.abnormal_count")
	if len(abn) != 1 {
		t.Fatalf("link.abnormal_count has %d series (%v), want 1", len(abn), abn)
	}
	if abn["link=rp.0"] != 0 {
		t.Errorf("link.abnormal_count = %d, want 0", abn["link=rp.0"])
	}
	bus := gaugeFloat(t, ms, "hostping.bus.utilization")
	if len(bus) != 1 || bus["link=rp.0"] != 0.1 {
		t.Errorf("bus.utilization = %v, want one series at 0.1", bus)
	}

	status := gaugeInt(t, ms, "hostping.link.status")
	want := map[string]int64{
		"link=rp.0,status=uncertain": 0,
		"link=rp.0,status=normal":    1,
		"link=rp.0,status=abnormal":  0,
		"link=rp.0,status=gray":      0,
	}
	if len(status) != len(want) {
		t.Fatalf("link.status has %d series (%v), want %d", len(status), status, len(want))
	}
	for k, v := range want {
		if status[k] != v {
			t.Errorf("link.status[%s] = %d, want %d", k, status[k], v)
		}
	}
}

func ptr(v float64) *float64 { return &v }

// An unread bus has no utilization series at all: a 0 would read as a link
// that was measured idle.
func TestRecordReportSkipsUnknownUtilization(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordReport(context.Background(), model.Report{Links: []model.LinkState{
		{ID: "mem.0", Status: model.LinkNormal},
		{ID: "pcie.rnic0", Status: model.LinkNormal, Utilization: ptr(0)},
	}})
	ms := collect(t, reader)

	bus := gaugeFloat(t, ms, "hostping.bus.utilization")
	if _, ok := bus["link=mem.0"]; ok {
		t.Errorf("unknown utilization exported as %v", bus["link=mem.0"])
	}
	if v, ok := bus["link=pcie.rnic0"]; !ok || v != 0 {
		t.Errorf("a measured 0 must still be exported: %v", bus)
	}
}

func TestRecordRatesInstruments(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordRates(context.Background(), []model.RNICRates{{
		Device:             "rnic0",
		ThroughputBPS:      150e9,
		PauseDurationRatio: 0.04,
		DropDelta:          7,
		PauseKnown:         true,
		DropKnown:          true,
	}})
	ms := collect(t, reader)

	key := "rnic=rnic0"
	if got := gaugeFloat(t, ms, "hostping.rnic.throughput")[key]; got != 150e9 {
		t.Errorf("rnic.throughput = %v, want 1.5e11", got)
	}
	if got := gaugeFloat(t, ms, "hostping.rnic.pause_duration_ratio")[key]; got != 0.04 {
		t.Errorf("rnic.pause_duration_ratio = %v, want 0.04", got)
	}
	if got := gaugeInt(t, ms, "hostping.rnic.drop.delta")[key]; got != 7 {
		t.Errorf("rnic.drop.delta = %d, want 7", got)
	}
}

// A pause ratio or drop delta the monitor did not measure (no source, an
// unreadable counter, or a re-baseline) is an absent series, not a 0 that a
// dashboard would read as healthy. A measured 0 is still exported.
func TestRecordRatesSkipsUnmeasuredPauseAndDrops(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordRates(context.Background(), []model.RNICRates{
		{Device: "rnic0", ThroughputBPS: 1},                                    // not applicable or unknown
		{Device: "rnic1", ThroughputBPS: 1, PauseKnown: true, DropKnown: true}, // measured 0
		{Device: "rnic2", ThroughputBPS: 1, DropKnown: true, DropDelta: 2},     // pause missing only
	})
	ms := collect(t, reader)

	pause := gaugeFloat(t, ms, "hostping.rnic.pause_duration_ratio")
	if _, ok := pause["rnic=rnic0"]; ok {
		t.Errorf("unmeasured pause ratio exported: %v", pause)
	}
	if _, ok := pause["rnic=rnic2"]; ok {
		t.Errorf("unmeasured pause ratio exported: %v", pause)
	}
	if v, ok := pause["rnic=rnic1"]; !ok || v != 0 {
		t.Errorf("a measured 0 pause ratio must be exported: %v", pause)
	}
	drops := gaugeInt(t, ms, "hostping.rnic.drop.delta")
	if _, ok := drops["rnic=rnic0"]; ok {
		t.Errorf("unmeasured drop delta exported: %v", drops)
	}
	if drops["rnic=rnic1"] != 0 || drops["rnic=rnic2"] != 2 || len(drops) != 2 {
		t.Errorf("drop.delta = %v, want rnic1=0 rnic2=2", drops)
	}
	if got := len(gaugeFloat(t, ms, "hostping.rnic.throughput")); got != 3 {
		t.Errorf("throughput series = %d, want 3 (unaffected)", got)
	}
}

// A nil *Telemetry is what a one-shot run without exporters gets.
func TestNilTelemetryRecordsNothing(t *testing.T) {
	var tel *Telemetry
	tel.RecordReport(context.Background(), sampleReport())
	tel.RecordRates(context.Background(), []model.RNICRates{{Device: "rnic0"}})
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// RecordReport and RecordRates are called from the agent loop and must stay
// safe to call concurrently now that the mutex is gone.
func TestRecordConcurrent(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				tel.RecordReport(ctx, sampleReport())
				tel.RecordRates(ctx, []model.RNICRates{{Device: "rnic0", ThroughputBPS: 1}})
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	ms := collect(t, reader)
	if got := sumInt(t, ms, "hostping.engine.run.count")["mode=idle,trigger=manual"]; got != 200 {
		t.Errorf("engine.run.count = %d, want 200", got)
	}
}

// Skipped endpoints are the silent half of a degraded run, so they are
// alertable. The gauge counts endpoints per kind and drops back to 0 when they
// come back, and it carries no reason attribute: the reason is free-form probe
// text and would strand a series behind every reason that gets fixed.
func TestRecordReportEndpointSkipped(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordReport(ctx, model.Report{SkippedEndpoints: []model.EndpointSkip{{
		Kind:      model.EndpointGPU,
		Reason:    "gpu endpoint requires GPUDirect RDMA: NoGDR: no cuda",
		Endpoints: []string{"gpu0", "gpu1"},
		Probes:    8,
	}}})
	skipped := gaugeInt(t, collect(t, reader), "hostping.endpoint.skipped")
	want := map[string]int64{"kind=gpu": 2, "kind=memory": 0}
	if len(skipped) != len(want) {
		t.Fatalf("endpoint.skipped has %d series (%v), want %d", len(skipped), skipped, len(want))
	}
	for k, v := range want {
		if skipped[k] != v {
			t.Errorf("endpoint.skipped[%s] = %d, want %d", k, skipped[k], v)
		}
	}

	tel.RecordReport(ctx, model.Report{})
	skipped = gaugeInt(t, collect(t, reader), "hostping.endpoint.skipped")
	if skipped["kind=gpu"] != 0 || len(skipped) != len(want) {
		t.Errorf("endpoint.skipped = %v, want every kind back at 0", skipped)
	}
}

// One endpoint can fail two ways at once (one reason per RNIC); the gauge
// counts endpoints, not skip groups.
func TestRecordReportEndpointSkippedCountsEndpointsOnce(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordReport(context.Background(), model.Report{SkippedEndpoints: []model.EndpointSkip{
		{Kind: model.EndpointGPU, Reason: "NoGDR: no cuda", Endpoints: []string{"gpu0", "gpu1"}, Probes: 2},
		{Kind: model.EndpointGPU, Reason: "NoGDR: alloc", Endpoints: []string{"gpu1"}, Probes: 1},
	}})
	if got := gaugeInt(t, collect(t, reader), "hostping.endpoint.skipped")["kind=gpu"]; got != 2 {
		t.Errorf("endpoint.skipped[kind=gpu] = %d, want 2 distinct endpoints", got)
	}
}

func assertGaugeSeries(t *testing.T, name string, got, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d series (%v), want %d", name, len(got), got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s[%s] = %d, want %d", name, k, got[k], v)
		}
	}
}

// hostping.path.abnormal is binary, so a path hostping never judged reports the
// same 0 as a healthy one and uncertain is invisible in metrics. path.status
// carries the status and, for an uncertain path, why — and like link.status it
// rewrites every series it owns on every report, so a path that changes status
// or reason leaves none of them stranded at 1.
func TestRecordReportPathStatus(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordReport(ctx, model.Report{Paths: []model.PathMeasurement{{
		RNIC:            "rnic0",
		Endpoint:        "gpu4",
		Status:          model.PathUncertain,
		UncertainReason: model.UncertainNonAffinitive,
	}}})

	want := map[string]int64{
		"endpoint=gpu4,reason=none,rnic=rnic0,status=uncertain":           0,
		"endpoint=gpu4,reason=no_path,rnic=rnic0,status=uncertain":        0,
		"endpoint=gpu4,reason=rnic_busy,rnic=rnic0,status=uncertain":      0,
		"endpoint=gpu4,reason=non_affinitive,rnic=rnic0,status=uncertain": 1,
		"endpoint=gpu4,reason=below_ceiling,rnic=rnic0,status=uncertain":  0,
		"endpoint=gpu4,reason=no_ceiling,rnic=rnic0,status=uncertain":     0,
		"endpoint=gpu4,reason=host_shift,rnic=rnic0,status=uncertain":     0,
		"endpoint=gpu4,reason=cpu_busy,rnic=rnic0,status=uncertain":       0,
		"endpoint=gpu4,reason=none,rnic=rnic0,status=normal":              0,
		"endpoint=gpu4,reason=none,rnic=rnic0,status=abnormal":            0,
	}
	ms := collect(t, reader)
	assertGaugeSeries(t, "path.status", gaugeInt(t, ms, "hostping.path.status"), want)
	if got := gaugeInt(t, ms, "hostping.path.abnormal")["endpoint=gpu4,rnic=rnic0"]; got != 0 {
		t.Errorf("path.abnormal = %d, want 0 on an uncertain path", got)
	}

	// The same path, now measurably degraded: its reason series has to fall
	// back to 0 rather than keep reporting a by-design uncertain path.
	tel.RecordReport(ctx, model.Report{Paths: []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu4", Status: model.PathAbnormal,
	}}})
	want["endpoint=gpu4,reason=non_affinitive,rnic=rnic0,status=uncertain"] = 0
	want["endpoint=gpu4,reason=none,rnic=rnic0,status=abnormal"] = 1
	ms = collect(t, reader)
	assertGaugeSeries(t, "path.status", gaugeInt(t, ms, "hostping.path.status"), want)
	if got := gaugeInt(t, ms, "hostping.path.abnormal")["endpoint=gpu4,rnic=rnic0"]; got != 1 {
		t.Errorf("path.abnormal = %d, want 1: the existing metric is unchanged", got)
	}
}

// Every reason reaches its own series, and exactly one series per path is 1.
// RecordReport writes whatever report it is handed, so a reason left on a
// decided path is ignored rather than leaving the path with no series at 1.
func TestRecordReportPathStatusReasons(t *testing.T) {
	cases := []struct {
		name string
		path model.PathMeasurement
		want string
	}{{
		name: "busy RNIC",
		path: model.PathMeasurement{Status: model.PathUncertain, UncertainReason: model.UncertainRNICBusy},
		want: "endpoint=gpu0,reason=rnic_busy,rnic=rnic0,status=uncertain",
	}, {
		name: "no graph route",
		path: model.PathMeasurement{Status: model.PathUncertain, UncertainReason: model.UncertainNoPath},
		want: "endpoint=gpu0,reason=no_path,rnic=rnic0,status=uncertain",
	}, {
		name: "affinitive path below its ceiling",
		path: model.PathMeasurement{Status: model.PathUncertain, UncertainReason: model.UncertainBelowCeiling},
		want: "endpoint=gpu0,reason=below_ceiling,rnic=rnic0,status=uncertain",
	}, {
		name: "affinitive path with no ceiling to hold it to",
		path: model.PathMeasurement{Status: model.PathUncertain, UncertainReason: model.UncertainNoCeiling},
		want: "endpoint=gpu0,reason=no_ceiling,rnic=rnic0,status=uncertain",
	}, {
		name: "host-wide slow state in the bandwidth recheck",
		path: model.PathMeasurement{Status: model.PathUncertain, UncertainReason: model.UncertainHostShift},
		want: "endpoint=gpu0,reason=host_shift,rnic=rnic0,status=uncertain",
	}, {
		name: "uncertain with no reason recorded",
		path: model.PathMeasurement{Status: model.PathUncertain},
		want: "endpoint=gpu0,reason=none,rnic=rnic0,status=uncertain",
	}, {
		name: "decided path carrying a stale reason",
		path: model.PathMeasurement{Status: model.PathNormal, UncertainReason: model.UncertainNonAffinitive},
		want: "endpoint=gpu0,reason=none,rnic=rnic0,status=normal",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tel, reader := newTestTelemetry(t)
			p := tc.path
			p.RNIC, p.Endpoint = "rnic0", "gpu0"
			tel.RecordReport(context.Background(), model.Report{Paths: []model.PathMeasurement{p}})

			var set []string
			for k, v := range gaugeInt(t, collect(t, reader), "hostping.path.status") {
				if v == 1 {
					set = append(set, k)
				}
			}
			if len(set) != 1 || set[0] != tc.want {
				t.Errorf("path.status series at 1 = %v, want exactly [%s]", set, tc.want)
			}
		})
	}
}

// hostping.path.latency_status is the advisory latency verdict. Like
// path.status it rewrites all of its series on every report, so a path whose
// latency recovers drops back to 0 on the abnormal series.
func TestRecordReportPathLatencyStatus(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordReport(ctx, model.Report{Paths: []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu3", Status: model.PathNormal,
		LatencyStatus: model.LatencyAbnormal, LatencyReason: model.LatencyBySiblings,
	}}})
	want := map[string]int64{
		"endpoint=gpu3,reason=none,rnic=rnic0,status=unknown":           0,
		"endpoint=gpu3,reason=busy,rnic=rnic0,status=unknown":           0,
		"endpoint=gpu3,reason=few_siblings,rnic=rnic0,status=unknown":   0,
		"endpoint=gpu3,reason=no_path,rnic=rnic0,status=unknown":        0,
		"endpoint=gpu3,reason=no_reading,rnic=rnic0,status=unknown":     0,
		"endpoint=gpu3,reason=cpu_busy,rnic=rnic0,status=unknown":       0,
		"endpoint=gpu3,reason=user_baseline,rnic=rnic0,status=normal":   0,
		"endpoint=gpu3,reason=siblings,rnic=rnic0,status=normal":        0,
		"endpoint=gpu3,reason=recheck_cleared,rnic=rnic0,status=normal": 0,
		"endpoint=gpu3,reason=locality_shift,rnic=rnic0,status=normal":  0,
		"endpoint=gpu3,reason=user_baseline,rnic=rnic0,status=abnormal": 0,
		"endpoint=gpu3,reason=siblings,rnic=rnic0,status=abnormal":      1,
	}
	ms := collect(t, reader)
	assertGaugeSeries(t, "path.latency_status", gaugeInt(t, ms, "hostping.path.latency_status"), want)
	// The bandwidth verdict is untouched by the latency one.
	if got := gaugeInt(t, ms, "hostping.path.abnormal")["endpoint=gpu3,rnic=rnic0"]; got != 0 {
		t.Errorf("path.abnormal = %d, want 0: latency does not make a path abnormal", got)
	}

	tel.RecordReport(ctx, model.Report{Paths: []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu3", LatencyStatus: model.LatencyUnknown, LatencyReason: model.LatencyBusy,
	}}})
	want["endpoint=gpu3,reason=siblings,rnic=rnic0,status=abnormal"] = 0
	want["endpoint=gpu3,reason=busy,rnic=rnic0,status=unknown"] = 1
	assertGaugeSeries(t, "path.latency_status", gaugeInt(t, collect(t, reader), "hostping.path.latency_status"), want)

	// A sibling verdict the paired recheck cleared is normal with its own reason.
	tel.RecordReport(ctx, model.Report{Paths: []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu3", LatencyStatus: model.LatencyNormal, LatencyReason: model.LatencyRecheckCleared,
	}}})
	want["endpoint=gpu3,reason=busy,rnic=rnic0,status=unknown"] = 0
	want["endpoint=gpu3,reason=recheck_cleared,rnic=rnic0,status=normal"] = 1
	assertGaugeSeries(t, "path.latency_status", gaugeInt(t, collect(t, reader), "hostping.path.latency_status"), want)

	// So is one cleared as a rise shared by the RNICs of its NUMA node.
	tel.RecordReport(ctx, model.Report{Paths: []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu3", LatencyStatus: model.LatencyNormal, LatencyReason: model.LatencyLocalityShift,
	}}})
	want["endpoint=gpu3,reason=recheck_cleared,rnic=rnic0,status=normal"] = 0
	want["endpoint=gpu3,reason=locality_shift,rnic=rnic0,status=normal"] = 1
	assertGaugeSeries(t, "path.latency_status", gaugeInt(t, collect(t, reader), "hostping.path.latency_status"), want)
}

// hostping.verb.duration carries one series per (path, verb): gpu_alloc only
// for a GPU endpoint, and nothing for a probe that reported no timings.
func TestRecordReportVerbDuration(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordReport(context.Background(), model.Report{Paths: []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "gpu0", EndpointKind: model.EndpointGPU,
			Verbs: model.VerbNs{GPUAlloc: 400_000, MRReg: 150_000, QPSetup: 250_000}},
		{RNIC: "rnic0", Endpoint: "mem0", EndpointKind: model.EndpointMemory,
			Verbs: model.VerbNs{MRReg: 40_000, QPSetup: 250_000}},
		{RNIC: "rnic1", Endpoint: "mem1", EndpointKind: model.EndpointMemory},
	}})
	got := gaugeFloat(t, collect(t, reader), "hostping.verb.duration")
	want := map[string]float64{
		"endpoint=gpu0,endpoint_kind=gpu,rnic=rnic0,verb=gpu_alloc":   400,
		"endpoint=gpu0,endpoint_kind=gpu,rnic=rnic0,verb=mr_reg":      150,
		"endpoint=gpu0,endpoint_kind=gpu,rnic=rnic0,verb=qp_setup":    250,
		"endpoint=mem0,endpoint_kind=memory,rnic=rnic0,verb=mr_reg":   40,
		"endpoint=mem0,endpoint_kind=memory,rnic=rnic0,verb=qp_setup": 250,
	}
	if len(got) != len(want) {
		t.Fatalf("series = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (us)", k, got[k], v)
		}
	}
}

// The scheduled probe's window is exported between engine runs, and a nil
// Telemetry (a one-shot run without exporters) records nothing.
func TestRecordMutualWindow(t *testing.T) {
	var none *Telemetry
	none.RecordMutual(context.Background(), []model.MutualMeasurement{{Src: "a", Dst: "b"}})
	tel, reader := newTestTelemetry(t)
	tel.RecordMutual(context.Background(), []model.MutualMeasurement{
		{Src: "rnic0", Dst: "rnic1", Connected: true, Reliable: true, Sent: 200, Recv: 150, RTTnsP50: 8000},
	})
	ms := collect(t, reader)
	if got := gaugeFloat(t, ms, "hostping.mutual.timeout_ratio")["dst=rnic1,src=rnic0"]; got != 0.25 {
		t.Fatalf("timeout_ratio = %v, want 0.25", got)
	}
	if got := gaugeFloat(t, ms, "hostping.mutual.rtt")["dst=rnic1,src=rnic0"]; got != 8 {
		t.Fatalf("rtt = %v, want 8 (us)", got)
	}
}

// hostping.mutual.timeout_ratio is 1 on a pair that never connected, and a pair
// with no probe count exports nothing rather than a healthy-looking 0.
func TestRecordReportMutualTimeoutRatio(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordReport(context.Background(), model.Report{Mutual: []model.MutualMeasurement{
		{Src: "rnic0", Dst: "rnic1", Connected: false, LossRatio: 1},
		{Src: "rnic0", Dst: "rnic2", Connected: true, Reliable: true, Sent: 64, Recv: 64},
		{Src: "rnic1", Dst: "rnic2", Connected: true},
	}})
	got := gaugeFloat(t, collect(t, reader), "hostping.mutual.timeout_ratio")
	want := map[string]float64{"dst=rnic1,src=rnic0": 1, "dst=rnic2,src=rnic0": 0}
	if len(got) != len(want) {
		t.Fatalf("timeout_ratio series = %v, want %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("timeout_ratio[%s] = %v (present %v), want %v", k, g, ok, v)
		}
	}
}

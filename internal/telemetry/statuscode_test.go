package telemetry

import (
	"context"
	"fmt"
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/yuuki/hostping/internal/model"
)

// The codes are the metrics contract. This table is written out by hand so
// that renumbering one (an enum reordered, a table edited) fails here rather
// than silently changing what dashboards and alerts compare against.
var wantStatusCodes = map[string]map[int64]string{
	"hostping.path.status_code": {1: "normal", 2: "uncertain", 3: "abnormal"},
	"hostping.path.uncertain_reason_code": {
		0: "none", 1: "no_path", 2: "rnic_busy", 3: "non_affinitive",
		4: "below_ceiling", 5: "no_ceiling", 6: "host_shift", 7: "cpu_busy",
	},
	"hostping.path.latency_status_code": {1: "normal", 2: "unknown", 3: "abnormal"},
	"hostping.path.latency_reason_code": {
		0: "none", 1: "user_baseline", 2: "siblings", 3: "busy", 4: "few_siblings",
		5: "no_path", 6: "no_reading", 7: "recheck_cleared", 8: "locality_shift", 9: "cpu_busy",
	},
	"hostping.link.status_code": {1: "normal", 2: "uncertain", 3: "abnormal", 4: "gray"},
}

func TestStatusCodesArePinned(t *testing.T) {
	got := map[string]map[int64]string{}
	for _, r := range statusCodeRows() {
		if got[r.metric] == nil {
			got[r.metric] = map[int64]string{}
		}
		name := r.status + r.reason
		if prev, dup := got[r.metric][r.code]; dup {
			t.Errorf("%s: code %d names both %q and %q", r.metric, r.code, prev, name)
		}
		got[r.metric][r.code] = name
	}
	if fmt.Sprint(got) != fmt.Sprint(wantStatusCodes) {
		t.Errorf("status codes changed:\n got %v\nwant %v", got, wantStatusCodes)
	}
}

// enumValues lists an int enum from 0 up to the first value whose String()
// falls through to the default branch, which is how the model enums end.
func enumValues[K ~int](sentinel string, str func(K) string) []K {
	var out []K
	for k := K(0); str(k) != sentinel; k++ {
		out = append(out, k)
	}
	return out
}

// Every model value has a code, so codeUnmapped is never written.
func TestStatusCodesCoverEnums(t *testing.T) {
	check := func(name string, n int, missing []string) {
		t.Helper()
		if len(missing) > 0 {
			t.Errorf("%s: no code for %v", name, missing)
		}
		if n == 0 {
			t.Errorf("%s: enumerated no values", name)
		}
	}
	var miss []string
	ps := enumValues[model.PathStatus]("unknown", model.PathStatus.String)
	for _, v := range ps {
		if pathStatusCode(v) == codeUnmapped {
			miss = append(miss, v.String())
		}
	}
	check("PathStatus", len(ps), miss)

	miss = nil
	ur := enumValues[model.PathUncertainReason]("unknown", model.PathUncertainReason.String)
	for _, v := range ur {
		if uncertainReasonCode(v) == codeUnmapped {
			miss = append(miss, v.String())
		}
	}
	check("PathUncertainReason", len(ur), miss)

	// LatencyUnknown's String() is "unknown"; the enum's default is "invalid".
	miss = nil
	ls := enumValues[model.LatencyStatus]("invalid", model.LatencyStatus.String)
	for _, v := range ls {
		if latencyStatusCode(v) == codeUnmapped {
			miss = append(miss, v.String())
		}
	}
	check("LatencyStatus", len(ls), miss)

	miss = nil
	lr := enumValues[model.LatencyReason]("unknown", model.LatencyReason.String)
	for _, v := range lr {
		if latencyReasonCode(v) == codeUnmapped {
			miss = append(miss, v.String())
		}
	}
	check("LatencyReason", len(lr), miss)

	miss = nil
	lk := enumValues[model.LinkStatus]("unknown", model.LinkStatus.String)
	for _, v := range lk {
		if linkStatusCode(v) == codeUnmapped {
			miss = append(miss, v.String())
		}
	}
	check("LinkStatus", len(lk), miss)

	if got := pathStatusCode(model.PathStatus(99)); got != codeUnmapped {
		t.Errorf("unmapped PathStatus = %d, want %d", got, codeUnmapped)
	}
}

// hostping.status_code.info is on the first collection, before any report,
// with one series per (metric, code).
func TestStatusCodeInfo(t *testing.T) {
	_, reader := newTestTelemetry(t)
	info := gaugeInt(t, collect(t, reader), "hostping.status_code.info")
	want := map[string]int64{}
	for metric, codes := range wantStatusCodes {
		label := "status"
		if metric == "hostping.path.uncertain_reason_code" || metric == "hostping.path.latency_reason_code" {
			label = "reason"
		}
		for code, name := range codes {
			// attribute.Set encodes keys in sorted order.
			k := fmt.Sprintf("code=%d,metric=%s,%s=%s", code, metric, label, name)
			want[k] = 1
		}
	}
	assertGaugeSeries(t, "status_code.info", info, want)
}

// Each path and link is one series per code gauge, whatever its state, and a
// report rewrites every path and link it carries.
func TestRecordReportCodeSeriesPerPath(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordReport(ctx, model.Report{
		Paths: []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu0", Status: model.PathAbnormal, LatencyStatus: model.LatencyAbnormal, LatencyReason: model.LatencyByBaseline},
			{RNIC: "rnic0", Endpoint: "gpu1", Status: model.PathUncertain, UncertainReason: model.UncertainRNICBusy, LatencyReason: model.LatencyBusy},
		},
		Links: []model.LinkState{{ID: "rp.0", Status: model.LinkGray}, {ID: "rp.1", Status: model.LinkAbnormal}},
	})
	tel.RecordReport(ctx, model.Report{
		Paths: []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu0", Status: model.PathNormal, LatencyStatus: model.LatencyNormal, LatencyReason: model.LatencyByBaseline},
			{RNIC: "rnic0", Endpoint: "gpu1", Status: model.PathNormal, LatencyStatus: model.LatencyNormal, LatencyReason: model.LatencyBySiblings},
		},
		Links: []model.LinkState{{ID: "rp.0", Status: model.LinkNormal}, {ID: "rp.1", Status: model.LinkNormal}},
	})
	ms := collect(t, reader)
	const p0, p1 = "endpoint=gpu0,rnic=rnic0", "endpoint=gpu1,rnic=rnic0"
	assertGaugeSeries(t, "path.status_code", gaugeInt(t, ms, "hostping.path.status_code"), map[string]int64{p0: 1, p1: 1})
	assertGaugeSeries(t, "path.uncertain_reason_code", gaugeInt(t, ms, "hostping.path.uncertain_reason_code"), map[string]int64{p0: 0, p1: 0})
	assertGaugeSeries(t, "path.latency_status_code", gaugeInt(t, ms, "hostping.path.latency_status_code"), map[string]int64{p0: 1, p1: 1})
	assertGaugeSeries(t, "path.latency_reason_code", gaugeInt(t, ms, "hostping.path.latency_reason_code"), map[string]int64{p0: 1, p1: 2})
	assertGaugeSeries(t, "link.status_code", gaugeInt(t, ms, "hostping.link.status_code"), map[string]int64{"link=rp.0": 1, "link=rp.1": 1})
}

// A path or link missing from a later report keeps its last code until
// restart, as the one-hot gauges kept their last 0/1 and every other path and
// link gauge keeps its last value: a busy run probes a reduced set, and the
// report of the current run is authoritative (docs/operations.md, "Kept until
// restart").
func TestRecordReportCodeOfMissingPathKept(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordReport(ctx, model.Report{
		Paths: []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu0", Status: model.PathNormal},
			{RNIC: "rnic0", Endpoint: "gpu1", Status: model.PathAbnormal},
		},
		Links: []model.LinkState{{ID: "rp.0", Status: model.LinkNormal}, {ID: "rp.1", Status: model.LinkAbnormal}},
	})
	tel.RecordReport(ctx, model.Report{
		Paths: []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "gpu0", Status: model.PathUncertain, UncertainReason: model.UncertainRNICBusy}},
		Links: []model.LinkState{{ID: "rp.0", Status: model.LinkUncertain}},
	})
	ms := collect(t, reader)
	const p0, p1 = "endpoint=gpu0,rnic=rnic0", "endpoint=gpu1,rnic=rnic0"
	assertGaugeSeries(t, "path.status_code", gaugeInt(t, ms, "hostping.path.status_code"), map[string]int64{p0: 2, p1: 3})
	assertGaugeSeries(t, "path.uncertain_reason_code", gaugeInt(t, ms, "hostping.path.uncertain_reason_code"), map[string]int64{p0: 2, p1: 0})
	assertGaugeSeries(t, "link.status_code", gaugeInt(t, ms, "hostping.link.status_code"), map[string]int64{"link=rp.0": 2, "link=rp.1": 3})
	if got := gaugeInt(t, ms, "hostping.path.abnormal")[p1]; got != 1 {
		t.Errorf("path.abnormal of the missing path = %d, want its last 1, like path.status_code", got)
	}
}

// The names and labels a PromQL query uses: hostping_path_status_code{rnic,
// endpoint} and hostping_status_code_info{metric,code,status|reason}, with no
// unit suffix on either.
func TestStatusCodePrometheusNames(t *testing.T) {
	reg := promclient.NewRegistry()
	exp, err := prometheus.New(prometheus.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("prometheus exporter: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tel, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tel.RecordReport(context.Background(), model.Report{
		Paths: []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "mem0", Status: model.PathUncertain, UncertainReason: model.UncertainCPUBusy}},
		Links: []model.LinkState{{ID: "mem.0", Status: model.LinkGray}},
	})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	values := map[string]map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			key := ""
			for _, l := range m.GetLabel() {
				if l.GetName() == "otel_scope_name" || l.GetName() == "otel_scope_version" || l.GetName() == "otel_scope_schema_url" {
					continue
				}
				key += l.GetName() + "=" + l.GetValue() + ","
			}
			if values[mf.GetName()] == nil {
				values[mf.GetName()] = map[string]float64{}
			}
			values[mf.GetName()][key] = m.GetGauge().GetValue()
		}
	}
	for name, want := range map[string]map[string]float64{
		"hostping_path_status_code":           {"endpoint=mem0,rnic=rnic0,": 2},
		"hostping_path_uncertain_reason_code": {"endpoint=mem0,rnic=rnic0,": 7},
		"hostping_path_latency_status_code":   {"endpoint=mem0,rnic=rnic0,": 2},
		"hostping_path_latency_reason_code":   {"endpoint=mem0,rnic=rnic0,": 0},
		"hostping_path_latency_streak":        {"endpoint=mem0,rnic=rnic0,": 0},
		"hostping_link_status_code":           {"link=mem.0,": 4},
	} {
		if fmt.Sprint(values[name]) != fmt.Sprint(want) {
			t.Errorf("%s = %v, want %v", name, values[name], want)
		}
	}
	info := values["hostping_status_code_info"]
	if len(info) != 28 {
		t.Errorf("hostping_status_code_info has %d series, want 28: %v", len(info), info)
	}
	if v, ok := info["code=7,metric=hostping.path.uncertain_reason_code,reason=cpu_busy,"]; !ok || v != 1 {
		t.Errorf("hostping_status_code_info has no cpu_busy reason row at 1: %v", info)
	}
	for _, old := range []string{"hostping_path_status", "hostping_path_latency_status", "hostping_link_status"} {
		if _, ok := values[old]; ok {
			t.Errorf("%s is still exported", old)
		}
	}
}

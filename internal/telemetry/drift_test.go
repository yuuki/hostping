package telemetry

import (
	"context"
	"maps"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

func driftPaths() []model.PathDrift {
	return []model.PathDrift{
		{RNIC: "rnic0", Endpoint: "gpu0", Status: model.DriftDrifted, Changes: []model.DriftChange{
			{Signal: model.DriftBandwidth, Change: 0.071},
			{Signal: model.DriftLatencyP50, Change: 0.002},
			{Signal: model.DriftLatencyP99, Change: -0.01},
		}},
		{RNIC: "rnic0", Endpoint: "gpu1", Status: model.DriftStable, Changes: []model.DriftChange{
			{Signal: model.DriftBandwidth, Change: 0},
			{Signal: model.DriftLatencyP50, Change: 0},
			{Signal: model.DriftLatencyP99, Change: 0},
		}},
		{RNIC: "rnic0", Endpoint: "mem0", Reason: model.DriftCollectingReference},
	}
}

// Each tracked path is one status and one reason series, and one change
// series per signal once it is judged. An unknown path has a reason and no
// change: there is none, and 0 would read as a path that has not moved.
func TestRecordDrift(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordDrift(context.Background(), driftPaths())
	ms := collect(t, reader)
	const p0, p1, p2 = "endpoint=gpu0,rnic=rnic0", "endpoint=gpu1,rnic=rnic0", "endpoint=mem0,rnic=rnic0"
	assertGaugeSeries(t, "path.drift.status_code", gaugeInt(t, ms, "hostping.path.drift.status_code"), map[string]int64{p0: 3, p1: 1, p2: 2})
	assertGaugeSeries(t, "path.drift.reason_code", gaugeInt(t, ms, "hostping.path.drift.reason_code"), map[string]int64{p0: 0, p1: 0, p2: 1})
	want := map[string]float64{
		"endpoint=gpu0,rnic=rnic0,signal=bandwidth":   0.071,
		"endpoint=gpu0,rnic=rnic0,signal=latency_p50": 0.002,
		"endpoint=gpu0,rnic=rnic0,signal=latency_p99": -0.01,
		"endpoint=gpu1,rnic=rnic0,signal=bandwidth":   0,
		"endpoint=gpu1,rnic=rnic0,signal=latency_p50": 0,
		"endpoint=gpu1,rnic=rnic0,signal=latency_p99": 0,
	}
	if got := gaugeFloat(t, ms, "hostping.path.drift.change_ratio"); !maps.Equal(got, want) {
		t.Fatalf("path.drift.change_ratio = %v, want %v", got, want)
	}
}

// A path that becomes unknown again says so in its status and reason; its
// change keeps the last judged value, as every gauge of a path keeps its
// last sample, and is not rewritten as 0.
func TestRecordDriftUnknownKeepsNoFalseChange(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordDrift(ctx, driftPaths())
	later := driftPaths()
	// A stale reason on a judged path and stale changes on an unknown one
	// are not exported.
	later[0] = model.PathDrift{RNIC: "rnic0", Endpoint: "gpu0", Reason: model.DriftFewRecentDays, Changes: later[0].Changes}
	later[0].Changes[0].Change = 0.5
	later[1].Reason = model.DriftClockBehind
	tel.RecordDrift(ctx, later)
	ms := collect(t, reader)
	const p0, p1 = "endpoint=gpu0,rnic=rnic0", "endpoint=gpu1,rnic=rnic0"
	if got := gaugeInt(t, ms, "hostping.path.drift.status_code"); got[p0] != 2 || got[p1] != 1 {
		t.Fatalf("status codes = %v", got)
	}
	if got := gaugeInt(t, ms, "hostping.path.drift.reason_code"); got[p0] != 2 || got[p1] != 0 {
		t.Fatalf("reason codes = %v, want few_recent_days on the unknown path and none on the stable one", got)
	}
	if got := gaugeFloat(t, ms, "hostping.path.drift.change_ratio")["endpoint=gpu0,rnic=rnic0,signal=bandwidth"]; got != 0.071 {
		t.Fatalf("change of the unknown path = %v, want its last judged 0.071", got)
	}
}

// Without --drift-tracking nothing is recorded, and no drift series exists.
func TestNoDriftSeriesWithoutTracking(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	tel.RecordReport(context.Background(), fullReport())
	tel.RecordDrift(context.Background(), nil)
	var nilTel *Telemetry
	nilTel.RecordDrift(context.Background(), driftPaths())
	ms := collect(t, reader)
	for _, name := range []string{"hostping.path.drift.status_code", "hostping.path.drift.reason_code", "hostping.path.drift.change_ratio"} {
		if _, ok := ms[name]; ok {
			t.Errorf("%s exported without a tracked path", name)
		}
	}
}

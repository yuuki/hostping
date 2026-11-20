package telemetry

import (
	"context"
	"math"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// A passive sample adds its link-error differences to the counter, zero
// included, and writes the budget where the link gave one. A link the sample
// could not difference is not in it, and so gets no series: unknown is not 0.
func TestRecordPCIe(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordPCIe(ctx, &model.PCIePassive{Sampled: true, Errors: []model.PCIeLinkErrors{
		{Link: "pcie.gpu0", Correctable: 17, Uncorrectable: 1, BudgetPerMinute: 15.36},
		{Link: "pcie.rnic0", BudgetPerMinute: 30.72},
		{Link: "rp.0", Correctable: 4},
	}})
	tel.RecordPCIe(ctx, &model.PCIePassive{Sampled: true, Errors: []model.PCIeLinkErrors{
		{Link: "pcie.gpu0", Correctable: 3, BudgetPerMinute: 7.68},
	}})
	// The polls between two samples repeat the state and must add nothing.
	tel.RecordPCIe(ctx, &model.PCIePassive{Errors: []model.PCIeLinkErrors{{Link: "pcie.gpu0", Correctable: 1000}}})
	tel.RecordPCIe(ctx, nil)

	ms := collect(t, reader)
	errs := sumInt(t, ms, "hostping.pcie.link.errors")
	want := map[string]int64{
		"link=pcie.gpu0,severity=correctable":    20,
		"link=pcie.gpu0,severity=uncorrectable":  1,
		"link=pcie.rnic0,severity=correctable":   0,
		"link=pcie.rnic0,severity=uncorrectable": 0,
		"link=rp.0,severity=correctable":         4,
		"link=rp.0,severity=uncorrectable":       0,
	}
	if len(errs) != len(want) {
		t.Fatalf("hostping.pcie.link.errors series %v, want %v", errs, want)
	}
	for k, v := range want {
		if errs[k] != v {
			t.Fatalf("hostping.pcie.link.errors{%s} = %d, want %d (all: %v)", k, errs[k], v, errs)
		}
	}
	budget := gaugeFloat(t, ms, "hostping.pcie.link.error_budget")
	if len(budget) != 2 || budget["link=pcie.gpu0"] != 7.68 || budget["link=pcie.rnic0"] != 30.72 {
		t.Fatalf("hostping.pcie.link.error_budget %v, want the last budget of the two links that have one", budget)
	}
}

func TestCountValueSaturates(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want int64
	}{{0, 0}, {41, 41}, {math.MaxInt64, math.MaxInt64}, {math.MaxInt64 + 1, math.MaxInt64}, {math.MaxUint64, math.MaxInt64}} {
		if got := countValue(tc.in); got != tc.want {
			t.Fatalf("countValue(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A passive report has no engine run behind it: its causes are counted and
// nothing else is written, so the run counters and the path, link and idle
// gauges keep what the last run left.
func TestRecordPassiveCountsOnlyDiagnoses(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordPassive(ctx, model.Report{
		Trigger: model.TriggerPassive, Mode: model.ModePassive, HostIdle: true,
		Paths: []model.PathMeasurement{}, Links: []model.LinkState{},
		Causes: []model.RootCause{
			{Code: model.CausePCIeLinkErrors, Confidence: model.ConfidenceMedium},
			{Code: model.CauseGPUPCIeFailure, Confidence: model.ConfidenceMedium},
			{Code: model.CausePCIeLinkErrors, Confidence: model.ConfidenceMedium},
		},
	})
	ms := collect(t, reader)
	diag := sumInt(t, ms, "hostping.diagnosis.count")
	if diag["code=pcie_link_errors,confidence=medium"] != 2 || diag["code=gpu_pcie_failure,confidence=medium"] != 1 || len(diag) != 2 {
		t.Fatalf("hostping.diagnosis.count %v", diag)
	}
	for _, name := range []string{"hostping.engine.run.count", "hostping.host.idle", "hostping.endpoint.skipped", "hostping.path.status_code", "hostping.link.status_code"} {
		if _, ok := ms[name]; ok {
			t.Fatalf("%s written by a passive report", name)
		}
	}
}

// A pcie_link run sweeps a few RNICs. The links it tested are rewritten; the
// rest keep the verdict of the last run that tested them instead of all
// turning uncertain. Every other run rewrites every link, as before.
func TestRecordReportPCIeLinkRunKeepsUntestedLinks(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	ctx := context.Background()
	tel.RecordReport(ctx, model.Report{Trigger: model.TriggerIdle, Mode: model.ModeIdle, Links: []model.LinkState{
		{ID: "pcie.gpu0", Status: model.LinkAbnormal, AbnormalCount: 2, Tested: true},
		{ID: "pcie.gpu3", Status: model.LinkNormal, Tested: true},
	}})
	targeted := []model.LinkState{
		{ID: "pcie.gpu0", Status: model.LinkUncertain},
		{ID: "pcie.gpu3", Status: model.LinkAbnormal, AbnormalCount: 1, Tested: true},
	}
	tel.RecordReport(ctx, model.Report{Trigger: model.TriggerPCIeLink, Mode: model.ModeBusy, Links: targeted})
	ms := collect(t, reader)
	assertGaugeSeries(t, "hostping.link.status_code", gaugeInt(t, ms, "hostping.link.status_code"), map[string]int64{
		"link=pcie.gpu0": linkStatusCode(model.LinkAbnormal),
		"link=pcie.gpu3": linkStatusCode(model.LinkAbnormal),
	})
	assertGaugeSeries(t, "hostping.link.abnormal_count", gaugeInt(t, ms, "hostping.link.abnormal_count"), map[string]int64{
		"link=pcie.gpu0": 2,
		"link=pcie.gpu3": 1,
	})

	// The same link states in a run of any other trigger rewrite both.
	tel.RecordReport(ctx, model.Report{Trigger: model.TriggerPFC, Mode: model.ModeBusy, Links: targeted})
	ms = collect(t, reader)
	assertGaugeSeries(t, "hostping.link.status_code", gaugeInt(t, ms, "hostping.link.status_code"), map[string]int64{
		"link=pcie.gpu0": linkStatusCode(model.LinkUncertain),
		"link=pcie.gpu3": linkStatusCode(model.LinkAbnormal),
	})
}

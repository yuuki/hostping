package telemetry

import (
	"context"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// fig2Report is a full-mesh report of the Fig.2 host: every RNIC-endpoint
// path, every link and every RNIC pair, the most a run of that host writes.
func fig2Report() model.Report {
	g := topology.Fig2()
	rep := model.Report{Trigger: model.TriggerIdle, Mode: model.ModeIdle, HostIdle: true}
	for _, n := range g.RNICs {
		for _, e := range g.Endpoints {
			rep.Paths = append(rep.Paths, model.PathMeasurement{
				RNIC: n.ID, Endpoint: e.ID, EndpointKind: e.Kind,
				BandwidthBPS: 190e9, SmallLatNsP50: 1800, Status: model.PathNormal,
				Verbs: model.VerbNs{MRReg: 40_000, QPSetup: 900_000},
			})
		}
	}
	for _, l := range g.Links {
		rep.Links = append(rep.Links, model.LinkState{ID: l.ID, Status: model.LinkNormal, Utilization: ptr(0.1)})
	}
	for _, a := range g.RNICs {
		for _, b := range g.RNICs {
			if a.ID != b.ID {
				rep.Mutual = append(rep.Mutual, model.MutualMeasurement{Src: a.ID, Dst: b.ID, Connected: true, RTTnsP50: 4000, Sent: 64, Recv: 64})
			}
		}
	}
	return rep
}

// BenchmarkRecordReport times writing one full-mesh report to the
// instruments, which the daemon does once per engine run.
func BenchmarkRecordReport(b *testing.B) {
	tel, _ := newTestTelemetry(b)
	rep := fig2Report()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		tel.RecordReport(ctx, rep)
	}
}

// BenchmarkRecordRates times one monitor poll of eight RNICs, which the
// daemon records every --metric-interval (1s).
func BenchmarkRecordRates(b *testing.B) {
	tel, _ := newTestTelemetry(b)
	rates := make([]model.RNICRates, 8)
	for i := range rates {
		rates[i] = model.RNICRates{Device: "rnic" + string(rune('0'+i)), ThroughputBPS: 150e9, PauseDurationRatio: 0.01, DropDelta: 3, PauseKnown: true, DropKnown: true}
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		tel.RecordRates(ctx, rates)
	}
}

package analyzer_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

func runSNCFault(t *testing.T, fault string) model.Report {
	t.Helper()
	g := topology.Fig2SNC()
	w := sim.New(g)
	if err := w.ApplyFault(fault); err != nil {
		t.Fatal(err)
	}
	e := engine.New(config.Defaults(), g, sim.Prober{World: w})
	rep, err := e.Run(context.Background(), engine.RunRequest{
		Trigger: model.TriggerManual, Mode: model.ModeIdle, Util: w.LinkUtil, PCIe: w.PCIe,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func causeLinks(rep model.Report, code string) []string {
	var out []string
	for _, c := range rep.Causes {
		if c.Code == code {
			out = append(out, c.Links...)
		}
	}
	return out
}

// On a sub-NUMA-clustered host a fault in the fabric between two
// NUMA nodes of one package is not an inter-socket bus fault.
func TestSNCMeshFaultIsNotUPI(t *testing.T) {
	rep := runSNCFault(t, "mesh:0-1")
	if got := causeLinks(rep, model.CauseSNCMeshFailure); !reflect.DeepEqual(got, []string{"mesh.0-1"}) {
		t.Fatalf("want snc_mesh_failure on mesh.0-1, got causes %+v", rep.Causes)
	}
	for _, bad := range []string{model.CauseUPIFailure, model.CauseOverloadedUPI, model.CauseCPURootPortFailure, model.CauseMemoryChannelFailure} {
		if hasCause(rep, bad) {
			t.Fatalf("extra cause %s in %v", bad, codes(rep))
		}
	}
}

func TestSNCUPIFaultStaysUPI(t *testing.T) {
	rep := runSNCFault(t, "upi")
	if got := causeLinks(rep, model.CauseUPIFailure); !reflect.DeepEqual(got, []string{"upi.0-1"}) {
		t.Fatalf("want upi_failure on upi.0-1, got causes %+v", rep.Causes)
	}
	if hasCause(rep, model.CauseSNCMeshFailure) || hasCause(rep, model.CauseCPURootPortFailure) {
		t.Fatalf("extra causes in %v", codes(rep))
	}
}

func TestSNCHealthy(t *testing.T) {
	rep := runSNCFault(t, "")
	if len(rep.Causes) != 0 {
		t.Fatalf("healthy fig2-snc: causes %+v", rep.Causes)
	}
}

func TestSNCMeshFaultNeedsMeshLink(t *testing.T) {
	w := sim.New(topology.Fig2())
	if err := w.ApplyFault("mesh:0-1"); err == nil {
		t.Fatal("Fig2 has no mesh link; the fault must be rejected")
	}
}

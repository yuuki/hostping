package engine

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// mutualRecorder is a flat prober that also lists the mutual pairs it was
// asked for.
type mutualRecorder struct {
	flatProber
	pairs []string
}

func (p *mutualRecorder) Mutual(ctx context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	p.pairs = append(p.pairs, src.ID+"<->"+dst.ID)
	return p.flatProber.Mutual(ctx, src, dst)
}

// sweptRNICs is the RNICs the prober read a loopback on, sorted.
func (r *probeRecorder) sweptRNICs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	for _, key := range r.order {
		rnic, _, _ := strings.Cut(key, "->")
		seen[rnic] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func sustained(link string) model.PCIeLinkFinding {
	return model.PCIeLinkFinding{
		Link: link, State: model.PCIeLinkSustained, ErrorsPerMinute: 412, BudgetPerMinute: 15.36, OverBudgetWindows: 3, WindowSeconds: 60,
		Ends: []model.PCIeLinkEnd{{BDF: "0000:39:00.0", Correctable: map[string]uint64{"BadTLP": 412}}},
	}
}

// A pcie_link run probes the RNICs it is given and no other: their busy
// probe set, the mutual pairs among them, nothing else. The report holds the
// paths it measured, every link (the untested ones uncertain), and the
// passive findings it was handed.
func TestOnlyRNICsRestrictsTheSweep(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	findings := []model.PCIeLinkFinding{sustained("pcie.gpu3")}

	p := &mutualRecorder{}
	e := quietEngine(cfg, g, p)
	e.Gray = map[string]int{"mem.1": 2, "upi.0": 1}
	rep := runWith(t, e, RunRequest{
		Trigger: model.TriggerPCIeLink, Mode: model.ModeBusy, BusyRNICs: map[string]bool{"rnic1": true},
		OnlyRNICs: map[string]bool{"rnic1": true}, PCIeLinks: findings,
	})
	if got := p.sweptRNICs(); !slices.Equal(got, []string{"rnic1"}) {
		t.Fatalf("loopbacks on %v, want rnic1 only", got)
	}
	// rnic1's busy probe set: both memory nodes and its two affinitive GPUs.
	var eps []string
	for _, m := range rep.Paths {
		if m.RNIC != "rnic1" {
			t.Fatalf("report holds a path of %s", m.RNIC)
		}
		eps = append(eps, m.Endpoint)
	}
	sort.Strings(eps)
	if !slices.Equal(eps, []string{"gpu2", "gpu3", "mem0", "mem1"}) {
		t.Fatalf("measured endpoints %v", eps)
	}
	if len(p.pairs) != 0 {
		t.Fatalf("mutual probes %v with one RNIC in the sweep", p.pairs)
	}
	if len(rep.Links) != len(g.Links) {
		t.Fatalf("%d link states, want every one of the graph's %d", len(rep.Links), len(g.Links))
	}
	for _, l := range rep.Links {
		if strings.HasSuffix(l.ID, "rnic0") || strings.HasSuffix(l.ID, "rnic2") || strings.HasSuffix(l.ID, "gpu7") {
			if l.Tested || l.Status != model.LinkUncertain {
				t.Fatalf("link %s %+v, want untested and uncertain: its RNIC was not swept", l.ID, l)
			}
		}
	}
	if !slices.Equal(linkIDs(rep.PCIeLinks), []string{"pcie.gpu3"}) {
		t.Fatalf("report findings %+v", rep.PCIeLinks)
	}
	// rnic1 is loaded and its paths read level, so the run decided no path
	// and tested no link: every gray streak carries over untouched.
	if e.Gray["mem.1"] != 2 || e.Gray["upi.0"] != 1 {
		t.Fatalf("gray streaks %v, want mem.1=2 and upi.0=1 as before the run", e.Gray)
	}

	// Two RNICs in the sweep: the one pair among them is probed.
	p = &mutualRecorder{}
	runWith(t, quietEngine(cfg, g, p), RunRequest{
		Mode: model.ModeBusy, BusyRNICs: map[string]bool{"rnic1": true},
		OnlyRNICs: map[string]bool{"rnic0": true, "rnic1": true},
	})
	if got := p.sweptRNICs(); !slices.Equal(got, []string{"rnic0", "rnic1"}) {
		t.Fatalf("loopbacks on %v, want rnic0 and rnic1", got)
	}
	if !slices.Equal(p.pairs, []string{"rnic0<->rnic1"}) {
		t.Fatalf("mutual pairs %v, want the one among the swept RNICs", p.pairs)
	}

	// Without a restriction the run is the one it always was.
	p = &mutualRecorder{}
	rep = runWith(t, quietEngine(cfg, g, p), RunRequest{Mode: model.ModeBusy, BusyRNICs: map[string]bool{"rnic1": true}})
	if got := p.sweptRNICs(); len(got) != len(g.RNICs) {
		t.Fatalf("loopbacks on %v without OnlyRNICs, want every RNIC", got)
	}
	if len(p.pairs) != len(g.MutualPairs(model.MutualScopeRail)) {
		t.Fatalf("%d mutual pairs without OnlyRNICs, want %d", len(p.pairs), len(g.MutualPairs(model.MutualScopeRail)))
	}
	if rep.PCIeLinks != nil {
		t.Fatalf("report findings %+v without any", rep.PCIeLinks)
	}

	// A restriction that names no RNIC of the graph measures nothing, which
	// is an error like any run without a measurement.
	if _, err := quietEngine(cfg, g, &mutualRecorder{}).Run(context.Background(), RunRequest{Mode: model.ModeBusy, OnlyRNICs: map[string]bool{}}); err == nil {
		t.Fatal("a run that sweeps no RNIC returned a report")
	}
}

func linkIDs(fs []model.PCIeLinkFinding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Link)
	}
	return out
}

// The point of the targeted run: on a loaded host, a GPU link the passive
// counters name is probed by its own RNIC alone, and the busy sibling
// comparison on that RNIC says whether the link is also a bottleneck.
func TestPCIeLinkRunConfirmsABottleneck(t *testing.T) {
	g := topology.Fig2()
	req := func(w *sim.World) RunRequest {
		return RunRequest{
			Trigger: model.TriggerPCIeLink, Mode: model.ModeBusy, BusyRNICs: w.Busy,
			PCIe: w.PCIeConfigs(), OnlyRNICs: map[string]bool{"rnic1": true},
			PCIeLinks: []model.PCIeLinkFinding{sustained("pcie.gpu3")},
		}
	}
	world := func(fault string) *sim.World {
		w := sim.New(g)
		if err := w.ApplyFault(fault); err != nil {
			t.Fatal(err)
		}
		return w
	}

	// The link errs and has lost bandwidth: the probes name the GPU, and the
	// counters, still medium, point at that cause.
	w := world("gpu_pcie:gpu3;busy:rnic1")
	rep := runWith(t, New(config.Defaults(), g, sim.Prober{World: w}), req(w))
	var gpu, link *model.RootCause
	for i := range rep.Causes {
		switch rep.Causes[i].Code {
		case model.CauseGPUPCIeFailure:
			gpu = &rep.Causes[i]
		case model.CausePCIeLinkErrors:
			link = &rep.Causes[i]
		}
	}
	if gpu == nil || gpu.Confidence != model.ConfidenceHigh || !slices.Equal(gpu.Endpoints, []string{"gpu3"}) {
		t.Fatalf("causes %+v, want gpu_pcie_failure [high] on gpu3 from the sibling comparison", rep.Causes)
	}
	if link == nil || link.Confidence != model.ConfidenceMedium || !slices.Equal(link.Links, []string{"pcie.gpu3"}) ||
		!strings.HasSuffix(link.Evidence, "; the same run also names the link (gpu_pcie_failure)") {
		t.Fatalf("causes %+v, want pcie_link_errors [medium] on pcie.gpu3 pointing at gpu_pcie_failure", rep.Causes)
	}

	// The link errs but carries its bandwidth: the counters stay medium and
	// the probes name nothing.
	w = world("busy:rnic1")
	rep = runWith(t, New(config.Defaults(), g, sim.Prober{World: w}), req(w))
	if codes := causeCodes(rep); !slices.Equal(codes, []string{model.CausePCIeLinkErrors}) {
		t.Fatalf("causes %v, want pcie_link_errors only", codes)
	}
	if rep.Causes[0].Confidence != model.ConfidenceMedium {
		t.Fatalf("cause %+v, want medium: no bottleneck was measured", rep.Causes[0])
	}
	for _, m := range rep.Paths {
		if m.Status == model.PathAbnormal {
			t.Fatalf("%s->%s abnormal on a healthy link", m.RNIC, m.Endpoint)
		}
	}
}

// An unloaded RNIC in a pcie_link run is judged against the idle baseline,
// and a slow window over its batch reads as a failed link. The references
// swept with it are what lets the paired bandwidth recheck clear that; a run
// of the RNIC alone has nothing to read against and keeps the false verdict.
func TestPCIeLinkRunNeedsItsReferences(t *testing.T) {
	g := topology.Fig2()
	busy := map[string]bool{"rnic0": true}
	run := func(only ...string) model.Report {
		p := newBWWindowProber(sim.New(g), memWindow(sweepOnly, "rnic1"))
		sweep := map[string]bool{}
		for _, id := range only {
			sweep[id] = true
		}
		rep, _ := runBW(t, g, p, RunRequest{Trigger: model.TriggerPCIeLink, Mode: model.ModeBusy, BusyRNICs: busy, OnlyRNICs: sweep})
		return rep
	}

	alone := run("rnic1")
	if !hasAbnormal(alone, "rnic1") {
		t.Fatalf("precondition: rnic1 swept alone should keep its slow memory paths abnormal: %+v", alone.Paths)
	}
	with := run("rnic1", "rnic2", "rnic3")
	if hasAbnormal(with, "rnic1") {
		t.Fatalf("rnic1 still abnormal with two references in the sweep: %+v", with.Causes)
	}
	if m := pathOf(t, with, "rnic1", "mem0"); m.BWRecheck == nil || m.BWRecheck.Outcome != model.BWRecheckCleared {
		t.Fatalf("rnic1->mem0 %+v, want the paired recheck to have cleared it", m)
	}
	if len(with.Causes) != 0 {
		t.Fatalf("causes %+v from a slow window", with.Causes)
	}
}

func hasAbnormal(rep model.Report, rnic string) bool {
	for _, m := range rep.Paths {
		if m.RNIC == rnic && m.Status == model.PathAbnormal {
			return true
		}
	}
	return false
}

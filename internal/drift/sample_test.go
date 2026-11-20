package drift

import (
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func cleanPath() model.PathMeasurement {
	return model.PathMeasurement{
		RNIC: "rnic0", Endpoint: "gpu0", EndpointKind: model.EndpointGPU,
		SmallLatNsP50: 1400, SmallLatNsP99: 1600, BandwidthBPS: 200e9,
		Status: model.PathNormal, LatencyStatus: model.LatencyNormal, LatencyReason: model.LatencyBySiblings,
	}
}

// Only a reading the run judged clean enters the history: a fault must not
// become the reference, and a reading taken in a state that says nothing
// about the path must not move it.
func TestClean(t *testing.T) {
	cases := []struct {
		name string
		edit func(*model.PathMeasurement)
		want bool
	}{
		{"normal", func(*model.PathMeasurement) {}, true},
		{"latency unjudged for lack of siblings", func(p *model.PathMeasurement) {
			p.LatencyStatus, p.LatencyReason = model.LatencyUnknown, model.LatencyFewSiblings
		}, true},
		{"uncertain: non-affinitive GPU", func(p *model.PathMeasurement) {
			p.Status, p.UncertainReason = model.PathUncertain, model.UncertainNonAffinitive
		}, true},
		{"uncertain: below its ceiling", func(p *model.PathMeasurement) {
			p.Status, p.UncertainReason = model.PathUncertain, model.UncertainBelowCeiling
		}, true},
		{"uncertain: no ceiling", func(p *model.PathMeasurement) {
			p.Status, p.UncertainReason = model.PathUncertain, model.UncertainNoCeiling
		}, true},

		{"abnormal", func(p *model.PathMeasurement) { p.Status = model.PathAbnormal }, false},
		{"uncertain: host shift", func(p *model.PathMeasurement) {
			p.Status, p.UncertainReason = model.PathUncertain, model.UncertainHostShift
		}, false},
		{"uncertain: CPU busy", func(p *model.PathMeasurement) {
			p.Status, p.UncertainReason = model.PathUncertain, model.UncertainCPUBusy
		}, false},
		{"uncertain: RNIC busy", func(p *model.PathMeasurement) {
			p.Status, p.UncertainReason = model.PathUncertain, model.UncertainRNICBusy
		}, false},
		{"uncertain: no path", func(p *model.PathMeasurement) {
			p.Status, p.UncertainReason = model.PathUncertain, model.UncertainNoPath
		}, false},
		{"uncertain without a reason", func(p *model.PathMeasurement) { p.Status = model.PathUncertain }, false},
		{"bandwidth rechecked and cleared", func(p *model.PathMeasurement) {
			p.BWRecheck = &model.BandwidthRecheck{Outcome: model.BWRecheckCleared}
		}, false},
		{"busy recheck", func(p *model.PathMeasurement) { p.BusyRecheck = &model.BusyRecheck{} }, false},
		{"latency abnormal", func(p *model.PathMeasurement) { p.LatencyStatus = model.LatencyAbnormal }, false},
		{"latency cleared by the recheck", func(p *model.PathMeasurement) { p.LatencyReason = model.LatencyRecheckCleared }, false},
		{"latency cleared as a node-wide rise", func(p *model.PathMeasurement) { p.LatencyReason = model.LatencyLocalityShift }, false},
		{"latency under CPU load", func(p *model.PathMeasurement) {
			p.LatencyStatus, p.LatencyReason = model.LatencyUnknown, model.LatencyCPUBusy
		}, false},
		{"latency of a busy run", func(p *model.PathMeasurement) {
			p.LatencyStatus, p.LatencyReason = model.LatencyUnknown, model.LatencyBusy
		}, false},
		{"no bandwidth", func(p *model.PathMeasurement) { p.BandwidthBPS = 0 }, false},
		{"no p50", func(p *model.PathMeasurement) { p.SmallLatNsP50 = 0 }, false},
		{"no p99", func(p *model.PathMeasurement) { p.SmallLatNsP99 = 0 }, false},
		{"p99 below p50", func(p *model.PathMeasurement) { p.SmallLatNsP99 = 1000 }, false},
		{"bandwidth beyond what the history holds", func(p *model.PathMeasurement) { p.BandwidthBPS = 1 << 63 }, false},
	}
	for _, c := range cases {
		p := cleanPath()
		c.edit(&p)
		if got := Clean(p); got != c.want {
			t.Errorf("%s: Clean = %v, want %v", c.name, got, c.want)
		}
	}
}

func idleReport() model.Report {
	mem := cleanPath()
	mem.Endpoint, mem.EndpointKind = "mem0", model.EndpointMemory
	bad := cleanPath()
	bad.Endpoint, bad.Status = "gpu1", model.PathAbnormal
	return model.Report{
		Trigger: model.TriggerIdle, Mode: model.ModeIdle, HostIdle: true,
		Paths:   []model.PathMeasurement{cleanPath(), mem, bad},
		CPULoad: []model.CPULoad{{NUMA: 0, Socket: 0, Busy: 0.1}},
	}
}

// Only an idle-trigger run on an idle host with no loaded CPU package feeds
// the history.
func TestSamplesOnlyFromCleanIdleRuns(t *testing.T) {
	if got := Samples(idleReport(), nil); len(got) != 2 || got[0].Endpoint != "gpu0" || got[1].Endpoint != "mem0" {
		t.Fatalf("idle run: %+v, want the two clean paths", got)
	}
	for name, edit := range map[string]func(*model.Report){
		"busy run":           func(r *model.Report) { r.Trigger, r.Mode, r.HostIdle = model.TriggerPFC, model.ModeBusy, false },
		"periodic run":       func(r *model.Report) { r.Trigger, r.Mode, r.HostIdle = model.TriggerPeriodic, model.ModeBusy, false },
		"mutual alarm, idle": func(r *model.Report) { r.Trigger = model.TriggerMutualTimeout },
		"manual run":         func(r *model.Report) { r.Trigger = model.TriggerManual },
		"host not idle":      func(r *model.Report) { r.HostIdle = false },
		"a CPU package loaded": func(r *model.Report) {
			r.CPULoad = append(r.CPULoad, model.CPULoad{NUMA: 1, Socket: 1, Busy: 0.9, High: true})
		},
	} {
		rep := idleReport()
		edit(&rep)
		if got := Samples(rep, nil); got != nil {
			t.Errorf("%s: %d samples, want none", name, len(got))
		}
	}
}

// A temperature is attached to the device it was read for, and one that was
// not read, or that no sensor of the endpoint could give, stays absent.
func TestSamplesTemperatures(t *testing.T) {
	got := Samples(idleReport(), map[string]float64{"rnic0": 55, "gpu0": 61, "mem0": 40, "gpu1": 99})
	if g := got[0]; g.TempRNIC == nil || *g.TempRNIC != 55 || g.TempEndpoint == nil || *g.TempEndpoint != 61 {
		t.Fatalf("gpu path temperatures = %v, %v", g.TempRNIC, g.TempEndpoint)
	}
	if m := got[1]; m.TempRNIC == nil || *m.TempRNIC != 55 || m.TempEndpoint != nil {
		t.Fatalf("memory path temperatures = %v, %v: a memory endpoint has no sensor", m.TempRNIC, m.TempEndpoint)
	}
	got = Samples(idleReport(), map[string]float64{"rnic0": 5000})
	if g := got[0]; g.TempRNIC != nil || g.TempEndpoint != nil {
		t.Fatalf("an implausible or missing temperature was kept: %v, %v", g.TempRNIC, g.TempEndpoint)
	}
	if g := got[0]; g.BandwidthBPS != 200e9 || g.LatP50Ns != 1400 || g.LatP99Ns != 1600 {
		t.Fatalf("reading = %+v", g.Reading)
	}
}

// A path's key is what it physically is, so the history of a path survives
// the renumbering that adding or removing another device causes.
func TestPathKeys(t *testing.T) {
	g := topology.Fig2()
	paths, rnics := Paths(g)
	if len(paths) != len(g.Paths) || len(rnics) != len(g.RNICs) {
		t.Fatalf("%d paths and %d RNIC keys for %d and %d", len(paths), len(rnics), len(g.Paths), len(g.RNICs))
	}
	byID := map[string]PathIdent{}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p.Key] {
			t.Fatalf("key %q names two paths", p.Key)
		}
		seen[p.Key] = true
		byID[p.RNIC+"->"+p.Endpoint] = p
	}
	if p := byID["rnic0->gpu0"]; p.Key != "0000:17:00.0->0000:18:00.0/same_switch" || !p.EndpointSensor {
		t.Fatalf("rnic0->gpu0 = %+v", p)
	}
	if p := byID["rnic0->mem1"]; p.Key != "0000:17:00.0->mem1of2/remote_mem" || p.EndpointSensor {
		t.Fatalf("rnic0->mem1 = %+v", p)
	}
	if rnics[0] != "0000:17:00.0" {
		t.Fatalf("RNIC key %q", rnics[0])
	}

	// The same host with its first RNIC and first GPU excluded: every
	// remaining device is renumbered, and every remaining path keeps its key.
	h := g
	h.RNICs, h.Endpoints, h.Paths = nil, nil, nil
	rename := map[string]string{}
	for _, n := range g.RNICs[1:] {
		id := "rnic" + string(rune('0'+len(h.RNICs)))
		rename[n.ID], n.ID = id, id
		h.RNICs = append(h.RNICs, n)
	}
	gpus := 0
	for _, e := range g.Endpoints[1:] {
		if e.Kind == model.EndpointGPU {
			id := "gpu" + string(rune('0'+gpus))
			rename[e.ID], e.ID = id, id
			gpus++
		} else {
			rename[e.ID] = e.ID
		}
		h.Endpoints = append(h.Endpoints, e)
	}
	for _, p := range g.Paths {
		r, okR := rename[p.RNIC]
		e, okE := rename[p.Endpoint]
		if okR && okE {
			old := byID[p.RNIC+"->"+p.Endpoint].Key
			p.RNIC, p.Endpoint = r, e
			h.Paths = append(h.Paths, p)
			rename[r+"->"+e] = old
		}
	}
	renumbered, _ := Paths(h)
	if len(renumbered) == 0 || len(renumbered) != len(h.Paths) {
		t.Fatalf("%d paths after the exclusion", len(renumbered))
	}
	for _, p := range renumbered {
		if want := rename[p.RNIC+"->"+p.Endpoint]; p.Key != want {
			t.Errorf("%s->%s is now keyed %q, was %q", p.RNIC, p.Endpoint, p.Key, want)
		}
	}
}

// A memory node is keyed with the number of memory nodes of the host: under
// sub-NUMA clustering "mem1" is another node, and its paths start over,
// while the GPU paths, which are the same hardware, keep their history.
func TestPathKeysUnderSubNUMAClustering(t *testing.T) {
	keys := func(name string) map[string]bool {
		g, err := topology.Named(name)
		if err != nil {
			t.Fatal(err)
		}
		paths, _ := Paths(g)
		out := map[string]bool{}
		for _, p := range paths {
			out[p.Key] = true
		}
		return out
	}
	plain, snc := keys("fig2"), keys("fig2-snc")
	gpu, mem := 0, 0
	for k := range plain {
		switch {
		case strings.Contains(k, "->mem"):
			mem++
			if snc[k] {
				t.Errorf("memory path %q keeps its key under sub-NUMA clustering", k)
			}
		case snc[k]:
			gpu++
		}
	}
	if mem == 0 || gpu == 0 {
		t.Fatalf("%d memory paths checked, %d GPU paths keep their key: the test proves nothing", mem, gpu)
	}
}

// Without PCI addresses the key falls back to names, a repeated address is
// told apart by the IDs, and a key the file could not hold is hashed.
func TestPathKeyFallbacks(t *testing.T) {
	g := topology.Graph{
		RNICs: []model.RNIC{
			{ID: "rnic0", Device: "mlx5_0"},
			{ID: "rnic1", Device: "mlx5_1", PCIeBDF: "0000:17:00.0"},
			{ID: "rnic2", Device: "mlx5_2", PCIeBDF: "0000:17:00.0"},
			{ID: "rnic3", Device: strings.Repeat("x", 400) + "\xff"},
			{ID: "rnic4"},
		},
		Endpoints: []model.Endpoint{{ID: "mem0", Kind: model.EndpointMemory}, {ID: "gpu0", Kind: model.EndpointGPU}},
	}
	for _, n := range g.RNICs {
		g.Paths = append(g.Paths, model.Path{RNIC: n.ID, Endpoint: "mem0", Distance: model.DistanceLocalMem})
	}
	g.Paths = append(g.Paths, model.Path{RNIC: "rnic0", Endpoint: "gpu0", Distance: model.DistanceCrossRoot},
		model.Path{RNIC: "rnic9", Endpoint: "mem0"}) // names no RNIC of the graph
	paths, rnics := Paths(g)
	want := []string{
		"mlx5_0->mem0of1/local_mem",
		"0000:17:00.0->mem0of1/local_mem",
		"0000:17:00.0->mem0of1/local_mem#rnic2->mem0",
		"", // hashed, below
		"rnic4->mem0of1/local_mem",
		"mlx5_0->gpu0/cross_root",
	}
	if len(paths) != len(want) {
		t.Fatalf("%d paths, want %d: %+v", len(paths), len(want), paths)
	}
	for i, p := range paths {
		if err := keyOK("paths", p.Key); err != nil {
			t.Errorf("path %d: %v", i, err)
		}
		if want[i] != "" && p.Key != want[i] {
			t.Errorf("path %d key %q, want %q", i, p.Key, want[i])
		}
	}
	if k := paths[3].Key; !strings.HasPrefix(k, "sha256:") || !strings.HasSuffix(k, "->mem0of1/local_mem") {
		t.Errorf("long key %q, want the hash of the RNIC name in its place", k)
	}
	if k := rnics[3]; !strings.HasPrefix(k, "sha256:") {
		t.Errorf("long RNIC key %q, want its hash", k)
	}
}

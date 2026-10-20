package analyzer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// update rewrites the golden files instead of comparing against them:
//
//	go test ./internal/analyzer -run 'TestGolden' -update
var update = flag.Bool("update", false, "rewrite testdata/golden/*.json from the current output")

// goldenDir holds one JSON file per case: the full model.Report of a
// simulated run, byte for byte. The files pin what a refactor of
// internal/analyzer and internal/model must not change: the JSON shape and
// field names of the report, and the exact Summary / Evidence / Confidence
// strings of every root cause.
const goldenDir = "testdata/golden"

// subClassBaseline is a --baseline-file with a learned cross_root sub-class
// for the {rp.0, rp.1} pair of Fig.2, so the report's baselines table carries
// sub_classes and the paths across that pair carry baseline_class.
const subClassBaseline = "testdata/subclass_baseline.json"

// goldenCase is one simulated run. The fault spec is what --fault accepts
// (sim.World.ApplyFault); snc selects the fig2-snc topology, which is the only
// one with a mesh link.
type goldenCase struct {
	name  string
	fault string
	mode  model.RunMode
	snc   bool
	// cpuHigh, when >= 0, reports that socket's NUMA nodes as CPU-busy on
	// the poll that started the run (engine.RunRequest.CPULoad).
	cpuHigh int
	// baselineFile is cfg.BaselineFile for the run.
	baselineFile string
	hostIdle     bool
}

// goldenCases covers every fault kind ApplyFault accepts, plus the healthy
// host, in idle mode and, where the fault is visible from a loaded RNIC, in
// busy mode too. Busy runs load the RNICs the fault touches (busy:...), as
// hostping --sim does when the monitor sees traffic on them.
var goldenCases = []goldenCase{
	// Healthy hosts.
	{name: "healthy_idle", fault: "", mode: model.ModeIdle, cpuHigh: -1, hostIdle: true},
	{name: "healthy_busy", fault: "busy:rnic0,rnic2", mode: model.ModeBusy, cpuHigh: -1},
	{name: "healthy_snc_idle", fault: "", mode: model.ModeIdle, snc: true, cpuHigh: -1, hostIdle: true},

	// NSDI'23 Table 2 link failures, idle.
	{name: "rnic_pcie_idle", fault: "rnic_pcie:rnic0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "gpu_pcie_idle", fault: "gpu_pcie:gpu0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "mem_idle", fault: "mem:mem0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "mem_flap_idle", fault: "mem_flap:mem0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "upi_idle", fault: "upi", mode: model.ModeIdle, cpuHigh: -1},
	{name: "mesh_idle", fault: "mesh:0-1", mode: model.ModeIdle, snc: true, cpuHigh: -1},
	{name: "rp_idle", fault: "rp:rp.0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "acs_idle", fault: "acs", mode: model.ModeIdle, cpuHigh: -1},
	{name: "ats_idle", fault: "ats:rnic0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "slow_start_idle", fault: "slow_start:rnic0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "tx_window_idle", fault: "tx_window:rnic0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "overload_upi_idle", fault: "overload_upi", mode: model.ModeIdle, cpuHigh: -1},
	{name: "overload_mem_idle", fault: "overload_mem:mem0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "lat_idle", fault: "lat:pcie.gpu0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "slow_mr_idle", fault: "slow_mr:gpu0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "compute_mode_idle", fault: "compute_mode:gpu0;compute_mode:gpu1=Prohibited", mode: model.ModeIdle, cpuHigh: -1},
	{name: "fast_rp_idle", fault: "fast_rp:rp.0,rp.1", mode: model.ModeIdle, cpuHigh: -1},

	// ToN'24 mutual probing, idle.
	{name: "connect_fail_idle", fault: "connect_fail:rnic0", mode: model.ModeIdle, cpuHigh: -1},
	{name: "drops_idle", fault: "drops:rnic1", mode: model.ModeIdle, cpuHigh: -1},
	{name: "flap_idle", fault: "flap:rnic1", mode: model.ModeIdle, cpuHigh: -1},
	{name: "tor_idle", fault: "tor", mode: model.ModeIdle, cpuHigh: -1},
	{name: "isolate_idle", fault: "isolate:rnic3", mode: model.ModeIdle, cpuHigh: -1},
	{name: "isolate_pair_idle", fault: "isolate:rnic2,rnic3", mode: model.ModeIdle, cpuHigh: -1},

	// Busy mode: the same faults seen from loaded RNICs (NSDI'23 §4.2,
	// Table 2 #11 / #12).
	{name: "rnic_pcie_busy", fault: "rnic_pcie:rnic0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "gpu_pcie_busy", fault: "gpu_pcie:gpu0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "mem_busy", fault: "mem:mem0;busy:rnic0,rnic2", mode: model.ModeBusy, cpuHigh: -1},
	{name: "upi_busy", fault: "upi;busy:rnic0,rnic2", mode: model.ModeBusy, cpuHigh: -1},
	{name: "rp_busy", fault: "rp:rp.0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "acs_busy", fault: "acs;busy:rnic0,rnic1", mode: model.ModeBusy, cpuHigh: -1},
	{name: "ats_busy", fault: "ats:rnic0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "slow_start_busy", fault: "slow_start:rnic0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "tx_window_busy", fault: "tx_window:rnic0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "overload_upi_busy", fault: "overload_upi;busy:rnic0,rnic2", mode: model.ModeBusy, cpuHigh: -1},
	{name: "overload_mem_busy", fault: "overload_mem:mem0;busy:rnic0,rnic2", mode: model.ModeBusy, cpuHigh: -1},
	{name: "lat_busy", fault: "lat:pcie.gpu0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "slow_mr_busy", fault: "slow_mr:gpu0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "compute_mode_busy", fault: "compute_mode:gpu0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "connect_fail_busy", fault: "connect_fail:rnic0;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1},
	{name: "drops_busy", fault: "drops:rnic1;busy:rnic1", mode: model.ModeBusy, cpuHigh: -1},
	{name: "flap_busy", fault: "flap:rnic1;busy:rnic1", mode: model.ModeBusy, cpuHigh: -1},
	{name: "tor_busy", fault: "tor;busy:rnic0,rnic1,rnic2,rnic3", mode: model.ModeBusy, cpuHigh: -1},
	{name: "mesh_busy", fault: "mesh:0-1;busy:rnic0,rnic1", mode: model.ModeBusy, snc: true, cpuHigh: -1},

	// Context the monitor adds to a run: a CPU-busy package, and a baseline
	// file with a learned sub-class.
	{name: "rp_cpu_busy_idle", fault: "rp:rp.2", mode: model.ModeIdle, cpuHigh: 1},
	{name: "healthy_cpu_busy_idle", fault: "", mode: model.ModeIdle, cpuHigh: 0, hostIdle: true},
	{name: "subclass_healthy_idle", fault: "fast_rp:rp.0,rp.1", mode: model.ModeIdle, cpuHigh: -1, baselineFile: subClassBaseline, hostIdle: true},
	{name: "subclass_gpu_pcie_idle", fault: "fast_rp:rp.0,rp.1;gpu_pcie:gpu2", mode: model.ModeIdle, cpuHigh: -1, baselineFile: subClassBaseline},
	{name: "subclass_busy", fault: "fast_rp:rp.0,rp.1;gpu_pcie:gpu2;busy:rnic0", mode: model.ModeBusy, cpuHigh: -1, baselineFile: subClassBaseline},
}

// runGolden runs one case end to end, as hostpingctl diagnose --sim does: the
// Eq.1 world with the fault, a one-shot engine, and the request the sim
// monitor would build (busy RNICs, link utilization, PCIe config, slow-start
// / Tx-window hints, GPU compute modes, CPU load).
func runGolden(t *testing.T, tc goldenCase) model.Report {
	t.Helper()
	g := topology.Fig2()
	if tc.snc {
		g = topology.Fig2SNC()
	}
	w := sim.New(g)
	if err := w.ApplyFault(tc.fault); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.BaselineFile = tc.baselineFile
	e := engine.New(cfg, g, sim.Prober{World: w})
	e.OneShot = true
	e.Logf = func(string, ...any) {}
	req := engine.RunRequest{
		Trigger:        model.TriggerManual,
		Mode:           tc.mode,
		BusyRNICs:      w.Busy,
		Util:           w.LinkUtil,
		PCIe:           w.PCIe,
		Hints:          map[string]string{},
		HostIdle:       tc.hostIdle,
		GPUComputeMode: w.ComputeMode,
	}
	for id, ok := range w.SlowStart {
		if ok {
			req.Hints[id] = "slow_start"
		}
	}
	for id, ok := range w.TxWindow {
		if ok {
			req.Hints[id] = "tx_window"
		}
	}
	if tc.cpuHigh >= 0 {
		for _, ep := range g.Endpoints {
			if ep.Kind != model.EndpointMemory {
				continue
			}
			l := model.CPULoad{NUMA: ep.NUMA, Socket: ep.Socket, Busy: 0.02}
			if ep.Socket == tc.cpuHigh {
				l.Busy, l.High = 0.97, true
			}
			req.CPULoad = append(req.CPULoad, l)
		}
	}
	rep, err := e.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// stripNondeterministic zeroes the fields that differ between two runs of the
// same case: the host name and the wall clock. Everything else — readings,
// verdicts, causes, baselines — is a pure function of the topology, the fault
// and the request.
func stripNondeterministic(rep model.Report) model.Report {
	rep.Host = ""
	rep.Started = time.Time{}
	rep.Elapsed = 0
	rep.ElapsedNs = 0
	return rep
}

// marshalGolden renders the report as one line of compact JSON: the golden
// files are large, and git stores them more cheaply that way. A mismatch is
// reported indented (see checkGolden), so the failure stays readable.
func marshalGolden(t *testing.T, rep model.Report) []byte {
	t.Helper()
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

// checkGolden compares got with testdata/golden/<name>.json, or rewrites the
// file under -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(goldenDir, name+".json")
	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file (run with -update to accept the change):\n%s", path, firstDiff(indented(want), indented(got)))
	}
}

// indented re-indents compact golden JSON so firstDiff can point at the
// changed field rather than at one very long line. Bytes that are not JSON
// (a truncated or hand-edited file) are returned as they are.
func indented(data []byte) []byte {
	var b bytes.Buffer
	if err := json.Indent(&b, bytes.TrimSpace(data), "", "  "); err != nil {
		return data
	}
	return b.Bytes()
}

// firstDiff renders the first differing lines of want and got, with a little
// context, so the failure says what changed without dumping two reports.
func firstDiff(want, got []byte) string {
	wl := strings.Split(string(want), "\n")
	gl := strings.Split(string(got), "\n")
	i := 0
	for i < len(wl) && i < len(gl) && wl[i] == gl[i] {
		i++
	}
	var b strings.Builder
	from := max(i-3, 0)
	for j := from; j < i; j++ {
		fmt.Fprintf(&b, "  %4d  %s\n", j+1, wl[j])
	}
	for j := i; j < min(i+5, len(wl)); j++ {
		fmt.Fprintf(&b, "- %4d  %s\n", j+1, wl[j])
	}
	for j := i; j < min(i+5, len(gl)); j++ {
		fmt.Fprintf(&b, "+ %4d  %s\n", j+1, gl[j])
	}
	if len(wl) != len(gl) {
		fmt.Fprintf(&b, "(golden has %d lines, output %d)\n", len(wl), len(gl))
	}
	return b.String()
}

// TestGolden pins the full JSON report of every simulated fault: the shape
// and field names of model.Report and the exact root-cause strings. A refactor
// of the analyzer or the model that changes any of it fails here, and an
// intended change is accepted with -update, where the diff is reviewed.
func TestGolden(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range goldenCases {
		if seen[tc.name] {
			t.Fatalf("duplicate golden case %q", tc.name)
		}
		seen[tc.name] = true
		t.Run(tc.name, func(t *testing.T) {
			rep := stripNondeterministic(runGolden(t, tc))
			checkGolden(t, tc.name, marshalGolden(t, rep))
		})
	}
}

// Every fault kind ApplyFault accepts has a golden case, so a new kind
// cannot be added without pinning its report.
func TestGoldenCoversEveryFaultKind(t *testing.T) {
	kinds := []string{
		"rnic_pcie", "gpu_pcie", "mem", "mem_flap", "upi", "mesh", "rp", "acs", "ats",
		"slow_start", "tx_window", "overload_upi", "overload_mem", "busy", "connect_fail",
		"drops", "slow_mr", "flap", "tor", "lat", "isolate", "compute_mode", "fast_rp",
	}
	covered := map[string]bool{}
	for _, tc := range goldenCases {
		for _, part := range strings.Split(tc.fault, ";") {
			kind, _, _ := strings.Cut(strings.TrimSpace(part), ":")
			covered[kind] = true
		}
	}
	for _, k := range kinds {
		if !covered[k] {
			t.Errorf("fault kind %q has no golden case", k)
		}
	}
	// And the list above is the simulator's: a kind it rejects is a typo
	// here, a kind it accepts that is missing here goes unpinned.
	for _, k := range kinds {
		w := sim.New(topology.Fig2SNC())
		arg := map[string]string{
			"rnic_pcie": "rnic0", "gpu_pcie": "gpu0", "mem": "mem0", "mem_flap": "mem0",
			"mesh": "0-1", "rp": "rp.0", "ats": "rnic0", "slow_start": "rnic0", "tx_window": "rnic0",
			"overload_mem": "mem0", "busy": "rnic0", "connect_fail": "rnic0", "drops": "rnic0",
			"slow_mr": "gpu0", "flap": "rnic0", "lat": "pcie.gpu0", "isolate": "rnic0",
			"compute_mode": "gpu0", "fast_rp": "rp.0,rp.1",
		}[k]
		spec := k
		if arg != "" {
			spec += ":" + arg
		}
		if err := w.ApplyFault(spec); err != nil {
			t.Errorf("ApplyFault(%q): %v", spec, err)
		}
	}
}

// fullReport is a model.Report with every JSON-visible field set, including
// the ones a simulated run rarely populates at once: a busy and an idle
// bandwidth recheck, a latency recheck, a capped candidate, a locality
// reference, a streak, a sub-class, skipped endpoints, a cross-rail mutual
// pair with counter drops, link utilization, CPU load, and a baseline table
// with a section and sub-classes. Its golden pins every struct tag of the
// report, so a rename in model.go is caught even where no simulation reaches.
func fullReport() model.Report {
	util := 0.95
	return model.Report{
		Trigger:   model.TriggerManual,
		Mode:      model.ModeIdle,
		Host:      "golden-host",
		Started:   time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC),
		Elapsed:   1500 * time.Millisecond,
		ElapsedNs: 1_500_000_000,
		Paths: []model.PathMeasurement{{
			RNIC:            "rnic0",
			Endpoint:        "gpu0",
			EndpointKind:    model.EndpointGPU,
			SmallLatNsP50:   2100,
			SmallLatNsP99:   2205,
			LargeLatNsP50:   12_700,
			BandwidthBPS:    196_000_000_000,
			Status:          model.PathAbnormal,
			UncertainReason: model.UncertainRNICBusy,
			BusyRecheck: &model.BusyRecheck{
				PathBPS:    120_000_000_000,
				SiblingBPS: 196_000_000_000,
				Siblings:   []string{"rnic0->gpu1", "rnic0->mem0"},
				Rounds:     3,
				Confirmed:  true,
			},
			BWRecheck: &model.BandwidthRecheck{
				Path:        "rnic0->gpu0",
				PathBPS:     120_000_000_000,
				RefBPS:      196_000_000_000,
				BaselineBPS: 197_000_000_000,
				Refs:        []string{"rnic1->gpu0", "rnic2->gpu6"},
				Rounds:      3,
				Outcome:     model.BWRecheckConfirmed,
			},
			LatencyStatus: model.LatencyAbnormal,
			LatencyReason: model.LatencyBySiblings,
			LatencyRefNs:  1000,
			LatencyRecheck: &model.LatencyRecheck{
				PathNsP50:    2100,
				SiblingNsP50: 1000,
				Siblings:     []string{"rnic1->gpu1", "rnic0->gpu1"},
				Confirmed:    true,
			},
			LatencyRecheckSkipped: model.LatencyRecheckSkipCap,
			LatencyLocality: &model.LatencyLocality{
				NUMA:  1,
				RefNs: 2050,
				RNICs: []string{"rnic3"},
			},
			LatencyStreak: 3,
			BaselineClass: "cross_root/fast",
			Verbs: model.VerbNs{
				GPUAlloc:  400_000,
				MRReg:     150_000,
				QPSetup:   250_000,
				MemWarmup: 10_000,
			},
		}},
		SkippedEndpoints: []model.EndpointSkip{{
			Kind:      model.EndpointGPU,
			Reason:    "compute mode Exclusive_Process",
			Endpoints: []string{"gpu1"},
			Probes:    4,
		}},
		Links: []model.LinkState{{
			ID:            "pcie.gpu0",
			Status:        model.LinkAbnormal,
			AbnormalCount: 2,
			Utilization:   &util,
		}},
		Causes: []model.RootCause{{
			Code:       model.CauseGPUPCIeFailure,
			Summary:    "golden summary",
			Links:      []string{"pcie.gpu0"},
			RNICs:      []string{"rnic0"},
			Endpoints:  []string{"gpu0"},
			Confidence: "high",
			Evidence:   "golden evidence",
		}},
		Mutual: []model.MutualMeasurement{{
			Src:          "rnic0",
			Dst:          "rnic1",
			Connected:    true,
			RTTnsP50:     4000,
			RTTnsP99:     5000,
			LossRatio:    0.015625,
			Sent:         64,
			Recv:         63,
			Reliable:     true,
			CounterDrops: 7,
			CrossRail:    true,
		}},
		HostIdle:     true,
		AbnormalRNIC: []string{"rnic0"},
		Baselines: []model.BaselineTable{{
			LineRateBPS:    200_000_000_000,
			PCIeCapBPS:     252_000_000_000,
			DerivedFromBPS: 200_000_000_000,
			Section:        "200g/pcie-252g",
			Entries: []model.BaselineEntry{{
				Distance:        model.DistanceCrossRoot.Key(),
				BandwidthBPS:    125_000_000_000,
				LatencyNs:       2200,
				BandwidthSource: model.BaselineSourceUser,
				LatencySource:   model.BaselineSourceUser,
				SubClasses: []model.BaselineSubClass{{
					Name:          "fast",
					BandwidthBPS:  197_000_000_000,
					LatencyNs:     1000,
					RootPortPairs: []model.RootPortPair{model.NewRootPortPair("rp.0", "rp.1")},
				}},
			}},
		}},
		CPULoad: []model.CPULoad{{NUMA: 1, Socket: 1, Busy: 0.97, High: true}},
	}
}

// TestGoldenSchema pins the JSON of a report with every field populated
// (testdata/golden/schema.json), and checks that fullReport does populate
// every JSON-visible field of model.Report, so a field added to the model
// has to be added here, and so to the golden, before it ships.
func TestGoldenSchema(t *testing.T) {
	rep := fullReport()
	var missing []string
	assertAllSet(reflect.ValueOf(rep), "Report", &missing)
	if len(missing) > 0 {
		t.Fatalf("fullReport leaves JSON-visible fields unset, add them (and update the golden):\n  %s", strings.Join(missing, "\n  "))
	}
	checkGolden(t, "schema", marshalGolden(t, rep))
}

// assertAllSet walks v and appends to missing the path of every exported,
// JSON-visible field that is zero: a nil pointer or empty slice, an empty
// string, a zero number or a false bool. Slices are checked through their
// first element.
func assertAllSet(v reflect.Value, path string, missing *[]string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			*missing = append(*missing, path)
			return
		}
		assertAllSet(v.Elem(), path, missing)
	case reflect.Slice:
		if v.Len() == 0 {
			*missing = append(*missing, path)
			return
		}
		assertAllSet(v.Index(0), path+"[0]", missing)
	case reflect.Array:
		for i := range v.Len() {
			assertAllSet(v.Index(i), fmt.Sprintf("%s[%d]", path, i), missing)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			if v.Interface().(time.Time).IsZero() {
				*missing = append(*missing, path)
			}
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if tag == "-" {
				continue
			}
			assertAllSet(v.Field(i), path+"."+f.Name, missing)
		}
	case reflect.Map:
		if v.Len() == 0 {
			*missing = append(*missing, path)
		}
	default:
		if v.IsZero() {
			*missing = append(*missing, path)
		}
	}
}

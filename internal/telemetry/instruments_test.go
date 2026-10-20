package telemetry

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// The instrument catalogue is a public contract: the names are what
// docs/metrics.md lists and what dashboards and alerts query, the descriptions
// are the Prometheus HELP lines, and the units become name suffixes on
// /metrics. The golden files under testdata pin all of it, from both sides of
// the exporter:
//
//   - instruments.golden is the OpenTelemetry view (name, unit, description,
//     aggregation kind, value type) read through a ManualReader;
//   - instruments_prometheus.golden is the HELP and TYPE lines the Prometheus
//     exporter writes, which also pins the translated names.
//
// Run `go test ./internal/telemetry -run Instruments -update` to rewrite them
// after an intentional change.
var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// instrumentsGolden and prometheusGolden are the snapshot files.
const (
	instrumentsGolden = "testdata/instruments.golden"
	prometheusGolden  = "testdata/instruments_prometheus.golden"
)

// metricsDoc is the metrics reference, whose table rows name every exported
// instrument.
const metricsDoc = "../../docs/metrics.md"

// newFullTelemetry builds the instruments on provider the way Start does for
// a daemon with every option on: the const gauges (build, topology, diagram
// and status-code info) registered and the idle-gate series written, so every
// instrument Start can create exists.
func newFullTelemetry(t *testing.T, provider *sdkmetric.MeterProvider) *Telemetry {
	t.Helper()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tel, err := New(provider)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tel.initIdleGateSeries()
	if err := tel.registerBuildInfo(testBuild); err != nil {
		t.Fatalf("registerBuildInfo: %v", err)
	}
	g := topology.Fig2()
	if err := tel.registerTopologyInfo(g.RNICs, g.Endpoints); err != nil {
		t.Fatalf("registerTopologyInfo: %v", err)
	}
	nodes, edges := g.Diagram()
	if err := tel.registerDiagramInfo(nodes, edges); err != nil {
		t.Fatalf("registerDiagramInfo: %v", err)
	}
	return tel
}

// fullReport is a report that touches every report-driven instrument: a GPU
// path and a memory path with verb timings (gpu_alloc on the GPU one,
// mem_warmup on the memory one), a latency verdict with a streak, a link with
// a read bus, a skipped endpoint, a mutual pair with a probe count, and a
// cause.
func fullReport() model.Report {
	return model.Report{
		Trigger:  model.TriggerManual,
		Mode:     model.ModeIdle,
		HostIdle: true,
		Paths: []model.PathMeasurement{
			{
				RNIC:          "rnic0",
				Endpoint:      "gpu0",
				EndpointKind:  model.EndpointGPU,
				SmallLatNsP50: 2000,
				BandwidthBPS:  200e9,
				Status:        model.PathAbnormal,
				LatencyStatus: model.LatencyAbnormal,
				LatencyReason: model.LatencyBySiblings,
				LatencyStreak: 3,
				Verbs:         model.VerbNs{GPUAlloc: 5000, MRReg: 3000, QPSetup: 7000},
			},
			{
				RNIC:            "rnic0",
				Endpoint:        "mem0",
				EndpointKind:    model.EndpointMemory,
				SmallLatNsP50:   1500,
				BandwidthBPS:    190e9,
				Status:          model.PathUncertain,
				UncertainReason: model.UncertainRNICBusy,
				LatencyStatus:   model.LatencyNormal,
				LatencyReason:   model.LatencyBySiblings,
				Verbs:           model.VerbNs{MRReg: 2000, QPSetup: 6000, MemWarmup: 9000},
			},
		},
		SkippedEndpoints: []model.EndpointSkip{{
			Kind: model.EndpointGPU, Reason: "compute mode is Exclusive_Process", Endpoints: []string{"gpu1"}, Probes: 1,
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

// recordEverything writes at least one data point to every instrument through
// the public Record* API. An instrument with no measurement is left out of a
// collection, so the snapshot would silently lose it.
func recordEverything(tel *Telemetry) {
	ctx := context.Background()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tel.RecordReport(ctx, fullReport())
	tel.RecordMutual(ctx, fullReport().Mutual)
	tel.RecordRates(ctx, []model.RNICRates{{
		Device: "rnic0", ThroughputBPS: 100e9, PauseDurationRatio: 0.01, DropDelta: 2, PauseKnown: true, DropKnown: true,
	}})
	tel.RecordCPULoad(ctx, []model.CPULoad{{NUMA: 0, Socket: 0, Busy: 0.5}})
	tel.RecordPoll(ctx, at, nil)
	tel.RecordPoll(ctx, at, errors.New("counters unreadable"))
	tel.RecordMonitorIdle(ctx, true)
	tel.RecordRunEnd(ctx, at)
	tel.RecordRunFailed(ctx, FailedEngine)
	tel.RecordSuccess(ctx, at)
	tel.RecordDiscarded(ctx, DiscardHostBusy)
	tel.RecordIdleGateSkipped(ctx, GateDenied)
}

// instrumentLine renders one collected instrument as a golden line:
// name | unit | description | kind | value type.
func instrumentLine(t *testing.T, m metricdata.Metrics) (line string, points int) {
	t.Helper()
	var kind, valueType string
	switch d := m.Data.(type) {
	case metricdata.Gauge[int64]:
		kind, valueType, points = "Gauge", "int64", len(d.DataPoints)
	case metricdata.Gauge[float64]:
		kind, valueType, points = "Gauge", "float64", len(d.DataPoints)
	case metricdata.Sum[int64]:
		kind, valueType, points = fmt.Sprintf("Sum(monotonic=%t, %s)", d.IsMonotonic, d.Temporality), "int64", len(d.DataPoints)
	case metricdata.Sum[float64]:
		kind, valueType, points = fmt.Sprintf("Sum(monotonic=%t, %s)", d.IsMonotonic, d.Temporality), "float64", len(d.DataPoints)
	default:
		t.Fatalf("instrument %s has aggregation %T, which the snapshot does not render", m.Name, m.Data)
	}
	for _, f := range []string{m.Name, m.Unit, m.Description} {
		if strings.Contains(f, "|") || strings.Contains(f, "\n") {
			t.Fatalf("instrument %s: field %q would break the snapshot line format", m.Name, f)
		}
	}
	return strings.Join([]string{m.Name, m.Unit, m.Description, kind, valueType}, " | "), points
}

// collectInstruments records everything into a ManualReader and returns the
// sorted snapshot lines, one per instrument.
func collectInstruments(t *testing.T) []string {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel := newFullTelemetry(t, provider)
	recordEverything(tel)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var lines []string
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != "github.com/yuuki/hostping" {
			t.Errorf("instruments under scope %q, want github.com/yuuki/hostping", sm.Scope.Name)
		}
		for _, m := range sm.Metrics {
			if seen[m.Name] {
				t.Errorf("instrument %s collected twice", m.Name)
			}
			seen[m.Name] = true
			line, points := instrumentLine(t, m)
			if points == 0 {
				t.Errorf("instrument %s has no data points", m.Name)
			}
			lines = append(lines, line)
		}
	}
	sort.Strings(lines)
	return lines
}

// scrapePrometheus records everything behind the Prometheus exporter (as Start
// wires it, with the default name translation) and serves one scrape through
// promhttp, the handler /metrics uses. It returns the HELP and TYPE lines of
// every hostping_ metric family, grouped per family and sorted by name.
func scrapePrometheus(t *testing.T) []string {
	t.Helper()
	reg := promclient.NewRegistry()
	exp, err := prometheus.New(prometheus.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("prometheus exporter: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	tel := newFullTelemetry(t, provider)
	recordEverything(tel)

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: %d %s", rec.Code, rec.Body.String())
	}
	type family struct{ help, typ string }
	families := map[string]*family{}
	sc := bufio.NewScanner(rec.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.SplitN(line, " ", 4)
		if len(fields) < 3 || fields[0] != "#" || !strings.HasPrefix(fields[2], "hostping_") {
			continue
		}
		f := families[fields[2]]
		if f == nil {
			f = &family{}
			families[fields[2]] = f
		}
		switch fields[1] {
		case "HELP":
			f.help = line
		case "TYPE":
			f.typ = line
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read scrape: %v", err)
	}
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		f := families[name]
		if f.typ == "" {
			t.Errorf("metric family %s has no TYPE line", name)
		}
		if f.help != "" {
			lines = append(lines, f.help)
		}
		lines = append(lines, f.typ)
	}
	return lines
}

// checkGolden compares got with the golden file at path, rewriting it under
// -update. A mismatch names every line on one side only.
func checkGolden(t *testing.T, path string, got []string) {
	t.Helper()
	text := strings.Join(got, "\n") + "\n"
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wantBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(wantBytes) == text {
		return
	}
	want := strings.Split(strings.TrimSuffix(string(wantBytes), "\n"), "\n")
	gotSet, wantSet := map[string]bool{}, map[string]bool{}
	for _, l := range got {
		gotSet[l] = true
	}
	for _, l := range want {
		wantSet[l] = true
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "%s differs from the collected instruments (run with -update after an intentional change)\n", path)
	for _, l := range want {
		if !gotSet[l] {
			fmt.Fprintf(&msg, "- %s\n", l)
		}
	}
	for _, l := range got {
		if !wantSet[l] {
			fmt.Fprintf(&msg, "+ %s\n", l)
		}
	}
	t.Error(msg.String())
}

// TestInstrumentsSnapshot pins every instrument's name, unit, description and
// aggregation as the SDK sees them.
func TestInstrumentsSnapshot(t *testing.T) {
	checkGolden(t, instrumentsGolden, collectInstruments(t))
}

// TestInstrumentsPrometheusSnapshot pins the HELP and TYPE lines of every
// hostping_ family on /metrics: the translated names (unit and counter
// suffixes included), the Prometheus types and the help text.
func TestInstrumentsPrometheusSnapshot(t *testing.T) {
	lines := scrapePrometheus(t)
	checkGolden(t, prometheusGolden, lines)

	// Every instrument reaches Prometheus as exactly one family.
	families := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "# TYPE ") {
			families++
		}
	}
	if n := len(collectInstruments(t)); families != n {
		t.Errorf("/metrics has %d hostping_ families, the SDK %d instruments", families, n)
	}
}

// docMetricRow matches a row of the metrics table in docs/metrics.md.
var docMetricRow = regexp.MustCompile("^\\| `(hostping\\.[^`]+)` \\|")

// TestInstrumentsMatchMetricsDoc: docs/metrics.md lists every instrument the
// code creates and nothing else. The doc is the public contract, so a new
// instrument needs a row and a removed one loses its row.
func TestInstrumentsMatchMetricsDoc(t *testing.T) {
	doc, err := os.ReadFile(metricsDoc)
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, line := range strings.Split(string(doc), "\n") {
		if m := docMetricRow.FindStringSubmatch(line); m != nil {
			if documented[m[1]] {
				t.Errorf("%s lists %s twice", metricsDoc, m[1])
			}
			documented[m[1]] = true
		}
	}
	if len(documented) == 0 {
		t.Fatalf("no metric rows found in %s", metricsDoc)
	}

	exported := map[string]bool{}
	for _, line := range collectInstruments(t) {
		exported[line[:strings.Index(line, " | ")]] = true
	}
	for name := range exported {
		if !documented[name] {
			t.Errorf("instrument %s is exported but not listed in %s", name, metricsDoc)
		}
	}
	for name := range documented {
		if !exported[name] {
			t.Errorf("%s lists %s, which no instrument exports", metricsDoc, name)
		}
	}
}

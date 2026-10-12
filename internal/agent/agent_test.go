package agent

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/zigprobe"
)

// A one-shot run has nobody to scrape /metrics, so it must not bind the default
// Prometheus port (nor start an OTLP reader) unless asked. telemetry.Start is the
// only thing that installs a global meter provider, so an unchanged global proves
// no exporter was started.
func TestRunOnceStartsNoTelemetryExporters(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.Once = true
	if cfg.PrometheusAddr == "" {
		t.Fatal("defaults must still carry the daemon listen address")
	}

	before := otel.GetMeterProvider()
	defer quiet(t)()
	if err := Run(cfg); err != nil {
		t.Fatal(err)
	}
	if otel.GetMeterProvider() != before {
		t.Fatalf("one-shot run started telemetry exporters (listener %s)", cfg.PrometheusAddr)
	}
	if addr, otlp := cfg.TelemetryExporters(); addr != "" || otlp != "" {
		t.Fatalf("one-shot exporters: addr=%q otlp=%q", addr, otlp)
	}
}

// quiet swallows the report JSON and log lines emitted by Run.
func quiet(t *testing.T) func() {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	log.SetOutput(io.Discard)
	return func() {
		os.Stdout = stdout
		log.SetOutput(os.Stderr)
		_ = devnull.Close()
	}
}

func TestRequestFromDecisionRefusesUnknownIdle(t *testing.T) {
	_, err := requestFromDecision(monitor.Decision{Ready: true, IdleKnown: false, HostIdle: false})
	if err == nil {
		t.Fatal("unknown idle must refuse")
	}
	_, err = requestFromDecision(monitor.Decision{Ready: false, IdleKnown: true, HostIdle: true})
	if err == nil {
		t.Fatal("warmup must refuse")
	}
	_, err = requestFromDecision(monitor.Decision{Ready: true, UnreadCounters: []string{"rnic3"}})
	if err == nil || !strings.Contains(err.Error(), "PFC pause or drop counters unread on rnic3") {
		t.Fatalf("unread PFC/drop counters must refuse and say so: %v", err)
	}
	ok, why := idleVerdict(monitor.Decision{Ready: true, UnreadCounters: []string{"rnic3"}})
	if ok || !strings.Contains(why, "rnic3") {
		t.Fatalf("idle hold must not confirm on unread counters: ok=%v why=%q", ok, why)
	}
}

func TestRequestFromDecisionBusyAndIdle(t *testing.T) {
	idle, err := requestFromDecision(monitor.Decision{
		Ready: true, IdleKnown: true, HostIdle: true, Trigger: model.TriggerIdle, Mode: model.ModeIdle,
	})
	if err != nil || idle.Mode != model.ModeIdle || !idle.HostIdle {
		t.Fatalf("idle %+v err=%v", idle, err)
	}
	busy, err := requestFromDecision(monitor.Decision{
		Ready: true, IdleKnown: true, HostIdle: false,
		BusyRNICs: map[string]bool{"rnic0": true},
		DropDelta: map[string]uint64{"rnic0": 3},
	})
	if err != nil || busy.Mode != model.ModeBusy || busy.HostIdle || busy.DropDelta["rnic0"] != 3 {
		t.Fatalf("busy %+v err=%v", busy, err)
	}
}

// "no intra-host bottleneck" is a reassuring line to print when the GPU half of
// the mesh was never probed. The engine names the reason only when it changes,
// so this line carries the count on every run.
func TestEmitNamesSkippedEndpointsInTheNoBottleneckLine(t *testing.T) {
	skipped := model.Report{
		Trigger: model.TriggerIdle,
		Mode:    model.ModeIdle,
		Paths:   make([]model.PathMeasurement, 20),
		SkippedEndpoints: []model.EndpointSkip{{
			Kind:      model.EndpointGPU,
			Reason:    "gpu endpoint requires GPUDirect RDMA: NoGDR: no cuda",
			Endpoints: []string{"gpu0", "gpu1", "gpu2", "gpu3", "gpu4", "gpu5", "gpu6", "gpu7"},
			Probes:    80,
		}},
	}
	if got := logged(t, skipped); !strings.Contains(got, "paths=20 skipped_endpoints=8") {
		t.Errorf("no-bottleneck line = %q, want the skipped endpoint count", got)
	}

	healthy := model.Report{Trigger: model.TriggerIdle, Mode: model.ModeIdle, Paths: make([]model.PathMeasurement, 100)}
	if got := logged(t, healthy); !strings.Contains(got, "paths=100)") {
		t.Errorf("a run with nothing skipped must keep the original line, got %q", got)
	}
}

// logged runs emit with stdout discarded and returns what it wrote to the log.
func logged(t *testing.T, rep model.Report) string {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	rp := &reporter{format: config.ReportFormatPretty, stdout: io.Discard, now: time.Now}
	if err := rp.emit(rep); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// --mutual-msg is plumbed from the config into the prober the engine uses.
func TestNewProberCarriesMutualMsg(t *testing.T) {
	cfg := config.Defaults()
	cfg.Sim = true
	cfg.MutualMsgBytes = 7
	g, err := LoadGraph(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := NewProber(cfg, g)
	if err != nil {
		t.Fatal(err)
	}
	zp, ok := p.(zigprobe.Prober)
	if !ok {
		t.Fatalf("prober is %T, want zigprobe.Prober", p)
	}
	if zp.MutualMsg != 7 {
		t.Fatalf("Prober.MutualMsg = %d, want 7", zp.MutualMsg)
	}
}

// Input.Util is keyed by topology link ID and assembled from the monitor's
// RNIC PCIe delta plus the sampler's live reads. A link no source read is
// absent (unknown), never 0.
func TestUtilFromMergesSourcesAndLeavesUnknownOut(t *testing.T) {
	s := monitor.StaticSampler{Util: map[string]float64{"pcie.gpu0": 0.7}}
	got := utilFrom(context.Background(), nil, s, map[string]float64{"pcie.rnic0": 0.95, "pcie.rnic1": 0})
	want := map[string]float64{"pcie.rnic0": 0.95, "pcie.rnic1": 0, "pcie.gpu0": 0.7}
	if len(got) != len(want) {
		t.Fatalf("util=%v, want %v", got, want)
	}
	for id, v := range want {
		if u, ok := got[id]; !ok || u != v {
			t.Fatalf("util[%s]=%v ok=%v, want %v", id, u, ok, v)
		}
	}
	if got := utilFrom(context.Background(), nil, monitor.StaticSampler{}, nil); got != nil {
		t.Fatalf("no source must yield no util, got %v", got)
	}
}

func TestRequestFromDecisionCarriesUtil(t *testing.T) {
	req, err := requestFromDecision(monitor.Decision{
		Ready: true, IdleKnown: true, Util: map[string]float64{"pcie.rnic0": 0.4},
	})
	if err != nil || req.Util["pcie.rnic0"] != 0.4 {
		t.Fatalf("req=%+v err=%v", req, err)
	}
}

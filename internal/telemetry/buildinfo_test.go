package telemetry

import (
	"context"
	"strings"
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/yuuki/hostping/internal/buildinfo"
)

var testBuild = buildinfo.Info{Version: "1.2.3", Commit: "abc1234", Zig: "0.2.0", Go: "go1.27.0"}

func TestBuildInfoExported(t *testing.T) {
	tel, reader := newTestTelemetry(t)
	if err := tel.registerBuildInfo(testBuild); err != nil {
		t.Fatalf("registerBuildInfo: %v", err)
	}
	// No report has been recorded: build info must be there regardless.
	got := gaugeInt(t, collect(t, reader), "hostping.build.info")
	want := attrKey(attribute.NewSet(
		attribute.String("version", "1.2.3"),
		attribute.String("commit", "abc1234"),
		attribute.String("zig", "0.2.0"),
		attribute.String("go", "go1.27.0"),
	))
	if len(got) != 1 || got[want] != 1 {
		t.Fatalf("hostping.build.info = %v, want {%s: 1}", got, want)
	}
}

// TestBuildInfoOnFirstScrape checks the Prometheus side: the series is on the
// first scrape, under the name dashboards query.
func TestBuildInfoOnFirstScrape(t *testing.T) {
	reg := promclient.NewRegistry()
	exp, err := prometheus.New(prometheus.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("prometheus exporter: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	tel := &Telemetry{provider: provider, meter: provider.Meter("github.com/yuuki/hostping")}
	if err := tel.initInstruments(); err != nil {
		t.Fatalf("initInstruments: %v", err)
	}
	if err := tel.registerBuildInfo(testBuild); err != nil {
		t.Fatalf("registerBuildInfo: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var names []string
	for _, mf := range mfs {
		names = append(names, mf.GetName())
		if mf.GetName() != "hostping_build_info" {
			continue
		}
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("hostping_build_info has %d series, want 1", len(mf.GetMetric()))
		}
		m := mf.GetMetric()[0]
		if v := m.GetGauge().GetValue(); v != 1 {
			t.Fatalf("hostping_build_info = %v, want 1", v)
		}
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		for k, v := range map[string]string{"version": "1.2.3", "commit": "abc1234", "zig": "0.2.0", "go": "go1.27.0"} {
			if labels[k] != v {
				t.Fatalf("hostping_build_info label %s = %q, want %q (labels %v)", k, labels[k], v, labels)
			}
		}
		return
	}
	t.Fatalf("hostping_build_info not on the first scrape; got %s", strings.Join(names, ", "))
}

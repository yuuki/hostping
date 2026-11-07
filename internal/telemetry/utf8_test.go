package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/yuuki/hostping/internal/topology"
)

// infoScrape registers the topology and diagram info series of g behind both
// a manual reader (what an OTLP export serializes) and the Prometheus
// exporter, and serves one scrape through promhttp with the options /metrics
// uses. It returns the scrape's status and body and the collected metrics.
func infoScrape(t testing.TB, g topology.Graph) (int, string, metricdata.ResourceMetrics) {
	t.Helper()
	reg := promclient.NewRegistry()
	exp, err := prometheus.New(prometheus.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("prometheus exporter: %v", err)
	}
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp), sdkmetric.WithReader(reader))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	tel := &Telemetry{provider: provider, meter: provider.Meter("github.com/yuuki/hostping")}
	if err := tel.initInstruments(); err != nil {
		t.Fatalf("initInstruments: %v", err)
	}
	if err := tel.registerTopologyInfo(g.RNICs, g.Endpoints); err != nil {
		t.Fatalf("registerTopologyInfo: %v", err)
	}
	if err := tel.registerDiagramInfo(g.Diagram()); err != nil {
		t.Fatalf("registerDiagramInfo: %v", err)
	}
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rec.Code, rec.Body.String(), rm
}

// infoLabels is every attribute of the rnic and topology node info series,
// by series name and RNIC or node ID.
func infoLabels(rm metricdata.ResourceMetrics) map[string]map[string]map[string]string {
	out := map[string]map[string]map[string]string{}
	key := map[string]string{"hostping.rnic.info": "rnic", "hostping.topology.node.info": "node"}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			k := key[m.Name]
			g, ok := m.Data.(metricdata.Gauge[int64])
			if k == "" || !ok {
				continue
			}
			out[m.Name] = map[string]map[string]string{}
			for _, dp := range g.DataPoints {
				labels := map[string]string{}
				for _, kv := range dp.Attributes.ToSlice() {
					labels[string(kv.Key)] = kv.Value.AsString()
				}
				out[m.Name][labels[k]] = labels
			}
		}
	}
	return out
}

// allAttrsUTF8 reports the first attribute value of rm that is not UTF-8:
// protobuf strings must be, so one such value fails a whole OTLP export.
func allAttrsUTF8(rm metricdata.ResourceMetrics) (string, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				continue
			}
			for _, dp := range g.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					if v := kv.Value.AsString(); !utf8.ValidString(v) {
						return m.Name + "{" + string(kv.Key) + "=" + strings.ToValidUTF8(v, "?") + "}", false
					}
				}
			}
		}
	}
	return "", true
}

// The kernel accepts any bytes but '/' in an RDMA device name (rdma dev set
// ... name) and any but '/', ':' and whitespace in a netdev name. Such a name
// must neither fail the scrape (client_golang rejects a label value that is
// not UTF-8, and promhttp then answers 500 for every series) nor reach OTLP,
// where it fails the export. Valid names are exported unchanged.
func TestTopologyInfoNonUTF8Names(t *testing.T) {
	g := topology.Fig2()
	g.RNICs[1].Device = "mlx5_\xff1"
	g.RNICs[1].Netdev = "eth\xc3"
	g.RNICs[1].Fabric = "gid:fe80\xe2\x82"

	code, body, rm := infoScrape(t, g)
	if code != http.StatusOK {
		t.Fatalf("scrape: %d %s", code, body)
	}
	if !utf8.ValidString(body) {
		t.Fatal("scrape body is not UTF-8")
	}
	if where, ok := allAttrsUTF8(rm); !ok {
		t.Fatalf("attribute value is not UTF-8: %s", where)
	}
	labels := infoLabels(rm)
	want := map[string]string{"device": "mlx5_�1", "netdev": "eth�", "fabric": "gid:fe80�"}
	for k, v := range want {
		if got := labels["hostping.rnic.info"]["rnic1"][k]; got != v {
			t.Errorf("hostping.rnic.info{rnic=rnic1} %s = %q, want %q", k, got, v)
		}
	}
	if got := labels["hostping.topology.node.info"]["rnic1"]["title"]; got != "mlx5_�1" {
		t.Errorf("hostping.topology.node.info{node=rnic1} title = %q, want %q", got, "mlx5_�1")
	}
	// The other RNICs keep their names byte for byte.
	if got := labels["hostping.rnic.info"]["rnic0"]; got["device"] != "mlx5_0" || got["netdev"] != "eth0" {
		t.Errorf("hostping.rnic.info{rnic=rnic0} = %v, want device mlx5_0, netdev eth0", got)
	}
}

// FuzzTopologyInfoLabels follows the strings discovery reads from sysfs
// (device and netdev names, the PCI address, the fabric key) to the exported
// info series: whatever their bytes, the scrape succeeds, every attribute is
// UTF-8, and a name that already is UTF-8 is exported unchanged.
func FuzzTopologyInfoLabels(f *testing.F) {
	f.Add("mlx5_0", "eth0", "0000:17:00.0", "v4:10.0.0.0")
	f.Add("mlx5_\xff1", "eth\xc3", "0000:37:00.0", "gid:fe80\xe2\x82")
	f.Add("", "", "", "")
	f.Add("rocep23s0", "ens1np0\"\\\n", "unknown", "ib:fe80000000000000")
	f.Fuzz(func(t *testing.T, device, netdev, bdf, fabric string) {
		g := topology.Fig2()
		n := &g.RNICs[2]
		n.Device, n.Netdev, n.PCIeBDF, n.Fabric = device, netdev, bdf, fabric

		code, body, rm := infoScrape(t, g)
		if code != http.StatusOK {
			t.Fatalf("scrape: %d %s", code, body)
		}
		if where, ok := allAttrsUTF8(rm); !ok {
			t.Fatalf("attribute value is not UTF-8: %s", where)
		}
		got := infoLabels(rm)["hostping.rnic.info"]["rnic2"]
		for k, in := range map[string]string{"device": device, "netdev": netdev, "pcie_bdf": bdf, "fabric": fabric} {
			want := in
			if want == "" {
				want = unknownAttr
			}
			if utf8.ValidString(in) && got[k] != want {
				t.Fatalf("%s = %q, want the UTF-8 input %q unchanged", k, got[k], want)
			}
		}
	})
}

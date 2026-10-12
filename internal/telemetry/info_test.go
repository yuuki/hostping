package telemetry

import (
	"context"
	"testing"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func rnicInfoKey(rnic, device, bdf, netdev, linkLayer, fabric, socket, numa string) string {
	return attrKey(attribute.NewSet(
		attribute.String("rnic", rnic),
		attribute.String("device", device),
		attribute.String("pcie_bdf", bdf),
		attribute.String("netdev", netdev),
		attribute.String("link_layer", linkLayer),
		attribute.String("fabric", fabric),
		attribute.String("socket", socket),
		attribute.String("numa", numa),
	))
}

func gpuInfoKey(ep, index, socket, numa, bdf, rp string) string {
	return attrKey(attribute.NewSet(
		attribute.String("endpoint", ep),
		attribute.String("kind", "gpu"),
		attribute.String("index", index),
		attribute.String("socket", socket),
		attribute.String("numa", numa),
		attribute.String("pcie_bdf", bdf),
		attribute.String("root_port", rp),
	))
}

func memInfoKey(ep, index, socket, numa string) string {
	return attrKey(attribute.NewSet(
		attribute.String("endpoint", ep),
		attribute.String("kind", "memory"),
		attribute.String("index", index),
		attribute.String("socket", socket),
		attribute.String("numa", numa),
	))
}

// TestTopologyInfoFig2 checks hostping.rnic.info and hostping.endpoint.info on
// the Fig.2 topology: one series per RNIC and per endpoint, always 1, with the
// physical identifiers as attributes, on every collection and before any
// report.
func TestTopologyInfoFig2(t *testing.T) {
	g := topology.Fig2()
	tel, reader := newTestTelemetry(t)
	if err := tel.registerTopologyInfo(g.RNICs, g.Endpoints); err != nil {
		t.Fatalf("registerTopologyInfo: %v", err)
	}
	// Fig.2 carries no link layer or fabric: those say "unknown".
	wantRNIC := map[string]int64{
		rnicInfoKey("rnic0", "mlx5_0", "0000:17:00.0", "eth0", "unknown", "unknown", "0", "0"): 1,
		rnicInfoKey("rnic1", "mlx5_1", "0000:37:00.0", "eth1", "unknown", "unknown", "0", "0"): 1,
		rnicInfoKey("rnic2", "mlx5_2", "0000:57:00.0", "eth2", "unknown", "unknown", "1", "1"): 1,
		rnicInfoKey("rnic3", "mlx5_3", "0000:77:00.0", "eth3", "unknown", "unknown", "1", "1"): 1,
	}
	wantEP := map[string]int64{
		gpuInfoKey("gpu0", "0", "0", "0", "0000:18:00.0", "rp.0"): 1,
		gpuInfoKey("gpu1", "1", "0", "0", "0000:19:00.0", "rp.0"): 1,
		gpuInfoKey("gpu2", "2", "0", "0", "0000:38:00.0", "rp.1"): 1,
		gpuInfoKey("gpu3", "3", "0", "0", "0000:39:00.0", "rp.1"): 1,
		gpuInfoKey("gpu4", "4", "1", "1", "0000:58:00.0", "rp.2"): 1,
		gpuInfoKey("gpu5", "5", "1", "1", "0000:59:00.0", "rp.2"): 1,
		gpuInfoKey("gpu6", "6", "1", "1", "0000:78:00.0", "rp.3"): 1,
		gpuInfoKey("gpu7", "7", "1", "1", "0000:79:00.0", "rp.3"): 1,
		memInfoKey("mem0", "0", "0", "0"):                         1,
		memInfoKey("mem1", "1", "1", "1"):                         1,
	}
	// Twice: an observable gauge writes the same series on every collection.
	for i := 0; i < 2; i++ {
		ms := collect(t, reader)
		assertGaugeSeries(t, "hostping.rnic.info", gaugeInt(t, ms, "hostping.rnic.info"), wantRNIC)
		assertGaugeSeries(t, "hostping.endpoint.info", gaugeInt(t, ms, "hostping.endpoint.info"), wantEP)
	}
	// Recording a report changes neither.
	tel.RecordReport(context.Background(), sampleReport())
	ms := collect(t, reader)
	assertGaugeSeries(t, "hostping.rnic.info", gaugeInt(t, ms, "hostping.rnic.info"), wantRNIC)
	assertGaugeSeries(t, "hostping.endpoint.info", gaugeInt(t, ms, "hostping.endpoint.info"), wantEP)
}

// TestRNICInfoFabricAndLinkLayer: an RNIC whose link layer and fabric are known
// exports them as discovery recorded them, and an empty identifier is
// "unknown" rather than an empty label.
func TestRNICInfoFabricAndLinkLayer(t *testing.T) {
	rnics := []model.RNIC{
		{ID: "rnic0", Device: "mlx5_0", Netdev: "ens1np0", PCIeBDF: "0000:1a:00.0", LinkLayer: model.LinkLayerEthernet, Fabric: "v4:10.0.0.0", Socket: 0, NUMA: 0},
		{ID: "rnic1", Device: "mlx5_1", Socket: 1, NUMA: 3},
	}
	tel, reader := newTestTelemetry(t)
	if err := tel.registerTopologyInfo(rnics, nil); err != nil {
		t.Fatalf("registerTopologyInfo: %v", err)
	}
	ms := collect(t, reader)
	assertGaugeSeries(t, "hostping.rnic.info", gaugeInt(t, ms, "hostping.rnic.info"), map[string]int64{
		rnicInfoKey("rnic0", "mlx5_0", "0000:1a:00.0", "ens1np0", "Ethernet", "v4:10.0.0.0", "0", "0"): 1,
		rnicInfoKey("rnic1", "mlx5_1", "unknown", "unknown", "unknown", "unknown", "1", "3"):           1,
	})
	if _, ok := ms["hostping.endpoint.info"]; ok {
		if got := gaugeInt(t, ms, "hostping.endpoint.info"); len(got) != 0 {
			t.Fatalf("hostping.endpoint.info = %v, want no series without endpoints", got)
		}
	}
}

// TestRNICInfoLeavesExcludedOut: a device dropped by --exclude-rnic is in
// Graph.Excluded, not Graph.RNICs, and gets no info series.
func TestRNICInfoLeavesExcludedOut(t *testing.T) {
	g := topology.Fig2()
	g.Excluded = []model.ExcludedRNIC{{Device: g.RNICs[3].Device, PCIeBDF: g.RNICs[3].PCIeBDF, Reason: model.ExcludeReasonOperator}}
	g.RNICs = g.RNICs[:3]
	var o options
	WithTopology(g.RNICs, g.Endpoints)(&o)
	tel, reader := newTestTelemetry(t)
	if err := tel.registerTopologyInfo(o.rnics, o.endpoints); err != nil {
		t.Fatalf("registerTopologyInfo: %v", err)
	}
	got := gaugeInt(t, collect(t, reader), "hostping.rnic.info")
	if len(got) != 3 {
		t.Fatalf("hostping.rnic.info has %d series, want 3: %v", len(got), got)
	}
	excluded := rnicInfoKey("rnic3", "mlx5_3", "0000:77:00.0", "eth3", "unknown", "unknown", "1", "1")
	if _, ok := got[excluded]; ok {
		t.Fatalf("excluded RNIC exported: %v", got)
	}
}

// TestWithTopologyCopies: the info series are fixed at start-up, so a later
// change to the caller's graph does not reach them.
func TestWithTopologyCopies(t *testing.T) {
	g := topology.Fig2()
	var o options
	WithTopology(g.RNICs, g.Endpoints)(&o)
	g.RNICs[0].Device = "changed"
	g.Endpoints[0].PCIeBDF = "changed"
	if o.rnics[0].Device != "mlx5_0" || o.endpoints[0].PCIeBDF != "0000:18:00.0" {
		t.Fatalf("WithTopology kept a reference to the caller's slices: %+v %+v", o.rnics[0], o.endpoints[0])
	}
}

// TestTopologyInfoPrometheusNames checks the names and labels a PromQL join
// uses: hostping_rnic_info{rnic,device,pcie_bdf,...} and
// hostping_endpoint_info{endpoint,kind,pcie_bdf,...}, on the first scrape.
func TestTopologyInfoPrometheusNames(t *testing.T) {
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
	g := topology.Fig2()
	if err := tel.registerTopologyInfo(g.RNICs, g.Endpoints); err != nil {
		t.Fatalf("registerTopologyInfo: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	series := map[string][]map[string]string{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if v := m.GetGauge().GetValue(); (mf.GetName() == "hostping_rnic_info" || mf.GetName() == "hostping_endpoint_info") && v != 1 {
				t.Fatalf("%s = %v, want 1", mf.GetName(), v)
			}
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			series[mf.GetName()] = append(series[mf.GetName()], labels)
		}
	}
	if n := len(series["hostping_rnic_info"]); n != len(g.RNICs) {
		t.Fatalf("hostping_rnic_info has %d series, want %d (families %v)", n, len(g.RNICs), series)
	}
	if n := len(series["hostping_endpoint_info"]); n != len(g.Endpoints) {
		t.Fatalf("hostping_endpoint_info has %d series, want %d", n, len(g.Endpoints))
	}
	found := false
	for _, l := range series["hostping_rnic_info"] {
		if l["rnic"] == "rnic2" {
			found = true
			if l["device"] != "mlx5_2" || l["pcie_bdf"] != "0000:57:00.0" || l["netdev"] != "eth2" || l["numa"] != "1" {
				t.Fatalf("hostping_rnic_info{rnic=rnic2} labels = %v", l)
			}
		}
	}
	if !found {
		t.Fatalf("hostping_rnic_info has no rnic=rnic2 series: %v", series["hostping_rnic_info"])
	}
	for _, l := range series["hostping_endpoint_info"] {
		switch l["endpoint"] {
		case "gpu5":
			if l["kind"] != "gpu" || l["pcie_bdf"] != "0000:59:00.0" || l["root_port"] != "rp.2" || l["index"] != "5" {
				t.Fatalf("hostping_endpoint_info{endpoint=gpu5} labels = %v", l)
			}
		case "mem1":
			if _, ok := l["pcie_bdf"]; ok {
				t.Fatalf("memory endpoint carries pcie_bdf: %v", l)
			}
			if l["kind"] != "memory" || l["numa"] != "1" {
				t.Fatalf("hostping_endpoint_info{endpoint=mem1} labels = %v", l)
			}
		}
	}
}

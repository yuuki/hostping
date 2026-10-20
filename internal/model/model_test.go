package model_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// enumCase pins one constant of an int enum: its iota value, its String()
// and its MarshalJSON output. The numeric value matters because telemetry's
// status-code table is keyed by it.
type enumCase struct {
	name  string
	value int
	str   string
}

// checkEnum walks every defined constant in iota order and then probes the
// sentinel for a value past the last constant and for a negative one. str
// and marshal are the enum's methods applied to a raw int.
func checkEnum(t *testing.T, enum string, cases []enumCase, sentinel string,
	str func(int) string, marshal func(int) ([]byte, error)) {
	t.Helper()
	for i, c := range cases {
		if c.value != i {
			t.Errorf("%s: %s = %d, want iota %d", enum, c.name, c.value, i)
		}
		if got := str(c.value); got != c.str {
			t.Errorf("%s: %s.String() = %q, want %q", enum, c.name, got, c.str)
		}
		b, err := marshal(c.value)
		if err != nil {
			t.Errorf("%s: %s.MarshalJSON: %v", enum, c.name, err)
		}
		if want := `"` + c.str + `"`; string(b) != want {
			t.Errorf("%s: %s.MarshalJSON() = %s, want %s", enum, c.name, b, want)
		}
	}
	for _, v := range []int{len(cases), len(cases) + 100, -1} {
		if got := str(v); got != sentinel {
			t.Errorf("%s(%d).String() = %q, want sentinel %q", enum, v, got, sentinel)
		}
		b, err := marshal(v)
		if err != nil {
			t.Errorf("%s(%d).MarshalJSON: %v", enum, v, err)
		}
		if want := `"` + sentinel + `"`; string(b) != want {
			t.Errorf("%s(%d).MarshalJSON() = %s, want %s", enum, v, b, want)
		}
	}
	// The sentinel must not collide with a defined constant's name: telemetry
	// enumerates the enum until the sentinel appears.
	for _, c := range cases {
		if c.str == sentinel {
			t.Errorf("%s: %s.String() == sentinel %q; enumeration would stop early", enum, c.name, sentinel)
		}
	}
}

func TestLinkStatus(t *testing.T) {
	checkEnum(t, "LinkStatus", []enumCase{
		{"LinkUncertain", int(model.LinkUncertain), "uncertain"},
		{"LinkNormal", int(model.LinkNormal), "normal"},
		{"LinkAbnormal", int(model.LinkAbnormal), "abnormal"},
		{"LinkGray", int(model.LinkGray), "gray"},
	}, "unknown",
		func(v int) string { return model.LinkStatus(v).String() },
		func(v int) ([]byte, error) { return model.LinkStatus(v).MarshalJSON() })
}

func TestPathStatus(t *testing.T) {
	checkEnum(t, "PathStatus", []enumCase{
		{"PathUncertain", int(model.PathUncertain), "uncertain"},
		{"PathNormal", int(model.PathNormal), "normal"},
		{"PathAbnormal", int(model.PathAbnormal), "abnormal"},
	}, "unknown",
		func(v int) string { return model.PathStatus(v).String() },
		func(v int) ([]byte, error) { return model.PathStatus(v).MarshalJSON() })
}

func TestPathUncertainReason(t *testing.T) {
	checkEnum(t, "PathUncertainReason", []enumCase{
		{"UncertainNone", int(model.UncertainNone), "none"},
		{"UncertainNoPath", int(model.UncertainNoPath), "no_path"},
		{"UncertainRNICBusy", int(model.UncertainRNICBusy), "rnic_busy"},
		{"UncertainNonAffinitive", int(model.UncertainNonAffinitive), "non_affinitive"},
		{"UncertainBelowCeiling", int(model.UncertainBelowCeiling), "below_ceiling"},
		{"UncertainNoCeiling", int(model.UncertainNoCeiling), "no_ceiling"},
		{"UncertainHostShift", int(model.UncertainHostShift), "host_shift"},
		{"UncertainCPUBusy", int(model.UncertainCPUBusy), "cpu_busy"},
	}, "unknown",
		func(v int) string { return model.PathUncertainReason(v).String() },
		func(v int) ([]byte, error) { return model.PathUncertainReason(v).MarshalJSON() })
}

func TestLatencyStatus(t *testing.T) {
	// LatencyUnknown's own String() is "unknown", so the sentinel is "invalid".
	checkEnum(t, "LatencyStatus", []enumCase{
		{"LatencyUnknown", int(model.LatencyUnknown), "unknown"},
		{"LatencyNormal", int(model.LatencyNormal), "normal"},
		{"LatencyAbnormal", int(model.LatencyAbnormal), "abnormal"},
	}, "invalid",
		func(v int) string { return model.LatencyStatus(v).String() },
		func(v int) ([]byte, error) { return model.LatencyStatus(v).MarshalJSON() })
}

func TestLatencyReason(t *testing.T) {
	checkEnum(t, "LatencyReason", []enumCase{
		{"LatencyReasonNone", int(model.LatencyReasonNone), "none"},
		{"LatencyByBaseline", int(model.LatencyByBaseline), "user_baseline"},
		{"LatencyBySiblings", int(model.LatencyBySiblings), "siblings"},
		{"LatencyBusy", int(model.LatencyBusy), "busy"},
		{"LatencyFewSiblings", int(model.LatencyFewSiblings), "few_siblings"},
		{"LatencyNoPath", int(model.LatencyNoPath), "no_path"},
		{"LatencyNoReading", int(model.LatencyNoReading), "no_reading"},
		{"LatencyRecheckCleared", int(model.LatencyRecheckCleared), "recheck_cleared"},
		{"LatencyLocalityShift", int(model.LatencyLocalityShift), "locality_shift"},
		{"LatencyCPUBusy", int(model.LatencyCPUBusy), "cpu_busy"},
	}, "unknown",
		func(v int) string { return model.LatencyReason(v).String() },
		func(v int) ([]byte, error) { return model.LatencyReason(v).MarshalJSON() })
}

// TestEnumJSONViaEncoder pins that encoding/json picks the value-receiver
// MarshalJSON up, both for a bare value and inside a struct.
func TestEnumJSONViaEncoder(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{model.LinkGray, `"gray"`},
		{model.PathAbnormal, `"abnormal"`},
		{model.UncertainHostShift, `"host_shift"`},
		{model.LatencyStatus(7), `"invalid"`},
		{model.LatencyLocalityShift, `"locality_shift"`},
		{struct {
			S model.LinkStatus `json:"s"`
		}{model.LinkNormal}, `{"s":"normal"}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%v: %v", c.v, err)
		}
		if string(b) != c.want {
			t.Errorf("json.Marshal(%v) = %s, want %s", c.v, b, c.want)
		}
	}
}

func TestDistance(t *testing.T) {
	want := []struct {
		d     model.Distance
		value int
		key   string
	}{
		{model.DistanceSameSwitch, 1, "same_switch"},
		{model.DistanceMultiSwitch, 2, "multi_switch"},
		{model.DistanceCrossRoot, 3, "cross_root"},
		{model.DistanceCrossSocket, 4, "cross_socket"},
		{model.DistanceLocalMem, 10, "local_mem"},
		{model.DistanceRemoteMem, 11, "remote_mem"},
	}
	got := model.Distances()
	if len(got) != len(want) {
		t.Fatalf("Distances() has %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w.d {
			t.Errorf("Distances()[%d] = %v, want %v", i, got[i], w.d)
		}
		if int(w.d) != w.value {
			t.Errorf("%s = %d, want %d", w.key, int(w.d), w.value)
		}
		if k := w.d.Key(); k != w.key {
			t.Errorf("Distance(%d).Key() = %q, want %q", int(w.d), k, w.key)
		}
		if s := w.d.String(); s != w.key {
			t.Errorf("Distance(%d).String() = %q, want %q", int(w.d), s, w.key)
		}
		// JSON is the bare number: Distance has no MarshalJSON.
		if b, _ := json.Marshal(w.d); string(b) != fmt.Sprint(w.value) {
			t.Errorf("json.Marshal(%s) = %s, want %d", w.key, b, w.value)
		}
		// Parsing is case- and space-insensitive.
		for _, in := range []string{w.key, strings.ToUpper(w.key), "  " + w.key + "\t"} {
			d, err := model.ParseDistance(in)
			if err != nil || d != w.d {
				t.Errorf("ParseDistance(%q) = %v, %v; want %v", in, d, err, w.d)
			}
		}
	}
	// Distance has gaps (5–9) and a zero value: all unknown.
	for _, d := range []model.Distance{0, 5, 9, 12, -1} {
		if k := d.Key(); k != "" {
			t.Errorf("Distance(%d).Key() = %q, want empty", int(d), k)
		}
		if s := d.String(); s != "unknown" {
			t.Errorf("Distance(%d).String() = %q, want unknown", int(d), s)
		}
	}
	for _, in := range []string{"", "nope", "same-switch", "same_switch x"} {
		d, err := model.ParseDistance(in)
		if err == nil {
			t.Errorf("ParseDistance(%q) = %v, want error", in, d)
			continue
		}
		if d != 0 {
			t.Errorf("ParseDistance(%q) returned %v with error, want 0", in, d)
		}
		want := fmt.Sprintf("unknown distance %q (want same_switch, multi_switch, cross_root, cross_socket, local_mem, remote_mem)", in)
		if err.Error() != want {
			t.Errorf("ParseDistance(%q) error = %q, want %q", in, err, want)
		}
	}
}

func TestTimeoutRatio(t *testing.T) {
	cases := []struct {
		name  string
		m     model.MutualMeasurement
		ratio float64
		ok    bool
	}{
		{"not connected", model.MutualMeasurement{Connected: false, Sent: 10, Recv: 10}, 1, true},
		{"not connected, nothing sent", model.MutualMeasurement{}, 1, true},
		{"connected, nothing sent", model.MutualMeasurement{Connected: true}, 0, false},
		{"all replied", model.MutualMeasurement{Connected: true, Sent: 10, Recv: 10}, 0, true},
		{"more received than sent", model.MutualMeasurement{Connected: true, Sent: 10, Recv: 12}, 0, true},
		{"half lost", model.MutualMeasurement{Connected: true, Sent: 10, Recv: 5}, 0.5, true},
		{"one of four lost", model.MutualMeasurement{Connected: true, Sent: 4, Recv: 3}, 0.25, true},
		{"all lost", model.MutualMeasurement{Connected: true, Sent: 7, Recv: 0}, 1, true},
	}
	for _, c := range cases {
		ratio, ok := c.m.TimeoutRatio()
		if ratio != c.ratio || ok != c.ok {
			t.Errorf("%s: TimeoutRatio() = %v, %v; want %v, %v", c.name, ratio, ok, c.ratio, c.ok)
		}
	}
}

func TestCPUBusySockets(t *testing.T) {
	if got := model.CPUBusySockets(nil); got != nil {
		t.Errorf("CPUBusySockets(nil) = %v, want nil", got)
	}
	none := []model.CPULoad{{NUMA: 0, Socket: 0, Busy: 0.9}, {NUMA: 1, Socket: 1, Busy: 0.95}}
	if got := model.CPUBusySockets(none); got != nil {
		t.Errorf("CPUBusySockets(no High) = %v, want nil", got)
	}
	// Two NUMA nodes of socket 1 (SNC) both High collapse to one socket; a
	// non-High node of socket 0 does not appear.
	loads := []model.CPULoad{
		{NUMA: 0, Socket: 0, Busy: 0.1},
		{NUMA: 1, Socket: 0, Busy: 0.2},
		{NUMA: 2, Socket: 1, Busy: 0.9, High: true},
		{NUMA: 3, Socket: 1, Busy: 0.8, High: true},
		{NUMA: 4, Socket: 2, Busy: 0.99, High: true},
	}
	got := model.CPUBusySockets(loads)
	want := map[int]bool{1: true, 2: true}
	if len(got) != len(want) {
		t.Fatalf("CPUBusySockets = %v, want %v", got, want)
	}
	for s := range want {
		if !got[s] {
			t.Errorf("CPUBusySockets = %v, want socket %d", got, s)
		}
	}
	if got[0] {
		t.Errorf("CPUBusySockets = %v, socket 0 must be absent", got)
	}
}

func TestSkippedEndpointCount(t *testing.T) {
	var empty model.Report
	if n := empty.SkippedEndpointCount(""); n != 0 {
		t.Errorf("empty report: %d, want 0", n)
	}
	r := model.Report{SkippedEndpoints: []model.EndpointSkip{
		{Kind: model.EndpointGPU, Reason: "NoGDR: no cuda", Endpoints: []string{"gpu0", "gpu1"}, Probes: 4},
		// gpu1 skipped again for a second reason (another RNIC): counted once.
		{Kind: model.EndpointGPU, Reason: "probe failed", Endpoints: []string{"gpu1", "gpu2"}, Probes: 2},
		{Kind: model.EndpointMemory, Reason: "no rdma device", Endpoints: []string{"mem0"}, Probes: 1},
		// The same ID under another kind is still one endpoint for kind "".
		{Kind: model.EndpointMemory, Reason: "x", Endpoints: []string{"gpu0"}, Probes: 1},
		{Kind: model.EndpointMemory, Reason: "nothing listed", Endpoints: nil, Probes: 3},
	}}
	cases := []struct {
		kind model.EndpointKind
		want int
	}{
		{"", 4},                   // gpu0, gpu1, gpu2, mem0
		{model.EndpointGPU, 3},    // gpu0, gpu1, gpu2
		{model.EndpointMemory, 2}, // mem0, gpu0
		{"nvme", 0},
	}
	for _, c := range cases {
		if n := r.SkippedEndpointCount(c.kind); n != c.want {
			t.Errorf("SkippedEndpointCount(%q) = %d, want %d", c.kind, n, c.want)
		}
	}
}

func TestComputeModeBlocksProbe(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{model.ComputeModeDefault, false},
		{model.ComputeModeExclusiveProcess, true},
		{model.ComputeModeExclusiveThread, true},
		{model.ComputeModeProhibited, true},
		{"", false},
		{"Unknown", false},
		{"default", false},           // case matters: nvidia-smi spelling only
		{"exclusive_process", false}, // ditto
		{"EXCLUSIVE_PROCESS", false},
	}
	for _, c := range cases {
		if got := model.ComputeModeBlocksProbe(c.mode); got != c.want {
			t.Errorf("ComputeModeBlocksProbe(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
	if model.ComputeModeDefault != "Default" || model.ComputeModeExclusiveProcess != "Exclusive_Process" ||
		model.ComputeModeExclusiveThread != "Exclusive_Thread" || model.ComputeModeProhibited != "Prohibited" {
		t.Error("compute mode constants changed from nvidia-smi spelling")
	}
}

func TestPortLoopbackError(t *testing.T) {
	if got := model.PortLoopbackError(nil); got != nil {
		t.Errorf("PortLoopbackError(nil) = %v, want nil", got)
	}
	inner := fmt.Errorf("mlx5_3: %w", model.ErrProbe)
	err := model.PortLoopbackError(inner)
	if err == nil {
		t.Fatal("PortLoopbackError(err) = nil")
	}
	if err.Error() != inner.Error() {
		t.Errorf("Error() = %q, want the wrapped text %q", err, inner)
	}
	if !errors.Is(err, model.ErrPortLoopback) {
		t.Error("errors.Is(err, ErrPortLoopback) = false")
	}
	if !errors.Is(err, inner) {
		t.Error("errors.Is(err, inner) = false")
	}
	if !errors.Is(err, model.ErrProbe) {
		t.Error("errors.Is(err, ErrProbe) = false: the inner chain must stay reachable")
	}
	if errors.Is(err, model.ErrNoGID) {
		t.Error("errors.Is(err, ErrNoGID) = true for an unrelated sentinel")
	}
	// Wrapping again keeps both matches and the same text.
	outer := fmt.Errorf("port 1: %w", err)
	if !errors.Is(outer, model.ErrPortLoopback) || !errors.Is(outer, model.ErrProbe) {
		t.Error("re-wrapped error lost a sentinel")
	}
	if errors.Is(inner, model.ErrPortLoopback) {
		t.Error("the unwrapped error must not match ErrPortLoopback")
	}
}

func TestSentinelErrors(t *testing.T) {
	cases := []struct {
		err  error
		text string
	}{
		{model.ErrNoGDR, "gpu endpoint requires GPUDirect RDMA"},
		{model.ErrNoDevice, "no rdma device"},
		{model.ErrNoGID, "no active rocev2 gid"},
		{model.ErrProbe, "probe failed"},
		{model.ErrNoLID, "infiniband port has no lid"},
		{model.ErrPortNotActive, "infiniband port not active"},
		{model.ErrPortLoopback, "loopback failed at the port"},
		{model.ErrComputeMode, "compute_mode"},
	}
	for _, c := range cases {
		if c.err.Error() != c.text {
			t.Errorf("%q, want %q", c.err, c.text)
		}
	}
}

func TestPathContainsLink(t *testing.T) {
	p := model.Path{RNIC: "rnic0", Endpoint: "gpu0", LinkIDs: []string{"pcie.rnic0", "sw.0", "rp.0", "pcie.gpu0"}}
	for _, id := range p.LinkIDs {
		if !p.ContainsLink(id) {
			t.Errorf("ContainsLink(%q) = false", id)
		}
	}
	for _, id := range []string{"", "rp.1", "pcie.rnic", "sw.0 "} {
		if p.ContainsLink(id) {
			t.Errorf("ContainsLink(%q) = true", id)
		}
	}
	var empty model.Path
	if empty.ContainsLink("") || empty.ContainsLink("rp.0") {
		t.Error("empty path contains a link")
	}
}

// PCIeLinkID is the "pcie." prefix discovery and the analyzer both rely on
// to find a device's own hop among a path's links.
func TestPCIeLinkID(t *testing.T) {
	for _, w := range []struct{ device, id string }{
		{"rnic0", "pcie.rnic0"},
		{"gpu3", "pcie.gpu3"},
		{"", "pcie."},
	} {
		if got := model.PCIeLinkID(w.device); got != w.id {
			t.Errorf("PCIeLinkID(%q) = %q, want %q", w.device, got, w.id)
		}
	}
}

func TestRootPortPair(t *testing.T) {
	ab := model.NewRootPortPair("rp.a", "rp.b")
	ba := model.NewRootPortPair("rp.b", "rp.a")
	if ab != ba {
		t.Errorf("NewRootPortPair is not canonical: %v vs %v", ab, ba)
	}
	if ab != (model.RootPortPair{"rp.a", "rp.b"}) {
		t.Errorf("NewRootPortPair = %v, want sorted {rp.a rp.b}", ab)
	}
	if s := ab.String(); s != "rp.a+rp.b" {
		t.Errorf("String() = %q, want rp.a+rp.b", s)
	}
	if ab.IsZero() {
		t.Error("IsZero() = true for a pair")
	}
	same := model.NewRootPortPair("rp.0", "rp.0")
	if same != (model.RootPortPair{"rp.0", "rp.0"}) || same.String() != "rp.0+rp.0" || same.IsZero() {
		t.Errorf("same-port pair = %v %q zero=%v", same, same.String(), same.IsZero())
	}
	// Sorting is bytewise, so "rp.10" sorts before "rp.2".
	if p := model.NewRootPortPair("rp.2", "rp.10"); p[0] != "rp.10" || p[1] != "rp.2" {
		t.Errorf("NewRootPortPair(rp.2, rp.10) = %v, want {rp.10 rp.2}", p)
	}
	var zero model.RootPortPair
	if !zero.IsZero() {
		t.Error("zero value IsZero() = false")
	}
	if s := zero.String(); s != "+" {
		t.Errorf("zero String() = %q, want \"+\"", s)
	}
	// A half pair (one empty side) is not the zero pair, and "" sorts first.
	half := model.NewRootPortPair("rp.0", "")
	if half.IsZero() || half != (model.RootPortPair{"", "rp.0"}) || half.String() != "+rp.0" {
		t.Errorf("half pair = %v zero=%v %q", half, half.IsZero(), half.String())
	}
	// It marshals as a JSON array of two strings.
	b, err := json.Marshal(ab)
	if err != nil || string(b) != `["rp.a","rp.b"]` {
		t.Errorf("json.Marshal(pair) = %s, %v", b, err)
	}
}

func TestLinkStateUtil(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name  string
		util  *float64
		ok    bool
		value float64
	}{
		{"unread", nil, false, 0},
		{"zero", f(0), true, 0},
		{"0.42", f(0.42), true, 0.42},
		{"one", f(1), true, 1},
	}
	for _, c := range cases {
		l := model.LinkState{ID: "x", Utilization: c.util}
		u, ok := l.Util()
		if ok != c.ok || u != c.value {
			t.Errorf("%s: Util() = %v, %v; want %v, %v", c.name, u, ok, c.value, c.ok)
		}
	}
}

// TestStringConstants pins the string constants that reach JSON reports,
// metric attributes, flags and the topology diagram. Each line is a contract
// with a consumer outside this package.
func TestStringConstants(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"AddrRoCEv2GID", model.AddrRoCEv2GID, "rocev2_gid"},
		{"AddrIBLID", model.AddrIBLID, "ib_lid"},
		{"AddrIBLIDGRH", model.AddrIBLIDGRH, "ib_lid_grh"},
		{"LinkLayerEthernet", model.LinkLayerEthernet, "Ethernet"},
		{"LinkLayerInfiniBand", model.LinkLayerInfiniBand, "InfiniBand"},

		{"EndpointGPU", string(model.EndpointGPU), "gpu"},
		{"EndpointMemory", string(model.EndpointMemory), "memory"},

		{"TriggerIdle", string(model.TriggerIdle), "idle"},
		{"TriggerPFC", string(model.TriggerPFC), "pfc"},
		{"TriggerDrops", string(model.TriggerDrops), "packet_drops"},
		{"TriggerManual", string(model.TriggerManual), "manual"},
		{"TriggerPeriodic", string(model.TriggerPeriodic), "periodic"},
		{"TriggerMutualTimeout", string(model.TriggerMutualTimeout), "mutual_timeout"},

		{"ModeIdle", string(model.ModeIdle), "idle"},
		{"ModeBusy", string(model.ModeBusy), "busy"},

		{"TopoNodeSocket", model.TopoNodeSocket, "socket"},
		{"TopoNodeNUMA", model.TopoNodeNUMA, "numa"},
		{"TopoNodeMemory", model.TopoNodeMemory, "memory"},
		{"TopoNodeRootPort", model.TopoNodeRootPort, "root_port"},
		{"TopoNodeSwitch", model.TopoNodeSwitch, "switch"},
		{"TopoNodeRNIC", model.TopoNodeRNIC, "rnic"},
		{"TopoNodeGPU", model.TopoNodeGPU, "gpu"},
		{"TopoEdgeNoLink", model.TopoEdgeNoLink, "none"},
		{"TopoEdgeKindPackage", model.TopoEdgeKindPackage, "package"},

		{"ExcludeReasonOperator", model.ExcludeReasonOperator, "operator"},
		{"LinkKindRootPort", model.LinkKindRootPort, "root_port"},
		{"LinkKindUPI", model.LinkKindUPI, "upi"},
		{"LinkKindSNCMesh", model.LinkKindSNCMesh, "snc_mesh"},

		{"LatencyRecheckSkipCap", string(model.LatencyRecheckSkipCap), "cap"},

		{"BWRecheckConfirmed", string(model.BWRecheckConfirmed), "confirmed"},
		{"BWRecheckCleared", string(model.BWRecheckCleared), "cleared"},
		{"BWRecheckHostShift", string(model.BWRecheckHostShift), "host_shift"},

		{"CauseRNICPCIeFailure", model.CauseRNICPCIeFailure, "rnic_pcie_failure"},
		{"CauseGPUPCIeFailure", model.CauseGPUPCIeFailure, "gpu_pcie_failure"},
		{"CauseMemoryChannelFailure", model.CauseMemoryChannelFailure, "memory_channel_failure"},
		{"CauseUPIFailure", model.CauseUPIFailure, "upi_failure"},
		{"CauseCPURootPortFailure", model.CauseCPURootPortFailure, "cpu_root_port_failure"},
		{"CauseMemoryChannelFlapping", model.CauseMemoryChannelFlapping, "memory_channel_flapping"},
		{"CauseACSEnabled", model.CauseACSEnabled, "acs_enabled"},
		{"CauseATSDisabled", model.CauseATSDisabled, "ats_disabled"},
		{"CauseRNICSlowStart", model.CauseRNICSlowStart, "rnic_slow_start"},
		{"CauseRNICTxWindow", model.CauseRNICTxWindow, "rnic_tx_window"},
		{"CauseOverloadedUPI", model.CauseOverloadedUPI, "overloaded_intersocket_bus"},
		{"CauseOverloadedMemory", model.CauseOverloadedMemory, "overloaded_memory_channel"},
		{"CauseRNICConnectFailure", model.CauseRNICConnectFailure, "rnic_connectivity_failure"},
		{"CauseRNICIntermittent", model.CauseRNICIntermittent, "rnic_intermittent_timeout"},
		{"CauseToRSwitchAnomaly", model.CauseToRSwitchAnomaly, "tor_switch_anomaly"},
		{"CauseRNICPacketDrops", model.CauseRNICPacketDrops, "rnic_packet_drops"},
		{"CauseOverloadedRNICPCIe", model.CauseOverloadedRNICPCIe, "overloaded_rnic_pcie"},
		{"CauseGPUMRRegistrationSlow", model.CauseGPUMRRegistrationSlow, "gpu_mr_registration_slow"},
		{"CausePathLatencyAbnormal", model.CausePathLatencyAbnormal, "path_latency_abnormal"},
		{"CauseRNICPCIeUndersized", model.CauseRNICPCIeUndersized, "rnic_pcie_undersized"},
		{"CauseUnknown", model.CauseUnknown, "unknown"},
		{"CauseRNICCrossRailUnreachable", model.CauseRNICCrossRailUnreachable, "rnic_cross_rail_unreachable"},
		{"CauseSNCMeshFailure", model.CauseSNCMeshFailure, "snc_mesh_failure"},

		{"MutualScopeRail", string(model.MutualScopeRail), "rail"},
		{"MutualScopeHost", string(model.MutualScopeHost), "host"},
		{"MutualTransportRC", string(model.MutualTransportRC), "rc"},
		{"MutualTransportUD", string(model.MutualTransportUD), "ud"},

		{"BaselineSourceDefault", model.BaselineSourceDefault, "default"},
		{"BaselineSourceUser", model.BaselineSourceUser, "user"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
		if seen[c.name] {
			t.Errorf("%s listed twice", c.name)
		}
		seen[c.name] = true
	}
	// The cause codes are distinct: a metric keyed by code must not merge two.
	codes := map[string]string{}
	for _, c := range cases {
		if !strings.HasPrefix(c.name, "Cause") {
			continue
		}
		if prev, dup := codes[c.want]; dup {
			t.Errorf("cause code %q is both %s and %s", c.want, prev, c.name)
		}
		codes[c.want] = c.name
	}
}

// fullReport is a Report with every field, nested struct and pointer set to a
// non-zero value, so marshalling it exercises every json tag in model.go.
func fullReport() model.Report {
	util := 0.42
	return model.Report{
		Trigger:   model.TriggerPFC,
		Mode:      model.ModeBusy,
		Host:      "gpu-node-01",
		Started:   time.Date(2026, time.January, 2, 3, 4, 5, 6, time.UTC),
		Elapsed:   1500 * time.Millisecond, // json:"-"
		ElapsedNs: 1500000000,
		Paths: []model.PathMeasurement{{
			RNIC:            "rnic0",
			Endpoint:        "gpu0",
			EndpointKind:    model.EndpointGPU,
			SmallLatNsP50:   1800,
			SmallLatNsP99:   2500,
			LargeLatNsP50:   64000,
			BandwidthBPS:    24000000000,
			Status:          model.PathUncertain,
			UncertainReason: model.UncertainHostShift,
			BusyRecheck: &model.BusyRecheck{
				PathBPS:    23000000000,
				SiblingBPS: 24500000000,
				Siblings:   []string{"rnic0->gpu1", "rnic0->gpu2"},
				Rounds:     3,
				Confirmed:  true,
			},
			BWRecheck: &model.BandwidthRecheck{
				Path:        "rnic0->gpu0",
				PathBPS:     23500000000,
				RefBPS:      23600000000,
				BaselineBPS: 24800000000,
				Refs:        []string{"rnic1->gpu4", "rnic2->gpu6"},
				Rounds:      2,
				Outcome:     model.BWRecheckHostShift,
			},
			LatencyStatus: model.LatencyAbnormal,
			LatencyReason: model.LatencyBySiblings,
			LatencyRefNs:  1500,
			LatencyRecheck: &model.LatencyRecheck{
				PathNsP50:    1790,
				SiblingNsP50: 1510,
				Siblings:     []string{"rnic1->gpu4"},
				Confirmed:    true,
			},
			LatencyRecheckSkipped: model.LatencyRecheckSkipCap,
			LatencyLocality: &model.LatencyLocality{
				NUMA:  1,
				RefNs: 1700,
				RNICs: []string{"rnic1", "rnic2"},
			},
			LatencyStreak: 3,
			BaselineClass: "cross_root/fast",
			Verbs: model.VerbNs{
				GPUAlloc:  100,
				MRReg:     200,
				QPSetup:   300,
				MemWarmup: 400,
			},
			Addressing: model.PortAddressing{Mode: model.AddrIBLID, Port: 1, LID: 7}, // json:"-"
		}},
		SkippedEndpoints: []model.EndpointSkip{{
			Kind:      model.EndpointGPU,
			Reason:    "NoGDR: no cuda",
			Endpoints: []string{"gpu7"},
			Probes:    2,
		}},
		Links: []model.LinkState{{
			ID:            "pcie.rnic0",
			Status:        model.LinkGray,
			AbnormalCount: 2,
			AbnormalRNICs: map[string]struct{}{"rnic0": {}}, // json:"-"
			Utilization:   &util,
			Tested:        true, // json:"-"
		}},
		Causes: []model.RootCause{{
			Code:       model.CauseRNICPCIeFailure,
			Summary:    "rnic0 PCIe link degraded",
			Links:      []string{"pcie.rnic0"},
			RNICs:      []string{"rnic0"},
			Endpoints:  []string{"gpu0"},
			Confidence: "high",
			Evidence:   "util=0.42",
		}},
		Mutual: []model.MutualMeasurement{{
			Src:          "rnic0",
			Dst:          "rnic1",
			Connected:    true,
			RTTnsP50:     5000,
			RTTnsP99:     9000,
			LossRatio:    0.25,
			Sent:         8,
			Recv:         6,
			Reliable:     true,
			CounterDrops: 3,
			CrossRail:    true,
		}},
		HostIdle:     true,
		AbnormalRNIC: []string{"rnic0"},
		Baselines: []model.BaselineTable{{
			LineRateBPS:    400000000000,
			PCIeCapBPS:     252000000000,
			DerivedFromBPS: 252000000000,
			Section:        "400g",
			Entries: []model.BaselineEntry{{
				Distance:        model.DistanceCrossRoot.Key(),
				BandwidthBPS:    24800000000,
				LatencyNs:       1500,
				BandwidthSource: model.BaselineSourceDefault,
				LatencySource:   model.BaselineSourceUser,
				SubClasses: []model.BaselineSubClass{{
					Name:          "fast",
					BandwidthBPS:  25000000000,
					LatencyNs:     1400,
					RootPortPairs: []model.RootPortPair{model.NewRootPortPair("rp.1", "rp.0")},
				}},
			}},
		}},
		CPULoad: []model.CPULoad{{NUMA: 1, Socket: 1, Busy: 0.93, High: true}},
	}
}

// zeroReport is a Report whose slices hold one zero element each, so the
// omitempty / omitzero decisions of every nested struct are pinned too.
func zeroReport() model.Report {
	return model.Report{
		Paths:            []model.PathMeasurement{{}},
		SkippedEndpoints: []model.EndpointSkip{{}},
		Links:            []model.LinkState{{}},
		Causes:           []model.RootCause{{}},
		Mutual:           []model.MutualMeasurement{{}},
		Baselines: []model.BaselineTable{{
			Entries: []model.BaselineEntry{{SubClasses: []model.BaselineSubClass{{}}}},
		}},
		CPULoad: []model.CPULoad{{}},
	}
}

// topology is every topology-side struct with json tags, in one document.
type topology struct {
	Links     []model.Link         `json:"links"`
	Nodes     []model.TopologyNode `json:"nodes"`
	Edges     []model.TopologyEdge `json:"edges"`
	Endpoints []model.Endpoint     `json:"endpoints"`
	RNICs     []model.RNIC         `json:"rnics"`
	Excluded  []model.ExcludedRNIC `json:"excluded"`
	Paths     []model.Path         `json:"paths"`
}

func fullTopology() topology {
	return topology{
		Links: []model.Link{{
			ID:              "pcie.rnic0",
			Kind:            "pcie_rnic",
			Device:          "mlx5_0",
			Socket:          1,
			RootPort:        "rp.0",
			CapacityBPS:     252000000000,
			NegotiatedBPS:   126000000000,
			CapacityAssumed: true,
			Upstream:        "sw.0",
		}, {}},
		Nodes: []model.TopologyNode{{
			ID:      "gpu0",
			Kind:    model.TopoNodeGPU,
			Title:   "GPU 0",
			Socket:  1,
			NUMA:    2,
			PCIeBDF: "0000:17:00.0",
		}, {NUMA: -1}},
		Edges: []model.TopologyEdge{{
			ID:              "e0",
			Link:            model.TopoEdgeNoLink,
			Source:          "socket0",
			Target:          "numa0",
			Kind:            model.TopoEdgeKindPackage,
			CapacityBPS:     1,
			NegotiatedBPS:   2,
			CapacityAssumed: true,
		}, {}},
		Endpoints: []model.Endpoint{{
			ID:       "gpu0",
			Kind:     model.EndpointGPU,
			Index:    3,
			Socket:   1,
			RootPort: "rp.0",
			NUMA:     2,
			PCIeBDF:  "0000:17:00.0",
			Switch:   "sw.0",
		}, {}},
		RNICs: []model.RNIC{{
			ID:         "rnic0",
			Device:     "mlx5_0",
			Netdev:     "eth0",
			Socket:     1,
			NUMA:       2,
			RootPort:   "rp.0",
			PCIeBDF:    "0000:18:00.0",
			LineRate:   400000000000,
			PCIeCapBPS: 252000000000,
			GIDIndex:   3,
			LinkLayer:  model.LinkLayerEthernet,
			Fabric:     "v4:10.0.1.0",
			Switch:     "sw.0",
		}, {}},
		Excluded: []model.ExcludedRNIC{{
			Device:  "mlx5_9",
			PCIeBDF: "0000:99:00.0",
			Reason:  model.ExcludeReasonOperator,
		}, {}},
		Paths: []model.Path{{
			RNIC:       "rnic0",
			Endpoint:   "gpu0",
			LinkIDs:    []string{"pcie.rnic0", "sw.0", "pcie.gpu0"},
			Distance:   model.DistanceSameSwitch,
			Affinitive: true,
		}, {}},
	}
}

// checkGolden marshals v with indentation and compares it with
// testdata/<name>; -update rewrites the file.
func checkGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from marshalled output (run with -update to accept):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

func TestReportJSONGolden(t *testing.T) {
	checkGolden(t, "report.golden", fullReport())
}

func TestReportZeroJSONGolden(t *testing.T) {
	checkGolden(t, "report_zero.golden", zeroReport())
}

func TestTopologyJSONGolden(t *testing.T) {
	checkGolden(t, "topology.golden", fullTopology())
}

// TestReportJSONHiddenFields pins the fields that must never reach JSON.
func TestReportJSONHiddenFields(t *testing.T) {
	b, err := json.Marshal(fullReport())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"Elapsed"`, `"elapsed"`, `"AbnormalRNICs"`, `"abnormal_rnics":{`, `"Tested"`, `"tested"`, `"Addressing"`, `"addressing"`, `"ib_lid"`} {
		if bytes.Contains(b, []byte(key)) {
			t.Errorf("report JSON contains %s", key)
		}
	}
	// A Report round-trips through encoding/json except for the hidden
	// fields and the enums, which have MarshalJSON but no UnmarshalJSON.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["started"] != "2026-01-02T03:04:05.000000006Z" {
		t.Errorf("started = %v", m["started"])
	}
	if m["elapsed_ns"] != float64(1500000000) {
		t.Errorf("elapsed_ns = %v", m["elapsed_ns"])
	}
}

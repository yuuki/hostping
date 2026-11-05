package analyzer

import (
	"reflect"
	"testing"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// fig2PathStatuses measures every Fig.2 path normal except the ones status
// names ("rnic->endpoint"), and leaves the ones unmeasured names out.
func fig2PathStatuses(g topology.Graph, status map[string]model.PathStatus, unmeasured ...string) []model.PathMeasurement {
	skip := map[string]bool{}
	for _, k := range unmeasured {
		skip[k] = true
	}
	var ms []model.PathMeasurement
	for _, p := range g.Paths {
		k := pathKey(p.RNIC, p.Endpoint)
		if skip[k] {
			continue
		}
		s, ok := status[k]
		if !ok {
			s = model.PathNormal
		}
		ms = append(ms, model.PathMeasurement{RNIC: p.RNIC, Endpoint: p.Endpoint, Status: s})
	}
	return ms
}

// linkStatuses is the link-state map with the given links set; every other
// link is untested.
func linkStatuses(status map[string]model.LinkStatus) map[string]model.LinkState {
	st := map[string]model.LinkState{}
	for id, s := range status {
		st[id] = model.LinkState{ID: id, Status: s}
	}
	return st
}

// NSDI'23 #5 on Fig.2: which abnormal root ports diagnoseRootPorts names, in
// what order, and the evidence each one carries.
func TestDiagnoseRootPorts(t *testing.T) {
	const (
		abn = model.PathAbnormal
		unc = model.PathUncertain
	)
	rp := func(id, ev string) model.RootCause {
		return model.RootCause{
			Code: model.CauseCPURootPortFailure, Summary: "CPU root port bandwidth degraded for transit traffic",
			Links: []string{id}, Confidence: model.ConfidenceHigh, Evidence: ev,
		}
	}
	cases := []struct {
		name       string
		links      map[string]model.LinkStatus
		paths      map[string]model.PathStatus
		unmeasured []string
		want       []model.RootCause
	}{
		{
			// The cross-root evidence wins over an abnormal memory link on
			// the socket, and lists at most four paths.
			name:  "cross-root GPU paths fail in transit",
			links: map[string]model.LinkStatus{"rp.0": model.LinkAbnormal, "mem.0": model.LinkAbnormal},
			paths: map[string]model.PathStatus{
				"rnic0->gpu2": abn, "rnic0->gpu4": abn, "rnic1->gpu0": abn, "rnic2->gpu1": abn, "rnic3->gpu0": abn,
				"rnic0->mem0": abn,
			},
			want: []model.RootCause{rp("rp.0", "P2P under rp.0 is healthy; cross-root GPU paths fail (rnic0->gpu2, rnic0->gpu4, rnic1->gpu0, rnic2->gpu1, …)")},
		},
		{
			// rnic2's own P2P is abnormal and gpu2's affinitive path is
			// uncertain, so neither rnic2->gpu0 nor rnic0->gpu2 (nor rnic1's
			// paths) is transit evidence; rnic0->gpu3 is. An unmeasured path
			// is none either.
			name:  "transit counts only from healthy P2P to a healthy GPU",
			links: map[string]model.LinkStatus{"rp.0": model.LinkAbnormal},
			paths: map[string]model.PathStatus{
				"rnic2->gpu4": abn, "rnic2->gpu0": abn,
				"rnic1->gpu2": unc, "rnic0->gpu2": abn, "rnic1->gpu0": abn,
				"rnic0->gpu3": abn,
			},
			unmeasured: []string{"rnic3->gpu1"},
			want:       []model.RootCause{rp("rp.0", "P2P under rp.0 is healthy; cross-root GPU paths fail (rnic0->gpu3)")},
		},
		{
			// mem.1 is on the other socket; rnic1->mem0 does not cross rp.0.
			name:  "memory on the socket is healthy and paths through the port fail",
			links: map[string]model.LinkStatus{"rp.0": model.LinkAbnormal, "mem.1": model.LinkAbnormal},
			paths: map[string]model.PathStatus{"rnic0->mem0": abn, "rnic0->mem1": abn, "rnic1->mem0": abn},
			want: []model.RootCause{rp("rp.0",
				"P2P under rp.0 is healthy and no memory link on socket 0 is abnormal; paths through it fail (rnic0->mem0, rnic0->mem1)")},
		},
		{
			name:  "an abnormal memory link on the socket explains the paths",
			links: map[string]model.LinkStatus{"rp.0": model.LinkAbnormal, "mem.0": model.LinkAbnormal},
			paths: map[string]model.PathStatus{"rnic0->mem0": abn, "rnic0->mem1": abn},
		},
		{
			name:  "P2P under the root port is unhealthy",
			links: map[string]model.LinkStatus{"rp.0": model.LinkAbnormal},
			paths: map[string]model.PathStatus{"rnic0->gpu1": abn, "rnic1->gpu0": abn},
		},
		{
			name:  "a gray root port is an Algorithm 1 conflict",
			links: map[string]model.LinkStatus{"rp.0": model.LinkGray},
			paths: map[string]model.PathStatus{"rnic1->gpu0": abn},
		},
		{
			// rnic1->gpu0 crosses rp.1 and rp.0, so it is evidence for both;
			// rp.3 fires on the memory branch with no failing path to list.
			name: "every abnormal root port is judged on its own, in graph order",
			links: map[string]model.LinkStatus{
				"rp.3": model.LinkAbnormal, "rp.1": model.LinkAbnormal, "rp.0": model.LinkAbnormal,
			},
			paths: map[string]model.PathStatus{"rnic1->gpu0": abn},
			want: []model.RootCause{
				rp("rp.0", "P2P under rp.0 is healthy; cross-root GPU paths fail (rnic1->gpu0)"),
				rp("rp.1", "P2P under rp.1 is healthy; cross-root GPU paths fail (rnic1->gpu0)"),
				rp("rp.3", "P2P under rp.3 is healthy and no memory link on socket 1 is abnormal; paths through it fail ()"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := topology.Fig2()
			got := diagnoseRootPorts(g, fig2PathStatuses(g, tc.paths, tc.unmeasured...), linkStatuses(tc.links))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diagnoseRootPorts:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

// Which causes suppressOverlaps drops on Fig.2 because another cause in the
// same run explains them, and that it keeps the rest in order.
func TestSuppressOverlaps(t *testing.T) {
	rp := func(links ...string) model.RootCause {
		return model.RootCause{Code: model.CauseCPURootPortFailure, Links: links}
	}
	mem := func(links ...string) model.RootCause {
		return model.RootCause{Code: model.CauseMemoryChannelFailure, Links: links}
	}
	gpu := func(eps ...string) model.RootCause {
		return model.RootCause{Code: model.CauseGPUPCIeFailure, Endpoints: eps}
	}
	acs := model.RootCause{Code: model.CauseACSEnabled}
	ats := model.RootCause{Code: model.CauseATSDisabled}
	type tcase struct {
		name     string
		in, want []model.RootCause
	}
	cases := []tcase{
		{name: "no causes"},
		{
			name: "memory channel on a failed root port's socket",
			in:   []model.RootCause{mem("mem.0"), rp("rp.0"), mem("mem.1")},
			want: []model.RootCause{rp("rp.0"), mem("mem.1")},
		},
		{
			name: "links the graph does not know",
			in:   []model.RootCause{rp("rp.9"), mem("mem.0"), rp("rp.2"), mem("mem.9", "mem.1"), mem("mem.9")},
			want: []model.RootCause{rp("rp.9"), mem("mem.0"), rp("rp.2"), mem("mem.9")},
		},
		{
			name: "ACS enabled suppresses every GPU PCIe failure",
			in:   []model.RootCause{gpu("gpu0"), acs, gpu("gpu5"), mem("mem.0")},
			want: []model.RootCause{acs, mem("mem.0")},
		},
		{
			name: "ATS disabled suppresses every GPU PCIe failure",
			in:   []model.RootCause{ats, gpu("gpu3")},
			want: []model.RootCause{ats},
		},
		{
			name: "GPU PCIe failure naming an RNIC-wide RNIC",
			in: []model.RootCause{
				{Code: model.CauseRNICSlowStart, RNICs: []string{"rnic1"}},
				{Code: model.CauseGPUPCIeFailure, RNICs: []string{"rnic1"}},
				{Code: model.CauseGPUPCIeFailure, RNICs: []string{"rnic2"}},
			},
			want: []model.RootCause{
				{Code: model.CauseRNICSlowStart, RNICs: []string{"rnic1"}},
				{Code: model.CauseGPUPCIeFailure, RNICs: []string{"rnic2"}},
			},
		},
		{
			// Only the failure codes take part: flapping, overload and
			// drops neither suppress nor are suppressed.
			name: "other codes",
			in: []model.RootCause{
				rp("rp.0"),
				{Code: model.CauseMemoryChannelFlapping, Links: []string{"mem.0"}},
				{Code: model.CauseOverloadedMemory, Links: []string{"mem.0"}},
				{Code: model.CauseRNICPacketDrops, RNICs: []string{"rnic0"}},
				gpu("gpu0"),
			},
			want: []model.RootCause{
				rp("rp.0"),
				{Code: model.CauseMemoryChannelFlapping, Links: []string{"mem.0"}},
				{Code: model.CauseOverloadedMemory, Links: []string{"mem.0"}},
				{Code: model.CauseRNICPacketDrops, RNICs: []string{"rnic0"}},
				gpu("gpu0"),
			},
		},
	}
	// A GPU under an RNIC with an RNIC-wide cause: gpu1 is affinitive to
	// rnic0, gpu2 to rnic1; gpu9 is unknown, gpu0 again rnic0's.
	for _, code := range []string{model.CauseRNICPCIeFailure, model.CauseRNICSlowStart, model.CauseRNICTxWindow, model.CauseOverloadedRNICPCIe} {
		wide := model.RootCause{Code: code, RNICs: []string{"rnic0"}}
		cases = append(cases, tcase{
			name: "GPU under an RNIC-wide " + code,
			in:   []model.RootCause{gpu("gpu1"), wide, gpu("gpu2"), gpu("gpu9", "gpu0"), gpu("gpu9")},
			want: []model.RootCause{wide, gpu("gpu2"), gpu("gpu9")},
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := suppressOverlaps(tc.in, topology.Fig2())
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("suppressOverlaps:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

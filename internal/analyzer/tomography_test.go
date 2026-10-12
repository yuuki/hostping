package analyzer_test

import (
	"encoding/json"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func linkStatuses(states []model.LinkState) map[string]model.LinkStatus {
	out := make(map[string]model.LinkStatus, len(states))
	for _, s := range states {
		out[s.ID] = s.Status
	}
	return out
}

// Algorithm 1 must not depend on the order the measurements arrive in.
//
// rnic0 -> mem0 has a single uncertain hop (rp.0); rnic0 -> mem1 crosses that
// same hop plus upi.0. A single greedy pass marks upi.0 abnormal when mem0 is
// processed first and leaves it uncertain otherwise. Missing GPU rows (NoGDR)
// make such partially covered hop sets normal on live hosts.
func TestAlgorithm1IsOrderIndependent(t *testing.T) {
	g := topology.Fig2()
	normal := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "gpu0", Status: model.PathNormal}, // pcie.rnic0, sw.0
		{RNIC: "rnic1", Endpoint: "mem0", Status: model.PathNormal}, // mem.0
		{RNIC: "rnic2", Endpoint: "mem1", Status: model.PathNormal}, // mem.1
	}
	mem0 := model.PathMeasurement{RNIC: "rnic0", Endpoint: "mem0", Status: model.PathAbnormal}
	mem1 := model.PathMeasurement{RNIC: "rnic0", Endpoint: "mem1", Status: model.PathAbnormal}

	forward := analyzer.DetectAbnormalLinks(g, append(append([]model.PathMeasurement(nil), normal...), mem0, mem1), model.ModeIdle, nil)
	reverse := analyzer.DetectAbnormalLinks(g, append(append([]model.PathMeasurement(nil), normal...), mem1, mem0), model.ModeIdle, nil)

	fwd, rev := linkStatuses(forward), linkStatuses(reverse)
	for id, want := range fwd {
		if got := rev[id]; got != want {
			t.Errorf("link %s: measurement order changed status %s -> %s", id, want, got)
		}
	}
	if fwd["rp.0"] != model.LinkAbnormal {
		t.Errorf("rp.0 is the unique uncertain hop of an abnormal path, got %s", fwd["rp.0"])
	}
	// rnic0 -> mem1 is already explained by the abnormal rp.0, so it cannot
	// isolate upi.0 as a second faulty hop.
	if fwd["upi.0"] != model.LinkUncertain {
		t.Errorf("upi.0 must stay uncertain on a path explained by rp.0, got %s", fwd["upi.0"])
	}
}

// A NoGDR host measures memory only, so several hops stay uncovered by normal
// paths. Every permutation of that measurement set must localize the same hops.
func TestAlgorithm1ShuffledMeasurementsAgree(t *testing.T) {
	g := topology.Fig2()
	ms := []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "gpu0", Status: model.PathNormal},
		{RNIC: "rnic1", Endpoint: "gpu2", Status: model.PathAbnormal},
		{RNIC: "rnic1", Endpoint: "mem0", Status: model.PathNormal},
		{RNIC: "rnic2", Endpoint: "mem1", Status: model.PathNormal},
		{RNIC: "rnic0", Endpoint: "mem0", Status: model.PathAbnormal},
		{RNIC: "rnic0", Endpoint: "mem1", Status: model.PathAbnormal},
		{RNIC: "rnic3", Endpoint: "mem0", Status: model.PathAbnormal},
	}
	want := linkStatuses(analyzer.DetectAbnormalLinks(g, ms, model.ModeIdle, nil))

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		shuffled := append([]model.PathMeasurement(nil), ms...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got := linkStatuses(analyzer.DetectAbnormalLinks(g, shuffled, model.ModeIdle, nil))
		for id, st := range want {
			if got[id] != st {
				t.Fatalf("permutation %d: link %s is %s, want %s", i, id, got[id], st)
			}
		}
	}
}

// Gray marking (Algorithm 1 line 15) must not depend on order either: two
// abnormal paths whose hops are all normal both indicate flapping.
func TestAlgorithm1GrayIsOrderIndependent(t *testing.T) {
	g := topology.Fig2()
	normal := []model.PathMeasurement{
		{RNIC: "rnic1", Endpoint: "gpu0", Status: model.PathNormal}, // pcie.gpu0
		{RNIC: "rnic1", Endpoint: "gpu1", Status: model.PathNormal}, // pcie.gpu1
		// pcie.rnic0 is crossed only by rnic0's own paths.
		{RNIC: "rnic0", Endpoint: "mem0", Status: model.PathNormal},
	}
	gpu0 := model.PathMeasurement{RNIC: "rnic0", Endpoint: "gpu0", Status: model.PathAbnormal}
	gpu1 := model.PathMeasurement{RNIC: "rnic0", Endpoint: "gpu1", Status: model.PathAbnormal}

	forward := linkStatuses(analyzer.DetectAbnormalLinks(g, append(append([]model.PathMeasurement(nil), normal...), gpu0, gpu1), model.ModeIdle, nil))
	reverse := linkStatuses(analyzer.DetectAbnormalLinks(g, append(append([]model.PathMeasurement(nil), normal...), gpu1, gpu0), model.ModeIdle, nil))

	for id, want := range forward {
		if got := reverse[id]; got != want {
			t.Errorf("link %s: measurement order changed status %s -> %s", id, want, got)
		}
	}
	for _, id := range []string{"pcie.gpu0", "pcie.gpu1"} {
		if forward[id] != model.LinkGray {
			t.Errorf("%s: abnormal path with only normal hops must go gray, got %s", id, forward[id])
		}
	}
}

// rp1TransitMeasurements leaves rp.1 as the only hop no normal path covers,
// and fails the paths that cross it: rnic1's own local memory path, rnic0's
// cross-root path to gpu2 and rnic2's cross-socket path to gpu3.
func rp1TransitMeasurements() []model.PathMeasurement {
	return []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "mem0", Status: model.PathNormal},   // pcie.rnic0, sw.0, rp.0, mem.0
		{RNIC: "rnic1", Endpoint: "gpu2", Status: model.PathNormal},   // pcie.rnic1, sw.1, pcie.gpu2
		{RNIC: "rnic1", Endpoint: "gpu3", Status: model.PathNormal},   // pcie.gpu3
		{RNIC: "rnic2", Endpoint: "mem0", Status: model.PathNormal},   // pcie.rnic2, sw.2, rp.2, upi.0
		{RNIC: "rnic1", Endpoint: "mem0", Status: model.PathAbnormal}, // + rp.1
		{RNIC: "rnic0", Endpoint: "gpu2", Status: model.PathAbnormal}, // rp.0, rp.1, sw.1, pcie.gpu2
		{RNIC: "rnic2", Endpoint: "gpu3", Status: model.PathAbnormal}, // rp.2, upi.0, rp.1, sw.1, pcie.gpu3
	}
}

func linkState(t *testing.T, states []model.LinkState, id string) model.LinkState {
	t.Helper()
	for _, s := range states {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no link %s", id)
	return model.LinkState{}
}

func abnormalRNICs(s model.LinkState) []string {
	var out []string
	for id := range s.AbnormalRNICs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// An idle run is the paper's full-mesh test: every abnormal path judges every
// hop it crosses, so RNICs under other root ports count toward a root port's
// abnormal_cnt (NSDI'23 §4.3). They used to be kept out, which left rnic0's and
// rnic2's paths with only normal hops and grayed their own, healthy links.
func TestAlgorithm1IdleRemotePathsJudgeRootPort(t *testing.T) {
	g := topology.Fig2()
	states := analyzer.DetectAbnormalLinks(g, rp1TransitMeasurements(), model.ModeIdle, nil)

	rp := linkState(t, states, "rp.1")
	if rp.Status != model.LinkAbnormal {
		t.Fatalf("rp.1: want abnormal, got %s", rp.Status)
	}
	if got, want := abnormalRNICs(rp), []string{"rnic0", "rnic1", "rnic2"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rp.1 judged abnormal by %v, want %v", got, want)
	}
	if rp.AbnormalCount != 3 {
		t.Errorf("rp.1 abnormal_cnt = %d, want 3", rp.AbnormalCount)
	}
	for _, s := range states {
		if s.Status == model.LinkGray {
			t.Errorf("%s: every abnormal path is explained by rp.1, got gray", s.ID)
		}
	}
}

// Busy mode keeps §4.3's restriction for the busy RNIC's root port: service
// traffic on it can depress what other RNICs measure through it, so only the
// busy RNIC may mark those links abnormal. Root ports without a busy RNIC are
// judged as in an idle run.
func TestAlgorithm1BusyRootPortJudgedOnlyByItsRNIC(t *testing.T) {
	g := topology.Fig2()

	t.Run("busy RNIC under the root port", func(t *testing.T) {
		states := analyzer.DetectAbnormalLinks(g, rp1TransitMeasurements(), model.ModeBusy, map[string]bool{"rnic1": true})
		rp := linkState(t, states, "rp.1")
		if rp.Status != model.LinkAbnormal {
			t.Fatalf("rp.1: want abnormal from its busy RNIC, got %s", rp.Status)
		}
		if got := abnormalRNICs(rp); strings.Join(got, ",") != "rnic1" {
			t.Errorf("rp.1 judged abnormal by %v, want only the busy rnic1", got)
		}
	})

	t.Run("other RNICs alone cannot mark it", func(t *testing.T) {
		var ms []model.PathMeasurement
		for _, m := range rp1TransitMeasurements() {
			if m.RNIC == "rnic1" && m.Endpoint == "mem0" {
				continue
			}
			ms = append(ms, m)
		}
		states := analyzer.DetectAbnormalLinks(g, ms, model.ModeBusy, map[string]bool{"rnic1": true})
		for _, id := range []string{"rp.1", "sw.1", "pcie.gpu2", "pcie.gpu3"} {
			if s := linkState(t, states, id); s.Status == model.LinkAbnormal || s.AbnormalCount != 0 {
				t.Errorf("%s: only busy rnic1 may mark it abnormal, got %s abnormal_cnt=%d", id, s.Status, s.AbnormalCount)
			}
		}
		// No path allowed to decide rp.1 crossed it, so this run did not test it.
		if s := linkState(t, states, "rp.1"); s.Tested {
			t.Error("rp.1: judged only by other RNICs' paths, must not count as tested")
		}
	})

	t.Run("busy RNIC elsewhere", func(t *testing.T) {
		states := analyzer.DetectAbnormalLinks(g, rp1TransitMeasurements(), model.ModeBusy, map[string]bool{"rnic3": true})
		if rp := linkState(t, states, "rp.1"); rp.AbnormalCount != 3 {
			t.Errorf("rp.1 has no busy RNIC, want idle abnormal_cnt 3, got %d", rp.AbnormalCount)
		}
	})
}

// Every uncertain path has to say why it is uncertain.
//
// The ways ClassifyPaths reaches uncertain are not the same answer: a
// non-affinitive GPU that cleared its reduced baseline is a path hostping
// declined to call healthy, while a missing graph route is a path it could not
// judge at all. The declined group is also the common one — every GPU is
// affinitive to exactly one RNIC, so most of a multi-GPU host's RNIC→GPU mesh
// reports uncertain on every healthy run, and without a reason that permanent
// block is indistinguishable from a fault.
func TestClassifyPathsRecordsUncertainReason(t *testing.T) {
	// An affinitive 200G path with nothing broken: over the Fig.4 baseline and
	// over min(line rate, hop capacity) × (1-AbnormalTh).
	const healthy = 170 * gbps

	cases := []struct {
		name       string
		mode       model.RunMode
		busy       map[string]bool
		ms         []model.PathMeasurement
		wantStatus model.PathStatus
		wantReason model.PathUncertainReason
	}{{
		name:       "endpoint the graph has no route to",
		mode:       model.ModeIdle,
		ms:         []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "gpu99", BandwidthBPS: healthy}},
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainNoPath,
	}, {
		name:       "idle non-affinitive GPU over its reduced baseline",
		mode:       model.ModeIdle,
		ms:         []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "gpu2", BandwidthBPS: healthy}},
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainNonAffinitive,
	}, {
		name:       "idle affinitive GPU at what the hardware carries",
		mode:       model.ModeIdle,
		ms:         []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: healthy}},
		wantStatus: model.PathNormal,
	}, {
		name:       "idle memory node over its baseline",
		mode:       model.ModeIdle,
		ms:         []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: healthy}},
		wantStatus: model.PathNormal,
	}, {
		name:       "idle path below its baseline",
		mode:       model.ModeIdle,
		ms:         []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 120 * gbps}},
		wantStatus: model.PathAbnormal,
	}, {
		name:       "busy RNIC, path outside the reduced probe set",
		mode:       model.ModeBusy,
		busy:       map[string]bool{"rnic0": true},
		ms:         []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "gpu4", BandwidthBPS: healthy}},
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainRNICBusy,
	}, {
		// Nothing affinitive measured anything, so there is no sibling to
		// stand in for the idle baseline the load invalidated.
		name:       "busy RNIC with no affinitive reference",
		mode:       model.ModeBusy,
		busy:       map[string]bool{"rnic0": true},
		ms:         []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 0}},
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainRNICBusy,
	}, {
		name: "busy RNIC, path matching its affinitive siblings",
		mode: model.ModeBusy,
		busy: map[string]bool{"rnic0": true},
		ms: []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: healthy},
			{RNIC: "rnic0", Endpoint: "gpu1", BandwidthBPS: healthy},
		},
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainRNICBusy,
	}, {
		name: "busy RNIC, path below its affinitive siblings",
		mode: model.ModeBusy,
		busy: map[string]bool{"rnic0": true},
		ms: []model.PathMeasurement{
			{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 100 * gbps},
			{RNIC: "rnic0", Endpoint: "gpu1", BandwidthBPS: healthy},
		},
		wantStatus: model.PathAbnormal,
	}, {
		// Busy mode is per RNIC: an unloaded one is still judged the idle way,
		// so its reason must be the idle one.
		name:       "unloaded RNIC during a busy run",
		mode:       model.ModeBusy,
		busy:       map[string]bool{"rnic1": true},
		ms:         []model.PathMeasurement{{RNIC: "rnic0", Endpoint: "gpu2", BandwidthBPS: healthy}},
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainNonAffinitive,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := analyzer.ClassifyPaths(topology.Fig2(), tc.ms, tc.mode, tc.busy, config.Defaults(), baseline.Overlay{})
			got := out[0]
			if got.Status != tc.wantStatus || got.UncertainReason != tc.wantReason {
				t.Fatalf("%s->%s at %d bps: got %s/%s, want %s/%s",
					got.RNIC, got.Endpoint, got.BandwidthBPS,
					got.Status, got.UncertainReason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// An affinitive path that clears its baseline but not its ceiling is left
// uncertain — a baseline alone never promotes a path to normal — but it must
// not be reported as non-affinitive. non_affinitive is the steady state of most
// of the RNIC→GPU mesh on a healthy host; an affinitive path short of what its
// hardware carries is not, and folding it into that bucket hides it from an
// operator who filters the steady state out.
func TestClassifyPathsBelowCeilingReason(t *testing.T) {
	cases := []struct {
		name       string
		lineRate   uint64
		endpoint   string
		bw         uint64
		wantStatus model.PathStatus
		wantReason model.PathUncertainReason
	}{{
		// 400G RNIC behind a ~252 Gb/s hop: the scaled Fig.4 baseline asks
		// for about 199 Gb/s, the ceiling for 252 × 0.8 = 201.6 Gb/s.
		name:       "400G affinitive GPU between baseline and ceiling",
		lineRate:   400 * gbps,
		endpoint:   "gpu0",
		bw:         200 * gbps,
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainBelowCeiling,
	}, {
		name:       "400G non-affinitive GPU at the same rate",
		lineRate:   400 * gbps,
		endpoint:   "gpu2",
		bw:         200 * gbps,
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainNonAffinitive,
	}, {
		// No line rate means no ceiling at all: the path is not below
		// anything, hostping just has nothing to hold it to.
		name:       "affinitive GPU on an RNIC with no line rate",
		lineRate:   0,
		endpoint:   "gpu0",
		bw:         170 * gbps,
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainNoCeiling,
	}, {
		name:       "non-affinitive GPU on an RNIC with no line rate",
		lineRate:   0,
		endpoint:   "gpu2",
		bw:         170 * gbps,
		wantStatus: model.PathUncertain,
		wantReason: model.UncertainNonAffinitive,
	}, {
		name:       "memory on an RNIC with no line rate",
		lineRate:   0,
		endpoint:   "mem0",
		bw:         170 * gbps,
		wantStatus: model.PathNormal,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := analyzer.ClassifyPaths(fig2WithLineRate(tc.lineRate), []model.PathMeasurement{{
				RNIC: "rnic0", Endpoint: tc.endpoint, BandwidthBPS: tc.bw,
			}}, model.ModeIdle, nil, config.Defaults(), baseline.Overlay{})
			got := out[0]
			if got.Status != tc.wantStatus || got.UncertainReason != tc.wantReason {
				t.Fatalf("rnic0->%s at %d bps on a %d bps RNIC: got %s/%s, want %s/%s",
					tc.endpoint, tc.bw, tc.lineRate,
					got.Status, got.UncertainReason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// The engine reclassifies measurements every run, and a path that is decided
// now must not keep the reason it carried while it was not.
func TestClassifyPathsClearsReasonOnceDecided(t *testing.T) {
	out := analyzer.ClassifyPaths(topology.Fig2(), []model.PathMeasurement{{
		RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 170 * gbps,
		Status: model.PathUncertain, UncertainReason: model.UncertainNonAffinitive,
	}}, model.ModeIdle, nil, config.Defaults(), baseline.Overlay{})

	if out[0].Status != model.PathNormal || out[0].UncertainReason != model.UncertainNone {
		t.Fatalf("rnic0->gpu0 = %s/%s, want normal carrying no reason", out[0].Status, out[0].UncertainReason)
	}
}

// The JSON report is where an operator reads this, so the reason has to survive
// marshalling — and stay absent on a decided path, where it would only read as
// a field that lost its meaning.
func TestClassifyPathsReportsReasonInJSON(t *testing.T) {
	out := analyzer.ClassifyPaths(topology.Fig2(), []model.PathMeasurement{
		{RNIC: "rnic0", Endpoint: "gpu2", BandwidthBPS: 170 * gbps},
		{RNIC: "rnic0", Endpoint: "gpu0", BandwidthBPS: 170 * gbps},
	}, model.ModeIdle, nil, config.Defaults(), baseline.Overlay{})

	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"status":"uncertain","uncertain_reason":"non_affinitive"`) {
		t.Errorf("non-affinitive GPU must report its reason, got %s", got)
	}
	if n := strings.Count(got, "uncertain_reason"); n != 1 {
		t.Errorf("uncertain_reason appears %d times, want only on the uncertain path: %s", n, got)
	}
}

// busyStatus classifies a busy-mode run of one loaded RNIC and returns the
// verdict per endpoint.
func busyStatus(t *testing.T, rnic string, bw map[string]uint64) map[string]model.PathMeasurement {
	t.Helper()
	return busyStatusIn(t, topology.Fig2(), rnic, bw)
}

func busyStatusIn(t *testing.T, g topology.Graph, rnic string, bw map[string]uint64) map[string]model.PathMeasurement {
	t.Helper()
	ms := make([]model.PathMeasurement, 0, len(bw))
	for _, ep := range []string{"gpu0", "gpu1", "gpu2", "mem0", "mem1"} {
		if v, ok := bw[ep]; ok {
			ms = append(ms, model.PathMeasurement{RNIC: rnic, Endpoint: ep, BandwidthBPS: v})
		}
	}
	if len(ms) != len(bw) {
		t.Fatalf("test names an endpoint outside gpu0/gpu1/gpu2/mem0/mem1: %v", bw)
	}
	out := analyzer.ClassifyPaths(g, ms,
		model.ModeBusy, map[string]bool{rnic: true}, config.Defaults(), baseline.Overlay{})
	got := make(map[string]model.PathMeasurement, len(out))
	for _, m := range out {
		got[m.Endpoint] = m
	}
	return got
}

// A loaded RNIC must not be asked to read the same bandwidth at every
// communication distance.
//
// The reduced busy probe set spans three of them: local memory, remote memory
// over the inter-socket bus, and a GPUDirect RDMA write. Those carry three
// baselines, and on a healthy host they are far enough apart to clear
// AbnormalTh on their own. Judging them against a single per-RNIC maximum
// therefore reported healthy paths as abnormal — and, because the widest
// spread moves between devices from run to run, a different RNIC each time.
func TestClassifyPathsBusyDoesNotCompareAcrossDistances(t *testing.T) {
	// Healthy under load: every distance sits where its own baseline puts it,
	// and the spread between the lowest and the highest is over AbnormalTh.
	got := busyStatus(t, "rnic0", map[string]uint64{
		"mem0": 190 * gbps, // local memory
		"gpu0": 150 * gbps, // GPUDirect RDMA, bus-limited
		"gpu1": 150 * gbps,
		"mem1": 120 * gbps, // remote memory, across the inter-socket bus
	})

	for _, ep := range []string{"mem0", "gpu0", "gpu1", "mem1"} {
		m := got[ep]
		if m.Status != model.PathUncertain || m.UncertainReason != model.UncertainRNICBusy {
			t.Errorf("rnic0->%s at %d bps: got %s/%s, want uncertain/rnic_busy — a healthy loaded host must not report an abnormal path",
				ep, m.BandwidthBPS, m.Status, m.UncertainReason)
		}
	}
}

// Scoping the comparison by distance must not amount to switching it off: a
// path that falls behind the siblings it is comparable with is still abnormal.
func TestClassifyPathsBusyCatchesDegradedSiblingAtSameDistance(t *testing.T) {
	// Same healthy cross-distance spread as above, with one of the two
	// GPUDirect paths a third below the other.
	got := busyStatus(t, "rnic0", map[string]uint64{
		"mem0": 190 * gbps,
		"gpu0": 150 * gbps,
		"gpu1": 100 * gbps, // degraded against its same-switch sibling
		"mem1": 120 * gbps,
	})

	if m := got["gpu1"]; m.Status != model.PathAbnormal {
		t.Errorf("rnic0->gpu1 at %d bps is %.0f%% below its same-switch sibling: got %s, want abnormal",
			m.BandwidthBPS, 100*(1-float64(m.BandwidthBPS)/float64(150*gbps)), m.Status)
	}
	// The sibling it is measured against, and the two memory distances, are
	// not implicated by it.
	for _, ep := range []string{"mem0", "gpu0", "mem1"} {
		if m := got[ep]; m.Status == model.PathAbnormal {
			t.Errorf("rnic0->%s at %d bps: a degraded GPUDirect path must not drag its neighbours abnormal", ep, m.BandwidthBPS)
		}
	}
}

// Remote memory is measured on a loaded RNIC but Fig.2 has one memory node per
// socket, so it has no sibling at its own distance. With nothing comparable to
// judge it by it stays uncertain — the honest answer, and the one §4.3 itself
// falls back on by looking for an idle RNIC that can still use the baseline.
func TestClassifyPathsBusyLeavesLoneDistanceUncertain(t *testing.T) {
	got := busyStatus(t, "rnic0", map[string]uint64{
		"gpu0": 150 * gbps,
		"gpu1": 150 * gbps,
		"mem1": 40 * gbps,
	})

	m := got["mem1"]
	if m.Status != model.PathUncertain || m.UncertainReason != model.UncertainRNICBusy {
		t.Fatalf("rnic0->mem1 is the only remote-memory path measured: got %s/%s, want uncertain/rnic_busy", m.Status, m.UncertainReason)
	}
}

// threeSiblingFig2 is Fig.2 with gpu2 moved under rnic0's switch, so the loaded
// rnic0 has a same-switch class of three paths: gpu0, gpu1 and gpu2.
func threeSiblingFig2() topology.Graph {
	g := topology.Fig2()
	for i, p := range g.Paths {
		if p.RNIC == "rnic0" && p.Endpoint == "gpu2" {
			g.Paths[i].Distance = model.DistanceSameSwitch
			g.Paths[i].Affinitive = true
		}
	}
	return g
}

// Healthy siblings on a loaded RNIC scatter by several percent per reading, and
// the highest of them is biased upward by that scatter. Judged against the
// maximum, a healthy path at the low end of the scatter falls more than
// AbnormalTh below one noisy-high sibling; judged against the median of the
// others, it does not.
func TestClassifyPathsBusyNoisyHighSiblingDoesNotFlagHealthyOnes(t *testing.T) {
	got := busyStatusIn(t, threeSiblingFig2(), "rnic0", map[string]uint64{
		"gpu0": 190 * gbps, // noisy-high reading
		"gpu1": 150 * gbps, // 21% below gpu0: abnormal against the maximum
		"gpu2": 160 * gbps,
	})
	if float64(150*gbps) >= float64(190*gbps)*(1-config.Defaults().AbnormalTh) {
		t.Fatal("gpu1 no longer sits past AbnormalTh below the noisy maximum; the case tests nothing")
	}
	for _, ep := range []string{"gpu0", "gpu1", "gpu2"} {
		if m := got[ep]; m.Status != model.PathUncertain || m.UncertainReason != model.UncertainRNICBusy {
			t.Errorf("rnic0->%s at %d bps: got %s/%s, want uncertain/rnic_busy — healthy siblings under noise must not be abnormal",
				ep, m.BandwidthBPS, m.Status, m.UncertainReason)
		}
	}
}

// A path that really is degraded against its siblings stays abnormal: the
// median of the other two is the healthy level it fell from.
func TestClassifyPathsBusyFlagsDegradedOneOfThree(t *testing.T) {
	got := busyStatusIn(t, threeSiblingFig2(), "rnic0", map[string]uint64{
		"gpu0": 150 * gbps,
		"gpu1": 100 * gbps, // a third below its healthy siblings
		"gpu2": 160 * gbps,
	})
	if m := got["gpu1"]; m.Status != model.PathAbnormal {
		t.Errorf("rnic0->gpu1 at %d bps is a third below its siblings: got %s, want abnormal", m.BandwidthBPS, m.Status)
	}
	for _, ep := range []string{"gpu0", "gpu2"} {
		if m := got[ep]; m.Status == model.PathAbnormal {
			t.Errorf("rnic0->%s at %d bps: the degraded sibling must not drag it abnormal", ep, m.BandwidthBPS)
		}
	}
}

// With one other sibling the median of the others is that sibling, which is
// exactly the comparison the class maximum made: the higher path compared
// against itself and passed, the lower one against the higher. A two-path
// class must classify as it always has.
func TestClassifyPathsBusyTwoSiblingClassUnchanged(t *testing.T) {
	th := config.Defaults().AbnormalTh
	for _, tc := range []struct {
		name         string
		gpu0, gpu1   uint64
		want0, want1 model.PathStatus
	}{
		{"equal", 150 * gbps, 150 * gbps, model.PathUncertain, model.PathUncertain},
		{"within threshold", 150 * gbps, 125 * gbps, model.PathUncertain, model.PathUncertain},
		{"lower past threshold", 150 * gbps, 100 * gbps, model.PathUncertain, model.PathAbnormal},
		{"higher past threshold", 100 * gbps, 150 * gbps, model.PathAbnormal, model.PathUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// What the class-maximum reference decided for the same readings.
			maxRef := max(tc.gpu0, tc.gpu1)
			byMax := func(bw uint64) model.PathStatus {
				if float64(bw) < float64(maxRef)*(1-th) {
					return model.PathAbnormal
				}
				return model.PathUncertain
			}
			if byMax(tc.gpu0) != tc.want0 || byMax(tc.gpu1) != tc.want1 {
				t.Fatal("case expectations disagree with the maximum reference")
			}
			got := busyStatus(t, "rnic0", map[string]uint64{"gpu0": tc.gpu0, "gpu1": tc.gpu1})
			if g := got["gpu0"].Status; g != tc.want0 {
				t.Errorf("gpu0 at %d bps: got %s, want %s", tc.gpu0, g, tc.want0)
			}
			if g := got["gpu1"].Status; g != tc.want1 {
				t.Errorf("gpu1 at %d bps: got %s, want %s", tc.gpu1, g, tc.want1)
			}
		})
	}
}

// A path alone at its distance has no other reading to be judged against,
// however low it reads, next to a three-sibling class as in Fig.2.
func TestClassifyPathsBusyLonePathUncertainNextToThreeSiblings(t *testing.T) {
	got := busyStatusIn(t, threeSiblingFig2(), "rnic0", map[string]uint64{
		"gpu0": 150 * gbps,
		"gpu1": 150 * gbps,
		"gpu2": 150 * gbps,
		"mem1": 10 * gbps,
	})
	if m := got["mem1"]; m.Status != model.PathUncertain || m.UncertainReason != model.UncertainRNICBusy {
		t.Fatalf("rnic0->mem1 alone at its distance: got %s/%s, want uncertain/rnic_busy", m.Status, m.UncertainReason)
	}
}

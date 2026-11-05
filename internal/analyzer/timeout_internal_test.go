package analyzer

import (
	"reflect"
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
)

// lost is a pair that never connected; any other recv is the number of the
// 64 probes that got a reply.
const lost = -1

// rail is a same-rail mutual pair of src and dst.
func rail(src, dst string, recv int) model.MutualMeasurement {
	if recv == lost {
		return model.MutualMeasurement{Src: src, Dst: dst, LossRatio: 1}
	}
	return model.MutualMeasurement{Src: src, Dst: dst, Connected: true, Reliable: true, Sent: 64, Recv: uint32(recv), RTTnsP50: 4000}
}

// xrail is a cross-rail mutual pair of src and dst (--mutual-scope=host).
func xrail(src, dst string, recv int) model.MutualMeasurement {
	m := rail(src, dst, recv)
	m.CrossRail = true
	return m
}

// crossMesh is the full cross-rail mesh of the RNICs in groups, a before b
// when a is listed first: pairs inside a group answer, pairs between groups
// never connect.
func crossMesh(groups ...[]string) []model.MutualMeasurement {
	var ids []string
	group := map[string]int{}
	for g, rs := range groups {
		for _, id := range rs {
			ids = append(ids, id)
			group[id] = g
		}
	}
	var out []model.MutualMeasurement
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			recv := 64
			if group[a] != group[b] {
				recv = lost
			}
			out = append(out, xrail(a, b, recv))
		}
	}
	return out
}

// Which timeout causes diagnoseTimeouts (and crossRailTimeouts and
// intermittentTimeouts under it) reports, in what order, with what evidence.
func TestDiagnoseTimeoutsCauses(t *testing.T) {
	down := func(id, ev string) model.RootCause {
		return model.RootCause{
			Code: model.CauseRNICConnectFailure, Summary: "RNIC unreachable: every mutual probe timed out",
			RNICs: []string{id}, Confidence: model.ConfidenceHigh, Evidence: ev,
		}
	}
	pairFail := func(a, b, ev string) model.RootCause {
		return model.RootCause{
			Code: model.CauseRNICConnectFailure, Summary: "RNIC mutual probe failed to connect",
			RNICs: []string{a, b}, Confidence: model.ConfidenceHigh, Evidence: ev,
		}
	}
	tor := func(ev string, ids ...string) model.RootCause {
		return model.RootCause{
			Code: model.CauseToRSwitchAnomaly, Summary: "every RNIC on the host times out on its mutual probes (ToR switch anomaly suspected)",
			RNICs: ids, Confidence: model.ConfidenceMedium, Evidence: ev,
		}
	}
	railsApart := func(ev string, ids ...string) model.RootCause {
		return model.RootCause{
			Code:    model.CauseRNICCrossRailUnreachable,
			Summary: "no RNIC of this host reaches another across subnets (isolated rails by design, or a ToR/fabric routing fault)",
			RNICs:   ids, Confidence: model.ConfidenceLow, Evidence: ev,
		}
	}
	route := func(a, b, ev string) model.RootCause {
		return model.RootCause{
			Code:    model.CauseRNICCrossRailUnreachable,
			Summary: "RNIC pair cannot reach each other across subnets while both reach other RNICs (route between their subnets suspected)",
			RNICs:   []string{a, b}, Confidence: model.ConfidenceMedium, Evidence: ev,
		}
	}
	isolated := func(ev string, ids ...string) model.RootCause {
		return model.RootCause{
			Code:    model.CauseRNICCrossRailUnreachable,
			Summary: "RNICs reach no RNIC outside their group across subnets while other RNICs do (cabled onto an isolated subnet suspected)",
			RNICs:   ids, Confidence: model.ConfidenceMedium, Evidence: ev,
		}
	}
	flap := func(id string, conf model.Confidence, ev string) model.RootCause {
		return model.RootCause{
			Code: model.CauseRNICIntermittent, Summary: "RNIC mutual probes time out intermittently (flapping suspected)",
			RNICs: []string{id}, Confidence: conf, Evidence: ev,
		}
	}
	const r0, r1, r2, r3, r4, r5, r6 = "rnic0", "rnic1", "rnic2", "rnic3", "rnic4", "rnic5", "rnic6"
	cases := []struct {
		name string
		ms   []model.MutualMeasurement
		want []model.RootCause
	}{
		{
			name: "no pair has a ratio",
			ms:   []model.MutualMeasurement{{Src: r0, Dst: r1, Connected: true}},
		},
		{
			// Read as healthy, the unknown pair would keep rnic0 up; read as
			// dead, it would make a second same-rail pair and blame the ToR.
			name: "a pair with no probes sent takes no part",
			ms:   []model.MutualMeasurement{rail(r0, r1, lost), {Src: r0, Dst: r2, Connected: true}},
			want: []model.RootCause{
				down(r0, "rnic0 timeout_ratio=1 on every pair (rnic1)"),
				down(r1, "rnic1 timeout_ratio=1 on every pair (rnic0)"),
			},
		},
		{
			name: "a self pair counts once",
			ms:   []model.MutualMeasurement{rail(r0, r0, lost), rail(r1, r1, 32)},
			want: []model.RootCause{
				down(r0, "rnic0 timeout_ratio=1 on every pair (rnic0)"),
				flap(r1, model.ConfidenceHigh, "rnic1 timeout_ratio=0.500 over 1 pairs > 0.10: rnic1 ↔ rnic1 32/64 timed out"),
			},
		},
		{
			// A QP that connected and got nothing back is as dead as one that
			// never connected.
			name: "an RNIC dead on every pair as their source is down",
			ms: []model.MutualMeasurement{
				rail(r0, r1, lost), rail(r0, r2, 0), rail(r0, r3, lost),
				rail(r1, r2, 64), rail(r1, r3, 64), rail(r2, r3, 64),
			},
			want: []model.RootCause{down(r0, "rnic0 timeout_ratio=1 on every pair (rnic1, rnic2, rnic3)")},
		},
		{
			// The peers are listed in pair order, not sorted.
			name: "an RNIC dead on every pair as their destination is down",
			ms: []model.MutualMeasurement{
				rail(r3, r0, lost), rail(r1, r0, lost), rail(r2, r0, 0),
				rail(r1, r2, 64), rail(r1, r3, 64), rail(r2, r3, 64),
			},
			want: []model.RootCause{down(r0, "rnic0 timeout_ratio=1 on every pair (rnic3, rnic1, rnic2)")},
		},
		{
			name: "dead same-rail pairs between RNICs that reach others are named on the pair",
			ms: []model.MutualMeasurement{
				rail(r0, r1, lost), rail(r0, r2, 64), rail(r0, r3, 64),
				rail(r1, r2, 64), rail(r1, r3, 64), rail(r2, r3, 0),
			},
			want: []model.RootCause{
				pairFail(r0, r1, "rnic0 ↔ rnic1 not connected"),
				pairFail(r2, r3, "rnic2 ↔ rnic3 64/64 timed out"),
			},
		},
		{
			name: "every RNIC dead on every pair blames the ToR",
			ms: []model.MutualMeasurement{
				rail(r0, r1, lost), rail(r0, r2, lost), rail(r0, r3, 0),
				rail(r1, r2, lost), rail(r1, r3, lost), rail(r2, r3, lost),
			},
			want: []model.RootCause{tor("all 4 RNICs time out on all 6 pairs", r0, r1, r2, r3)},
		},
		{
			name: "two dead same-rail pairs are enough for the ToR rule",
			ms:   []model.MutualMeasurement{rail(r2, r3, 0), rail(r0, r1, lost)},
			want: []model.RootCause{tor("all 4 RNICs time out on all 2 pairs", r0, r1, r2, r3)},
		},
		{
			name: "one dead pair is either RNIC, not the ToR",
			ms:   []model.MutualMeasurement{rail(r0, r1, lost)},
			want: []model.RootCause{
				down(r0, "rnic0 timeout_ratio=1 on every pair (rnic1)"),
				down(r1, "rnic1 timeout_ratio=1 on every pair (rnic0)"),
			},
		},
		{
			// rnic4 has only cross-rail pairs, so it is not down, but it
			// times out on all of them, which is all the ToR rule asks.
			name: "an RNIC with only dead cross-rail pairs counts toward the ToR",
			ms: []model.MutualMeasurement{
				rail(r0, r1, lost), rail(r2, r3, lost), xrail(r0, r4, lost), xrail(r4, r3, lost),
			},
			want: []model.RootCause{tor("all 5 RNICs time out on all 4 pairs", r0, r1, r2, r3, r4)},
		},
		{
			name: "one pair that answers keeps the ToR out",
			ms: []model.MutualMeasurement{
				rail(r0, r1, lost), rail(r0, r2, lost), rail(r1, r2, lost), rail(r3, r4, 64),
			},
			want: []model.RootCause{
				down(r0, "rnic0 timeout_ratio=1 on every pair (rnic1, rnic2)"),
				down(r1, "rnic1 timeout_ratio=1 on every pair (rnic0, rnic2)"),
				down(r2, "rnic2 timeout_ratio=1 on every pair (rnic0, rnic1)"),
			},
		},
		{
			name: "an RNIC dead on only cross-rail pairs is not down but cut off",
			ms: []model.MutualMeasurement{
				xrail(r0, r1, 64), xrail(r0, r2, 64), xrail(r0, r3, lost),
				xrail(r1, r2, 64), xrail(r1, r3, lost), xrail(r2, r3, 0),
			},
			want: []model.RootCause{isolated(
				"group [rnic3] times out to every RNIC outside it: rnic0 ↔ rnic3 not connected, rnic1 ↔ rnic3 not connected, rnic2 ↔ rnic3 64/64 timed out",
				r3)},
		},
		{
			// rnic0 is the source of one dead cross-rail pair and the
			// destination of the other; neither is judged again.
			name: "a down RNIC explains its dead cross-rail pairs",
			ms: []model.MutualMeasurement{
				rail(r0, r1, lost), rail(r2, r3, 64),
				xrail(r0, r2, lost), xrail(r3, r0, lost), xrail(r1, r2, 64), xrail(r1, r3, 64),
			},
			want: []model.RootCause{down(r0, "rnic0 timeout_ratio=1 on every pair (rnic1, rnic2, rnic3)")},
		},
		{
			// The same-rail pairs answer, which does not count as reaching
			// across rails.
			name: "rails that never reach each other are one low-confidence note",
			ms: []model.MutualMeasurement{
				rail(r0, r1, 64), rail(r2, r3, 64),
				xrail(r0, r2, lost), xrail(r0, r3, lost), xrail(r1, r2, 0), xrail(r1, r3, lost),
			},
			want: []model.RootCause{railsApart(
				"all 4 cross-rail pairs time out: rnic0 ↔ rnic2 not connected, rnic0 ↔ rnic3 not connected, rnic1 ↔ rnic2 64/64 timed out, rnic1 ↔ rnic3 not connected",
				r0, r1, r2, r3)},
		},
		{
			name: "the low-confidence note sorts its RNICs and lists at most eight pairs",
			ms: []model.MutualMeasurement{
				xrail(r4, r0, lost), xrail(r4, r1, lost), xrail(r4, r2, lost), xrail(r4, r3, lost),
				xrail(r3, r0, lost), xrail(r3, r1, lost), xrail(r3, r2, lost),
				xrail(r2, r0, lost), xrail(r2, r1, lost), xrail(r1, r0, lost),
			},
			want: []model.RootCause{railsApart(
				"all 10 cross-rail pairs time out: rnic4 ↔ rnic0 not connected, rnic4 ↔ rnic1 not connected, "+
					"rnic4 ↔ rnic2 not connected, rnic4 ↔ rnic3 not connected, rnic3 ↔ rnic0 not connected, "+
					"rnic3 ↔ rnic1 not connected, rnic3 ↔ rnic2 not connected, rnic2 ↔ rnic0 not connected, …",
				r0, r1, r2, r3, r4)},
		},
		{
			name: "a group cut off from a unique largest group is named alone",
			ms:   crossMesh([]string{r0, r1, r2, r3}, []string{r4, r5, r6}),
			want: []model.RootCause{isolated(
				"group [rnic4 rnic5 rnic6] times out to every RNIC outside it: rnic0 ↔ rnic4 not connected, "+
					"rnic0 ↔ rnic5 not connected, rnic0 ↔ rnic6 not connected, rnic1 ↔ rnic4 not connected, "+
					"rnic1 ↔ rnic5 not connected, rnic1 ↔ rnic6 not connected, rnic2 ↔ rnic4 not connected, "+
					"rnic2 ↔ rnic5 not connected, …",
				r4, r5, r6)},
		},
		{
			// The first dead pair meets rnic2's group first; the groups are
			// still reported by their first RNIC.
			name: "two equal groups are both named, in RNIC order",
			ms: []model.MutualMeasurement{
				xrail(r0, r1, 64), xrail(r2, r3, 64),
				xrail(r3, r1, lost), xrail(r2, r0, lost), xrail(r3, r0, lost), xrail(r2, r1, lost),
			},
			want: []model.RootCause{
				isolated("group [rnic0 rnic1] times out to every RNIC outside it: rnic3 ↔ rnic1 not connected, "+
					"rnic2 ↔ rnic0 not connected, rnic3 ↔ rnic0 not connected, rnic2 ↔ rnic1 not connected", r0, r1),
				isolated("group [rnic2 rnic3] times out to every RNIC outside it: rnic3 ↔ rnic1 not connected, "+
					"rnic2 ↔ rnic0 not connected, rnic3 ↔ rnic0 not connected, rnic2 ↔ rnic1 not connected", r2, r3),
			},
		},
		{
			// rnic0 and rnic1 are joined by their same-rail pair only; one
			// cross-rail pair that answers makes the split a cut, not rails
			// apart by design.
			name: "a same-rail pair that answers joins a group",
			ms: []model.MutualMeasurement{
				rail(r0, r1, 64), xrail(r2, r3, 64),
				xrail(r0, r2, lost), xrail(r0, r3, lost), xrail(r1, r2, lost), xrail(r1, r3, lost),
			},
			want: []model.RootCause{
				isolated("group [rnic0 rnic1] times out to every RNIC outside it: rnic0 ↔ rnic2 not connected, "+
					"rnic0 ↔ rnic3 not connected, rnic1 ↔ rnic2 not connected, rnic1 ↔ rnic3 not connected", r0, r1),
				isolated("group [rnic2 rnic3] times out to every RNIC outside it: rnic0 ↔ rnic2 not connected, "+
					"rnic0 ↔ rnic3 not connected, rnic1 ↔ rnic2 not connected, rnic1 ↔ rnic3 not connected", r2, r3),
			},
		},
		{
			name: "a dead cross-rail pair inside a group is the route between two subnets",
			ms: []model.MutualMeasurement{
				xrail(r0, r1, 64), xrail(r2, r0, 0), xrail(r0, r3, 64),
				xrail(r1, r2, 64), xrail(r1, r3, 64), xrail(r2, r3, 64),
			},
			want: []model.RootCause{route(r2, r0, "rnic2 ↔ rnic0 64/64 timed out")},
		},
		{
			// The two groups are as large as each other, so both are named;
			// rnic3 ↔ rnic5 is inside one of them and is no part of its cut.
			name: "a dead pair inside a named group is a route, not part of the cut",
			ms: []model.MutualMeasurement{
				xrail(r3, r5, lost),
				xrail(r0, r1, 64), xrail(r1, r2, 64), xrail(r3, r4, 64), xrail(r4, r5, 64),
				xrail(r0, r3, lost), xrail(r2, r4, lost),
			},
			want: []model.RootCause{
				route(r3, r5, "rnic3 ↔ rnic5 not connected"),
				isolated("group [rnic0 rnic1 rnic2] times out to every RNIC outside it: rnic0 ↔ rnic3 not connected, rnic2 ↔ rnic4 not connected", r0, r1, r2),
				isolated("group [rnic3 rnic4 rnic5] times out to every RNIC outside it: rnic0 ↔ rnic3 not connected, rnic2 ↔ rnic4 not connected", r3, r4, r5),
			},
		},
		{
			// rnic1's peers see its losses on their pair with it and are
			// cleared, since rnic1's own ratio is higher.
			name: "a flapping RNIC is named, the peers it explains are not",
			ms: []model.MutualMeasurement{
				rail(r0, r1, 32), rail(r0, r2, 64), rail(r0, r3, 64),
				rail(r1, r2, 32), rail(r1, r3, 32), rail(r2, r3, 64),
			},
			want: []model.RootCause{flap(r1, model.ConfidenceHigh,
				"rnic1 timeout_ratio=0.500 over 3 pairs > 0.10: rnic0 ↔ rnic1 32/64 timed out, rnic1 ↔ rnic2 32/64 timed out, rnic1 ↔ rnic3 32/64 timed out")},
		},
		{
			// One of each rule: rnic0 is down; rnic2 ↔ rnic3 is a dead
			// same-rail pair; rnic4 ↔ rnic2 is a dead cross-rail pair inside
			// the group {rnic1..rnic4}; {rnic5, rnic6} is cut off from it; and
			// rnic4 flaps on its cross-rail pair with rnic1, which it
			// explains for rnic1.
			name: "the rules report in order",
			ms: []model.MutualMeasurement{
				rail(r0, r1, lost), xrail(r0, r2, lost), rail(r1, r2, 64), rail(r2, r3, lost),
				xrail(r3, r1, 64), xrail(r1, r4, 32), xrail(r4, r2, lost),
				rail(r5, r6, 64), xrail(r5, r3, lost), xrail(r6, r1, lost),
			},
			want: []model.RootCause{
				down(r0, "rnic0 timeout_ratio=1 on every pair (rnic1, rnic2)"),
				pairFail(r2, r3, "rnic2 ↔ rnic3 not connected"),
				route(r4, r2, "rnic4 ↔ rnic2 not connected"),
				isolated("group [rnic5 rnic6] times out to every RNIC outside it: rnic5 ↔ rnic3 not connected, rnic6 ↔ rnic1 not connected", r5, r6),
				flap(r4, model.ConfidenceMedium, "rnic4 timeout_ratio=0.500 over 1 pairs > 0.10: rnic1 ↔ rnic4 32/64 timed out"),
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diagnoseTimeouts(Input{Cfg: config.Defaults(), Mutual: c.ms})
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("diagnoseTimeouts:\n got  %+v\n want %+v", got, c.want)
			}
		})
	}
}

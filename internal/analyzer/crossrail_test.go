package analyzer_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/model"
)

// Cross-rail pairs exist only under --mutual-scope=host. The RNIC names below
// are opaque: which rail an RNIC sits on is carried by the CrossRail mark on
// its pairs, which the engine derives from the fabric keys.

func xprobed(src, dst string, recv uint32) model.MutualMeasurement {
	m := probed(src, dst, recv)
	m.CrossRail = true
	return m
}

func xdead(src, dst string) model.MutualMeasurement {
	m := dead(src, dst)
	m.CrossRail = true
	return m
}

// meshOf builds the full pair mesh of ids, cross-rail everywhere (one RNIC
// per rail, the rail-optimized layout), with the listed pairs dead.
func meshOf(ids []string, deadPairs ...string) []model.MutualMeasurement {
	isDead := map[string]bool{}
	for _, p := range deadPairs {
		isDead[p] = true
	}
	var out []model.MutualMeasurement
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			if isDead[a+"-"+b] {
				out = append(out, xdead(a, b))
			} else {
				out = append(out, xprobed(a, b, 64))
			}
		}
	}
	return out
}

var fourRails = []string{"rnic0", "rnic1", "rnic2", "rnic3"}

func assertCauses(t *testing.T, cs []model.RootCause, want []causeKey) {
	t.Helper()
	if got := keys(cs); len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A rail-optimized host whose RNICs all reach each other across rails has
// nothing to report.
func TestCrossRailAllReachable(t *testing.T) {
	assertCauses(t, timeoutCauses(t, meshOf(fourRails)...), nil)
}

// ToN #14: rnic3 is cabled onto an isolated subnet. It fails to every other
// RNIC while they reach each other across rails. With no same-rail pair it
// is not called down, and the ToR is not blamed; it is named as a
// cross-rail-unreachable RNIC at medium confidence.
func TestCrossRailIsolatedRNIC(t *testing.T) {
	cs := timeoutCauses(t, meshOf(fourRails, "rnic0-rnic3", "rnic1-rnic3", "rnic2-rnic3")...)
	assertCauses(t, cs, []causeKey{{model.CauseRNICCrossRailUnreachable, "rnic3", "medium"}})
	for _, peer := range []string{"rnic0", "rnic1", "rnic2"} {
		if !strings.Contains(cs[0].Evidence, peer+" ↔ rnic3") {
			t.Errorf("evidence %q does not list the pair with %s", cs[0].Evidence, peer)
		}
	}
}

// A subset of RNICs isolated together still reaches itself: the group is
// named as one, and the majority it cannot reach is not.
func TestCrossRailIsolatedGroup(t *testing.T) {
	ids := []string{"rnic0", "rnic1", "rnic2", "rnic3", "rnic4", "rnic5"}
	var cut []string
	for _, a := range []string{"rnic0", "rnic1", "rnic2", "rnic3"} {
		for _, b := range []string{"rnic4", "rnic5"} {
			cut = append(cut, a+"-"+b)
		}
	}
	cs := timeoutCauses(t, meshOf(ids, cut...)...)
	assertCauses(t, cs, []causeKey{{model.CauseRNICCrossRailUnreachable, "rnic4,rnic5", "medium"}})
}

// Two equal halves that cannot reach each other: which one is cut off is not
// something a majority can decide, so both are named.
func TestCrossRailEvenSplitNamesBothGroups(t *testing.T) {
	cs := timeoutCauses(t, meshOf(fourRails, "rnic0-rnic2", "rnic0-rnic3", "rnic1-rnic2", "rnic1-rnic3")...)
	assertCauses(t, cs, []causeKey{
		{model.CauseRNICCrossRailUnreachable, "rnic0,rnic1", "medium"},
		{model.CauseRNICCrossRailUnreachable, "rnic2,rnic3", "medium"},
	})
}

// No RNIC reaches any other across rails. That is what isolated rails look
// like by design, so it is one low-confidence note naming every RNIC, not a
// ToR anomaly and not four unreachable RNICs.
func TestCrossRailUniformFailureIsLowConfidence(t *testing.T) {
	cs := timeoutCauses(t, meshOf(fourRails,
		"rnic0-rnic1", "rnic0-rnic2", "rnic0-rnic3", "rnic1-rnic2", "rnic1-rnic3", "rnic2-rnic3")...)
	assertCauses(t, cs, []causeKey{{model.CauseRNICCrossRailUnreachable, "rnic0,rnic1,rnic2,rnic3", "low"}})
}

// Two rails of two RNICs: the same-rail pairs answer, no cross-rail pair
// does. Still the uniform pattern, since no rail reaches another.
func TestCrossRailUniformFailureWithSameRailPairs(t *testing.T) {
	cs := timeoutCauses(t,
		probed("rnic0", "rnic1", 64), probed("rnic2", "rnic3", 64),
		xdead("rnic0", "rnic2"), xdead("rnic0", "rnic3"), xdead("rnic1", "rnic2"), xdead("rnic1", "rnic3"),
	)
	assertCauses(t, cs, []causeKey{{model.CauseRNICCrossRailUnreachable, "rnic0,rnic1,rnic2,rnic3", "low"}})
}

// An RNIC that times out on its same-rail pair and on every cross-rail pair
// is down, as in rail scope; its dead cross-rail pairs are explained by it.
func TestCrossRailDownRNICWithSameRailPair(t *testing.T) {
	cs := timeoutCauses(t,
		dead("rnic0", "rnic1"), probed("rnic2", "rnic3", 64),
		xdead("rnic0", "rnic2"), xdead("rnic0", "rnic3"), xprobed("rnic1", "rnic2", 64), xprobed("rnic1", "rnic3", 64),
	)
	assertCauses(t, cs, []causeKey{{model.CauseRNICConnectFailure, "rnic0", "high"}})
}

// A cross-rail pair that answers proves its RNICs are up, so a single dead
// same-rail pair between them is reported on the pair, not as two dead RNICs
// the way rail scope, with only that pair to go on, has to.
func TestCrossRailAnswerVetoesDown(t *testing.T) {
	cs := timeoutCauses(t,
		dead("rnic0", "rnic1"),
		xprobed("rnic0", "rnic2", 64), xprobed("rnic1", "rnic2", 64),
	)
	assertCauses(t, cs, []causeKey{{model.CauseRNICConnectFailure, "rnic0,rnic1", "high"}})
}

// One dead cross-rail pair between RNICs that otherwise reach everyone is the
// route between those two subnets.
func TestCrossRailSingleDeadPair(t *testing.T) {
	cs := timeoutCauses(t, meshOf(fourRails, "rnic0-rnic2")...)
	assertCauses(t, cs, []causeKey{{model.CauseRNICCrossRailUnreachable, "rnic0,rnic2", "medium"}})
}

// Every pair dead, with at least two same-rail pairs among them: the ToR rule
// of rail scope still applies.
func TestCrossRailEverythingDeadIsToR(t *testing.T) {
	cs := timeoutCauses(t,
		dead("rnic0", "rnic1"), dead("rnic2", "rnic3"),
		xdead("rnic0", "rnic2"), xdead("rnic0", "rnic3"), xdead("rnic1", "rnic2"), xdead("rnic1", "rnic3"),
	)
	assertCauses(t, cs, []causeKey{{model.CauseToRSwitchAnomaly, "rnic0,rnic1,rnic2,rnic3", "medium"}})
}

// Partial loss on cross-rail pairs only still flags a flapping RNIC, at
// medium confidence since the pairs cross more of the fabric.
func TestCrossRailIntermittentIsMedium(t *testing.T) {
	cs := timeoutCauses(t,
		xprobed("rnic0", "rnic1", 32), xprobed("rnic0", "rnic2", 64), xprobed("rnic0", "rnic3", 64),
		xprobed("rnic1", "rnic2", 32), xprobed("rnic1", "rnic3", 32), xprobed("rnic2", "rnic3", 64),
	)
	assertCauses(t, cs, []causeKey{{model.CauseRNICIntermittent, "rnic1", "medium"}})
}

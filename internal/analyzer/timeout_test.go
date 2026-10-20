package analyzer_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

var timeoutCodes = map[string]bool{
	model.CauseRNICConnectFailure:       true,
	model.CauseRNICIntermittent:         true,
	model.CauseToRSwitchAnomaly:         true,
	model.CauseRNICCrossRailUnreachable: true,
}

func timeoutCauses(t *testing.T, ms ...model.MutualMeasurement) []model.RootCause {
	t.Helper()
	var out []model.RootCause
	for _, c := range analyzer.Analyze(analyzer.Input{
		Graph: topology.Fig2(), Cfg: config.Defaults(), Mutual: ms,
	}).Causes {
		if timeoutCodes[c.Code] {
			out = append(out, c)
		}
	}
	return out
}

// probed is a connected RC pair that got recv of 64 replies back.
func probed(src, dst string, recv uint32) model.MutualMeasurement {
	return model.MutualMeasurement{Src: src, Dst: dst, Connected: true, Reliable: true, Sent: 64, Recv: recv, RTTnsP50: 4000}
}

func dead(src, dst string) model.MutualMeasurement {
	return model.MutualMeasurement{Src: src, Dst: dst, Connected: false, LossRatio: 1}
}

type causeKey struct {
	code  string
	rnics string
	conf  model.Confidence
}

func keys(cs []model.RootCause) []causeKey {
	out := make([]causeKey, 0, len(cs))
	for _, c := range cs {
		out = append(out, causeKey{c.Code, strings.Join(c.RNICs, ","), c.Confidence})
	}
	return out
}

func TestTimeoutRatio(t *testing.T) {
	for _, tc := range []struct {
		m     model.MutualMeasurement
		want  float64
		known bool
	}{
		{probed("a", "b", 64), 0, true},
		{probed("a", "b", 48), 0.25, true},
		{probed("a", "b", 0), 1, true},
		{dead("a", "b"), 1, true},
		{model.MutualMeasurement{Connected: true}, 0, false},
	} {
		got, ok := tc.m.TimeoutRatio()
		if got != tc.want || ok != tc.known {
			t.Errorf("%+v: TimeoutRatio = %v,%v want %v,%v", tc.m, got, ok, tc.want, tc.known)
		}
	}
}

func TestTimeoutHealthyHostHasNoCause(t *testing.T) {
	cs := timeoutCauses(t,
		probed("rnic0", "rnic1", 64), probed("rnic0", "rnic2", 64), probed("rnic0", "rnic3", 64),
		probed("rnic1", "rnic2", 64), probed("rnic1", "rnic3", 63), probed("rnic2", "rnic3", 64),
	)
	if len(cs) != 0 {
		t.Fatalf("want no timeout cause, got %+v", cs)
	}
}

// ToN #13: an RNIC that times out on every pair is down. Its peers' dead pairs
// are explained by it, so only the RNIC itself is named.
func TestTimeoutTotalOnOneRNICIsUnreachable(t *testing.T) {
	cs := timeoutCauses(t,
		dead("rnic0", "rnic1"), dead("rnic0", "rnic2"), dead("rnic0", "rnic3"),
		probed("rnic1", "rnic2", 64), probed("rnic1", "rnic3", 64), probed("rnic2", "rnic3", 64),
	)
	want := []causeKey{{model.CauseRNICConnectFailure, "rnic0", "high"}}
	if got := keys(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A connected QP whose replies all stopped is as unreachable as one that
// never connected.
func TestTimeoutConnectedButNothingBackIsUnreachable(t *testing.T) {
	cs := timeoutCauses(t,
		probed("rnic0", "rnic1", 0), probed("rnic0", "rnic2", 0), probed("rnic0", "rnic3", 0),
		probed("rnic1", "rnic2", 64), probed("rnic1", "rnic3", 64), probed("rnic2", "rnic3", 64),
	)
	want := []causeKey{{model.CauseRNICConnectFailure, "rnic0", "high"}}
	if got := keys(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// ToN #15/#16: a flapping RNIC loses a share of its probes on every pair. The
// healthy peers see the same timeouts on their one pair with it, which the
// flapping RNIC explains, so only it is named.
func TestTimeoutPartialIsIntermittent(t *testing.T) {
	cs := timeoutCauses(t,
		probed("rnic0", "rnic1", 32), probed("rnic0", "rnic2", 64), probed("rnic0", "rnic3", 64),
		probed("rnic1", "rnic2", 32), probed("rnic1", "rnic3", 32), probed("rnic2", "rnic3", 64),
	)
	want := []causeKey{{model.CauseRNICIntermittent, "rnic1", "high"}}
	if got := keys(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, peer := range []string{"rnic0", "rnic2", "rnic3"} {
		if !strings.Contains(cs[0].Evidence, peer) {
			t.Errorf("evidence %q does not list the pair with %s", cs[0].Evidence, peer)
		}
	}
}

// --timeout-th decides: 6/64 ≈ 0.094 on every pair of rnic1 stays under the
// default 0.1 and is flagged under 0.05.
func TestTimeoutThreshold(t *testing.T) {
	ms := []model.MutualMeasurement{
		probed("rnic0", "rnic1", 58), probed("rnic0", "rnic2", 64), probed("rnic0", "rnic3", 64),
		probed("rnic1", "rnic2", 58), probed("rnic1", "rnic3", 58), probed("rnic2", "rnic3", 64),
	}
	cs := timeoutCauses(t, ms...)
	if len(cs) != 0 {
		t.Fatalf("below Timeout_th must not flag, got %+v", cs)
	}
	cfg := config.Defaults()
	cfg.TimeoutTh = 0.05
	res := analyzer.Analyze(analyzer.Input{Graph: topology.Fig2(), Cfg: cfg, Mutual: ms})
	var got []causeKey
	for _, c := range res.Causes {
		if timeoutCodes[c.Code] {
			got = append(got, causeKey{c.Code, strings.Join(c.RNICs, ","), c.Confidence})
		}
	}
	if want := []causeKey{{model.CauseRNICIntermittent, "rnic1", "high"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("--timeout-th 0.05: got %+v, want %+v", got, want)
	}
}

// Two RNICs with only each other to probe cannot be told apart.
func TestTimeoutSinglePartialPairNamesBoth(t *testing.T) {
	cs := timeoutCauses(t, probed("rnic0", "rnic1", 40))
	want := []causeKey{
		{model.CauseRNICIntermittent, "rnic0", "high"},
		{model.CauseRNICIntermittent, "rnic1", "high"},
	}
	if got := keys(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Every RNIC of the host timing out on every pair points at the ToR switch,
// not at four independent RNIC failures.
func TestTimeoutEveryRNICIsToRAnomaly(t *testing.T) {
	cs := timeoutCauses(t,
		dead("rnic0", "rnic1"), dead("rnic0", "rnic2"), dead("rnic0", "rnic3"),
		dead("rnic1", "rnic2"), dead("rnic1", "rnic3"), dead("rnic2", "rnic3"),
	)
	want := []causeKey{{model.CauseToRSwitchAnomaly, "rnic0,rnic1,rnic2,rnic3", "medium"}}
	if got := keys(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// One dead pair alone is not host-wide evidence: either side explains it.
func TestTimeoutSingleDeadPairIsNotToR(t *testing.T) {
	cs := timeoutCauses(t, dead("rnic0", "rnic1"))
	want := []causeKey{
		{model.CauseRNICConnectFailure, "rnic0", "high"},
		{model.CauseRNICConnectFailure, "rnic1", "high"},
	}
	if got := keys(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A dead pair between two RNICs that reach everyone else is that pair's
// routing, reported on the pair as before.
func TestTimeoutDeadPairBetweenReachableRNICs(t *testing.T) {
	cs := timeoutCauses(t,
		dead("rnic0", "rnic1"), probed("rnic0", "rnic2", 64), probed("rnic0", "rnic3", 64),
		probed("rnic1", "rnic2", 64), probed("rnic1", "rnic3", 64), probed("rnic2", "rnic3", 64),
	)
	want := []causeKey{{model.CauseRNICConnectFailure, "rnic0,rnic1", "high"}}
	if got := keys(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A pair with no probe count has no ratio and must not read as healthy or as
// timed out.
func TestTimeoutUnknownPairIsSkipped(t *testing.T) {
	cs := timeoutCauses(t, model.MutualMeasurement{Src: "rnic0", Dst: "rnic1", Connected: true})
	if len(cs) != 0 {
		t.Fatalf("want no cause, got %+v", cs)
	}
}

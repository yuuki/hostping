package engine

import (
	"testing"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

type stubWindow struct {
	ms    []model.MutualMeasurement
	ready bool
}

func (w stubWindow) Snapshot() ([]model.MutualMeasurement, bool) { return w.ms, w.ready }

// A full window replaces the per-run burst: the report carries the window's
// pairs, the prober is not asked, and the counter drops still join them. The
// window's timeout ratio reaches the analyzer.
func TestEngineUsesFullMutualWindow(t *testing.T) {
	g := topology.Fig2()
	a, b := g.RNICs[0].ID, g.RNICs[1].ID
	burst := 0
	e := New(config.Defaults(), g, stubProber{mut: func(src, dst model.RNIC) (model.MutualMeasurement, error) {
		burst++
		return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: true}, nil
	}})
	e.Mutual = stubWindow{ready: true, ms: []model.MutualMeasurement{
		{Src: a, Dst: b, Connected: true, Sent: 200, Recv: 100, Reliable: true},
	}}
	rep := runWith(t, e, RunRequest{Mode: model.ModeIdle, DropDelta: map[string]model.RNICDrops{a: {Fault: map[string]uint64{"rx_discards_phy": 3}, Lossy: map[string]uint64{"rx_prio0_buf_discard": 50}}}})
	if burst != 0 {
		t.Fatalf("a full window must replace the burst, prober asked %d times", burst)
	}
	if len(rep.Mutual) != 1 || rep.Mutual[0].Sent != 200 || rep.Mutual[0].CounterDrops != 3 {
		t.Fatalf("report mutual: %+v", rep.Mutual)
	}
	found := false
	for _, c := range rep.Causes {
		if c.Code == model.CauseRNICIntermittent || c.Code == model.CauseRNICConnectFailure {
			found = true
		}
	}
	if !found {
		t.Fatalf("a 50%% windowed timeout ratio must be diagnosed: %+v", rep.Causes)
	}
}

// Until the window has filled, a run probes in a burst as before.
func TestEngineFallsBackToBurstBeforeWindowFills(t *testing.T) {
	g := topology.Fig2()
	burst := 0
	e := New(config.Defaults(), g, stubProber{mut: func(src, dst model.RNIC) (model.MutualMeasurement, error) {
		burst++
		return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: true, Sent: 64, Recv: 64}, nil
	}})
	e.Mutual = stubWindow{ready: false}
	rep := runWith(t, e, RunRequest{Mode: model.ModeIdle})
	if want := len(g.MutualPairs(config.Defaults().MutualScope)); burst != want || len(rep.Mutual) != want {
		t.Fatalf("want a burst over %d pairs, prober asked %d times, report %d", want, burst, len(rep.Mutual))
	}
}

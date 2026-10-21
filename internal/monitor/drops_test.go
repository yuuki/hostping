package monitor

import (
	"maps"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

func TestDropDeltasPerCounter(t *testing.T) {
	prev := map[string]uint64{"rx_discards_phy": 10, "out_of_buffer": 5, "gone": 1}
	cur := map[string]uint64{"rx_discards_phy": 14, "out_of_buffer": 5, "new": 1 << 40}
	// A counter that appears is baselined, not counted from 0; one that
	// vanishes is not a wrap.
	if got, want := dropDeltas(prev, cur), map[string]uint64{"rx_discards_phy": 4}; !maps.Equal(got, want) {
		t.Fatalf("dropDeltas = %v, want %v", got, want)
	}
	if got := dropDeltas(prev, prev); got != nil {
		t.Fatalf("no movement must be nil, got %v", got)
	}
}

func TestCounterPriority(t *testing.T) {
	for name, want := range map[string]int{
		"rx_prio0_buf_discard":  0,
		"rx_prio3_discards":     3,
		"tx_prio7_discards":     7,
		"rx_prio5_cong_discard": 5,
	} {
		if p, ok := counterPriority(name); !ok || p != want {
			t.Errorf("counterPriority(%q) = %d, %v; want %d", name, p, ok, want)
		}
	}
	for _, name := range []string{"rx_discards_phy", "out_of_buffer", "rx_prio_discards", "rx_prio3x_discards", "rx_noprio3_discards"} {
		if p, ok := counterPriority(name); ok {
			t.Errorf("counterPriority(%q) = %d, want none", name, p)
		}
	}
}

func TestSplitDrops(t *testing.T) {
	lossless3 := func(p int) bool { return p == 3 }
	all := func(int) bool { return true }
	for _, tc := range []struct {
		name         string
		deltas       map[string]uint64
		lossless     func(int) bool
		fault, lossy map[string]uint64
	}{
		{
			// The port aggregate is the sum of rx_prioN_discards: it is not
			// counted again.
			name:     "aggregate explained by lossless priority",
			deltas:   map[string]uint64{"rx_discards_phy": 120, "rx_prio3_discards": 120},
			lossless: lossless3,
			fault:    map[string]uint64{"rx_prio3_discards": 120},
		},
		{
			// Nor does it carry a lossy priority's discards back into the
			// fault class.
			name:     "aggregate explained by lossy priority",
			deltas:   map[string]uint64{"rx_discards_phy": 20, "rx_prio0_discards": 20, "rx_prio0_buf_discard": 900},
			lossless: lossless3,
			lossy:    map[string]uint64{"rx_prio0_discards": 20, "rx_prio0_buf_discard": 900},
		},
		{
			// What the per-priority counters leave unexplained stays a fault.
			name:     "aggregate residual",
			deltas:   map[string]uint64{"rx_discards_phy": 25, "rx_prio0_discards": 20},
			lossless: lossless3,
			fault:    map[string]uint64{"rx_discards_phy": 5},
			lossy:    map[string]uint64{"rx_prio0_discards": 20},
		},
		{
			name:     "aggregate without per-priority counters",
			deltas:   map[string]uint64{"rx_discards_phy": 8, "out_of_buffer": 1},
			lossless: lossless3,
			fault:    map[string]uint64{"rx_discards_phy": 8, "out_of_buffer": 1},
		},
		{
			// Without --lossless-priorities every priority is a fault, as
			// before.
			name:     "no lossless priorities configured",
			deltas:   map[string]uint64{"rx_prio0_buf_discard": 9, "rx_discards_phy": 3, "rx_prio3_discards": 3},
			lossless: all,
			fault:    map[string]uint64{"rx_prio0_buf_discard": 9, "rx_prio3_discards": 3},
		},
		{
			name:     "no drops",
			lossless: lossless3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitDrops(tc.deltas, tc.lossless)
			if !maps.Equal(got.Fault, tc.fault) || !maps.Equal(got.Lossy, tc.lossy) {
				t.Fatalf("splitDrops = fault %v lossy %v, want fault %v lossy %v", got.Fault, got.Lossy, tc.fault, tc.lossy)
			}
		})
	}
}

// Lossy-class drops and drops on an RNIC named by --drop-ignore-rnic start
// no busy run and are not passed on for diagnosis, but they are still load:
// the RNIC is busy, the host is not idle, and telemetry keeps the delta.
func TestNonFaultDropsDoNotTrigger(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       func(*config.Config)
		cur       map[string]uint64
		wantDelta bool
	}{
		{
			name:      "lossy class",
			cfg:       func(c *config.Config) { c.LosslessPriorities = []int{3} },
			cur:       map[string]uint64{"rx_prio0_buf_discard": 300, "rx_prio3_discards": 0},
			wantDelta: true,
		},
		{
			name: "ignored RNIC",
			cfg:  func(c *config.Config) { c.DropIgnoreRNICs = []string{"mlx5_0"} },
			cur:  map[string]uint64{"rx_prio0_buf_discard": 0, "rx_prio3_discards": 120},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := topology.Fig2()
			g.RNICs[0].Device = "mlx5_0"
			cfg := config.Defaults()
			cfg.IdleInterval = 0
			cfg.BusyCooldown = 0
			tc.cfg(&cfg)
			s := StaticSampler{
				Counters: map[string]model.RNICCounters{"rnic0": {Drops: map[string]uint64{"rx_prio0_buf_discard": 0, "rx_prio3_discards": 0}}},
				GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
			}
			m := New(cfg, g, s)
			m.Logf = func(string, ...any) {}
			poll(t, m)
			s.Counters["rnic0"] = model.RNICCounters{Drops: tc.cur}
			d := poll(t, m)
			if d.Run || d.Trigger == model.TriggerDrops {
				t.Fatalf("non-fault drops must not trigger: %+v", d)
			}
			if !d.BusyRNICs["rnic0"] || d.HostIdle {
				t.Fatalf("dropping RNIC must still count as load: %+v", d)
			}
			if d.Rates[0].DropDelta == 0 {
				t.Fatalf("telemetry must keep the drop delta: %+v", d.Rates[0])
			}
			got, ok := d.DropDelta["rnic0"]
			if ok != tc.wantDelta || got.FaultTotal() != 0 {
				t.Fatalf("DropDelta[rnic0] = %+v (present %v), want present %v with no fault drops", got, ok, tc.wantDelta)
			}
			if tc.wantDelta && got.LossyTotal() != 300 {
				t.Fatalf("lossy drops = %+v, want 300", got)
			}
		})
	}
}

// A lossless-priority discard still triggers with --lossless-priorities set.
func TestLosslessClassDropsTrigger(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	cfg.LosslessPriorities = []int{3}
	base := map[string]uint64{"rx_discards_phy": 0, "rx_prio3_discards": 0}
	s := StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {Drops: base}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m := New(cfg, topology.Fig2(), s)
	m.Logf = func(string, ...any) {}
	poll(t, m)
	s.Counters["rnic0"] = model.RNICCounters{Drops: map[string]uint64{"rx_discards_phy": 120, "rx_prio3_discards": 120}}
	d := poll(t, m)
	if !d.Run || d.Trigger != model.TriggerDrops {
		t.Fatalf("lossless-class drops must trigger: %+v", d)
	}
	if got := d.DropDelta["rnic0"]; got.FaultTotal() != 120 || got.LossyTotal() != 0 {
		t.Fatalf("DropDelta[rnic0] = %+v, want 120 fault drops counted once", got)
	}
}

package analyzer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

func runFault(t *testing.T, fault string, mode model.RunMode) model.Report {
	t.Helper()
	g := topology.Fig2()
	w := sim.New(g)
	if err := w.ApplyFault(fault); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	e := engine.New(cfg, g, sim.Prober{World: w})
	// One run of a one-shot engine, as hostpingctl diagnose does.
	e.OneShot = true
	req := engine.RunRequest{
		Trigger:   model.TriggerManual,
		Mode:      mode,
		BusyRNICs: w.Busy,
		Util:      w.LinkUtil,
		PCIe:      w.PCIe,
		Hints:     map[string]string{},
	}
	for id, ok := range w.SlowStart {
		if ok {
			req.Hints[id] = model.HintSlowStart
		}
	}
	for id, ok := range w.TxWindow {
		if ok {
			req.Hints[id] = model.HintTxWindow
		}
	}
	rep, err := e.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func hasCause(rep model.Report, code string) bool {
	for _, c := range rep.Causes {
		if c.Code == code {
			return true
		}
	}
	return false
}

func codes(rep model.Report) []string {
	var out []string
	for _, c := range rep.Causes {
		out = append(out, c.Code)
	}
	return out
}

func TestPaperIdleLinkFailures(t *testing.T) {
	cases := []struct {
		name   string
		fault  string
		want   string
		forbid []string
		only   string
	}{
		{"#1 RNIC PCIe", "rnic_pcie:rnic0", model.CauseRNICPCIeFailure, []string{model.CauseGPUPCIeFailure, model.CauseCPURootPortFailure}, ""},
		{"#2 GPU PCIe", "gpu_pcie:gpu0", model.CauseGPUPCIeFailure, []string{model.CauseCPURootPortFailure, model.CauseRNICPCIeFailure}, ""},
		{"#3 memory channel", "mem:mem0", model.CauseMemoryChannelFailure, []string{model.CauseCPURootPortFailure}, ""},
		{"#4 UPI", "upi", model.CauseUPIFailure, []string{model.CauseCPURootPortFailure}, ""},
		{"#5 CPU root port", "rp:rp.0", model.CauseCPURootPortFailure, []string{model.CauseGPUPCIeFailure, model.CauseMemoryChannelFailure}, "rp.0"},
		{"#7 ACS", "acs", model.CauseACSEnabled, []string{model.CauseGPUPCIeFailure}, ""},
		{"#8 ATS", "ats:rnic0", model.CauseATSDisabled, []string{model.CauseGPUPCIeFailure}, ""},
		{"#9 slow start", "slow_start:rnic0", model.CauseRNICSlowStart, []string{model.CauseGPUPCIeFailure, model.CauseRNICPCIeFailure}, ""},
		{"#10 Tx window", "tx_window:rnic0", model.CauseRNICTxWindow, []string{model.CauseGPUPCIeFailure, model.CauseRNICPCIeFailure}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := runFault(t, tc.fault, model.ModeIdle)
			if !hasCause(rep, tc.want) {
				t.Fatalf("fault %s: want cause %s, got %v", tc.fault, tc.want, codes(rep))
			}
			for _, bad := range tc.forbid {
				if hasCause(rep, bad) {
					t.Fatalf("fault %s: extra cause %s in %v", tc.fault, bad, codes(rep))
				}
			}
			if tc.only != "" {
				for _, c := range rep.Causes {
					if c.Code != tc.want {
						continue
					}
					for _, id := range c.Links {
						if id != tc.only {
							t.Fatalf("fault %s: %s on unexpected link %s (causes %v)", tc.fault, tc.want, id, rep.Causes)
						}
					}
				}
			}
		})
	}
}

func TestPaperMemoryFlapping(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	w := sim.New(g)
	e := engine.New(cfg, g, sim.Prober{World: w})
	rep, err := e.Run(context.Background(), engine.RunRequest{Mode: model.ModeIdle, Trigger: model.TriggerIdle})
	if err != nil {
		t.Fatal(err)
	}
	ms := append([]model.PathMeasurement(nil), rep.Paths...)
	for i := range ms {
		if ms[i].RNIC == "rnic0" && ms[i].Endpoint == "mem0" {
			ms[i].BandwidthBPS = ms[i].BandwidthBPS / 3
		}
	}
	var gray map[string]int
	var res analyzer.Result
	for i := 0; i < cfg.FlapWindow; i++ {
		res = analyzer.Analyze(analyzer.Input{
			Graph: g, Cfg: cfg, Mode: model.ModeIdle, Measurements: ms, GrayHits: gray,
		})
		gray = res.GrayHits
	}
	if !hasCause(model.Report{Causes: res.Causes}, model.CauseMemoryChannelFlapping) {
		t.Fatalf("expected flapping after %d gray tests, gray=%v causes=%v", cfg.FlapWindow, gray, res.Causes)
	}
}

// TestPaperFlappingNeedsConsecutiveGray pins NSDI'23 §4.3: "links marked as gray
// in three consecutive loopback tests will be identified as flapping links".
// The streak on mem.0 must break on any run that tested it without graying it,
// survive a run that never tested it, and only report while it is gray.
func TestPaperFlappingNeedsConsecutiveGray(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	w := sim.New(g)
	e := engine.New(cfg, g, sim.Prober{World: w})
	rep, err := e.Run(context.Background(), engine.RunRequest{Mode: model.ModeIdle, Trigger: model.TriggerIdle})
	if err != nil {
		t.Fatal(err)
	}
	healthy := append([]model.PathMeasurement(nil), rep.Paths...)
	degrade := func(ms []model.PathMeasurement) []model.PathMeasurement {
		out := append([]model.PathMeasurement(nil), ms...)
		for i := range out {
			if out[i].RNIC == "rnic0" && out[i].Endpoint == "mem0" {
				out[i].BandwidthBPS /= 3
			}
		}
		return out
	}
	// Every other hop of rnic0->mem0 is proven normal by another path, so the
	// degraded path grays mem.0 (Algorithm 1 line 15).
	grayRun := degrade(healthy)
	// rnic0->mem0 alone, degraded: the abnormal path judges four uncertain hops
	// and isolates none, so mem.0 is tested but stays uncertain.
	var uncertainRun []model.PathMeasurement
	for _, m := range grayRun {
		if m.RNIC == "rnic0" && m.Endpoint == "mem0" {
			uncertainRun = append(uncertainRun, m)
		}
	}
	// No measured path crosses mem.0.
	var untestedRun []model.PathMeasurement
	for _, m := range healthy {
		p, _ := g.Path(m.RNIC, m.Endpoint)
		crosses := false
		for _, id := range p.LinkIDs {
			if id == "mem.0" {
				crosses = true
			}
		}
		if !crosses {
			untestedRun = append(untestedRun, m)
		}
	}

	type step struct {
		ms       []model.PathMeasurement
		status   model.LinkStatus
		hits     int
		flapping bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"three consecutive grays flap", []step{
			{grayRun, model.LinkGray, 1, false},
			{grayRun, model.LinkGray, 2, false},
			{grayRun, model.LinkGray, 3, true},
			{grayRun, model.LinkGray, 4, true},
		}},
		{"tested uncertain run breaks the streak", []step{
			{grayRun, model.LinkGray, 1, false},
			{uncertainRun, model.LinkUncertain, 0, false},
			{grayRun, model.LinkGray, 1, false},
			{grayRun, model.LinkGray, 2, false},
		}},
		{"untested run keeps the streak but does not report", []step{
			{grayRun, model.LinkGray, 1, false},
			{grayRun, model.LinkGray, 2, false},
			{grayRun, model.LinkGray, 3, true},
			{untestedRun, model.LinkUncertain, 3, false},
			{nil, model.LinkUncertain, 3, false},
			{grayRun, model.LinkGray, 4, true},
		}},
		{"normal run resets", []step{
			{grayRun, model.LinkGray, 1, false},
			{grayRun, model.LinkGray, 2, false},
			{grayRun, model.LinkGray, 3, true},
			{healthy, model.LinkNormal, 0, false},
			{grayRun, model.LinkGray, 1, false},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gray map[string]int
			for i, s := range tc.steps {
				res := analyzer.Analyze(analyzer.Input{
					Graph: g, Cfg: cfg, Mode: model.ModeIdle, Measurements: s.ms, GrayHits: gray,
				})
				gray = res.GrayHits
				for _, l := range res.Links {
					if l.ID == "mem.0" && l.Status != s.status {
						t.Fatalf("run %d: mem.0 status = %v, want %v", i, l.Status, s.status)
					}
				}
				if gray["mem.0"] != s.hits {
					t.Fatalf("run %d: GrayHits[mem.0] = %d, want %d", i, gray["mem.0"], s.hits)
				}
				got := hasCause(model.Report{Causes: res.Causes}, model.CauseMemoryChannelFlapping)
				if got != s.flapping {
					t.Fatalf("run %d: flapping = %v, want %v (causes=%v)", i, got, s.flapping, res.Causes)
				}
			}
		})
	}

	// The issue's case: a carried-over streak with nothing measured this run.
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg, Mode: model.ModeIdle, GrayHits: map[string]int{"mem.0": 3},
	})
	if hasCause(model.Report{Causes: res.Causes}, model.CauseMemoryChannelFlapping) {
		t.Fatalf("flapping reported with no measurements: %v", res.Causes)
	}
	if res.GrayHits["mem.0"] != 3 {
		t.Fatalf("GrayHits[mem.0] = %d, want 3 kept", res.GrayHits["mem.0"])
	}
}

// A persistent fault must not read as flapping elsewhere. When an idle run
// kept remote RNICs from judging a faulty GPU link, their cross-root and
// cross-socket paths to that GPU were left with only normal hops and grayed
// them — upi.0 among them — so a steady #2 turned into an inter-socket bus
// flapping report after FlapWindow runs.
func TestPersistentGPUFaultIsNotFlapping(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	w := sim.New(g)
	if err := w.ApplyFault("gpu_pcie:gpu0"); err != nil {
		t.Fatal(err)
	}
	e := engine.New(cfg, g, sim.Prober{World: w})
	rep, err := e.Run(context.Background(), engine.RunRequest{Mode: model.ModeIdle, Trigger: model.TriggerIdle, PCIe: w.PCIe})
	if err != nil {
		t.Fatal(err)
	}
	var gray map[string]int
	var res analyzer.Result
	for i := 0; i < cfg.FlapWindow; i++ {
		res = analyzer.Analyze(analyzer.Input{
			Graph: g, Cfg: cfg, Mode: model.ModeIdle, Measurements: rep.Paths, PCIe: w.PCIe, GrayHits: gray,
		})
		gray = res.GrayHits
	}
	for _, c := range res.Causes {
		if c.Code != model.CauseGPUPCIeFailure {
			t.Errorf("persistent gpu0 fault: unexpected cause %s on %v", c.Code, c.Links)
		}
	}
	if len(gray) != 0 {
		t.Errorf("persistent gpu0 fault grayed links %v", gray)
	}
}

func TestPaperBusyContention(t *testing.T) {
	t.Run("#11 overloaded UPI", func(t *testing.T) {
		rep := runFault(t, "overload_upi;busy:rnic0,rnic2", model.ModeBusy)
		if !hasCause(rep, model.CauseOverloadedUPI) {
			t.Fatalf("got %v", codes(rep))
		}
	})
	t.Run("#12 overloaded memory", func(t *testing.T) {
		rep := runFault(t, "overload_mem:mem0;busy:rnic0,rnic2", model.ModeBusy)
		if !hasCause(rep, model.CauseOverloadedMemory) {
			t.Fatalf("got %v", codes(rep))
		}
	})
	t.Run("busy GPU PCIe still visible from loaded RNIC", func(t *testing.T) {
		rep := runFault(t, "gpu_pcie:gpu0;busy:rnic0", model.ModeBusy)
		if !hasCause(rep, model.CauseGPUPCIeFailure) {
			t.Fatalf("got %v", codes(rep))
		}
		if hasCause(rep, model.CauseCPURootPortFailure) {
			t.Fatalf("busy #2 must not invent #5: %v", codes(rep))
		}
	})
	t.Run("#1 equally throttled busy RNIC", func(t *testing.T) {
		rep := runFault(t, "rnic_pcie:rnic0;busy:rnic0", model.ModeBusy)
		if !hasCause(rep, model.CauseRNICPCIeFailure) {
			t.Fatalf("busy equal-throttle must diagnose RNIC PCIe, got %v", codes(rep))
		}
		if hasCause(rep, model.CauseRNICSlowStart) || hasCause(rep, model.CauseGPUPCIeFailure) {
			t.Fatalf("must not invert #1 into slow-start or #2: %v", codes(rep))
		}
	})
}

func TestTONMutualProbing(t *testing.T) {
	t.Run("connectivity failure", func(t *testing.T) {
		rep := runFault(t, "connect_fail:rnic0", model.ModeIdle)
		if !hasCause(rep, model.CauseRNICConnectFailure) {
			t.Fatalf("got %v", codes(rep))
		}
	})
	t.Run("packet drops", func(t *testing.T) {
		rep := runFault(t, "drops:rnic1", model.ModeIdle)
		if !hasCause(rep, model.CauseRNICPacketDrops) {
			t.Fatalf("got %v", codes(rep))
		}
		// 5% loss stays under Timeout_th.
		if hasCause(rep, model.CauseRNICIntermittent) {
			t.Fatalf("5%% loss must not be an intermittent timeout: %v", codes(rep))
		}
	})
	// ToN'24 §IV-C-2: a down RNIC, a flapping one and a ToR switch anomaly are
	// told apart by the timeout ratio over each RNIC's pairs.
	for _, tc := range []struct {
		fault, code, rnics, conf string
	}{
		{"connect_fail:rnic0", model.CauseRNICConnectFailure, "rnic0", "high"},
		{"flap:rnic1", model.CauseRNICIntermittent, "rnic1", "high"},
		{"tor", model.CauseToRSwitchAnomaly, "rnic0,rnic1,rnic2,rnic3", "medium"},
	} {
		t.Run("timeout "+tc.fault, func(t *testing.T) {
			rep := runFault(t, tc.fault, model.ModeIdle)
			var got []string
			for _, c := range rep.Causes {
				switch c.Code {
				case model.CauseRNICConnectFailure, model.CauseRNICIntermittent, model.CauseToRSwitchAnomaly:
					got = append(got, c.Code+"["+strings.Join(c.RNICs, ",")+"]/"+string(c.Confidence))
				}
			}
			want := tc.code + "[" + tc.rnics + "]/" + tc.conf
			if len(got) != 1 || got[0] != want {
				t.Fatalf("got %v, want [%s]", got, want)
			}
		})
	}
}

func TestInvisiblePCIeIsNotSlowStart(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	w.LinkBW["pcie.rnic0"] = 67_000_000_000
	cfg := config.Defaults()
	e := engine.New(cfg, g, sim.Prober{World: w})
	rep, err := e.Run(context.Background(), engine.RunRequest{
		Trigger: model.TriggerManual, Mode: model.ModeIdle, PCIe: w.PCIe,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasCause(rep, model.CauseRNICSlowStart) || hasCause(rep, model.CauseRNICTxWindow) || hasCause(rep, model.CauseGPUPCIeFailure) {
		t.Fatalf("invisible-to-lspci PCIe must stay #1, got %v", codes(rep))
	}
	if !hasCause(rep, model.CauseRNICPCIeFailure) {
		t.Fatalf("want rnic_pcie_failure, got %v", codes(rep))
	}
}

func TestReliableMutualDoesNotUseRCLoss(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg,
		Mutual: []model.MutualMeasurement{{
			Src: "rnic0", Dst: "rnic1", Connected: true, LossRatio: 0.5, Reliable: true,
		}},
	})
	if hasCause(model.Report{Causes: res.Causes}, model.CauseRNICPacketDrops) {
		t.Fatalf("RC loss_ratio must not diagnose drops: %v", res.Causes)
	}
	res = analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg,
		Mutual: []model.MutualMeasurement{{
			Src: "rnic0", Dst: "rnic1", Connected: true, Reliable: true,
		}},
		DropDelta: map[string]model.RNICDrops{"rnic0": {Fault: map[string]uint64{"rx_discards_phy": 12}}},
	})
	if !hasCause(model.Report{Causes: res.Causes}, model.CauseRNICPacketDrops) {
		t.Fatalf("hardware discard delta should diagnose drops: %v", res.Causes)
	}
}

func TestHealthyIdleHasNoCause(t *testing.T) {
	rep := runFault(t, "", model.ModeIdle)
	if len(rep.Causes) != 0 {
		t.Fatalf("healthy host diagnosed %v", codes(rep))
	}
	abn := 0
	crossUncertain := false
	for _, p := range rep.Paths {
		if p.Status == model.PathAbnormal {
			abn++
		}
		if p.RNIC == "rnic0" && p.Endpoint == "gpu4" && p.Status == model.PathUncertain {
			crossUncertain = true
		}
	}
	if abn != 0 {
		t.Fatalf("healthy host has %d abnormal paths", abn)
	}
	if !crossUncertain {
		t.Fatal("cross-socket GPU that meets the reduced baseline must stay uncertain, not normal")
	}
}

func TestHealthyBusyHasNoCause(t *testing.T) {
	rep := runFault(t, "busy:rnic0", model.ModeBusy)
	if len(rep.Causes) != 0 {
		t.Fatalf("healthy busy host must not invent a cause: %v", codes(rep))
	}
}

// TestOverloadNeedsAbnormalLink pins NSDI'23 §4.3 / ToN'24 §IV-C: bus
// utilization explains an abnormal memory channel or inter-socket bus; it is
// not a fault on its own. A busy but healthy link must yield no cause.
func TestOverloadNeedsAbnormalLink(t *testing.T) {
	run := func(t *testing.T, fault string, util map[string]float64) model.Report {
		t.Helper()
		g := topology.Fig2()
		w := sim.New(g)
		if err := w.ApplyFault(fault); err != nil {
			t.Fatal(err)
		}
		for id, u := range util {
			w.LinkUtil[id] = u
		}
		e := engine.New(config.Defaults(), g, sim.Prober{World: w})
		rep, err := e.Run(context.Background(), engine.RunRequest{
			Trigger: model.TriggerManual, Mode: model.ModeIdle, Util: w.LinkUtil, PCIe: w.PCIe,
		})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	onLink := func(rep model.Report, code, link string) bool {
		for _, c := range rep.Causes {
			if c.Code != code {
				continue
			}
			for _, id := range c.Links {
				if id == link {
					return true
				}
			}
		}
		return false
	}

	t.Run("healthy busy links", func(t *testing.T) {
		rep := run(t, "", map[string]float64{"mem.0": 0.95, "upi.0": 0.95})
		if len(rep.Causes) != 0 {
			t.Fatalf("high util on healthy links must not be a cause, got %v", codes(rep))
		}
	})
	cases := []struct {
		name, fault, link string
		util              float64
		want, forbid      string
	}{
		{"abnormal busy memory", "mem:mem0", "mem.0", 0.95, model.CauseOverloadedMemory, model.CauseMemoryChannelFailure},
		{"abnormal quiet memory", "mem:mem0", "mem.0", 0.10, model.CauseMemoryChannelFailure, model.CauseOverloadedMemory},
		{"abnormal busy UPI", "upi", "upi.0", 0.95, model.CauseOverloadedUPI, model.CauseUPIFailure},
		{"abnormal quiet UPI", "upi", "upi.0", 0.10, model.CauseUPIFailure, model.CauseOverloadedUPI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := run(t, tc.fault, map[string]float64{tc.link: tc.util})
			if !onLink(rep, tc.want, tc.link) {
				t.Fatalf("want %s on %s, got %v", tc.want, tc.link, rep.Causes)
			}
			if onLink(rep, tc.forbid, tc.link) {
				t.Fatalf("must not report %s on %s: %v", tc.forbid, tc.link, rep.Causes)
			}
		})
	}

	// A live host has no memory-channel source: the utilization is unknown,
	// which is neither overloaded nor a measured 0, and says so.
	t.Run("abnormal memory with unknown util", func(t *testing.T) {
		rep := run(t, "mem:mem0", nil)
		if onLink(rep, model.CauseOverloadedMemory, "mem.0") || !onLink(rep, model.CauseMemoryChannelFailure, "mem.0") {
			t.Fatalf("unknown util must leave a failure, got %v", rep.Causes)
		}
		for _, c := range rep.Causes {
			if c.Code == model.CauseMemoryChannelFailure && !strings.Contains(c.Evidence, "util=unknown") {
				t.Fatalf("evidence %q must say the util is unknown", c.Evidence)
			}
		}
		for _, l := range rep.Links {
			if l.ID == "mem.0" && l.Utilization != nil {
				t.Fatalf("unread util reported as %v", *l.Utilization)
			}
		}
	})
}

func TestSlightRootPortFig8e(t *testing.T) {
	g := topology.Fig2()
	w := sim.New(g)
	w.LinkBW["rp.0"] = 110_000_000_000
	cfg := config.Defaults()
	e := engine.New(cfg, g, sim.Prober{World: w})
	rep, err := e.Run(context.Background(), engine.RunRequest{
		Trigger: model.TriggerManual, Mode: model.ModeIdle, PCIe: w.PCIe,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasCause(rep, model.CauseCPURootPortFailure) {
		t.Fatalf("Fig.8(e) slight RP degrade: want #5, got %v", codes(rep))
	}
	for _, c := range rep.Causes {
		if c.Code == model.CauseCPURootPortFailure {
			for _, id := range c.Links {
				if id != "rp.0" {
					t.Fatalf("Fig.8(e) must isolate rp.0, also named %s: %v", id, codes(rep))
				}
			}
		}
		if c.Code == model.CauseGPUPCIeFailure || c.Code == model.CauseMemoryChannelFailure {
			t.Fatalf("Fig.8(e) extra cause %s: %v", c.Code, codes(rep))
		}
	}
	for _, p := range rep.Paths {
		if p.RNIC == "rnic1" && p.Endpoint == "gpu0" && p.Status == model.PathNormal {
			t.Fatal("other-RP RNIC must not mark a slightly degraded cross-root path Normal")
		}
	}
}

func TestAlgorithm1RNICLinkIsUnique(t *testing.T) {
	rep := runFault(t, "rnic_pcie:rnic0", model.ModeIdle)
	if !hasCause(rep, model.CauseRNICPCIeFailure) {
		t.Fatalf("want rnic_pcie_failure, got %v", codes(rep))
	}
	if hasCause(rep, model.CauseGPUPCIeFailure) {
		t.Fatalf("RNIC-wide cap must not unique-hop GPU links: %v", codes(rep))
	}
	for _, l := range rep.Links {
		if (l.ID == "pcie.gpu0" || l.ID == "pcie.gpu1") && l.Status == model.LinkAbnormal {
			t.Fatalf("%s must stay unisolated on an RNIC-wide cap, status %s", l.ID, l.Status)
		}
	}
}

func TestSkippedGPUPathsStillDiagnoseRNICPCIe(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	// Live hosts without GDR measure memory only. Missing GPU rows must not
	// look like "not all affinitive paths abnormal."
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg, Mode: model.ModeIdle,
		Measurements: []model.PathMeasurement{{
			RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 1000,
		}},
		PCIe: map[string]model.PCIeConfig{"rnic0": {Speed: "8GT/s", Width: 8}},
	})
	if !hasCause(model.Report{Causes: res.Causes}, model.CauseRNICPCIeFailure) {
		t.Fatalf("memory-only abnormal affinitive path should diagnose #1, got %v", res.Causes)
	}
}

func TestDownTrainedPCIeWidthIsDegraded(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	// Gen4 speed but only four lanes of a x16 device: ~63 Gb/s, far under the
	// 200 Gb/s line rate. The speed string alone looks healthy; the width
	// below the device maximum is the evidence.
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg, Mode: model.ModeIdle,
		Measurements: []model.PathMeasurement{{
			RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 1000,
		}},
		PCIe: map[string]model.PCIeConfig{"rnic0": {
			Speed: "16.0 GT/s PCIe", Width: 4, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 16,
		}},
		RNICHints: map[string]string{"rnic0": "slow_start"},
	})
	rep := model.Report{Causes: res.Causes}
	if !hasCause(rep, model.CauseRNICPCIeFailure) {
		t.Fatalf("x4 link at 16 GT/s must diagnose #1, got %v", codes(rep))
	}
	if hasCause(rep, model.CauseRNICSlowStart) {
		t.Fatalf("down-trained width must outrank the slow-start hint: %v", codes(rep))
	}
}

func TestPCIeBelowDeviceMaxWithEnoughBandwidthIsNotPCIe(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	// Gen5 x8 is half of what the device advertises, but sysfs max_link_* is
	// the card's capability, not the slot's: 252 Gb/s still carries the 200
	// Gb/s line rate, so the device-config hint keeps the cause.
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg, Mode: model.ModeIdle,
		Measurements: []model.PathMeasurement{{
			RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 1000,
		}},
		PCIe: map[string]model.PCIeConfig{"rnic0": {
			Speed: "32.0 GT/s PCIe", Width: 8, MaxSpeed: "32.0 GT/s PCIe", MaxWidth: 16,
		}},
		RNICHints: map[string]string{"rnic0": "slow_start"},
	})
	rep := model.Report{Causes: res.Causes}
	if hasCause(rep, model.CauseRNICPCIeFailure) {
		t.Fatalf("a link below device max that still carries the line rate is not #1: %v", codes(rep))
	}
	if !hasCause(rep, model.CauseRNICSlowStart) {
		t.Fatalf("want the slow-start hint to stand, got %v", codes(rep))
	}
}

func causeFor(rep model.Report, code, rnic string) (model.RootCause, bool) {
	for _, c := range rep.Causes {
		if c.Code != code {
			continue
		}
		for _, id := range c.RNICs {
			if id == rnic {
				return c, true
			}
		}
	}
	return model.RootCause{}, false
}

// TestLiveRNICWideCapWithoutPCIeEvidenceIsMedium pins NSDI'23 #9/#10: a
// slow-start or Tx-window RNIC caps every affinitive path equally and was
// first misdiagnosed as an RNIC PCIe failure. A live host has no device-config
// hint, so hostping must not report that verdict as certain when the
// negotiated link is healthy or unread.
func TestLiveRNICWideCapWithoutPCIeEvidenceIsMedium(t *testing.T) {
	for _, fault := range []string{"slow_start:rnic0", "tx_window:rnic0"} {
		for _, pcie := range []struct {
			name string
			cfg  func(w *sim.World) map[string]model.PCIeConfig
			ev   string
		}{
			{"healthy link", func(w *sim.World) map[string]model.PCIeConfig { return w.PCIe }, "PCIe config space shows no degradation"},
			{"unknown link", func(*sim.World) map[string]model.PCIeConfig { return nil }, "negotiated PCIe link unknown"},
		} {
			t.Run(fault+"/"+pcie.name, func(t *testing.T) {
				g := topology.Fig2()
				w := sim.New(g)
				if err := w.ApplyFault(fault); err != nil {
					t.Fatal(err)
				}
				e := engine.New(config.Defaults(), g, sim.Prober{World: w})
				// No Hints: what a live run passes, since hintsFrom has no
				// sim world to read.
				rep, err := e.Run(context.Background(), engine.RunRequest{
					Trigger: model.TriggerManual, Mode: model.ModeIdle, PCIe: pcie.cfg(w),
				})
				if err != nil {
					t.Fatal(err)
				}
				c, ok := causeFor(rep, model.CauseRNICPCIeFailure, "rnic0")
				if !ok {
					t.Fatalf("want rnic_pcie_failure on rnic0, got %v", codes(rep))
				}
				if c.Confidence != "medium" {
					t.Fatalf("no PCIe evidence must stay medium, got %q (%s)", c.Confidence, c.Evidence)
				}
				for _, want := range []string{pcie.ev, "slow start / Tx window", "not ruled out"} {
					if !strings.Contains(c.Evidence, want) {
						t.Fatalf("evidence %q missing %q", c.Evidence, want)
					}
				}
			})
		}
	}
}

func TestDownTrainedRNICWideCapStaysHigh(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: cfg, Mode: model.ModeIdle,
		Measurements: []model.PathMeasurement{{
			RNIC: "rnic0", Endpoint: "mem0", BandwidthBPS: 40_000_000_000, SmallLatNsP50: 1000,
		}},
		PCIe: map[string]model.PCIeConfig{"rnic0": {
			Speed: "8GT/s", Width: 8, MaxSpeed: "16.0 GT/s PCIe", MaxWidth: 16,
		}},
	})
	c, ok := causeFor(model.Report{Causes: res.Causes}, model.CauseRNICPCIeFailure, "rnic0")
	if !ok {
		t.Fatalf("want rnic_pcie_failure on rnic0, got %v", res.Causes)
	}
	if c.Confidence != "high" {
		t.Fatalf("a link down-trained below line rate is PCIe evidence, want high, got %q", c.Confidence)
	}
}

func TestFig2PathCounts(t *testing.T) {
	g := topology.Fig2()
	if len(g.RNICs) != 4 || len(g.Endpoints) != 10 {
		t.Fatalf("rnics=%d eps=%d", len(g.RNICs), len(g.Endpoints))
	}
	if len(g.Paths) != 40 {
		t.Fatalf("paths=%d want 40", len(g.Paths))
	}
	p, ok := g.Path("rnic0", "gpu0")
	if !ok || p.Distance != model.DistanceSameSwitch || !p.Affinitive {
		t.Fatalf("rnic0-gpu0 %+v", p)
	}
	p, _ = g.Path("rnic0", "gpu2")
	if p.Distance != model.DistanceCrossRoot {
		t.Fatalf("cross-root distance %d", p.Distance)
	}
	p, _ = g.Path("rnic0", "gpu4")
	if p.Distance != model.DistanceCrossSocket || p.ContainsLink("upi.0") == false {
		t.Fatalf("cross-socket %+v", p)
	}
}

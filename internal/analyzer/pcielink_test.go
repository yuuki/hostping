package analyzer_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// linkFinding is a passive finding on link: 412 link errors a minute at the
// device end of a Gen4 x16 link, in the state given.
func linkFinding(link string, state model.PCIeLinkState) model.PCIeLinkFinding {
	f := model.PCIeLinkFinding{
		Link: link, State: state,
		ErrorsPerMinute: 412, BudgetPerMinute: 15.36, WindowSeconds: 60,
		RecentWindows: 3, OverBudgetWindows: 3, OverBudgetRuns: 1, OverBudgetLongestRun: 3, RecentErrorsPerMinute: 412,
		Ends: []model.PCIeLinkEnd{
			{BDF: "0000:16:00.0", Port: true},
			{BDF: "0000:17:00.0", Correctable: map[string]uint64{"BadTLP": 409, "Timeout": 3}},
		},
	}
	if state == model.PCIeLinkUncorrectable {
		f.ErrorsPerMinute, f.OverBudgetWindows, f.OverBudgetRuns, f.OverBudgetLongestRun, f.RecentErrorsPerMinute = 0, 0, 0, 0, 0
		f.Ends = []model.PCIeLinkEnd{{BDF: "0000:16:00.0", Port: true, Uncorrectable: map[string]uint64{"SDES": 1}}}
	}
	return f
}

// healthyRun is an idle full mesh of the Gen5 host with nothing wrong, plus
// the passive findings given.
func healthyRun(g topology.Graph, findings ...model.PCIeLinkFinding) analyzer.Input {
	return analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle,
		Measurements: gen5Measurements(g, 270*gbps, 273*gbps),
		PCIe:         healthyGen5PCIe(g),
		PCIeLinks:    findings,
	}
}

// A link that counts link errors beyond its budget, on a host whose probes
// all read healthy, is reported for what it is: the counters, at medium,
// naming the link and its device. No path, link status or other cause moves.
func TestLinkErrorsAloneAreMedium(t *testing.T) {
	g := fig2Gen5()
	plain := analyzer.Analyze(healthyRun(g))
	res := analyzer.Analyze(healthyRun(g,
		linkFinding("pcie.rnic1", model.PCIeLinkSustained),
		linkFinding("pcie.gpu5", model.PCIeLinkUncorrectable),
		linkFinding("rp.2", model.PCIeLinkSustained)))

	if len(res.Causes) != 3 {
		t.Fatalf("causes %v, want the three link findings and nothing else", codes(model.Report{Causes: res.Causes}))
	}
	// In report order: by the cause's identity, its RNICs first.
	want := []struct {
		link, summary, evidence string
		rnics, endpoints        []string
	}{
		{"pcie.gpu5", "PCIe link reported an uncorrectable link error",
			"AER: 0 link errors/min against a budget of 16/min in the last window; 0000:16:00.0 (port): uncorrectable SDES=1; last window 60s; the same run names no other cause at the link (counters only)",
			nil, []string{"gpu5"}},
		{"rp.2", "PCIe link counts link errors beyond its bit-error budget", "", nil, nil},
		{"pcie.rnic1", "PCIe link counts link errors beyond its bit-error budget",
			"AER: 412 link errors/min against a budget of 16/min in the last window; over the budget in 3 of the last 3 windows (1 time, at most 3 in a row), 412/min across them; 0000:17:00.0: BadTLP=409 Timeout=3; last window 60s; the same run names no other cause at the link (counters only)",
			[]string{"rnic1"}, nil},
	}
	for i, w := range want {
		c := res.Causes[i]
		if c.Code != model.CausePCIeLinkErrors || c.Confidence != model.ConfidenceMedium {
			t.Fatalf("cause %d %+v, want pcie_link_errors at medium", i, c)
		}
		if !slices.Equal(c.Links, []string{w.link}) || !slices.Equal(c.RNICs, w.rnics) || !slices.Equal(c.Endpoints, w.endpoints) {
			t.Fatalf("cause %d names links=%v rnics=%v endpoints=%v, want %s %v %v", i, c.Links, c.RNICs, c.Endpoints, w.link, w.rnics, w.endpoints)
		}
		if c.Summary != w.summary {
			t.Fatalf("cause %d summary %q, want %q", i, c.Summary, w.summary)
		}
		if w.evidence != "" && c.Evidence != w.evidence {
			t.Fatalf("cause %d evidence\n  %q\nwant\n  %q", i, c.Evidence, w.evidence)
		}
	}
	// Advisory to the tomography: every path and link reads as without them.
	for i := range res.Paths {
		if res.Paths[i].Status != plain.Paths[i].Status {
			t.Fatalf("path %s->%s status %s, %s without the findings", res.Paths[i].RNIC, res.Paths[i].Endpoint, res.Paths[i].Status, plain.Paths[i].Status)
		}
	}
	for i := range res.Links {
		if res.Links[i].Status != plain.Links[i].Status || res.Links[i].AbnormalCount != plain.Links[i].AbnormalCount {
			t.Fatalf("link %s %+v, %+v without the findings", res.Links[i].ID, res.Links[i], plain.Links[i])
		}
	}
}

// A suspect is a hint: it names no cause, whatever the probes say.
func TestSuspectLinkNamesNoCause(t *testing.T) {
	g := fig2Gen5()
	in := healthyRun(g, linkFinding("pcie.gpu0", model.PCIeLinkSuspect), linkFinding("pcie.nosuch", model.PCIeLinkSustained))
	if res := analyzer.Analyze(in); len(res.Causes) != 0 {
		t.Fatalf("causes %+v from a suspect and a link the graph does not have", res.Causes)
	}
	for i := range in.Measurements {
		if in.Measurements[i].RNIC == "rnic0" && in.Measurements[i].Endpoint == "gpu0" {
			in.Measurements[i].BandwidthBPS = 60 * gbps
		}
	}
	res := analyzer.Analyze(in)
	if got := causesOf(res, model.CausePCIeLinkErrors); len(got) != 0 {
		t.Fatalf("a suspect became %+v once a path through it read abnormal", got)
	}
	if got := causesOf(res, model.CauseGPUPCIeFailure); len(got) != 1 {
		t.Fatalf("the abnormal GPU path lost its own cause: %+v", res.Causes)
	}
}

// The counters stay medium whatever the probes measure: they show a link that
// errs, and the bottleneck the probes find at it is another cause of the same
// report, which the evidence points to. They neither replace nor raise it.
func TestLinkErrorsStayMediumNextToAMeasuredBottleneck(t *testing.T) {
	g := fig2Gen5()
	in := healthyRun(g,
		linkFinding("pcie.rnic0", model.PCIeLinkSustained),
		linkFinding("pcie.gpu0", model.PCIeLinkSustained),
		linkFinding("pcie.gpu4", model.PCIeLinkSustained))
	for i := range in.Measurements {
		if in.Measurements[i].RNIC == "rnic0" && in.Measurements[i].Endpoint == "gpu0" {
			in.Measurements[i].BandwidthBPS = 60 * gbps
		}
	}
	without := in
	without.PCIeLinks = nil
	res := analyzer.Analyze(in)

	byLink := map[string]model.RootCause{}
	for _, c := range causesOf(res, model.CausePCIeLinkErrors) {
		if c.Confidence != model.ConfidenceMedium {
			t.Fatalf("%s is %s, want medium", c.Links[0], c.Confidence)
		}
		byLink[c.Links[0]] = c
	}
	if len(byLink) != 3 {
		t.Fatalf("pcie_link_errors causes %+v, want one per finding", byLink)
	}
	// The probes isolate gpu0's link; rnic0's and gpu4's carry no cause.
	if ev := byLink["pcie.gpu0"].Evidence; !strings.HasSuffix(ev, "; the same run also names the link (gpu_pcie_failure)") {
		t.Fatalf("pcie.gpu0 evidence %q, want it to point at the bandwidth cause", ev)
	}
	for _, link := range []string{"pcie.rnic0", "pcie.gpu4"} {
		if ev := byLink[link].Evidence; !strings.HasSuffix(ev, "; the same run names no other cause at the link (counters only)") {
			t.Fatalf("%s evidence %q", link, ev)
		}
	}
	// Every other cause is exactly what the run reports without the counters.
	var other []model.RootCause
	for _, c := range res.Causes {
		if c.Code != model.CausePCIeLinkErrors {
			other = append(other, c)
		}
	}
	plain := analyzer.Analyze(without).Causes
	if len(other) != len(plain) || len(plain) != 1 {
		t.Fatalf("other causes %+v, without the counters %+v", other, plain)
	}
	if o, p := other[0], plain[0]; o.Code != p.Code || o.Confidence != p.Confidence || o.Evidence != p.Evidence || !slices.Equal(o.Links, p.Links) {
		t.Fatalf("gpu_pcie_failure %+v changed next to the counters, was %+v", o, p)
	}
}

// The link below a root port: Algorithm 1 isolates it in the sim's root-port
// fault, and the counters on it point at that cause.
func TestLinkErrorsNextToAFailedRootPort(t *testing.T) {
	g := topology.Fig2()
	rep := runWorld(t, "rp:rp.1", model.ModeIdle, nil)
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: rep.Paths,
		PCIeLinks: []model.PCIeLinkFinding{linkFinding("rp.1", model.PCIeLinkSustained), linkFinding("rp.3", model.PCIeLinkSustained)},
	})
	if got := causesOf(res, model.CauseCPURootPortFailure); len(got) != 1 || got[0].Links[0] != "rp.1" || got[0].Confidence != model.ConfidenceHigh {
		t.Fatalf("cpu_root_port_failure %+v, want rp.1 at high from the probes", got)
	}
	got := causesOf(res, model.CausePCIeLinkErrors)
	if len(got) != 2 {
		t.Fatalf("%d pcie_link_errors causes, want 2: %+v", len(got), res.Causes)
	}
	for _, c := range got {
		want := "; the same run names no other cause at the link (counters only)"
		if c.Links[0] == "rp.1" {
			want = "; the same run also names the link (cpu_root_port_failure)"
		}
		if c.Confidence != model.ConfidenceMedium || !strings.HasSuffix(c.Evidence, want) {
			t.Fatalf("%s: %s, evidence %q; want medium ending %q", c.Links[0], c.Confidence, c.Evidence, want)
		}
	}
}

// What the passive reading proves with no engine run behind it: a link that
// trained below its capability by the down-train rules, at the confidence
// they give config space alone, and the proven link-error findings.
func TestPassiveCauses(t *testing.T) {
	g := fig2Gen5()
	cfgs := healthyGen5PCIe(g)
	if got := analyzer.PassiveCauses(g, model.PCIePassive{Configs: cfgs}); len(got) != 0 {
		t.Fatalf("causes %+v on a host whose links are all at their capability", got)
	}

	cfgs["rnic0"] = gen5Link(pcieGen5, 8)        // x16 -> x8: 252 Gb/s under a 400G line rate
	cfgs["rnic1"] = gen5Link(pcieGen4, 16)       // Gen5 -> Gen4: 252 Gb/s
	cfgs["gpu3"] = gen5Link(pcieGen5, 8)         // a GPU's width counts
	cfgs["gpu4"] = gen5Link("2.5 GT/s PCIe", 16) // a GPU's speed does not
	delete(cfgs, "rnic2")                        // not read twice: not judged
	got := analyzer.PassiveCauses(g, model.PCIePassive{Configs: cfgs, Findings: []model.PCIeLinkFinding{
		linkFinding("pcie.gpu5", model.PCIeLinkSustained),
		linkFinding("pcie.gpu6", model.PCIeLinkSuspect),
		linkFinding("pcie.nosuch", model.PCIeLinkSustained),
	}})
	var names []string
	for _, c := range got {
		if c.Confidence != model.ConfidenceMedium {
			t.Fatalf("%s on %v is %s without a probe, want medium", c.Code, c.Links, c.Confidence)
		}
		if len(c.Links) != 1 {
			t.Fatalf("%s names links %v, want one", c.Code, c.Links)
		}
		names = append(names, c.Code+" "+c.Links[0])
	}
	want := []string{
		"gpu_pcie_failure pcie.gpu3",
		"pcie_link_errors pcie.gpu5",
		"rnic_pcie_failure pcie.rnic0",
		"rnic_pcie_failure pcie.rnic1",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("passive causes %v, want %v", names, want)
	}
	// No path was measured, so the evidence must not speak of one.
	for _, c := range got {
		if strings.Contains(c.Evidence, "no affinitive path abnormal yet") {
			t.Fatalf("%s evidence %q claims a measurement a passive report does not have", c.Code, c.Evidence)
		}
	}
	if ev := got[2].Evidence; ev != "PCIe 32.0 GT/s PCIe x8 (max 32.0 GT/s PCIe x16, port max 32.0 GT/s PCIe x16) trained below its width capability; 252 Gb/s cannot carry the 400 Gb/s line rate; read without a probe on two consecutive samples (config space only)" {
		t.Fatalf("rnic0 evidence %q", ev)
	}
	if ev := got[0].Evidence; ev != "PCIe 32.0 GT/s PCIe x8 (max 32.0 GT/s PCIe x16, port max 32.0 GT/s PCIe x16) trained below its width capability; read without a probe on two consecutive samples (config space only)" {
		t.Fatalf("gpu3 evidence %q", ev)
	}
	if ev := got[1].Evidence; !strings.HasPrefix(ev, "AER: 412 link errors/min against a budget of 16/min") || !strings.HasSuffix(ev, "; read without a probe (counters only)") {
		t.Fatalf("gpu5 evidence %q", ev)
	}
}

// The passive rules are the engine run's own: whatever PassiveCauses names
// for a link, a run that reads the same config space names too, under the
// same code and link, so the two kinds of report agree.
func TestPassiveDownTrainMatchesTheRunRule(t *testing.T) {
	g := fig2Gen5()
	for _, tc := range []struct {
		name string
		id   string
		cfg  model.PCIeConfig
	}{
		{"rnic narrower", "rnic0", gen5Link(pcieGen5, 8)},
		{"rnic slower", "rnic0", gen5Link(pcieGen4, 16)},
		{"rnic at its capability", "rnic0", gen5Link(pcieGen5, 16)},
		{"rnic port maximum unread", "rnic0", model.PCIeConfig{Speed: pcieGen4, Width: 8, MaxSpeed: pcieGen5, MaxWidth: 16}},
		{"gpu narrower", "gpu2", gen5Link(pcieGen5, 4)},
		{"gpu slower", "gpu2", gen5Link(pcieGen4, 16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgs := healthyGen5PCIe(g)
			cfgs[tc.id] = tc.cfg
			run := analyzer.Analyze(analyzer.Input{
				Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle,
				Measurements: gen5Measurements(g, 270*gbps, 273*gbps), PCIe: cfgs,
			})
			key := func(cs []model.RootCause) []string {
				var out []string
				for _, c := range cs {
					if c.Code == model.CauseRNICPCIeFailure || c.Code == model.CauseGPUPCIeFailure {
						out = append(out, c.Code+" "+strings.Join(c.Links, ",")+" "+string(c.Confidence))
					}
				}
				return out
			}
			passive := analyzer.PassiveCauses(g, model.PCIePassive{Configs: cfgs})
			if !slices.Equal(key(passive), key(run.Causes)) {
				t.Fatalf("passive %v, run %v", key(passive), key(run.Causes))
			}
		})
	}
}

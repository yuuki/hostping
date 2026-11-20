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

// linkFinding is a passive finding on link: 412 Bad TLPs a minute at the
// device end of a Gen4 x16 link, in the state given.
func linkFinding(link string, state model.PCIeLinkState) model.PCIeLinkFinding {
	f := model.PCIeLinkFinding{
		Link: link, State: state,
		ErrorsPerMinute: 412, BudgetPerMinute: 15.36, OverBudgetWindows: 3, WindowSeconds: 60,
		Ends: []model.PCIeLinkEnd{
			{BDF: "0000:16:00.0", Port: true},
			{BDF: "0000:17:00.0", Correctable: map[string]uint64{"BadTLP": 412, "Timeout": 3}},
		},
	}
	if state == model.PCIeLinkUncorrectable {
		f.ErrorsPerMinute, f.OverBudgetWindows = 0, 0
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
			"AER: 0 link errors/min against a budget of 16/min; 0000:16:00.0 (port): uncorrectable SDES=1; last window 60s; no bottleneck measured at the link in this run (counters only)",
			nil, []string{"gpu5"}},
		{"rp.2", "PCIe link counts link errors beyond its bit-error budget", "", nil, nil},
		{"pcie.rnic1", "PCIe link counts link errors beyond its bit-error budget",
			"AER: 412 link errors/min against a budget of 16/min, over it in 3 consecutive windows; 0000:17:00.0: BadTLP=412 Timeout=3; last window 60s; no bottleneck measured at the link in this run (counters only)",
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

// The counters are high only when the same run measured a bottleneck at the
// same link, and they neither replace nor raise the cause the probes name.
func TestLinkErrorsWithSymptomAreHigh(t *testing.T) {
	g := fig2Gen5()
	slow := func(rnic, ep string) analyzer.Input {
		in := healthyRun(g,
			linkFinding("pcie.rnic0", model.PCIeLinkSustained),
			linkFinding("pcie.gpu0", model.PCIeLinkSustained),
			linkFinding("pcie.gpu4", model.PCIeLinkSustained))
		for i := range in.Measurements {
			if in.Measurements[i].RNIC == rnic && in.Measurements[i].Endpoint == ep {
				in.Measurements[i].BandwidthBPS = 60 * gbps
			}
		}
		return in
	}
	res := analyzer.Analyze(slow("rnic0", "gpu0"))
	byLink := map[string]model.RootCause{}
	for _, c := range causesOf(res, model.CausePCIeLinkErrors) {
		byLink[c.Links[0]] = c
	}
	if len(byLink) != 3 {
		t.Fatalf("pcie_link_errors causes %+v, want one per finding", byLink)
	}
	// rnic0 -> gpu0 is an affinitive path of rnic0 and gpu0's affinitive path.
	for _, link := range []string{"pcie.rnic0", "pcie.gpu0"} {
		c := byLink[link]
		if c.Confidence != model.ConfidenceHigh || !strings.HasSuffix(c.Evidence, "; 1 affinitive path(s) abnormal") {
			t.Fatalf("%s %+v, want high with the abnormal path as its symptom", link, c)
		}
	}
	if c := byLink["pcie.rnic0"]; !slices.Equal(c.Endpoints, []string{"gpu0"}) {
		t.Fatalf("pcie.rnic0 endpoints %v, want the abnormal affinitive endpoint", c.Endpoints)
	}
	// gpu4 is under another root port: nothing was measured wrong there.
	if c := byLink["pcie.gpu4"]; c.Confidence != model.ConfidenceMedium {
		t.Fatalf("pcie.gpu4 %+v, want medium", c)
	}
	got := causesOf(res, model.CauseGPUPCIeFailure)
	if len(got) != 1 || got[0].Confidence != model.ConfidenceHigh || !slices.Equal(got[0].Endpoints, []string{"gpu0"}) {
		t.Fatalf("gpu_pcie_failure %+v, want the bandwidth verdict on gpu0 as without the counters", got)
	}

	// A path of another RNIC to gpu0 is no symptom of gpu0's link errors:
	// only its affinitive path is.
	res = analyzer.Analyze(slow("rnic2", "gpu0"))
	for _, c := range causesOf(res, model.CausePCIeLinkErrors) {
		if c.Confidence != model.ConfidenceMedium {
			t.Fatalf("%s is %s on a cross-socket path reading low", c.Links[0], c.Confidence)
		}
	}
}

// For the link below a root port the symptom is Algorithm 1 marking that hop
// abnormal, which the sim's root-port fault produces.
func TestLinkErrorsOnAbnormalRootPortAreHigh(t *testing.T) {
	g := topology.Fig2()
	rep := runWorld(t, "rp:rp.1", model.ModeIdle, nil)
	res := analyzer.Analyze(analyzer.Input{
		Graph: g, Cfg: config.Defaults(), Mode: model.ModeIdle, Measurements: rep.Paths,
		PCIeLinks: []model.PCIeLinkFinding{linkFinding("rp.1", model.PCIeLinkSustained), linkFinding("rp.3", model.PCIeLinkSustained)},
	})
	if got := causesOf(res, model.CauseCPURootPortFailure); len(got) != 1 || got[0].Links[0] != "rp.1" {
		t.Fatalf("cpu_root_port_failure %+v, want rp.1 from the probes", got)
	}
	for _, c := range causesOf(res, model.CausePCIeLinkErrors) {
		want := model.ConfidenceMedium
		if c.Links[0] == "rp.1" {
			want = model.ConfidenceHigh
		}
		if c.Confidence != want {
			t.Fatalf("%s is %s, want %s: %s", c.Links[0], c.Confidence, want, c.Evidence)
		}
	}
	if n := len(causesOf(res, model.CausePCIeLinkErrors)); n != 2 {
		t.Fatalf("%d pcie_link_errors causes, want 2: %+v", n, res.Causes)
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

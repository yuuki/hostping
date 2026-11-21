package monitor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/analyzer"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/topology"
)

const (
	gen4 = "16.0 GT/s PCIe"
	gen5 = "32.0 GT/s PCIe"
	// The functions of the scripted link: a root port and the device below.
	scriptPort = "0000:3a:00.0"
	scriptDev  = "0000:3b:00.0"
)

func TestPCIeErrorBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ber   float64
		speed string
		width int
		want  float64
	}{
		// 10^-12 × 16×10^9 bit/s × 16 lanes × 60 s.
		{"gen4 x16", 1e-12, gen4, 16, 15.36},
		{"gen5 x16", 1e-12, gen5, 16, 30.72},
		{"gen3 x8", 1e-12, "8.0 GT/s PCIe", 8, 3.84},
		{"gen1 x1", 1e-12, "2.5 GT/s PCIe", 1, 0.15},
		{"a looser ratio scales it", 1e-10, gen4, 16, 1536},
		{"no ratio", 0, gen4, 16, 0},
		{"negative ratio", -1e-12, gen4, 16, 0},
		{"NaN ratio", math.NaN(), gen4, 16, 0},
		// Above 32 GT/s the rate is held to no budget.
		{"gen6 x16", 1e-12, "64.0 GT/s PCIe", 16, 0},
		{"speed unknown", 1e-12, "Unknown", 16, 0},
		{"speed unread", 1e-12, "", 16, 0},
		{"width unread", 1e-12, gen4, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PCIeErrorBudget(tc.ber, model.PCIeConfig{Speed: tc.speed, Width: tc.width})
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("budget = %v, want %v", got, tc.want)
			}
		})
	}
}

// linkScript steps a pcieWatch through the samples of one Gen4 x16 device
// link whose two ends each keep cumulative counters, as sysfs does.
type linkScript struct {
	t    *testing.T
	w    *pcieWatch
	now  time.Time
	logs []string
	cfg  model.PCIeConfig
	// cfgOK is whether the negotiated link reads.
	cfgOK bool
	// port and dev are the cumulative counts of the two ends; portErr and
	// devErr, when set, make that end unreadable.
	port, dev       pci.LinkErrors
	portErr, devErr error
	last            model.PCIePassive
}

func newLinkScript(t *testing.T, ber float64) *linkScript {
	t.Helper()
	s := &linkScript{t: t, now: time.Unix(1_700_000_000, 0), cfgOK: true,
		cfg:  model.PCIeConfig{BDF: scriptDev, Speed: gen4, Width: 16, MaxSpeed: gen4, MaxWidth: 16, PortMaxSpeed: gen4, PortMaxWidth: 16},
		port: zeroErrors(), dev: zeroErrors()}
	s.w = newPCIeWatch(ber, func(f string, a ...any) { s.logs = append(s.logs, fmt.Sprintf(f, a...)) })
	return s
}

func zeroErrors() pci.LinkErrors {
	return pci.LinkErrors{
		Correctable:   map[string]uint64{pci.AERRxErr: 0, pci.AERBadTLP: 0, pci.AERBadDLLP: 0, pci.AERRollover: 0, pci.AERTimeout: 0},
		Uncorrectable: map[string]uint64{pci.AERDLP: 0, pci.AERSDES: 0},
	}
}

// sample takes one sample d after the last one.
func (s *linkScript) sample(d time.Duration) model.PCIePassive {
	s.t.Helper()
	s.now = s.now.Add(d)
	clone := func(e pci.LinkErrors) pci.LinkErrors {
		return pci.LinkErrors{Correctable: cloneRows(e.Correctable), Uncorrectable: cloneRows(e.Uncorrectable)}
	}
	smp := PCIeLinkSample{Link: "pcie.rnic0", Device: "rnic0", Config: s.cfg, ConfigOK: s.cfgOK, Ends: []PCIeEndSample{
		{BDF: scriptPort, Port: true, Errors: clone(s.port), Err: s.portErr},
		{BDF: scriptDev, Errors: clone(s.dev), Err: s.devErr},
	}}
	s.last = s.w.observe(s.now, []PCIeLinkSample{smp})
	return s.last
}

func cloneRows(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// minute takes the six ten-second samples of one window, adding perMinute
// Bad TLPs at the device end over it, and returns the state after it.
func (s *linkScript) minute(perMinute uint64) model.PCIePassive {
	s.t.Helper()
	for i := 0; i < 6; i++ {
		add := perMinute / 6
		if i == 0 {
			add += perMinute % 6
		}
		s.dev.Correctable[pci.AERBadTLP] += add
		s.sample(10 * time.Second)
	}
	return s.last
}

// state is the finding state of the scripted link, "" without one.
func (s *linkScript) state() model.PCIeLinkState {
	if len(s.last.Findings) == 0 {
		return ""
	}
	if len(s.last.Findings) > 1 || s.last.Findings[0].Link != "pcie.rnic0" {
		s.t.Fatalf("findings %+v, want at most one, on pcie.rnic0", s.last.Findings)
	}
	return s.last.Findings[0].State
}

func (s *linkScript) logged(substr string) bool {
	for _, l := range s.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// The counters a function had before the daemon started are a baseline, not
// evidence: a link that has counted a million errors in its life and none
// since is quiet.
func TestPCIeWatchLifetimeTotalIsNoEvidence(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.dev.Correctable[pci.AERBadTLP] = 1_000_000
	s.port.Correctable[pci.AERRxErr] = 5_000_000
	s.port.Uncorrectable[pci.AERSDES] = 3
	first := s.sample(0)
	if len(first.Errors) != 0 || len(first.Findings) != 0 {
		t.Fatalf("first sample %+v, want no errors and no finding: it has nothing to be differenced against", first)
	}
	for i := 0; i < 5; i++ {
		if got := s.minute(0); len(got.Findings) != 0 {
			t.Fatalf("window %d: findings %+v on a link whose counters did not move", i, got.Findings)
		}
	}
	// Every later sample is measured, and measures zero.
	if len(s.last.Errors) != 1 || s.last.Errors[0] != (model.PCIeLinkErrors{Link: "pcie.rnic0", BudgetPerMinute: 15.36}) {
		t.Fatalf("errors %+v, want pcie.rnic0 measured at 0 with its budget", s.last.Errors)
	}
}

// Gen4 x16 is allowed ceil(15.36) = 16 link errors a minute per end. A rate
// within that, however steady, is no finding; one over it is a hint for two
// windows and a verdict from the third. The finding then stands for as long
// as one of the last 30 windows was over the budget, and says so.
func TestPCIeWatchSuspectThenSustainedThenEnds(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 10; i++ {
		if s.minute(16); s.state() != "" {
			t.Fatalf("window %d: 16 errors/min is the budget, not over it: %+v", i, s.last.Findings)
		}
	}
	want := []model.PCIeLinkState{model.PCIeLinkSuspect, model.PCIeLinkSuspect, model.PCIeLinkSustained, model.PCIeLinkSustained}
	for i, w := range want {
		s.minute(17)
		if s.state() != w {
			t.Fatalf("window %d over the budget: state %q, want %q", i+1, s.state(), w)
		}
		f := s.last.Findings[0]
		if f.OverBudgetWindows != i+1 || f.OverBudgetRuns != 1 || f.OverBudgetLongestRun != i+1 || f.RecentWindows != 11+i ||
			f.ErrorsPerMinute != 17 || f.BudgetPerMinute != 15.36 || f.WindowSeconds != 60 {
			t.Fatalf("window %d finding %+v", i+1, f)
		}
		if len(f.Ends) != 2 || f.Ends[0].BDF != scriptPort || !f.Ends[0].Port || f.Ends[0].Correctable != nil ||
			f.Ends[1].BDF != scriptDev || f.Ends[1].Correctable[pci.AERBadTLP] != 17 {
			t.Fatalf("window %d ends %+v, want the port first with nothing and the device with 17 Bad TLPs", i+1, f.Ends)
		}
	}
	if !s.logged("pcie.rnic0: suspect") || !s.logged("pcie.rnic0: sustained") || !s.logged("reported as pcie_link_errors") {
		t.Fatalf("state changes not logged: %q", s.logs)
	}
	logs := len(s.logs)
	// Windows within the budget: the verdict stands on the four before, and
	// the evidence says which windows it stands on.
	for i := 0; i < 2; i++ {
		if s.minute(3); s.state() != model.PCIeLinkSustained {
			t.Fatalf("clean window %d: state %q, want the sustained finding to stand", i+1, s.state())
		}
		if f := s.last.Findings[0]; f.OverBudgetWindows != 4 || f.OverBudgetLongestRun != 4 || f.ErrorsPerMinute != 3 {
			t.Fatalf("clean window %d finding %+v, want the last window's rate and the four windows over the budget", i+1, f)
		}
	}
	if ev := s.last.Findings[0].Evidence(); !strings.HasPrefix(ev, "3 link errors/min against a budget of 16/min in the last window; over the budget in 4 of the last 16 windows (1 time, at most 4 in a row), 15/min across them; ") {
		t.Fatalf("evidence %q", ev)
	}
	// A relapse is one more time over the budget, and the 30 windows start
	// over from it.
	if s.minute(40); s.state() != model.PCIeLinkSustained || s.last.Findings[0].OverBudgetWindows != 5 || s.last.Findings[0].OverBudgetRuns != 2 {
		t.Fatalf("relapse: %+v", s.last.Findings)
	}
	for i := 1; i < model.PCIeRecentWindows; i++ {
		if s.minute(0); s.state() != model.PCIeLinkSustained {
			t.Fatalf("clean window %d after the relapse: state %q, want the finding to stand while a window over the budget is among the last %d", i, s.state(), model.PCIeRecentWindows)
		}
	}
	if f := s.last.Findings[0]; f.OverBudgetWindows != 1 || f.RecentWindows != model.PCIeRecentWindows {
		t.Fatalf("finding %+v, want it to stand on the relapse alone by now", f)
	}
	if s.minute(0); s.state() != "" {
		t.Fatalf("%d clean windows: finding %+v still stands", model.PCIeRecentWindows, s.last.Findings)
	}
	if got := s.logs[logs:]; len(got) != 1 || !strings.Contains(got[0], "pcie.rnic0: no window over the budget among the last 30; the sustained finding ends") {
		t.Fatalf("log lines since the verdict %q, want the end of the finding and nothing else", got)
	}
}

// One burst of errors, however large, is a hint and never a verdict: it is
// over the budget in one window, or in two in a row when it straddles their
// boundary. Neither are two bursts. The hint stands, logged once, until none
// of the last 30 windows was over the budget.
func TestPCIeWatchOneBurstIsOnlyAHint(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	if s.minute(1_000_000); s.state() != model.PCIeLinkSuspect {
		t.Fatalf("burst: state %q, want suspect", s.state())
	}
	if ev := s.last.Findings[0].Evidence(); !strings.Contains(ev, "over the budget in the one window judged so far") {
		t.Fatalf("evidence %q", ev)
	}
	for i := 1; i < model.PCIeRecentWindows; i++ {
		if s.minute(0); s.state() != model.PCIeLinkSuspect || s.last.Findings[0].OverBudgetWindows != 1 {
			t.Fatalf("window %d after the burst: %+v, want the suspect to stand on the one window", i, s.last.Findings)
		}
	}
	if s.minute(0); s.state() != "" {
		t.Fatalf("%d windows after the burst: %+v", model.PCIeRecentWindows, s.last.Findings)
	}
	if !s.logged("pcie.rnic0: no window over the budget among the last 30; the suspect finding ends") {
		t.Fatalf("end of the hint not logged: %q", s.logs)
	}
	logs := len(s.logs)
	// Two bursts, each across a window boundary: four windows over the
	// budget, two separate times, and still a hint.
	for i, n := range []uint64{1_000_000, 1_000_000, 0, 0, 0, 1_000_000, 1_000_000, 0, 0, 0, 0, 0} {
		s.minute(n)
		if s.state() != model.PCIeLinkSuspect {
			t.Fatalf("window %d (%d errors): state %q, want a standing suspect", i, n, s.state())
		}
	}
	if f := s.last.Findings[0]; f.OverBudgetWindows != 4 || f.OverBudgetRuns != 2 || f.OverBudgetLongestRun != 2 {
		t.Fatalf("finding %+v, want 4 windows over the budget in 2 runs of 2", f)
	}
	if got := len(s.logs) - logs; got != 1 {
		t.Fatalf("%d log lines for one suspect that never ended: %q", got, s.logs[logs:])
	}
}

// A link that errs in bursts is over its budget in some windows only, never
// three in a row. It is a verdict from the third separate time, and stays
// one: the finding does not come and go with the bursts.
func TestPCIeWatchBurstsMakeAVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		// errors is what window i counts.
		errors func(i int) uint64
		// verdict is the window, from 1, that makes the verdict.
		verdict int
	}{
		{"every other minute", func(i int) uint64 { return uint64(100_000 * (1 - i%2)) }, 5},
		{"two minutes in three", func(i int) uint64 {
			if i%3 == 2 {
				return 0
			}
			return 100_000
		}, 7},
		{"one minute in fourteen", func(i int) uint64 {
			if i%14 == 0 {
				return 1200
			}
			return 0
		}, 29},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLinkScript(t, 1e-12)
			s.sample(0)
			for i := 0; i < 240; i++ {
				s.minute(tc.errors(i))
				want := model.PCIeLinkSuspect
				if i+1 >= tc.verdict {
					want = model.PCIeLinkSustained
				}
				if s.state() != want {
					t.Fatalf("window %d: state %q, want %q: %+v", i+1, s.state(), want, s.last.Findings)
				}
			}
			if len(s.logs) != 2 || !strings.Contains(s.logs[0], "suspect") || !strings.Contains(s.logs[1], "sustained") || !strings.Contains(s.logs[1], "reported as pcie_link_errors") {
				t.Fatalf("log of four hours %q, want the hint and the verdict, once each", s.logs)
			}
		})
	}

	// 1200 errors in one minute of every three, 75 times the budget while
	// they last: the third burst is the verdict, and the evidence names what
	// it stands on in a window that counted nothing.
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 8; i++ {
		n := uint64(0)
		if i%3 == 0 {
			n = 1200
		}
		s.minute(n)
		want := model.PCIeLinkSuspect
		if i >= 6 {
			want = model.PCIeLinkSustained
		}
		if s.state() != want {
			t.Fatalf("window %d: state %q, want %q", i+1, s.state(), want)
		}
	}
	want := "0 link errors/min against a budget of 16/min in the last window; over the budget in 3 of the last 8 windows (3 separate times, at most 1 in a row), 450/min across them; last window 60s"
	if got := s.last.Findings[0].Evidence(); got != want {
		t.Fatalf("evidence %q, want %q", got, want)
	}
}

// What bursts do not make a verdict. A link whose errors stay within its
// budget over the windows looked back on is not accused for being over it
// in a window three separate times: at a rate near the budget that happens
// by chance. And bursts so far apart that fewer than three fall into the
// last 30 windows stay a hint, which is the price of looking back no
// further.
func TestPCIeWatchBurstsThatAreNoVerdict(t *testing.T) {
	// 17 errors in one window of every ten, nothing in between: over the
	// budget of 16 a minute three times in 30 windows, at 1.7 a minute.
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 240; i++ {
		n := uint64(0)
		if i%10 == 0 {
			n = 17
		}
		if s.minute(n); s.state() != model.PCIeLinkSuspect {
			t.Fatalf("window %d: state %q, want suspect: %+v", i+1, s.state(), s.last.Findings)
		}
	}
	if f := s.last.Findings[0]; f.OverBudgetRuns != 3 || math.Abs(f.RecentErrorsPerMinute-1.7) > 1e-9 {
		t.Fatalf("finding %+v, want 3 times over the budget at 1.7 errors a minute", f)
	}

	// A large burst every 15 minutes: two in any 30 windows.
	s = newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 240; i++ {
		n := uint64(0)
		if i%15 == 0 {
			n = 100_000
		}
		if s.minute(n); s.state() != model.PCIeLinkSuspect {
			t.Fatalf("window %d: state %q, want suspect: %+v", i+1, s.state(), s.last.Findings)
		}
	}
	if len(s.logs) != 1 {
		t.Fatalf("log %q, want the one hint", s.logs)
	}
}

// One error can add to more than one row: the kernel adds one to every row
// whose status bit an error report carries, and a corrupted TLP sets
// Receiver Error and Bad TLP. An end's errors are therefore its largest row,
// not the sum: nine such errors a minute are nine, within Gen4 x16's 16.
func TestPCIeWatchCountsAnEndByItsLargestRow(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for w := 0; w < 4; w++ {
		for i := 0; i < 6; i++ {
			add := uint64(1)
			if i < 3 {
				add = 2
			}
			s.dev.Correctable[pci.AERRxErr] += add
			s.dev.Correctable[pci.AERBadTLP] += add
			s.sample(10 * time.Second)
		}
		if s.state() != "" {
			t.Fatalf("window %d: %+v for 9 errors a minute that each set two rows", w+1, s.last.Findings)
		}
	}
	// What was read is still exported row by row.
	if e := s.last.Errors; len(e) != 1 || e[0].Correctable != 2 {
		t.Fatalf("errors %+v, want the 2 row increments of the last sample", e)
	}
	// Three rows of 10 at one end are at least 10 errors, not 30.
	for w := 0; w < 4; w++ {
		s.dev.Correctable[pci.AERRxErr] += 10
		s.dev.Correctable[pci.AERBadDLLP] += 10
		if s.minute(10); s.state() != "" {
			t.Fatalf("window %d: %+v for three rows of 10 at one end", w+1, s.last.Findings)
		}
	}
	// The largest row over the budget is over it, whatever the others hold.
	s.dev.Correctable[pci.AERRxErr] += 5
	if s.minute(17); s.state() != model.PCIeLinkSuspect || s.last.Findings[0].ErrorsPerMinute != 17 {
		t.Fatalf("17 Bad TLPs and 5 Receiver Errors: %+v, want a suspect at 17 a minute", s.last.Findings)
	}
}

// The evidence prints the rate and the budget the way they compared. Windows
// are seldom exactly a minute long, so a count just over the budget is a
// rate that rounds to the budget.
func TestPCIeWatchEvidenceNeverSaysOverAtTheBudget(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 5; i++ {
		s.sample(10 * time.Second)
	}
	// The sixth sample comes two seconds late, as a loaded daemon's does:
	// 17 errors in 62 s are 16.45 a minute, over 16.
	s.dev.Correctable[pci.AERBadTLP] += 17
	s.sample(12 * time.Second)
	if s.state() != model.PCIeLinkSuspect {
		t.Fatalf("state %q, want suspect", s.state())
	}
	want := "16.5 link errors/min against a budget of 16/min in the last window; over the budget in the one window judged so far; 0000:3b:00.0: BadTLP=17; last window 62s"
	if got := s.last.Findings[0].Evidence(); got != want {
		t.Fatalf("evidence %q, want %q", got, want)
	}
}

// A window is held to the largest budget one of its samples gave. A GPU that
// drops its link speed while idle and retrains under load is not over a
// Gen1 budget for the errors it counted at Gen4.
func TestPCIeWatchBudgetFollowsTheFastestSpeedInTheWindow(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.cfg.Speed = "2.5 GT/s PCIe" // 2.4 a minute at x16: over it from 4
	s.sample(0)
	for i := 0; i < 3; i++ {
		s.minute(10)
	}
	if s.state() != model.PCIeLinkSustained {
		t.Fatalf("state %q for 10 errors a minute at Gen1 x16, want sustained", s.state())
	}
	for i := 0; i < model.PCIeRecentWindows; i++ {
		s.minute(0)
	}
	if s.state() != "" {
		t.Fatalf("finding %+v outlived %d quiet windows", s.last.Findings, model.PCIeRecentWindows)
	}
	// Each window now has one sample at Gen4, in its middle.
	for i := 0; i < 4; i++ {
		for j := 0; j < 6; j++ {
			s.cfg.Speed = "2.5 GT/s PCIe"
			if j == 2 {
				s.cfg.Speed = gen4
			}
			s.dev.Correctable[pci.AERBadTLP] += 2 // 12 a minute, under Gen4 x16's 16
			s.sample(10 * time.Second)
		}
		if s.state() != "" {
			t.Fatalf("window %d: %+v for 12 errors a minute on a link that ran at Gen4 in it", i, s.last.Findings)
		}
	}
	// The gauge follows the sample, not the window.
	if e := s.last.Errors; len(e) != 1 || math.Abs(e[0].BudgetPerMinute-2.4) > 1e-9 {
		t.Fatalf("errors %+v, want the budget of the speed the last sample read", e)
	}
}

// The budget is per direction, so each end is held to it on its own: two
// ends just within it are not one link over it.
func TestPCIeWatchJudgesTheWorseEnd(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 4; i++ {
		s.port.Correctable[pci.AERRxErr] += 16
		if s.minute(16); s.state() != "" {
			t.Fatalf("window %d: 16/min at each end: %+v", i, s.last.Findings)
		}
	}
	// The port alone over it implicates the link just the same.
	for i := 0; i < 3; i++ {
		s.port.Correctable[pci.AERTimeout] += 30
		s.minute(0)
	}
	if s.state() != model.PCIeLinkSustained {
		t.Fatalf("state %q, want sustained on the port's count", s.state())
	}
	f := s.last.Findings[0]
	if f.ErrorsPerMinute != 30 || f.Ends[0].Correctable[pci.AERTimeout] != 30 || f.Ends[1].Correctable != nil {
		t.Fatalf("finding %+v, want 30/min counted at the port", f)
	}
}

// An uncorrectable link error is a verdict at once, needs no budget, and
// stands for three windows.
func TestPCIeWatchUncorrectableIsImmediate(t *testing.T) {
	for _, ber := range []float64{1e-12, 0} {
		s := newLinkScript(t, ber)
		s.sample(0)
		s.minute(0)
		s.port.Uncorrectable[pci.AERSDES]++
		if s.minute(0); s.state() != model.PCIeLinkUncorrectable {
			t.Fatalf("ber=%g: state %q after a Surprise Down, want uncorrectable", ber, s.state())
		}
		if f := s.last.Findings[0]; f.Ends[0].Uncorrectable[pci.AERSDES] != 1 || f.OverBudgetWindows != 0 {
			t.Fatalf("ber=%g: finding %+v", ber, f)
		}
		for i := 0; i < 2; i++ {
			if s.minute(0); s.state() != model.PCIeLinkUncorrectable {
				t.Fatalf("ber=%g: window %d after it: state %q, want it to stand", ber, i+1, s.state())
			}
		}
		if s.minute(0); s.state() != "" {
			t.Fatalf("ber=%g: finding %+v outlived three windows", ber, s.last.Findings)
		}
	}
}

// --pcie-ber 0 judges no rate: no count, however large, is a finding.
func TestPCIeWatchNoRatioJudgesNoRate(t *testing.T) {
	s := newLinkScript(t, 0)
	s.sample(0)
	for i := 0; i < 5; i++ {
		if s.minute(1_000_000); s.state() != "" {
			t.Fatalf("window %d: %+v", i, s.last.Findings)
		}
	}
	if e := s.last.Errors; len(e) != 1 || e[0].BudgetPerMinute != 0 || e[0].Correctable == 0 {
		t.Fatalf("errors %+v, want them counted with no budget", e)
	}
	if len(s.logs) != 0 {
		t.Fatalf("logged %q; an operator who turned the rate off needs no line about it", s.logs)
	}
}

// A counter that goes down belongs to a device that was enumerated again.
// Its interval is unknown, not 0 and not 2^64, the end is left out of that
// window, and the other end still measures.
func TestPCIeWatchCounterReset(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.dev.Correctable[pci.AERBadTLP] = 900
	s.sample(0)
	s.minute(0)
	// The device comes back with fresh counters in the middle of a window
	// in which the port counted a surprise down.
	s.sample(10 * time.Second)
	s.dev = zeroErrors()
	s.port.Uncorrectable[pci.AERSDES]++
	got := s.sample(10 * time.Second)
	if len(got.Errors) != 1 || got.Errors[0].Correctable != 0 || got.Errors[0].Uncorrectable != 1 {
		t.Fatalf("errors %+v on the reset sample, want only the port's surprise down", got.Errors)
	}
	if !s.logged("AER counters of " + scriptDev + " went down") {
		t.Fatalf("reset not logged: %q", s.logs)
	}
	// The fresh device errs heavily for the rest of the window. Its count
	// does not stand for the window, because it was not read through it.
	for i := 0; i < 4; i++ {
		s.dev.Correctable[pci.AERBadTLP] += 500
		s.sample(10 * time.Second)
	}
	if s.state() != model.PCIeLinkUncorrectable {
		t.Fatalf("state %q, want uncorrectable from the port", s.state())
	}
	if f := s.last.Findings[0]; len(f.Ends) != 1 || f.Ends[0].BDF != scriptPort || f.ErrorsPerMinute != 0 {
		t.Fatalf("finding %+v, want the port only: the device was re-baselined inside the window", f)
	}
	// From the next window on the device counts again.
	s.minute(600)
	if f := s.last.Findings[0]; len(f.Ends) != 2 || f.ErrorsPerMinute != 600 {
		t.Fatalf("next window %+v, want the device measured again", f)
	}
}

// A function whose counters stop reading is unknown. A window no end was read
// through ends a finding instead of letting it stand on old evidence or
// clearing it as healthy, and the first sample that reads again is a
// baseline.
func TestPCIeWatchUnreadableCounters(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 3; i++ {
		s.minute(100)
	}
	if s.state() != model.PCIeLinkSustained {
		t.Fatalf("state %q, want sustained", s.state())
	}
	gone := fmt.Errorf("%w: aer_dev_correctable: gone", pci.ErrAERAbsent)
	s.portErr, s.devErr = gone, gone
	got := s.sample(10 * time.Second)
	if len(got.Errors) != 0 {
		t.Fatalf("errors %+v from a link no end of which reads, want none (not a 0)", got.Errors)
	}
	if s.state() != model.PCIeLinkSustained {
		t.Fatalf("state %q inside the window, want the finding until the window is judged", s.state())
	}
	for i := 0; i < 5; i++ {
		s.sample(10 * time.Second)
	}
	if s.state() != "" {
		t.Fatalf("finding %+v stands on a window that was not read", s.last.Findings)
	}
	if !s.logged("could not be read") || !s.logged("the sustained finding is dropped, not cleared") {
		t.Fatalf("logs %q", s.logs)
	}
	// They read again, far above where they were: no delta spans the gap.
	s.portErr, s.devErr = nil, nil
	s.dev.Correctable[pci.AERBadTLP] += 1_000_000
	if got := s.sample(10 * time.Second); len(got.Errors) != 0 {
		t.Fatalf("errors %+v on the first sample after the gap, want a baseline", got.Errors)
	}
	if !s.logged("read again; re-baselined") {
		t.Fatalf("logs %q", s.logs)
	}
	// The gap fell into this window, so it is unknown too.
	for i := 0; i < 5; i++ {
		s.sample(10 * time.Second)
	}
	if s.state() != "" {
		t.Fatalf("finding %+v from a window with a gap", s.last.Findings)
	}
	if s.minute(100); s.state() != model.PCIeLinkSuspect {
		t.Fatalf("state %q in the first whole window, want suspect", s.state())
	}
}

// Without a negotiated speed and width there is no budget, and a rate that
// cannot be judged is neither over it nor within it.
func TestPCIeWatchUnreadLinkIsNotJudged(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 3; i++ {
		s.minute(100)
	}
	if s.state() != model.PCIeLinkSustained {
		t.Fatalf("state %q, want sustained", s.state())
	}
	// The window that begins on a sample that still read the link keeps
	// that sample's budget; the next one has none.
	s.cfgOK = false
	if s.minute(100); s.state() != model.PCIeLinkSustained {
		t.Fatalf("state %q in the window that began with a known link, want sustained", s.state())
	}
	if s.minute(100); s.state() != "" {
		t.Fatalf("finding %+v with no budget to hold the rate to", s.last.Findings)
	}
	if !s.logged("has no budget to be judged against") || !s.logged("its link-error rate can no longer be judged; the sustained finding ends") {
		t.Fatalf("logs %q", s.logs)
	}
	if e := s.last.Errors; len(e) != 1 || e[0].BudgetPerMinute != 0 {
		t.Fatalf("errors %+v, want the count with no budget", e)
	}
	n := len(s.logs)
	if s.minute(100); len(s.logs) != n {
		t.Fatalf("the unjudged link is logged again: %q", s.logs[n:])
	}
	// An "Unknown" speed is as good as none.
	s.cfgOK, s.cfg.Speed = true, "Unknown"
	if s.minute(100); s.state() != "" {
		t.Fatalf("finding %+v at an unknown speed", s.last.Findings)
	}
	s.cfg.Speed = gen4
	if s.minute(100); s.state() != model.PCIeLinkSuspect {
		t.Fatalf("state %q once the link reads again, want suspect", s.state())
	}
}

// A link at 64 GT/s corrects bit errors as it runs, and what its AER counters
// count then is not established: its rate is held to no budget, which is
// said once, and only its uncorrectable errors are judged.
func TestPCIeWatchJudgesNoRateAbove32GT(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.cfg.Speed, s.cfg.MaxSpeed, s.cfg.PortMaxSpeed = "64.0 GT/s PCIe", "64.0 GT/s PCIe", "64.0 GT/s PCIe"
	s.sample(0)
	for i := 0; i < 5; i++ {
		if s.minute(1_000_000); s.state() != "" {
			t.Fatalf("window %d: %+v for a rate with no budget", i+1, s.last.Findings)
		}
	}
	if e := s.last.Errors; len(e) != 1 || e[0].BudgetPerMinute != 0 || e[0].Correctable == 0 {
		t.Fatalf("errors %+v, want them counted with no budget", e)
	}
	if len(s.logs) != 1 || !strings.Contains(s.logs[0], "pcie.rnic0: trained at 64.0 GT/s PCIe: a link faster than 32 GT/s corrects bit errors as it runs") {
		t.Fatalf("logs %q, want the reason once", s.logs)
	}
	s.port.Uncorrectable[pci.AERDLP]++
	if s.minute(1_000_000); s.state() != model.PCIeLinkUncorrectable {
		t.Fatalf("state %q after a Data Link Protocol Error, want uncorrectable", s.state())
	}
	f := s.last.Findings[0]
	if f.BudgetPerMinute != 0 || f.RecentWindows != 0 || f.OverBudgetWindows != 0 {
		t.Fatalf("finding %+v, want no budget and no window judged", f)
	}
	if ev := f.Evidence(); !strings.HasPrefix(ev, "1000000 link errors/min with no budget to judge them against; 0000:3a:00.0 (port): uncorrectable DLP=1; ") {
		t.Fatalf("evidence %q", ev)
	}
	// A link that retrains to 32 GT/s is judged from its first window that
	// holds no sample at 64 GT/s: the window it retrained in began on one.
	s.cfg.Speed = gen5
	if s.minute(1_000_000); s.last.Findings[0].RecentWindows != 0 {
		t.Fatalf("findings %+v for the window the link retrained in, want it unjudged", s.last.Findings)
	}
	if s.minute(1_000_000); len(s.last.Findings) != 1 || s.last.Findings[0].OverBudgetWindows != 1 || s.last.Findings[0].BudgetPerMinute != 30.72 {
		t.Fatalf("findings %+v once the link runs at 32 GT/s, want its first window over the budget", s.last.Findings)
	}
}

// An uncorrectable finding ends because three windows counted no
// uncorrectable error, and the log says that whatever became of the rate:
// here the device left the bus, and the link's rate has no budget either.
func TestPCIeWatchSaysWhyAnUncorrectableFindingEnds(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	s.minute(0)
	s.port.Uncorrectable[pci.AERSDES]++
	s.cfgOK = false
	s.devErr = fmt.Errorf("%w: %s: gone", pci.ErrAERAbsent, scriptDev)
	if s.minute(0); s.state() != model.PCIeLinkUncorrectable {
		t.Fatalf("state %q after the Surprise Down, want uncorrectable", s.state())
	}
	for s.state() != "" {
		s.minute(0)
		if len(s.logs) > 20 {
			t.Fatalf("the finding does not end: %q", s.logs)
		}
	}
	last := s.logs[len(s.logs)-1]
	if want := "hostping: pcie: pcie.rnic0: no uncorrectable link error in the last 3 windows; the uncorrectable finding ends"; last != want {
		t.Fatalf("log %q, want %q", last, want)
	}
}

// A sample interval longer than a minute makes every sample a window, and
// the rate is still per minute.
func TestPCIeWatchLongInterval(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	s.sample(0)
	for i := 0; i < 3; i++ {
		s.dev.Correctable[pci.AERBadTLP] += 170 // 17 a minute over ten minutes
		s.sample(10 * time.Minute)
	}
	if s.state() != model.PCIeLinkSustained {
		t.Fatalf("state %q, want sustained", s.state())
	}
	if f := s.last.Findings[0]; f.ErrorsPerMinute != 17 || f.WindowSeconds != 600 {
		t.Fatalf("finding %+v", f)
	}
}

// A device link is handed on for the down-train rules only once its speed
// and width read the same on two consecutive samples.
func TestPCIeWatchConfigNeedsTwoEqualReads(t *testing.T) {
	s := newLinkScript(t, 1e-12)
	if got := s.sample(0); got.Configs != nil {
		t.Fatalf("configs %+v after one read", got.Configs)
	}
	if got := s.sample(10 * time.Second); got.Configs["rnic0"].Width != 16 {
		t.Fatalf("configs %+v after two equal reads", got.Configs)
	}
	s.cfg.Width = 8
	if got := s.sample(10 * time.Second); got.Configs != nil {
		t.Fatalf("configs %+v on the sample the width changed", got.Configs)
	}
	if got := s.sample(10 * time.Second); got.Configs["rnic0"].Width != 8 {
		t.Fatalf("configs %+v, want the narrowed link once it read twice", got.Configs)
	}
	s.cfg.Speed = "8.0 GT/s PCIe"
	if got := s.sample(10 * time.Second); got.Configs != nil {
		t.Fatalf("configs %+v on the sample the speed changed", got.Configs)
	}
	s.cfgOK = false
	s.sample(10 * time.Second)
	s.cfgOK = true
	if got := s.sample(10 * time.Second); got.Configs != nil {
		t.Fatalf("configs %+v right after an unread sample", got.Configs)
	}
}

func TestRowDeltas(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prev, cur map[string]uint64
		want      map[string]uint64
		ok        bool
	}{
		{"moved", map[string]uint64{"a": 1, "b": 5}, map[string]uint64{"a": 4, "b": 5}, map[string]uint64{"a": 3}, true},
		{"still", map[string]uint64{"a": 1}, map[string]uint64{"a": 1}, map[string]uint64{}, true},
		{"went down", map[string]uint64{"a": 9, "b": 1}, map[string]uint64{"a": 0, "b": 7}, nil, false},
		{"went down from the upper half", map[string]uint64{"a": math.MaxUint64}, map[string]uint64{"a": 3}, nil, false},
		{"row vanished", map[string]uint64{"a": 1, "b": 1}, map[string]uint64{"a": 1}, nil, false},
		{"row appeared", map[string]uint64{"a": 1}, map[string]uint64{"a": 1, "b": 0}, nil, false},
		{"row renamed", map[string]uint64{"a": 1}, map[string]uint64{"b": 1}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rowDeltas(tc.prev, tc.cur)
			if ok != tc.ok || fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("rowDeltas = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// pcieHost drives a Fig.2 monitor with a passive PCIe reading on a stepped
// clock. busy lists the RNICs that carry traffic on every step.
type pcieHost struct {
	t       *testing.T
	m       *Monitor
	s       StaticSampler
	clock   time.Time
	busy    []string
	tx      uint64
	samples int
	badTLP  uint64
	need    []string
	failGPU bool
}

func newPCIeHost(t *testing.T, cfg config.Config, busy ...string) *pcieHost {
	t.Helper()
	g := topology.Fig2()
	h := &pcieHost{t: t, clock: time.Unix(1_700_000_000, 0), busy: busy}
	h.s = StaticSampler{Counters: map[string]model.RNICCounters{}, GPU: []model.GPUStatus{{ID: "gpu0", Idle: len(busy) == 0}}}
	h.s.PCIe = func(context.Context) []PCIeLinkSample {
		h.samples++
		e := zeroErrors()
		e.Correctable[pci.AERBadTLP] = h.badTLP
		return []PCIeLinkSample{{Link: "pcie.gpu3", Device: "gpu3", ConfigOK: true,
			Config: model.PCIeConfig{Speed: gen4, Width: 16}, Ends: []PCIeEndSample{{BDF: "0000:39:00.0", Errors: e}}}}
	}
	h.m = New(cfg, g, failingGPUs{StaticSampler: h.s, fail: &h.failGPU})
	h.m.Logf = func(string, ...any) {}
	h.m.SetClock(func() time.Time { return h.clock })
	h.m.PCIeNeed = func() []string { return h.need }
	return h
}

// failingGPUs fails the GPU read, and with it the poll, while *fail is set.
type failingGPUs struct {
	StaticSampler
	fail *bool
}

func (s failingGPUs) GPUs(ctx context.Context) ([]model.GPUStatus, error) {
	if *s.fail {
		return nil, errors.New("gpu state unreadable")
	}
	return s.StaticSampler.GPUs(ctx)
}

// step advances the clock by d, moves the busy RNICs' counters by 90 % of
// line rate over it, and polls.
func (h *pcieHost) step(d time.Duration) (Decision, error) {
	h.t.Helper()
	h.clock = h.clock.Add(d)
	h.tx += uint64(0.9 * 200e9 / 8 * d.Seconds())
	for _, id := range h.busy {
		h.s.Counters[id] = model.RNICCounters{TxBytes: h.tx}
	}
	return h.m.Poll(context.Background())
}

func (h *pcieHost) mustStep(d time.Duration) Decision {
	h.t.Helper()
	dec, err := h.step(d)
	if err != nil {
		h.t.Fatal(err)
	}
	return dec
}

func TestPollPCIeOffByDefault(t *testing.T) {
	cfg := config.Defaults()
	if cfg.PCIeInterval != 0 || cfg.PCIeProbeInterval != 0 {
		t.Fatalf("the passive reading and its probe must default to off: %v, %v", cfg.PCIeInterval, cfg.PCIeProbeInterval)
	}
	h := newPCIeHost(t, cfg)
	for i := 0; i < 5; i++ {
		if d := h.mustStep(time.Second); d.PCIe != nil {
			t.Fatalf("poll %d: passive reading %+v without --pcie-interval", i, d.PCIe)
		}
	}
	if h.samples != 0 {
		t.Fatalf("%d passive samples taken without --pcie-interval", h.samples)
	}
	// A sampler without a passive reading is fine too.
	cfg.PCIeInterval = time.Second
	m := New(cfg, topology.Fig2(), noPCIe{})
	if d, err := m.Poll(context.Background()); err != nil || d.PCIe != nil {
		t.Fatalf("decision %+v, %v from a sampler with no passive reading", d, err)
	}
}

// noPCIe is a Sampler that does not implement PCIeSampler.
type noPCIe struct{}

func (noPCIe) RNIC(context.Context, model.RNIC) (model.RNICCounters, error) {
	return model.RNICCounters{PauseSources: 1, DropSources: 1}, nil
}
func (noPCIe) GPUs(context.Context) ([]model.GPUStatus, error) { return nil, nil }
func (noPCIe) Bus(context.Context) map[string]float64          { return nil }

// The passive reading is taken once per --pcie-interval, not once per poll,
// and it does not wait for the RNIC counters to be ready. What a sample
// added is handed on exactly once, and by the poll that took it even when
// that poll fails on another reading.
func TestPollPCIeSamplesOncePerInterval(t *testing.T) {
	cfg := config.Defaults()
	cfg.PCIeInterval = 10 * time.Second
	cfg.IdleInterval = time.Hour
	h := newPCIeHost(t, cfg)

	d := h.mustStep(time.Second)
	if d.Ready || d.PCIe == nil || !d.PCIe.Sampled || h.samples != 1 {
		t.Fatalf("warm-up poll: ready=%v pcie=%+v samples=%d, want the first passive sample on it", d.Ready, d.PCIe, h.samples)
	}
	for i := 0; i < 9; i++ {
		d = h.mustStep(time.Second)
		if d.PCIe == nil || d.PCIe.Sampled || len(d.PCIe.Errors) != 0 {
			t.Fatalf("poll %d inside the interval: %+v, want the state without a new sample", i, d.PCIe)
		}
	}
	if h.samples != 1 {
		t.Fatalf("%d samples within one interval, want 1", h.samples)
	}
	h.badTLP = 7
	d = h.mustStep(time.Second)
	if h.samples != 2 || !d.PCIe.Sampled || len(d.PCIe.Errors) != 1 || d.PCIe.Errors[0].Correctable != 7 {
		t.Fatalf("poll at the interval: samples=%d pcie=%+v, want 7 new errors", h.samples, d.PCIe)
	}

	// Two samples fall into polls that fail on the GPUs: each failed poll
	// hands its own sample on with the error, and nothing else.
	h.failGPU = true
	for _, tc := range []struct{ total, added uint64 }{{10, 3}, {15, 5}} {
		h.badTLP = tc.total
		d, err := h.step(10 * time.Second)
		if err == nil {
			t.Fatal("poll with an unreadable GPU succeeded")
		}
		if d.PCIe == nil || !d.PCIe.Sampled || len(d.PCIe.Errors) != 1 || d.PCIe.Errors[0].Correctable != tc.added {
			t.Fatalf("failed poll: passive reading %+v, want the %d errors of its sample", d.PCIe, tc.added)
		}
		if d.Ready || d.Run || d.Rates != nil || d.BusyRNICs != nil {
			t.Fatalf("failed poll: decision %+v, want nothing but the passive reading", d)
		}
	}
	h.failGPU = false
	if d = h.mustStep(time.Second); d.PCIe.Sampled || len(d.PCIe.Errors) != 0 {
		t.Fatalf("first poll after the failures: %+v, want nothing handed on twice", d.PCIe)
	}
}

// orderSampler records the order of the reads of one poll, and can fail the
// read of one RNIC.
type orderSampler struct {
	StaticSampler
	calls *[]string
	gone  string
}

func (s orderSampler) RNIC(ctx context.Context, n model.RNIC) (model.RNICCounters, error) {
	*s.calls = append(*s.calls, "rnic")
	if n.ID == s.gone {
		return model.RNICCounters{}, fmt.Errorf("no RNIC counters readable for %s", n.Device)
	}
	return s.StaticSampler.RNIC(ctx, n)
}

func (s orderSampler) PCIeLinks(ctx context.Context) []PCIeLinkSample {
	*s.calls = append(*s.calls, "pcie")
	return s.StaticSampler.PCIeLinks(ctx)
}

// The RNIC counters are stamped with the poll's time, so nothing that can
// take long may be read between that time and them: the passive reading,
// whose sysfs files can wake a suspended bridge, comes after every RNIC. A
// constant load would otherwise read too high on the polls that sample.
func TestPollReadsThePassiveSignalsAfterTheRNICCounters(t *testing.T) {
	cfg := config.Defaults()
	cfg.PCIeInterval = time.Second
	var calls []string
	s := orderSampler{StaticSampler: StaticSampler{PCIe: func(context.Context) []PCIeLinkSample { return nil }}, calls: &calls}
	m := New(cfg, topology.Fig2(), s)
	if _, err := m.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"rnic", "rnic", "rnic", "rnic", "pcie"}; !slices.Equal(calls, want) {
		t.Fatalf("reads in the order %v, want %v", calls, want)
	}
}

// An RNIC whose link went down leaves the bus, and every poll fails on its
// counters from then on. The passive reading of the same poll, which may hold
// the Surprise Down that explains it, is handed on with the error.
func TestPollHandsThePassiveReadingOnWhenAnRNICCannotBeRead(t *testing.T) {
	cfg := config.Defaults()
	cfg.PCIeInterval = 10 * time.Second
	port := zeroErrors()
	var calls []string
	s := orderSampler{calls: &calls, StaticSampler: StaticSampler{PCIe: func(context.Context) []PCIeLinkSample {
		return []PCIeLinkSample{{Link: "pcie.rnic1", Device: "rnic1", Ends: []PCIeEndSample{
			{BDF: scriptPort, Port: true, Errors: pci.LinkErrors{Correctable: cloneRows(port.Correctable), Uncorrectable: cloneRows(port.Uncorrectable)}},
			{BDF: scriptDev, Err: fmt.Errorf("%w: %s: gone", pci.ErrAERAbsent, scriptDev)},
		}}}
	}}}
	m := New(cfg, topology.Fig2(), s)
	m.Logf = func(string, ...any) {}
	clock := time.Unix(1_700_000_000, 0)
	m.SetClock(func() time.Time { return clock })
	if d, err := m.Poll(context.Background()); err != nil || d.PCIe == nil {
		t.Fatalf("first poll: %+v, %v", d, err)
	}

	// rnic1 leaves the bus; its port counts the Surprise Down.
	s.gone = "rnic1"
	m.Sampler = s
	port.Uncorrectable[pci.AERSDES] = 1
	var state model.PCIeLinkState
	var uncorrectable uint64
	for i := 0; i < 7; i++ {
		clock = clock.Add(10 * time.Second)
		d, err := m.Poll(context.Background())
		if err == nil {
			t.Fatalf("poll %d succeeded without rnic1's counters", i)
		}
		if d.PCIe == nil {
			t.Fatalf("poll %d: failed with %v and handed no passive reading on", i, err)
		}
		if d.Ready || d.Run || len(d.Rates) != 0 {
			t.Fatalf("poll %d: decision %+v, want nothing but the passive reading", i, d)
		}
		for _, e := range d.PCIe.Errors {
			uncorrectable += e.Uncorrectable
		}
		if len(d.PCIe.Findings) == 1 {
			state = d.PCIe.Findings[0].State
		}
	}
	if state != model.PCIeLinkUncorrectable || uncorrectable != 1 {
		t.Fatalf("state %q and %d uncorrectable errors handed on by polls that failed; want the uncorrectable finding and the 1 error, once", state, uncorrectable)
	}

	// A canceled poll is a shutdown: it hands nothing on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d, err := m.Poll(ctx); err == nil || d.PCIe != nil {
		t.Fatalf("canceled poll: %+v, %v", d, err)
	}
}

// With --pcie-probe-interval a link with a passive finding starts a busy-mode
// run of the RNICs whose probes cross it, on a loaded host, at most once per
// interval.
func TestPCIeLinkTriggerProbesTheAffectedRNICs(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	cfg.PCIeInterval = 10 * time.Second
	cfg.PCIeProbeInterval = 5 * time.Minute // cooldown stays at its 30 s default
	h := newPCIeHost(t, cfg, "rnic0", "rnic1")
	h.mustStep(time.Second) // warm-up

	for i := 0; i < 120; i++ {
		if d := h.mustStep(time.Second); d.Run {
			t.Fatalf("step %d: run %+v with no finding to answer", i, d)
		}
	}
	// gpu3 sits under rp.1 with rnic1: in busy mode only rnic1 probes it.
	h.need = []string{"pcie.gpu3"}
	var runs []int
	for i := 1; i <= 620; i++ {
		d := h.mustStep(time.Second)
		if !d.Run {
			if d.ProbeLinks != nil || d.ProbeRNICs != nil {
				t.Fatalf("step %d: probe set on a poll that starts no run: %+v", i, d)
			}
			continue
		}
		runs = append(runs, i)
		if d.Trigger != model.TriggerPCIeLink || d.Mode != model.ModeBusy {
			t.Fatalf("step %d decision %+v", i, d)
		}
		if !slices.Equal(d.ProbeLinks, []string{"pcie.gpu3"}) {
			t.Fatalf("step %d: probe links %v", i, d.ProbeLinks)
		}
		// rnic1 is loaded, so it is judged against its own siblings and
		// needs no reference RNIC.
		if len(d.ProbeRNICs) != 1 || !d.ProbeRNICs["rnic1"] {
			t.Fatalf("step %d: probe RNICs %v, want rnic1 only", i, d.ProbeRNICs)
		}
		h.m.MarkRun(h.clock)
	}
	// The first poll that sees the need, then one probe interval after the
	// end of each run.
	if !slices.Equal(runs, []int{1, 301, 601}) {
		t.Fatalf("pcie_link runs at steps %v, want [1 301 601]", runs)
	}
}

// A pcie_link run is the last resort: an idle host leaves the link to the
// idle run, the flag is opt-in, the cooldown holds it back, and a run of any
// other trigger counts as the link's probe.
func TestPCIeLinkTriggerConditions(t *testing.T) {
	base := config.Defaults()
	base.IdleInterval = time.Hour
	base.PCIeInterval = 10 * time.Second
	base.PCIeProbeInterval = time.Minute
	base.BusyCooldown = 0

	t.Run("off by default", func(t *testing.T) {
		cfg := base
		cfg.PCIeProbeInterval = 0
		h := newPCIeHost(t, cfg, "rnic1")
		h.need = []string{"pcie.gpu3"}
		for i := 0; i < 300; i++ {
			if d := h.mustStep(time.Second); d.Run {
				t.Fatalf("step %d: %+v without --pcie-probe-interval", i, d)
			}
		}
	})
	t.Run("idle host", func(t *testing.T) {
		h := newPCIeHost(t, base)
		h.need = []string{"pcie.gpu3"}
		h.mustStep(time.Second)
		d := h.mustStep(time.Second)
		if !d.Run || d.Trigger != model.TriggerIdle {
			t.Fatalf("first ready poll %+v, want the idle run", d)
		}
		h.m.MarkRun(h.clock)
		for i := 0; i < 300; i++ {
			if d := h.mustStep(time.Second); d.Run {
				t.Fatalf("step %d: %+v on an idle host, whose idle run sweeps every link", i, d)
			}
		}
	})
	t.Run("cooldown", func(t *testing.T) {
		cfg := base
		cfg.BusyCooldown = 90 * time.Second
		cfg.PCIeProbeInterval = time.Second
		h := newPCIeHost(t, cfg, "rnic1")
		h.need = []string{"pcie.gpu3"}
		h.mustStep(time.Second)
		var runs []int
		for i := 1; i <= 200; i++ {
			if d := h.mustStep(time.Second); d.Run {
				runs = append(runs, i)
				h.m.MarkRun(h.clock)
			}
		}
		if !slices.Equal(runs, []int{1, 91, 181}) {
			t.Fatalf("runs at steps %v, want one per cooldown: [1 91 181]", runs)
		}
	})
	t.Run("unanswerable link", func(t *testing.T) {
		h := newPCIeHost(t, base, "rnic1")
		// A link no path crosses has no RNIC to probe it.
		h.need = []string{"pcie.nosuch"}
		for i := 0; i < 100; i++ {
			if d := h.mustStep(time.Second); d.Run {
				t.Fatalf("step %d: %+v for a link no RNIC probes", i, d)
			}
		}
	})
	t.Run("another run is the probe", func(t *testing.T) {
		cfg := base
		cfg.BusyInterval = 40 * time.Second
		cfg.PCIeProbeInterval = time.Minute
		h := newPCIeHost(t, cfg, "rnic1")
		h.mustStep(time.Second)
		// The periodic busy run sweeps every RNIC every 40 s, so the link
		// is never a minute without a probe.
		h.need = []string{"pcie.gpu3"}
		first := h.mustStep(time.Second)
		if !first.Run || first.Trigger != model.TriggerPCIeLink {
			t.Fatalf("first poll with the need %+v, want the pcie_link run: no run has swept the host yet", first)
		}
		h.m.MarkRun(h.clock)
		for i := 0; i < 400; i++ {
			d := h.mustStep(time.Second)
			if !d.Run {
				continue
			}
			if d.Trigger != model.TriggerPeriodic || d.ProbeRNICs != nil {
				t.Fatalf("step %d: %+v, want only periodic runs of every RNIC", i, d)
			}
			h.m.MarkRun(h.clock)
		}
	})
	t.Run("leaves the busy interval counting", func(t *testing.T) {
		cfg := base
		cfg.BusyInterval = 100 * time.Second
		cfg.PCIeProbeInterval = 30 * time.Second
		h := newPCIeHost(t, cfg, "rnic1")
		h.mustStep(time.Second)
		h.need = []string{"pcie.gpu3"}
		var periodic []int
		for i := 1; i <= 250; i++ {
			d := h.mustStep(time.Second)
			if d.Run {
				if d.Trigger == model.TriggerPeriodic {
					periodic = append(periodic, i)
				}
				h.m.MarkRun(h.clock)
			}
		}
		// The interval starts at the first ready poll (step 1) and is not
		// pushed back by the pcie_link runs in between, which sweep one RNIC.
		if !slices.Equal(periodic, []int{101, 201}) {
			t.Fatalf("periodic runs at steps %v, want [101 201]", periodic)
		}
	})
}

// A pcie_link run sweeps the RNICs whose busy probes cross the links. When
// one of them is not loaded it is judged against the idle baseline, so up to
// two other unloaded RNICs are swept with it as the references of the
// bandwidth recheck.
func TestPCIeProbeSweep(t *testing.T) {
	if pcieProbeRefs != analyzer.BWRecheckMaxRefs {
		t.Fatalf("pcieProbeRefs = %d, the bandwidth recheck reads %d references", pcieProbeRefs, analyzer.BWRecheckMaxRefs)
	}
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.PCIeProbeInterval = time.Minute
	for _, tc := range []struct {
		name  string
		links []string
		busy  []string
		plan  []string
		want  []string
	}{
		{"loaded RNIC's own link", []string{"pcie.rnic1"}, []string{"rnic1"}, []string{"pcie.rnic1"}, []string{"rnic1"}},
		{"unloaded RNIC's own link takes two references", []string{"pcie.rnic1"}, []string{"rnic0"}, []string{"pcie.rnic1"}, []string{"rnic1", "rnic2", "rnic3"}},
		{"a GPU link is probed by its affinitive RNIC", []string{"pcie.gpu5"}, []string{"rnic2"}, []string{"pcie.gpu5"}, []string{"rnic2"}},
		{"references are unloaded RNICs only", []string{"pcie.gpu5"}, []string{"rnic0", "rnic1"}, []string{"pcie.gpu5"}, []string{"rnic2", "rnic3"}},
		{"no unloaded RNIC to refer to", []string{"pcie.gpu5"}, []string{"rnic0", "rnic1", "rnic3"}, []string{"pcie.gpu5"}, []string{"rnic2"}},
		{"a root port is crossed by its RNIC's memory paths", []string{"rp.3"}, []string{"rnic3"}, []string{"rp.3"}, []string{"rnic3"}},
		{"two links, one run", []string{"pcie.gpu0", "pcie.gpu7", "pcie.gpu0"}, []string{"rnic0", "rnic3"}, []string{"pcie.gpu0", "pcie.gpu7"}, []string{"rnic0", "rnic3"}},
		{"a link the graph does not have", []string{"pcie.gpu9", "pcie.rnic0"}, []string{"rnic0"}, []string{"pcie.rnic0"}, []string{"rnic0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(cfg, g, StaticSampler{})
			m.PCIeNeed = func() []string { return tc.links }
			plan := m.planPCIeProbe()
			if !slices.Equal(plan.links, tc.plan) {
				t.Fatalf("planned links %v, want %v", plan.links, tc.plan)
			}
			busy := map[string]bool{}
			for _, id := range tc.busy {
				busy[id] = true
			}
			var got []string
			for id := range plan.sweep(g, plan.links, busy) {
				got = append(got, id)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("sweep %v, want %v", got, tc.want)
			}
		})
	}
}

// aerFixture writes the AER statistics of the function at rel (a path under
// devices/) with badTLP Bad TLPs counted.
func aerFixture(t *testing.T, root, rel string, badTLP uint64) {
	t.Helper()
	dir := filepath.Join(root, "devices", rel)
	mustWriteFile(t, filepath.Join(dir, "aer_dev_correctable"),
		fmt.Sprintf("RxErr 0\nBadTLP %d\nBadDLLP 0\nRollover 0\nTimeout 0\nNonFatalErr 0\nCorrIntErr 0\nHeaderOF 0\nTOTAL_ERR_COR %d\n", badTLP, badTLP))
	const unc = "Undefined 0\nDLP 0\nSDES 0\nTLP 0\nFCP 0\nCmpltTO 0\nCmpltAbrt 0\nUnxCmplt 0\nRxOF 0\nMalfTLP 0\nECRC 0\nUnsupReq 0\nACSViol 0\nUncorrIntErr 0\nBlockedTLP 0\nAtomicOpBlocked 0\nTLPBlockedErr 0\nPoisonTLPBlocked 0\n"
	mustWriteFile(t, filepath.Join(dir, "aer_dev_nonfatal"), unc+"TOTAL_ERR_NONFATAL 0\n")
	mustWriteFile(t, filepath.Join(dir, "aer_dev_fatal"), unc+"TOTAL_ERR_FATAL 0\n")
}

// pciFixture creates the PCI function at rel (a path under devices/) with
// its bus/pci/devices link and, when speed is set, its link attributes.
func pciFixture(t *testing.T, root, rel, speed string, width int) {
	t.Helper()
	dir := filepath.Join(root, "devices", rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bus/pci/devices"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bus/pci/devices", filepath.Base(rel))
	if _, err := os.Lstat(link); err != nil {
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
	}
	if speed != "" {
		mustWriteFile(t, filepath.Join(dir, "current_link_speed"), speed+"\n")
		mustWriteFile(t, filepath.Join(dir, "max_link_speed"), speed+"\n")
		mustWriteFile(t, filepath.Join(dir, "current_link_width"), fmt.Sprintf("%d\n", width))
		mustWriteFile(t, filepath.Join(dir, "max_link_width"), "16\n")
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pcieTreeGraph is a graph over the fixture tree of TestPCIeLinkSpecs: rnic0
// and gpu0 behind two cascaded switches under one root port, rnic1 right
// under another root port with a second function, gpu1 behind a port without
// a readable ancestry.
func pcieTreeGraph() topology.Graph {
	link := func(id, kind, dev string) model.Link { return model.Link{ID: id, Kind: kind, Device: dev} }
	return topology.Graph{
		RNICs: []model.RNIC{
			{ID: "rnic0", PCIeBDF: "0000:3f:00.0"},
			{ID: "rnic1", PCIeBDF: "0000:5b:00.0"},
			{ID: "rnic2", PCIeBDF: ""},
		},
		Endpoints: []model.Endpoint{
			{ID: "gpu0", Kind: model.EndpointGPU, PCIeBDF: "0000:41:00.0"},
			{ID: "gpu1", Kind: model.EndpointGPU, PCIeBDF: "0000:7c:00.0"},
			{ID: "mem0", Kind: model.EndpointMemory},
		},
		Links: []model.Link{
			link("pcie.rnic0", model.LinkKindPCIeRNIC, "rnic0"),
			link("pcie.rnic1", model.LinkKindPCIeRNIC, "rnic1"),
			link("pcie.rnic2", model.LinkKindPCIeRNIC, "rnic2"),
			link("pcie.gpu0", model.LinkKindPCIeGPU, "gpu0"),
			link("pcie.gpu1", model.LinkKindPCIeGPU, "gpu1"),
			link("rp.0000:3a:00.0", model.LinkKindRootPort, ""),
			link("rp.0000:5a:00.0", model.LinkKindRootPort, ""),
			link("rp.0000:7a:00.0", model.LinkKindRootPort, ""),
			link("sw.0000:3b:00.0", model.LinkKindPCIeSwitch, ""),
			link("sw.0000:3d:00.0", model.LinkKindPCIeSwitch, ""),
		},
	}
}

// pcieTree writes the fixture tree and returns its root.
func pcieTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	const (
		rp   = "pci0000:3a/0000:3a:00.0"
		up   = rp + "/0000:3b:00.0"
		dn   = up + "/0000:3c:00.0"
		up2  = dn + "/0000:3d:00.0"
		dnA  = up2 + "/0000:3e:00.0"
		dnB  = up2 + "/0000:3e:01.0"
		rnic = dnA + "/0000:3f:00.0"
		gpu  = dnB + "/0000:41:00.0"
		rp1  = "pci0000:5a/0000:5a:00.0"
	)
	for _, port := range []string{rp, up, dn, up2, dnA, dnB, rp1} {
		pciFixture(t, root, port, gen5, 16)
		aerFixture(t, root, port, 0)
	}
	pciFixture(t, root, rnic, gen5, 16)
	aerFixture(t, root, rnic, 0)
	pciFixture(t, root, gpu, gen4, 16)
	aerFixture(t, root, gpu, 0)
	// rnic1: two functions of one card right under a root port; the second
	// function has no AER statistics.
	pciFixture(t, root, rp1+"/0000:5b:00.0", gen4, 8)
	aerFixture(t, root, rp1+"/0000:5b:00.0", 0)
	pciFixture(t, root, rp1+"/0000:5b:00.1", gen4, 8)
	// gpu1: a root port, one bridge and the GPU, which is no root port and
	// switch-port pairs; neither the bridge nor the GPU has AER statistics.
	pciFixture(t, root, "pci0000:7a/0000:7a:00.0", gen4, 16)
	pciFixture(t, root, "pci0000:7a/0000:7a:00.0/0000:7b:00.0", gen4, 16)
	pciFixture(t, root, "pci0000:7a/0000:7a:00.0/0000:7b:00.0/0000:7c:00.0", gen4, 16)
	return root
}

func TestPCIeLinkSpecs(t *testing.T) {
	root := pcieTree(t)
	specs, notes := PCIeLinkSpecs(root, pcieTreeGraph())

	var got []string
	for _, s := range specs {
		var ends []string
		for _, e := range s.Ends {
			end := e.BDF
			if e.Port {
				end += "(port)"
			}
			ends = append(ends, end)
		}
		got = append(got, fmt.Sprintf("%s dev=%q lower=%s ends=%s", s.Link, s.Device, s.Lower, strings.Join(ends, ",")))
	}
	want := []string{
		// rnic0's own link, then the links above it under the topology link
		// of their lower end: the root-port link and the second switch.
		`pcie.rnic0 dev="rnic0" lower=0000:3f:00.0 ends=0000:3e:00.0(port),0000:3f:00.0`,
		`rp.0000:3a:00.0 dev="" lower=0000:3b:00.0 ends=0000:3a:00.0(port),0000:3b:00.0`,
		`sw.0000:3d:00.0 dev="" lower=0000:3d:00.0 ends=0000:3c:00.0(port),0000:3d:00.0`,
		// rnic1 hangs off its root port: one link, the device's. Its second
		// function has no statistics and is no end.
		`pcie.rnic1 dev="rnic1" lower=0000:5b:00.0 ends=0000:5a:00.0(port),0000:5b:00.0`,
		// gpu0 shares rnic0's uplinks, which are listed once.
		`pcie.gpu0 dev="gpu0" lower=0000:41:00.0 ends=0000:3e:01.0(port),0000:41:00.0`,
		// gpu1 has no AER anywhere: still read for its negotiated link.
		`pcie.gpu1 dev="gpu1" lower=0000:7c:00.0 ends=`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("specs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, n := range []string{
		"pcie.rnic1: no AER statistics at 0000:5b:00.1 (",
		// One line per link, however many of its functions lack them.
		"pcie.gpu1: no AER statistics at 0000:7b:00.0, 0000:7c:00.0 (",
		"pcie.gpu1: the links above it are not read",
	} {
		if !slices.ContainsFunc(notes, func(s string) bool { return strings.HasPrefix(s, n) }) {
			t.Fatalf("notes %q lack %q", notes, n)
		}
	}
	if len(notes) != 3 {
		t.Fatalf("notes %q, want the three above and each once", notes)
	}
}

// The live sampler reads a link's negotiated width and its ends' counters
// from sysfs as they change, and an end whose files vanish as an error, not
// as zero.
func TestSysfsSamplerPCIeLinks(t *testing.T) {
	root := pcieTree(t)
	g := pcieTreeGraph()
	specs, _ := PCIeLinkSpecs(root, g)
	s := SysfsSampler{Graph: g, Sysroot: root, PCIe: specs}
	byLink := func() map[string]PCIeLinkSample {
		out := map[string]PCIeLinkSample{}
		for _, smp := range s.PCIeLinks(context.Background()) {
			out[smp.Link] = smp
		}
		return out
	}

	got := byLink()
	if len(got) != len(specs) {
		t.Fatalf("%d samples for %d links", len(got), len(specs))
	}
	r1 := got["pcie.rnic1"]
	if !r1.ConfigOK || r1.Config.Width != 8 || r1.Config.MaxWidth != 16 || r1.Config.PortMaxWidth != 16 || r1.Device != "rnic1" {
		t.Fatalf("pcie.rnic1 %+v, want an x8 link of an x16 device in an x16 port", r1)
	}
	if len(r1.Ends) != 2 || !r1.Ends[0].Port || r1.Ends[0].Err != nil || r1.Ends[1].Err != nil || r1.Ends[1].Errors.Correctable[pci.AERBadTLP] != 0 {
		t.Fatalf("pcie.rnic1 ends %+v", r1.Ends)
	}
	if up := got["rp.0000:3a:00.0"]; !up.ConfigOK || up.Device != "" || pci.GTs(up.Config.Speed) != 32 {
		t.Fatalf("root-port link %+v, want it read at its switch upstream port", up)
	}

	// The card's link errs, and the device vanishes from sysfs.
	aerFixture(t, root, "pci0000:5a/0000:5a:00.0", 41)
	if err := os.RemoveAll(filepath.Join(root, "devices/pci0000:5a/0000:5a:00.0/0000:5b:00.0")); err != nil {
		t.Fatal(err)
	}
	r1 = byLink()["pcie.rnic1"]
	if r1.ConfigOK {
		t.Fatalf("pcie.rnic1 config %+v read from a device that is gone", r1.Config)
	}
	if r1.Ends[0].Errors.Correctable[pci.AERBadTLP] != 41 {
		t.Fatalf("port end %+v, want its 41 Bad TLPs", r1.Ends[0])
	}
	if !errors.Is(r1.Ends[1].Err, pci.ErrAERAbsent) || r1.Ends[1].Errors.Correctable != nil {
		t.Fatalf("device end %+v, want an error and no counts", r1.Ends[1])
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.PCIeLinks(ctx); len(got) != 0 {
		t.Fatalf("%d samples read on a canceled context", len(got))
	}
}

// End to end over sysfs: a link that starts counting Bad TLPs over its budget
// becomes a suspect after a minute and a verdict after three, read by the
// live sampler from the files as they change.
func TestMonitorPCIeFromSysfs(t *testing.T) {
	root := pcieTree(t)
	g := pcieTreeGraph()
	specs, _ := PCIeLinkSpecs(root, g)
	cfg := config.Defaults()
	cfg.PCIeInterval = 20 * time.Second
	w := newPCIeWatch(cfg.PCIeBER, func(string, ...any) {})
	s := SysfsSampler{Graph: g, Sysroot: root, PCIe: specs}
	now := time.Unix(1_700_000_000, 0)
	const dev = "pci0000:3a/0000:3a:00.0/0000:3b:00.0/0000:3c:00.0/0000:3d:00.0/0000:3e:01.0/0000:41:00.0"
	var total uint64 = 12345 // counted before the daemon started
	aerFixture(t, root, dev, total)
	var last model.PCIePassive
	for i := 0; i <= 9; i++ {
		last = w.observe(now, s.PCIeLinks(context.Background()))
		now = now.Add(cfg.PCIeInterval)
		total += 20 // 60 a minute against Gen4 x16's 16
		aerFixture(t, root, dev, total)
		switch {
		case i < 3 && len(last.Findings) != 0:
			t.Fatalf("sample %d: %+v before the first window closed", i, last.Findings)
		case i >= 3 && i < 9 && (len(last.Findings) != 1 || last.Findings[0].State != model.PCIeLinkSuspect):
			t.Fatalf("sample %d: %+v, want pcie.gpu0 suspect", i, last.Findings)
		}
	}
	if len(last.Findings) != 1 {
		t.Fatalf("findings %+v", last.Findings)
	}
	f := last.Findings[0]
	if f.Link != "pcie.gpu0" || f.State != model.PCIeLinkSustained || f.ErrorsPerMinute != 60 || f.OverBudgetWindows != 3 {
		t.Fatalf("finding %+v, want pcie.gpu0 sustained at 60/min", f)
	}
	want := "60 link errors/min against a budget of 16/min in the last window; over the budget in 3 of the last 3 windows (1 time, at most 3 in a row), 60/min across them; 0000:41:00.0: BadTLP=60; last window 60s"
	if f.Evidence() != want {
		t.Fatalf("evidence %q, want %q", f.Evidence(), want)
	}
}

// The live sampler: an RNIC whose RDMA device and netdev are gone from sysfs
// (the card left the bus) makes every poll an error, and the passive reading
// of the PCI tree, which is still there, is handed on with it.
func TestSysfsPollHandsThePassiveReadingOnWithoutTheRNIC(t *testing.T) {
	root := pcieTree(t) // a PCI tree with AER files and no class/infiniband
	g := pcieTreeGraph()
	g.RNICs = []model.RNIC{{ID: "rnic1", Device: "mlx5_1", Netdev: "eth1", PCIeBDF: "0000:5b:00.0", LineRate: 200e9}}
	specs, _ := PCIeLinkSpecs(root, g)
	cfg := config.Defaults()
	cfg.PCIeInterval = time.Second
	m := New(cfg, g, SysfsSampler{Graph: topology.Graph{RNICs: g.RNICs}, Sysroot: root, PCIe: specs})
	m.Logf = func(string, ...any) {}
	clock := time.Unix(1_700_000_000, 0)
	m.SetClock(func() time.Time { return clock })
	for i := 0; i < 2; i++ {
		clock = clock.Add(time.Second)
		d, err := m.Poll(context.Background())
		if err == nil || !strings.Contains(err.Error(), "mlx5_1") {
			t.Fatalf("poll %d: err %v, want the unreadable RNIC", i, err)
		}
		if d.PCIe == nil || !d.PCIe.Sampled {
			t.Fatalf("poll %d: passive reading %+v, want the sample it took", i, d.PCIe)
		}
		// The second sample differences the first: every link with AER
		// counters has its series, at no error.
		if i == 1 && len(d.PCIe.Errors) == 0 {
			t.Fatalf("poll %d: no link differenced: %+v", i, d.PCIe)
		}
	}
}

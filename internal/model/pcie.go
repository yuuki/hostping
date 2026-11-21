package model

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// PCIeLinkState is what the passive AER counters of a PCIe link amount to
// (the hardware monitor's passive reading, internal/monitor/pcie.go). The
// zero value is no finding.
type PCIeLinkState string

const (
	// PCIeLinkSuspect: one of the link's last PCIeRecentWindows judged
	// windows counted more link errors than its bit-error budget allows, and
	// those windows do not make a verdict. It is a hint: it can start a
	// targeted probe (--pcie-probe-interval) and names no cause.
	PCIeLinkSuspect PCIeLinkState = "suspect"
	// PCIeLinkSustained: the link keeps counting more link errors than its
	// budget allows. PCIeSustainWindows judged windows in a row were over
	// it, or the link was over it that many separate times among the last
	// PCIeRecentWindows while the errors of all of those together are over
	// what the budget allows them. It is a verdict, pcie_link_errors.
	PCIeLinkSustained PCIeLinkState = "sustained"
	// PCIeLinkUncorrectable: an end of the link recorded an uncorrectable
	// link error (Data Link Protocol Error, Surprise Down) in one of the
	// last PCIeSustainWindows windows. It is a verdict, pcie_link_errors.
	PCIeLinkUncorrectable PCIeLinkState = "uncorrectable"
)

// Proven reports whether the state is a verdict on its own, as opposed to
// the hint PCIeLinkSuspect.
func (s PCIeLinkState) Proven() bool {
	return s == PCIeLinkSustained || s == PCIeLinkUncorrectable
}

// PCIeSustainWindows is how many judged windows in a row over its bit-error
// budget, or how many separate times over it, make a link's errors
// sustained: one burst, however large, touches two windows in a row at
// most. It is also how many windows an uncorrectable link error stands for.
const PCIeSustainWindows = 3

// PCIeRecentWindows is how many judged windows a link's errors are looked
// back over: a link that errs in bursts is over its budget in some of them
// only. A suspect or sustained finding stands until none of them was over
// the budget.
const PCIeRecentWindows = 30

// PCIeLinkFinding is the passive evidence the hardware monitor holds against
// one PCIe link: what the AER counters of its two ends counted in the last
// measured window and in the judged windows before it, against what the
// link's bit-error budget allows. No probe is behind it.
type PCIeLinkFinding struct {
	// Link is the topology link ID (pcie.<rnic>, pcie.<gpu>, rp.<bdf>,
	// sw.<bdf>).
	Link  string        `json:"link"`
	State PCIeLinkState `json:"state"`
	// ErrorsPerMinute is the correctable link errors per minute of the end
	// that counted the most in the last measured window. An end's count is
	// its largest row: one error can add to more than one.
	ErrorsPerMinute float64 `json:"errors_per_minute"`
	// BudgetPerMinute is the link errors per minute one direction of the
	// link may show at --pcie-ber, for the fastest speed and width it was
	// trained to in the window. It is absent when the rate has no budget:
	// the negotiated link could not be read, or it runs faster than
	// 32 GT/s.
	BudgetPerMinute float64 `json:"budget_per_minute,omitempty"`
	// RecentWindows is how many judged windows the finding looks back over,
	// the last one included: at most PCIeRecentWindows, fewer while the
	// link has not been judged for that long. OverBudgetWindows is how many
	// of them were over the budget, OverBudgetRuns how many separate times
	// that was (windows in a row count as one), OverBudgetLongestRun the
	// most windows in a row, and RecentErrorsPerMinute the errors per
	// minute across all of them. A rate with no budget is not judged: no
	// window is looked back over, the counts are 0 and the rate is absent,
	// as it is when the windows counted no error.
	RecentWindows         int     `json:"recent_windows"`
	OverBudgetWindows     int     `json:"over_budget_windows"`
	OverBudgetRuns        int     `json:"over_budget_runs"`
	OverBudgetLongestRun  int     `json:"over_budget_longest_run"`
	RecentErrorsPerMinute float64 `json:"recent_errors_per_minute,omitempty"`
	// WindowSeconds is the length of the last measured window.
	WindowSeconds float64 `json:"window_seconds"`
	// Ends are the functions at the two ends of the link whose counters
	// were measured over that window, the port above the link first.
	Ends []PCIeLinkEnd `json:"ends"`
}

// Evidence renders the finding's numbers for a cause or a log line: the rate
// of the last window against the budget, how many of the windows before it
// were over the budget, and what each end counted in the last window. A
// finding outlives the window that made it, so the evidence always says what
// it stands on.
func (f PCIeLinkFinding) Evidence() string {
	var b strings.Builder
	if f.BudgetPerMinute > 0 {
		budget := math.Ceil(f.BudgetPerMinute)
		fmt.Fprintf(&b, "%s link errors/min against a budget of %.0f/min in the last window", formatRate(f.ErrorsPerMinute, budget), budget)
		if f.OverBudgetWindows > 0 {
			if f.RecentWindows == 1 {
				b.WriteString("; over the budget in the one window judged so far")
			} else {
				fmt.Fprintf(&b, "; over the budget in %d of the last %d windows (%s, at most %d in a row), %s/min across them",
					f.OverBudgetWindows, f.RecentWindows, times(f.OverBudgetRuns), f.OverBudgetLongestRun, formatRate(f.RecentErrorsPerMinute, budget))
			}
		}
	} else {
		fmt.Fprintf(&b, "%.0f link errors/min with no budget to judge them against", f.ErrorsPerMinute)
	}
	if f.State == PCIeLinkUncorrectable && !f.countedUncorrectable() {
		fmt.Fprintf(&b, "; an uncorrectable link error was counted within the last %d windows", PCIeSustainWindows)
	}
	for _, e := range f.Ends {
		rows := formatRows(e.Correctable)
		if u := formatRows(e.Uncorrectable); u != "" {
			rows = strings.TrimSpace(rows + " uncorrectable " + u)
		}
		if rows == "" {
			continue
		}
		end := e.BDF
		if e.Port {
			end += " (port)"
		}
		fmt.Fprintf(&b, "; %s: %s", end, rows)
	}
	fmt.Fprintf(&b, "; last window %.0fs", f.WindowSeconds)
	return b.String()
}

// times spells a count of separate times.
func times(n int) string {
	if n == 1 {
		return "1 time"
	}
	return fmt.Sprintf("%d separate times", n)
}

// formatRate prints a rate next to the budget it was judged against so that
// the two read the way they compared: a rate over the budget never prints as
// the budget itself or less, and one within it never as more. It takes the
// fewest decimals that keep that.
func formatRate(rate, budget float64) string {
	for decimals := 0; decimals <= 6; decimals++ {
		s := strconv.FormatFloat(rate, 'f', decimals, 64)
		if v, err := strconv.ParseFloat(s, 64); err == nil && (v > budget) == (rate > budget) {
			return s
		}
	}
	// The shortest form that reads back as the same number.
	return strconv.FormatFloat(rate, 'f', -1, 64)
}

// countedUncorrectable reports whether an end counted an uncorrectable link
// error in the last window.
func (f PCIeLinkFinding) countedUncorrectable() bool {
	for _, e := range f.Ends {
		if len(e.Uncorrectable) > 0 {
			return true
		}
	}
	return false
}

// formatRows lists the rows of m as name=count, by name.
func formatRows(m map[string]uint64) string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		names[i] = fmt.Sprintf("%s=%d", name, m[name])
	}
	return strings.Join(names, " ")
}

// PCIeLinkEnd is the link errors one PCI function counted over a window, by
// kernel row name (pci.AERRxErr, ...). Rows that did not move are left out.
type PCIeLinkEnd struct {
	BDF string `json:"bdf"`
	// Port marks the port above the link (a root port or a switch
	// downstream port); the other end is the device, or the upstream port
	// of a switch.
	Port          bool              `json:"port,omitempty"`
	Correctable   map[string]uint64 `json:"correctable,omitempty"`
	Uncorrectable map[string]uint64 `json:"uncorrectable,omitempty"`
}

// PCIeLinkErrors is what one passive sample added to a link's error counts:
// the row deltas of every end that was differenced on that sample, summed.
// It feeds hostping.pcie.link.errors. One error can add to more than one
// row, so the sum is what the kernel counted, not a number of errors; the
// findings count each end by its largest row.
type PCIeLinkErrors struct {
	Link string
	// Correctable and Uncorrectable are the link errors counted since the
	// previous sample.
	Correctable   uint64
	Uncorrectable uint64
	// BudgetPerMinute is PCIeLinkFinding.BudgetPerMinute for the link as it
	// read on this sample; 0 when the rate has no budget.
	BudgetPerMinute float64
}

// PCIePassive is the hardware monitor's passive reading of the host's PCIe
// links (--pcie-interval): no probe is sent for any of it.
type PCIePassive struct {
	// Sampled is set on the poll that took a new sample. Errors is that
	// sample's; the other fields are the state after it and repeat on the
	// polls in between.
	Sampled bool
	// Errors lists, in link order, the links with an end whose counters were
	// differenced on this sample. A link whose counters could not be read,
	// or that is being re-baselined, is absent: unknown, never 0.
	Errors []PCIeLinkErrors
	// Findings are the links with a standing finding, in link order.
	Findings []PCIeLinkFinding
	// Configs is the negotiated and capable link of every RNIC and GPU, by
	// RNIC or endpoint ID, whose negotiated speed and width read the same on
	// the last two samples. A link read once, or caught while it retrains,
	// is absent.
	Configs map[string]PCIeConfig
}

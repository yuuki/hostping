package model

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// PCIeLinkState is what the passive AER counters of a PCIe link amount to
// (monitor.PCIeWatch). The zero value is no finding.
type PCIeLinkState string

const (
	// PCIeLinkSuspect: the link's last measured window counted more link
	// errors than its bit-error budget allows, but not yet for long enough
	// to be a verdict. It is a hint: it can start a targeted probe
	// (--pcie-probe-interval) and names no cause.
	PCIeLinkSuspect PCIeLinkState = "suspect"
	// PCIeLinkSustained: PCIeSustainWindows consecutive measured windows
	// were over the budget. It is a verdict, pcie_link_errors.
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

// PCIeSustainWindows is how many consecutive measured windows over its
// bit-error budget make a link's errors sustained, and how many consecutive
// windows within it end a finding again.
const PCIeSustainWindows = 3

// PCIeLinkFinding is the passive evidence the hardware monitor holds against
// one PCIe link: what the AER counters of its two ends counted in the last
// measured window, against what the link's bit-error budget allows. No probe
// is behind it.
type PCIeLinkFinding struct {
	// Link is the topology link ID (pcie.<rnic>, pcie.<gpu>, rp.<bdf>,
	// sw.<bdf>).
	Link  string        `json:"link"`
	State PCIeLinkState `json:"state"`
	// ErrorsPerMinute is the correctable link errors per minute of the end
	// that counted the most in the last measured window.
	ErrorsPerMinute float64 `json:"errors_per_minute"`
	// BudgetPerMinute is the link errors per minute one direction of the
	// link may show at --pcie-ber, for the speed and width it trained to;
	// 0 when the negotiated link could not be read.
	BudgetPerMinute float64 `json:"budget_per_minute"`
	// OverBudgetWindows is the number of consecutive measured windows over
	// the budget, the last one included.
	OverBudgetWindows int `json:"over_budget_windows"`
	// WindowSeconds is the length of the last measured window.
	WindowSeconds float64 `json:"window_seconds"`
	// Ends are the functions at the two ends of the link whose counters
	// were measured over that window, the port above the link first.
	Ends []PCIeLinkEnd `json:"ends"`
}

// Evidence renders the finding's numbers for a cause or a log line: the rate
// against the budget, how long it has been over it, and what each end
// counted in the last window.
func (f PCIeLinkFinding) Evidence() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%.0f link errors/min", f.ErrorsPerMinute)
	if f.BudgetPerMinute > 0 {
		fmt.Fprintf(&b, " against a budget of %.0f/min", math.Ceil(f.BudgetPerMinute))
	} else {
		b.WriteString(" with no budget to judge them against")
	}
	if f.OverBudgetWindows > 0 {
		fmt.Fprintf(&b, ", over it in %d consecutive windows", f.OverBudgetWindows)
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
// the deltas of every end that was differenced on that sample, summed. It
// feeds hostping.pcie.link.errors.
type PCIeLinkErrors struct {
	Link string
	// Correctable and Uncorrectable are the link errors counted since the
	// previous sample.
	Correctable   uint64
	Uncorrectable uint64
	// BudgetPerMinute is PCIeLinkFinding.BudgetPerMinute for the link as it
	// read on this sample; 0 when unknown.
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

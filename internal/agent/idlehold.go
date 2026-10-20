package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yuuki/hostping/internal/engine"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/telemetry"
)

// idleHold keeps the report of an idle-mode run until the monitor confirms
// the host was still idle after it.
//
// The idle verdict (every RNIC below Thp_low, no PFC or drops, every GPU idle)
// is taken on the poll that starts the run, and a full-mesh run takes tens of
// seconds. A workload that restarts inside that window — a collective that
// pauses between iterations — loads the paths probed after it, and those are
// then judged against idle baselines: paths measured at a fraction of their
// idle bandwidth come out abnormal on a healthy host. So an idle-mode report
// is held across the re-baseline poll that MarkRun causes (its delta holds
// the run's own probe traffic) and resolved on the next Ready poll, the first
// clean interval after the run: emitted if that poll finds the host idle,
// discarded if it finds load or cannot classify the host. A discarded run
// leaves no trace in the engine: its gray and slow-registration streaks are
// rolled back, so a series of polluted runs cannot add up to a flapping or
// gpu_mr_registration_slow diagnosis. The next
// idle attempt follows the usual --idle-interval; retrying sooner would put a
// full mesh into every pause of a workload that pauses between iterations,
// and each of those would be discarded as well.
//
// Limitation: load that returns and ends entirely within one run is not
// caught. A workload that comes back keeps running past the run, which is the
// case this catches.
//
// Busy-mode runs (PFC/drops, periodic, a mutual alarm on a loaded host) are
// not held: they already expect load. A mutual-alarm run on an idle host is
// held like the idle trigger's, since it is the same idle-mode judgement
// against idle baselines.
type idleHold struct {
	rep     model.Report
	streaks engine.Streaks
	held    bool
}

// holdOutcome is what resolve did with the held report.
type holdOutcome int

const (
	holdNone      holdOutcome = iota // nothing was held
	holdWaiting                      // still waiting for a clean poll
	holdConfirmed                    // the host stayed idle: emit the report
	holdDiscarded                    // the host was busy or unclassified: drop it
)

// needsIdleConfirm reports whether a run in mode must be confirmed idle after
// it before its report is used.
func needsIdleConfirm(mode model.RunMode) bool { return mode == model.ModeIdle }

// hold stores the report of an idle-mode run that just ended (after MarkRun)
// together with the engine's streaks from before the run
// (Engine.SnapshotStreaks), which a discard restores.
func (h *idleHold) hold(rep model.Report, before engine.Streaks) {
	h.rep, h.streaks, h.held = rep, before, true
}

// resolve is fed every poll decision, before the decision's own run starts.
// It returns the held report and holdConfirmed on the first Ready poll whose
// host is idle. On a Ready poll whose host is busy or unclassified it drops
// the report, rolls the engine's streaks back to before the run, and
// returns holdDiscarded with the reason. No other run can start between hold
// and resolve, so the snapshot is still the engine's state before this run.
func (h *idleHold) resolve(eng *engine.Engine, d monitor.Decision) (model.Report, holdOutcome, string) {
	if !h.held {
		return model.Report{}, holdNone, ""
	}
	if !d.Ready {
		// The re-baseline poll right after the run (or a warmup after a
		// failed poll): its delta is not a clean interval.
		return model.Report{}, holdWaiting, ""
	}
	rep, before := h.rep, h.streaks
	h.rep, h.streaks, h.held = model.Report{}, engine.Streaks{}, false
	ok, why := idleVerdict(d)
	if ok {
		why = cpuLoadRose(rep.CPULoad, d)
		ok = why == ""
	}
	if !ok {
		if eng != nil {
			eng.RestoreStreaks(before)
		}
		return rep, holdDiscarded, why
	}
	return rep, holdConfirmed, ""
}

// discardReason is the hostping.engine.run.discarded reason for a Ready poll
// that idleVerdict refused.
func discardReason(d monitor.Decision) string {
	if !d.IdleKnown {
		return telemetry.DiscardIdleUnknown
	}
	return telemetry.DiscardHostBusy
}

// unknownIdleKind is why a Ready poll could not tell idle from busy
// (Decision.IdleKnown false).
type unknownIdleKind int

const (
	// countersUnread: a PFC pause or drop counter could not be read on the
	// RNICs the detail names (comma-separated).
	countersUnread unknownIdleKind = iota
	// cpuUnread: the CPU load could not be read.
	cpuUnread
	// gpuOrRateUnread: GPU utilization or an RNIC line rate is unknown.
	gpuOrRateUnread
)

// unknownIdleCause classifies why d could not classify the host, and names
// the RNICs for countersUnread. Each caller words its own message.
func unknownIdleCause(d monitor.Decision) (unknownIdleKind, string) {
	if len(d.UnreadCounters) > 0 {
		return countersUnread, strings.Join(d.UnreadCounters, ",")
	}
	if d.CPUUnknown {
		return cpuUnread, ""
	}
	return gpuOrRateUnread, ""
}

// idleVerdict reports whether a Ready poll still finds the host idle, and if
// not, briefly what was busy.
func idleVerdict(d monitor.Decision) (bool, string) {
	if !d.IdleKnown {
		switch kind, rnics := unknownIdleCause(d); kind {
		case countersUnread:
			return false, "idle state could not be read (PFC pause or drop counters unread on " + rnics + ")"
		case cpuUnread:
			return false, "idle state could not be read (CPU load unknown)"
		default:
			return false, "idle state could not be read (GPU utilization or RNIC line rate unknown)"
		}
	}
	if d.HostIdle {
		return true, ""
	}
	var parts []string
	if len(d.BusyRNICs) > 0 {
		ids := make([]string, 0, len(d.BusyRNICs))
		for id := range d.BusyRNICs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		parts = append(parts, fmt.Sprintf("busy RNICs (>= Thp_low, PFC or drops): %s", strings.Join(ids, ",")))
	}
	if len(d.BusyGPUs) > 0 {
		parts = append(parts, fmt.Sprintf("busy GPUs: %s", strings.Join(d.BusyGPUs, ",")))
	}
	if len(parts) == 0 {
		return false, "host not idle"
	}
	return false, strings.Join(parts, "; ")
}

// cpuLoadRose names the CPU packages that d reads loaded (a NUMA node at or
// above --cpu-busy-high) but that were not loaded in before, the CPU load the
// run started with; "" when there are none.
//
// The run judged the paths through a package loaded at its start as cpu_busy,
// so load that persists there changes nothing. Load that appeared on another
// package during the run reached paths that were judged against idle
// baselines, the race idleHold exists for, so the run is discarded like one
// whose RNICs or GPUs became busy.
func cpuLoadRose(before []model.CPULoad, d monitor.Decision) string {
	was := model.CPUBusySockets(before)
	var parts []string
	seen := map[int]bool{}
	for _, l := range d.CPULoad {
		if !l.High || was[l.Socket] || seen[l.Socket] {
			continue
		}
		seen[l.Socket] = true
		parts = append(parts, fmt.Sprintf("socket %d (NUMA node %d at %.0f%%)", l.Socket, l.NUMA, l.Busy*100))
	}
	if len(parts) == 0 {
		return ""
	}
	return "CPU load rose on " + strings.Join(parts, ", ")
}

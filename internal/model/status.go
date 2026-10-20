package model

import "fmt"

// enumName is the String() of an int enum whose constants run from 0 in
// table order: names[i], or sentinel when i falls outside the table, a
// negative value included.
func enumName(names []string, i int, sentinel string) string {
	if i < 0 || i >= len(names) {
		return sentinel
	}
	return names[i]
}

// marshalName encodes an enum as its String() in JSON quotes.
func marshalName(s fmt.Stringer) ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// LinkStatus is the binary-tomography state of an intra-host link (NSDI'23 Algorithm 1).
type LinkStatus int

const (
	LinkUncertain LinkStatus = iota
	LinkNormal
	LinkAbnormal
	LinkGray
)

var linkStatusNames = [...]string{
	LinkUncertain: "uncertain",
	LinkNormal:    "normal",
	LinkAbnormal:  "abnormal",
	LinkGray:      "gray",
}

func (s LinkStatus) String() string { return enumName(linkStatusNames[:], int(s), "unknown") }

func (s LinkStatus) MarshalJSON() ([]byte, error) { return marshalName(s) }

// PathStatus is the measured status of an RNIC→endpoint path.
type PathStatus int

const (
	PathUncertain PathStatus = iota
	PathNormal
	PathAbnormal
)

var pathStatusNames = [...]string{
	PathUncertain: "uncertain",
	PathNormal:    "normal",
	PathAbnormal:  "abnormal",
}

func (s PathStatus) String() string { return enumName(pathStatusNames[:], int(s), "unknown") }

func (s PathStatus) MarshalJSON() ([]byte, error) { return marshalName(s) }

// PathUncertainReason is why a path ended up PathUncertain.
//
// "uncertain" alone conflates two very different answers: a path hostping
// declined to judge because the evidence it had could not prove health, and a
// path it could not measure against anything at all. Every GPU is affinitive to
// exactly one RNIC, so on a multi-GPU host most of the RNIC→GPU mesh sits in the
// first group on every healthy run; without a reason an operator cannot tell
// that steady state from a real gap.
type PathUncertainReason int

const (
	// UncertainNone is a decided path (normal or abnormal).
	UncertainNone PathUncertainReason = iota
	// UncertainNoPath: the topology graph has no RNIC→endpoint route for this
	// measurement, so there is no distance, baseline or hop list to judge it by.
	UncertainNoPath
	// UncertainRNICBusy: the RNIC was carrying traffic, so the idle baseline
	// does not apply. The path either sits outside the reduced busy probe set,
	// or matched its affinitive siblings — which rules out a fault that only
	// this path sees, but cannot show the RNIC-wide load is not hiding one.
	UncertainRNICBusy
	// UncertainNonAffinitive: an idle-mode GPU that is not under the RNIC's
	// root port and cleared its reduced ❸/❹ baseline. That baseline is a floor
	// for a cross-root/cross-socket hop, not evidence of a clean path (NSDI'23
	// Fig.8 gray), so the path is deliberately left unjudged.
	UncertainNonAffinitive
	// UncertainBelowCeiling: an idle-mode affinitive GPU that cleared its
	// distance baseline but not min(line rate, narrowest hop) × (1-AbnormalTh).
	// Nothing is wrong enough to call abnormal, yet a baseline — which an
	// operator may have set loose — is not the evidence of health the ceiling
	// is, so the path is left unjudged. Unlike UncertainNonAffinitive this is
	// not the steady state of a healthy host: an affinitive path reaches its
	// ceiling when nothing is broken, so a path that keeps landing here is
	// running short of what its hardware carries.
	UncertainBelowCeiling
	// UncertainNoCeiling: an idle-mode affinitive GPU that cleared its distance
	// baseline on an RNIC whose line rate is unknown (0), so there is no ceiling
	// to hold it to. The path may well be healthy; hostping cannot tell, and the
	// fix is in discovery rather than on the path.
	UncertainNoCeiling
	// UncertainHostShift: an idle-judged path the sweep read below its
	// baseline whose paired bandwidth re-read (BandwidthRecheck) read level
	// with paths of the same distance class on other RNICs, yet still below
	// the baseline. The host as a whole read lower than when the baseline was
	// measured, so the path cannot be judged against it.
	UncertainHostShift
	// UncertainCPUBusy: a path the sweep read abnormal that transits a CPU
	// package whose CPUs were busy above --cpu-busy-high on the poll that
	// started the run (CPULoad). CPU load on a package contends for its
	// on-die fabric, which every path through the package crosses (RNIC to
	// memory, local or remote, and RNIC to GPU across the package), and no
	// RNIC counter or bus utilization shows it. A drop on such a path is not
	// evidence of a hardware fault, so it cannot feed Algorithm 1 or a cause.
	UncertainCPUBusy
)

var pathUncertainReasonNames = [...]string{
	UncertainNone:          "none",
	UncertainNoPath:        "no_path",
	UncertainRNICBusy:      "rnic_busy",
	UncertainNonAffinitive: "non_affinitive",
	UncertainBelowCeiling:  "below_ceiling",
	UncertainNoCeiling:     "no_ceiling",
	UncertainHostShift:     "host_shift",
	UncertainCPUBusy:       "cpu_busy",
}

func (r PathUncertainReason) String() string {
	return enumName(pathUncertainReasonNames[:], int(r), "unknown")
}

func (r PathUncertainReason) MarshalJSON() ([]byte, error) { return marshalName(r) }

// LatencyStatus is a path's small-message latency verdict, kept apart from the
// bandwidth-derived PathStatus that Algorithm 1 reads.
//
// NSDI'23 §3.2 names latency increase as a symptom of its own, and §5 applies
// Abnormal_th to "latency or bandwidth". hostping judges the two separately
// because only bandwidth has a default baseline that scales to the hardware:
// the Fig.4 latencies were measured on one server and do not carry over to
// another, so a path is judged on latency only against a latency baseline the
// operator supplied, or against its siblings. A verdict that weak must not let
// the bandwidth tomography paint hops abnormal, so it stays advisory.
type LatencyStatus int

const (
	LatencyUnknown LatencyStatus = iota
	LatencyNormal
	LatencyAbnormal
)

// latencyStatusNames spells LatencyUnknown as "unknown", so the sentinel for
// a value outside the table is "invalid": telemetry enumerates the enum until
// the sentinel appears, and "unknown" would stop it at the first constant.
var latencyStatusNames = [...]string{
	LatencyUnknown:  "unknown",
	LatencyNormal:   "normal",
	LatencyAbnormal: "abnormal",
}

func (s LatencyStatus) String() string { return enumName(latencyStatusNames[:], int(s), "invalid") }

func (s LatencyStatus) MarshalJSON() ([]byte, error) { return marshalName(s) }

// LatencyReason says what a latency verdict was judged against or, for
// LatencyUnknown, why the latency could not be judged.
type LatencyReason int

const (
	// LatencyReasonNone is a path that was never classified.
	LatencyReasonNone LatencyReason = iota
	// LatencyByBaseline: judged against the latency the operator set for the
	// path's distance (--baseline KEY=BW,LAT or --baseline-file).
	LatencyByBaseline
	// LatencyBySiblings: no latency baseline was set, so judged against the
	// median of the host's other paths at the same distance.
	LatencyBySiblings
	// LatencyBusy: a busy-mode run. Service traffic inflates a loaded RNIC's
	// posting overhead (Tproc, Eq.1), so its latency says nothing about the path.
	LatencyBusy
	// LatencyFewSiblings: no latency baseline, and too few other paths at the
	// same distance to take a median from.
	LatencyFewSiblings
	// LatencyNoPath: no RNIC→endpoint route in the topology graph, so no distance.
	LatencyNoPath
	// LatencyNoReading: the probe reported no small-message latency.
	LatencyNoReading
	// LatencyRecheckCleared: the sweep read the path abnormal against its
	// siblings, but a paired re-measurement interleaved with siblings of its
	// class read it within --abnormal-th and the floor of them, so the first
	// reading was the time it was taken in, not the path. LatencyRecheck
	// carries the re-reads.
	LatencyRecheckCleared
	// LatencyLocalityShift: the path is still abnormal against its siblings
	// after the recheck, but it reads within --abnormal-th and the floor of
	// the same-class paths of the other RNICs on its RNIC's NUMA node, so the
	// rise is the node's platform state, not the path. LatencyLocality carries
	// that reference.
	LatencyLocalityShift
	// LatencyCPUBusy: the path read abnormal, but it transits a CPU package
	// whose CPUs were busy above --cpu-busy-high when the run started
	// (UncertainCPUBusy), so the rise says nothing about the path.
	LatencyCPUBusy
)

var latencyReasonNames = [...]string{
	LatencyReasonNone:     "none",
	LatencyByBaseline:     "user_baseline",
	LatencyBySiblings:     "siblings",
	LatencyBusy:           "busy",
	LatencyFewSiblings:    "few_siblings",
	LatencyNoPath:         "no_path",
	LatencyNoReading:      "no_reading",
	LatencyRecheckCleared: "recheck_cleared",
	LatencyLocalityShift:  "locality_shift",
	LatencyCPUBusy:        "cpu_busy",
}

func (r LatencyReason) String() string { return enumName(latencyReasonNames[:], int(r), "unknown") }

func (r LatencyReason) MarshalJSON() ([]byte, error) { return marshalName(r) }

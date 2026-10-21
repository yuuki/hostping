package model

import "time"

// RNICCounters is one poll of throughput / PFC / drop counters.
type RNICCounters struct {
	Device  string
	Netdev  string
	RxBytes uint64
	TxBytes uint64
	// Drops is every drop counter read, by lower-cased counter name, with
	// its cumulative value. A name read from two sources is summed.
	Drops             map[string]uint64
	TxPauseDurationUs uint64
	RxPauseDurationUs uint64
	// PauseSources and DropSources are the sampler's bitmask of the sources
	// the pause-duration and drop counters were read from on this poll. 0
	// means none was readable, so the value is not 0 but unread. The monitor
	// only compares them: a set that shrinks is a counter that can no longer
	// be read, and one that changes cannot be differenced against the last.
	PauseSources uint8
	DropSources  uint8
	// PauseGap and DropGap say why a family has no source, for the log.
	PauseGap  string
	DropGap   string
	Timestamp time.Time
}

// RNICRates is derived from consecutive counter snapshots.
type RNICRates struct {
	Device             string
	ThroughputBPS      uint64
	PauseDurationRatio float64
	DropDelta          uint64
	// PauseKnown and DropKnown are set when PauseDurationRatio and DropDelta
	// were measured on this poll. Otherwise the value is 0 but means nothing:
	// the counter has no source, could not be read, or is being re-baselined.
	PauseKnown bool
	DropKnown  bool
	Idle       bool
}

// RNICDrops is one RNIC's drop-counter deltas over a monitor poll, by
// counter name, split by what they say about the RNIC. Counters that did not
// move are left out, so both maps are nil on a poll without drops.
type RNICDrops struct {
	// Fault holds the deltas that count against the RNIC: they start a
	// busy-mode run and are reported as rnic_packet_drops.
	Fault map[string]uint64
	// Lossy holds the per-priority deltas on priorities PFC does not
	// protect (--lossless-priorities). A lossy class drops under load by
	// design, so they start no run and are reported apart, at low
	// confidence.
	Lossy map[string]uint64
}

// FaultTotal is the sum of the Fault deltas.
func (d RNICDrops) FaultTotal() uint64 { return sumCounts(d.Fault) }

// LossyTotal is the sum of the Lossy deltas.
func (d RNICDrops) LossyTotal() uint64 { return sumCounts(d.Lossy) }

func sumCounts(m map[string]uint64) uint64 {
	var n uint64
	for _, v := range m {
		n += v
	}
	return n
}

// GPUStatus is idle vs active for the hardware monitor.
type GPUStatus struct {
	ID      string
	Idle    bool
	Util    float64
	Unknown bool // true when utilization could not be read; unknown ≠ idle
	// ComputeMode is the GPU's compute mode (ComputeModeDefault, ...), or ""
	// when it could not be read or the GPU has no such setting.
	ComputeMode string
}

// PCIeConfig captures ACS/ATS and negotiated link speed used in root-cause rules.
// Speed/Width are the negotiated (current) values; MaxSpeed/MaxWidth are what
// the device advertises, and PortMaxSpeed/PortMaxWidth what the upstream port
// it hangs off advertises. The lower of the two maxima is what the link can
// train to (the port stands in for the slot), so a down-train is visible even
// when the negotiated link still carries the line rate. Max* and PortMax* are
// empty / 0 when unknown.
type PCIeConfig struct {
	BDF          string
	Speed        string
	Width        int
	MaxSpeed     string
	MaxWidth     int
	PortMaxSpeed string
	PortMaxWidth int
	ACSEnabled   bool
	ATSEnabled   bool
	ACSKnown     bool
	ATSKnown     bool
	NUMA         int
}

// EndpointSkip is one group of loopback probes the engine skipped because the
// endpoint could not be registered (ErrNoGDR, ErrNoDevice, ErrNoGID, ErrNoLID,
// ErrPortNotActive, ErrProbe)
// or, for a GPU, was not probed because of its compute mode (ErrComputeMode).
// Reason is the probe's own error text, which carries the detail string that
// names the cause ("NoGDR: no cuda"); it is the only place that reason is
// stated, so the report keeps it verbatim.
type EndpointSkip struct {
	Kind EndpointKind `json:"kind"`
	// Reason is unbounded probe text: fine in JSON and logs, not as a metric attribute.
	Reason    string   `json:"reason"`
	Endpoints []string `json:"endpoints"`
	// Probes counts skipped RNIC→endpoint probes, which is larger than
	// len(Endpoints) whenever several RNICs reach the same endpoint.
	Probes int `json:"probes"`
}

// CPULoad is the busy fraction of one NUMA node's CPUs over a monitor poll
// interval, from /proc/stat.
type CPULoad struct {
	NUMA   int `json:"numa"`
	Socket int `json:"socket"`
	// Busy is the fraction of the node's CPU time not spent idle or in
	// iowait, 0–1.
	Busy float64 `json:"busy"`
	// High is set when Busy reached --cpu-busy-high. Every path that
	// transits Socket is then judged cpu_busy rather than abnormal.
	High bool `json:"high,omitempty"`
}

// CPUBusySockets is the set of CPU packages with a NUMA node whose load is
// High; nil when there is none.
func CPUBusySockets(loads []CPULoad) map[int]bool {
	var out map[int]bool
	for _, l := range loads {
		if !l.High {
			continue
		}
		if out == nil {
			out = map[int]bool{}
		}
		out[l.Socket] = true
	}
	return out
}

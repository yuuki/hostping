package model

import "time"

// Trigger is why the Hostping engine ran (hardware monitor, §4.2).
type Trigger string

const (
	TriggerIdle     Trigger = "idle"
	TriggerPFC      Trigger = "pfc"
	TriggerDrops    Trigger = "packet_drops"
	TriggerManual   Trigger = "manual"
	TriggerPeriodic Trigger = "periodic"
	// TriggerMutualTimeout is the scheduled mutual probe's window showing a
	// pair above Timeout_th (ToN'24 §IV-A-2).
	TriggerMutualTimeout Trigger = "mutual_timeout"
)

// RunMode selects full-mesh (idle) vs affinitive-only (busy) probing.
type RunMode string

const (
	ModeIdle RunMode = "idle"
	ModeBusy RunMode = "busy"
)

// Report is one Hostping engine+analyzer pass, uploaded as OTel attributes and JSON.
type Report struct {
	Trigger   Trigger           `json:"trigger"`
	Mode      RunMode           `json:"mode"`
	Host      string            `json:"host"`
	Started   time.Time         `json:"started"`
	Elapsed   time.Duration     `json:"-"`
	ElapsedNs int64             `json:"elapsed_ns"`
	Paths     []PathMeasurement `json:"paths"`
	// SkippedEndpoints names the endpoints missing from Paths, so a report with
	// only memory rows says why the GPU half was never measured.
	SkippedEndpoints []EndpointSkip      `json:"skipped_endpoints,omitempty"`
	Links            []LinkState         `json:"links"`
	Causes           []RootCause         `json:"causes"`
	Mutual           []MutualMeasurement `json:"mutual,omitempty"`
	HostIdle         bool                `json:"host_idle"`
	AbnormalRNIC     []string            `json:"abnormal_rnics,omitempty"`
	Baselines        []BaselineTable     `json:"baselines,omitempty"`
	// CPULoad is each NUMA node's CPU busy fraction on the monitor poll that
	// started the run, in NUMA order; empty when the run had no CPU reading.
	CPULoad []CPULoad `json:"cpu_load,omitempty"`
}

// SkippedEndpointCount counts the distinct endpoints this run skipped. An
// empty kind counts every kind, and an endpoint skipped for two reasons (one
// per RNIC) counts once.
func (r Report) SkippedEndpointCount(kind EndpointKind) int {
	seen := map[string]struct{}{}
	for _, s := range r.SkippedEndpoints {
		if kind != "" && s.Kind != kind {
			continue
		}
		for _, ep := range s.Endpoints {
			seen[ep] = struct{}{}
		}
	}
	return len(seen)
}

const (
	BaselineSourceDefault = "default"
	BaselineSourceUser    = "user"
)

// BaselineTable is the effective idle baseline for one RNIC line rate and the
// PCIe hop feeding it.
type BaselineTable struct {
	LineRateBPS uint64 `json:"line_rate_bps"`
	// PCIeCapBPS is the RNIC PCIe hop capacity the table was derived with;
	// 0 when unknown.
	PCIeCapBPS uint64 `json:"pcie_capacity_bps,omitempty"`
	// DerivedFromBPS is the rate the Fig.4 ratios were scaled from:
	// min(LineRateBPS, PCIeCapBPS, the Fig.4 Gen4 x16 bus). It is below
	// LineRateBPS when the wire outruns the bus feeding it, which is why a 400G
	// RNIC is not judged against 0.985×400G.
	DerivedFromBPS uint64 `json:"derived_from_bps"`
	// Section is the key of the baseline-file "tables" section the
	// operator's values came from (see baseline.TableKey); "" when they came
	// from the file's top level, or nothing was set.
	Section string          `json:"section,omitempty"`
	Entries []BaselineEntry `json:"entries"`
}

// BaselineEntry is one distance row, noting whether BW / latency came from the user.
type BaselineEntry struct {
	Distance        string `json:"distance"`
	BandwidthBPS    uint64 `json:"bandwidth_bps"`
	LatencyNs       uint64 `json:"latency_ns"`
	BandwidthSource string `json:"bandwidth_source"`
	LatencySource   string `json:"latency_source"`
	// SubClasses are the distance's learned sub-classes from the baseline
	// file, with effective values: a value the sub-class leaves unset is the
	// distance's own.
	SubClasses []BaselineSubClass `json:"sub_classes,omitempty"`
}

// BaselineSubClass is one learned sub-class of a distance: paths across one
// of its root-port pairs are judged against its values instead of the
// distance's.
type BaselineSubClass struct {
	Name          string         `json:"name"`
	BandwidthBPS  uint64         `json:"bandwidth_bps"`
	LatencyNs     uint64         `json:"latency_ns"`
	RootPortPairs []RootPortPair `json:"root_port_pairs"`
}

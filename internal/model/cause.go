package model

// RootCause is a diagnosed intra-host / RNIC problem.
type RootCause struct {
	Code       string     `json:"code"`
	Summary    string     `json:"summary"`
	Links      []string   `json:"links,omitempty"`
	RNICs      []string   `json:"rnics,omitempty"`
	Endpoints  []string   `json:"endpoints,omitempty"`
	Confidence Confidence `json:"confidence"`
	Evidence   string     `json:"evidence"`
}

// Confidence is how far a RootCause's evidence supports it. It is a string
// so the JSON report prints it as is.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// RNIC hints the --sim world passes to the analyzer (RNICHints), naming a
// fault the live host cannot tell apart from an RNIC PCIe failure.
const (
	HintSlowStart = "slow_start"
	HintTxWindow  = "tx_window"
)

// Known bottleneck codes from NSDI'23 §6 and ToN'24 RNIC mutual probing.
const (
	CauseRNICPCIeFailure       = "rnic_pcie_failure"
	CauseGPUPCIeFailure        = "gpu_pcie_failure"
	CauseMemoryChannelFailure  = "memory_channel_failure"
	CauseUPIFailure            = "upi_failure"
	CauseCPURootPortFailure    = "cpu_root_port_failure"
	CauseMemoryChannelFlapping = "memory_channel_flapping"
	CauseACSEnabled            = "acs_enabled"
	CauseATSDisabled           = "ats_disabled"
	CauseRNICSlowStart         = "rnic_slow_start"
	CauseRNICTxWindow          = "rnic_tx_window"
	CauseOverloadedUPI         = "overloaded_intersocket_bus"
	CauseOverloadedMemory      = "overloaded_memory_channel"
	CauseRNICConnectFailure    = "rnic_connectivity_failure"
	CauseRNICIntermittent      = "rnic_intermittent_timeout"
	CauseToRSwitchAnomaly      = "tor_switch_anomaly"
	CauseRNICPacketDrops       = "rnic_packet_drops"
	CauseOverloadedRNICPCIe    = "overloaded_rnic_pcie"
	// CauseGPUMRRegistrationSlow is advisory (ToN'24 §VI-C): a GPU whose
	// memory registration takes far longer than its siblings'. It never
	// changes a path or link status.
	CauseGPUMRRegistrationSlow = "gpu_mr_registration_slow"
	// CausePathLatencyAbnormal is advisory (NSDI'23 §3.2, §4.1.1): paths whose
	// small-message latency rose past Abnormal_th while their bandwidth did
	// not degrade. It never changes a path or link status.
	CausePathLatencyAbnormal = "path_latency_abnormal"
	// CauseRNICPCIeUndersized is advisory: an RNIC whose PCIe link trained at
	// its maximum, but whose maximum cannot carry the line rate (a slot that
	// is too narrow or slow by design). It is not a fault and never changes a
	// path or link status.
	CauseRNICPCIeUndersized = "rnic_pcie_undersized"
	CauseUnknown            = "unknown"
)

// CauseRNICCrossRailUnreachable is reported from cross-rail mutual pairs only
// (--mutual-scope=host): RNICs of the host that cannot reach each other across
// subnets. Whether they should is a property of the fabric the host cannot
// see, so it is never high confidence.
const CauseRNICCrossRailUnreachable = "rnic_cross_rail_unreachable"

// CauseSNCMeshFailure is a fault Algorithm 1 localizes to an snc_mesh link:
// the fabric between two NUMA nodes of one package under sub-NUMA clustering.
// No inter-socket bus is involved, so it is never upi_failure. It is judged by
// the upi_failure rules.
const CauseSNCMeshFailure = "snc_mesh_failure"

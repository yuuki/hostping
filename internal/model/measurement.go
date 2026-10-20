package model

// PathMeasurement is one loopback result (small-message latency + derived bandwidth).
type PathMeasurement struct {
	RNIC     string `json:"rnic"`
	Endpoint string `json:"endpoint"`
	// EndpointKind is set by the engine from the topology graph.
	EndpointKind  EndpointKind `json:"endpoint_kind,omitempty"`
	SmallLatNsP50 uint64       `json:"small_lat_ns_p50"`
	SmallLatNsP99 uint64       `json:"small_lat_ns_p99"`
	LargeLatNsP50 uint64       `json:"large_lat_ns_p50"`
	BandwidthBPS  uint64       `json:"bandwidth_bps"`
	Status        PathStatus   `json:"status"`
	// UncertainReason narrows Status == PathUncertain. It is omitted on a
	// decided path, where it carries no meaning.
	UncertainReason PathUncertainReason `json:"uncertain_reason,omitempty"`
	// BusyRecheck is the paired re-read of a loaded RNIC's path the busy
	// sweep read abnormal against its siblings; nil when the path was not
	// rechecked. A path it did not confirm is uncertain with rnic_busy.
	BusyRecheck *BusyRecheck `json:"busy_recheck,omitempty"`
	// BWRecheck is the paired re-read of the group (RNIC, distance class) of
	// a path the sweep read abnormal against its idle baseline; nil when the
	// group was not rechecked. Every path of a rechecked group carries the
	// same record, and its Outcome decided the path's Status.
	BWRecheck *BandwidthRecheck `json:"bw_recheck,omitempty"`
	// LatencyStatus is the verdict on SmallLatNsP50, and LatencyReason what it
	// was judged against or why it was not. It is advisory: it never feeds
	// Status or Algorithm 1.
	LatencyStatus LatencyStatus `json:"latency_status"`
	LatencyReason LatencyReason `json:"latency_reason,omitempty"`
	// LatencyRefNs is the latency the path was judged against; 0 when unjudged.
	// A path cleared by the recheck keeps the sweep's sibling median here.
	LatencyRefNs uint64 `json:"latency_ref_ns,omitempty"`
	// LatencyRecheck is the paired re-measurement of a path the sweep read
	// abnormal against its siblings; nil when the path was not rechecked.
	LatencyRecheck *LatencyRecheck `json:"latency_recheck,omitempty"`
	// LatencyRecheckSkipped says why a path still abnormal against its
	// siblings was not rechecked although its class was: "cap" when the run
	// reached the recheck's per-run path cap. Such a verdict was never
	// confirmed, so it neither extends LatencyStreak (which holds its value)
	// nor becomes a cause in this run. Empty otherwise.
	LatencyRecheckSkipped LatencyRecheckSkip `json:"latency_recheck_skipped,omitempty"`
	// LatencyLocality is the reference that cleared a sibling verdict as a
	// rise shared by the RNICs of one NUMA node (LatencyLocalityShift); nil
	// otherwise.
	LatencyLocality *LatencyLocality `json:"latency_locality,omitempty"`
	// LatencyStreak is how many consecutive idle runs, this one included,
	// have read the path abnormal against its siblings (after the recheck and
	// the locality shift); 0 when this run did not, and the held count when
	// the recheck's cap left the path out (LatencyRecheckSkipped). path_latency_abnormal
	// reports a sibling verdict only once the streak reaches three (or, in a
	// one-shot run, when the paired recheck confirmed it), so a path below
	// that is abnormal here but named by no cause.
	LatencyStreak int `json:"latency_streak,omitempty"`
	// BaselineClass names the learned sub-class whose baseline judges the
	// path, as "distance/name" (e.g. "cross_root/fast"): the baseline file
	// lists the path's root-port pair in that sub-class of its distance.
	// Empty means the distance's own baseline. Busy mode compares the path
	// only with siblings of the same class.
	BaselineClass string `json:"baseline_class,omitempty"`
	// Verbs is how long the probe's setup verbs took. It is advisory: it
	// never feeds Status or Algorithm 1. Omitted when the probe reported none.
	Verbs VerbNs `json:"verb_ns,omitzero"`
	// Addressing is how the live probe addressed the RNIC port. The engine
	// logs it; it is not part of the report.
	Addressing PortAddressing `json:"-"`
}

// LatencyRecheckSkip is why a path flagged against its siblings, in a class
// the recheck covered, was not rechecked.
type LatencyRecheckSkip string

// LatencyRecheckSkipCap: the run had more candidates than the recheck's
// per-run path cap, and this path was not among those selected.
const LatencyRecheckSkipCap LatencyRecheckSkip = "cap"

// LatencyRecheck is the paired re-measurement of one path whose sweep reading
// was latency-abnormal against its siblings: the path re-read in turn with
// siblings of its class, preferably on other RNICs, so both sides are read in
// the same time window. It changes only the latency verdict; the sweep's
// readings (SmallLatNsP50, BandwidthBPS, ...) stay as they were.
type LatencyRecheck struct {
	// PathNsP50 is the median of the path's re-read small-message p50s.
	PathNsP50 uint64 `json:"path_ns_p50"`
	// SiblingNsP50 is the median of the interleaved sibling re-reads.
	SiblingNsP50 uint64 `json:"sibling_ns_p50"`
	// Siblings names the sibling paths read, as "rnic->endpoint", in order.
	Siblings []string `json:"siblings"`
	// Confirmed is true when the re-reads still exceed the siblings by
	// --abnormal-th and the sibling floor, keeping the abnormal verdict.
	Confirmed bool `json:"confirmed"`
}

// BusyRecheck is the paired re-read of one busy-probed path whose busy sweep
// reading was abnormal against the median of its siblings (same loaded RNIC,
// distance and sub-class): the path re-read in rounds, each round reading the
// path and then every sibling once, so both sides share a time window. It
// changes only the bandwidth verdict; the sweep's readings (BandwidthBPS, ...)
// stay as they were.
type BusyRecheck struct {
	// PathBPS is the median of the path's re-read bandwidths.
	PathBPS uint64 `json:"path_bps"`
	// SiblingBPS is the median of every sibling re-read of the same rounds.
	SiblingBPS uint64 `json:"sibling_bps"`
	// Siblings names the sibling paths read in each round, as
	// "rnic->endpoint", in order.
	Siblings []string `json:"siblings"`
	// Rounds is how many rounds were read.
	Rounds int `json:"rounds"`
	// Confirmed is true when the re-reads still sit --abnormal-th below the
	// siblings, keeping the abnormal verdict.
	Confirmed bool `json:"confirmed"`
}

// BWRecheckOutcome is what a BandwidthRecheck decided for its group.
type BWRecheckOutcome string

const (
	// BWRecheckConfirmed: the representative still read --abnormal-th below
	// the references. The group stays abnormal.
	BWRecheckConfirmed BWRecheckOutcome = "confirmed"
	// BWRecheckCleared: the representative read level with the references
	// and met its baseline. The group is judged as a path that met it.
	BWRecheckCleared BWRecheckOutcome = "cleared"
	// BWRecheckHostShift: the representative read level with the references
	// and below its baseline, whether or not the references cleared it. The
	// group is uncertain with host_shift.
	BWRecheckHostShift BWRecheckOutcome = "host_shift"
)

// BandwidthRecheck is the paired re-read of one group of idle-judged paths —
// one RNIC's paths of one distance class (sub-class included) that the sweep
// read abnormal against their baseline. A representative path of the group is
// re-read in rounds, interleaved with reference paths of the same class on
// other RNICs that met their baseline in the sweep, so both sides share a
// time window. It changes only the bandwidth verdict; the sweep's readings
// stay as they were.
type BandwidthRecheck struct {
	// Path names the representative re-read for the group, as
	// "rnic->endpoint".
	Path string `json:"path"`
	// PathBPS is the median of the representative's re-read bandwidths.
	PathBPS uint64 `json:"path_bps"`
	// RefBPS is the median of every reference re-read of the same rounds.
	RefBPS uint64 `json:"ref_bps"`
	// BaselineBPS is the idle baseline the group is judged against.
	BaselineBPS uint64 `json:"baseline_bps"`
	// Refs names the reference paths read in each round, as
	// "rnic->endpoint", in order.
	Refs []string `json:"refs"`
	// Rounds is how many rounds were read.
	Rounds int `json:"rounds"`
	// Outcome is what the re-reads decided for the group.
	Outcome BWRecheckOutcome `json:"outcome"`
}

// LatencyLocality is the locality reference of a path whose sibling verdict
// was cleared as a NUMA-node-wide rise: the same-class paths of the other
// RNICs on its RNIC's NUMA node, read in the sweep. The path's SmallLatNsP50
// did not exceed RefNs by --abnormal-th and the sibling floor.
type LatencyLocality struct {
	// NUMA is the NUMA node of the path's RNIC.
	NUMA int `json:"numa"`
	// RefNs is the median sweep small-message p50 of the locality siblings.
	RefNs uint64 `json:"ref_ns"`
	// RNICs names the other RNICs on the node whose paths formed RefNs.
	RNICs []string `json:"rnics"`
}

// VerbNs is the execution time of the RDMA verbs one loopback probe called
// before it could measure anything, in nanoseconds (ToN'24 §VI-C: slow verbs
// — e.g. GPU memory registration on a failing GPU — are a bottleneck of their
// own that latency and bandwidth never show). Zero means not measured.
type VerbNs struct {
	// GPUAlloc is the device lookup and allocation of the GPU buffer; always
	// 0 for a memory endpoint.
	GPUAlloc uint64 `json:"gpu_alloc,omitempty"`
	// MRReg is memory registration, summed over every MR the probe
	// registered (one over the GPU buffer, two over host buffers).
	MRReg uint64 `json:"mr_reg,omitempty"`
	// QPSetup is creating both loopback QPs and moving them to RTS,
	// including retries over candidate GIDs.
	QPSetup uint64 `json:"qp_setup,omitempty"`
	// MemWarmup is the --mem-warmup fill that ran before the timed
	// iterations of a memory endpoint's probe. It is not a verb; it is here
	// so the warm-up's cost is reported next to the setup it follows.
	MemWarmup uint64 `json:"mem_warmup,omitempty"`
}

// LinkState is Algorithm 1 output for one hop.
type LinkState struct {
	ID            string              `json:"id"`
	Status        LinkStatus          `json:"status"`
	AbnormalCount int                 `json:"abnormal_count"`
	AbnormalRNICs map[string]struct{} `json:"-"`
	// Utilization is the bus utilization (0–1) sampled before the run, or nil
	// when no live source could read it. Unknown is not 0: an unread bus is
	// evidence neither that it was quiet nor that it was overloaded.
	Utilization *float64 `json:"utilization,omitempty"`
	// Tested reports that this run put the hop through a loopback test: some
	// measured path decided normal or abnormal spoke about it. Status alone
	// cannot say so, because LinkUncertain covers both a hop no decided path
	// reached and one an abnormal path crossed without isolating it.
	Tested bool `json:"-"`
}

// Util returns the link's utilization and whether it was read.
func (l LinkState) Util() (float64, bool) {
	if l.Utilization == nil {
		return 0, false
	}
	return *l.Utilization, true
}

// MutualMeasurement is ToN RNIC-to-RNIC connectivity probe output.
type MutualMeasurement struct {
	Src       string  `json:"src"`
	Dst       string  `json:"dst"`
	Connected bool    `json:"connected"`
	RTTnsP50  uint64  `json:"rtt_ns_p50"`
	RTTnsP99  uint64  `json:"rtt_ns_p99"`
	LossRatio float64 `json:"loss_ratio"`
	Sent      uint32  `json:"sent"`
	Recv      uint32  `json:"recv"`
	// Reliable is true for the live verbs probe: a lost probe is a timeout
	// (Recv < Sent), and LossRatio is not Ethernet drop evidence — drops come
	// from the RNIC counters. It is false only in the simulator, whose lost
	// probes are its injected drops.
	Reliable     bool   `json:"reliable,omitempty"`
	CounterDrops uint64 `json:"counter_drops,omitempty"`
	// CrossRail marks a pair whose RNICs sit on different rails (fabric
	// keys), probed only under --mutual-scope=host. Its traffic leaves the
	// rail, so its failures are judged apart from same-rail ones.
	CrossRail bool `json:"cross_rail,omitempty"`
}

// MutualScope selects which RNIC pairs the mutual probe measures.
type MutualScope string

const (
	// MutualScopeRail pairs RNICs that share a rail (IPv4 subnet or typed
	// GID prefix). It is the default.
	MutualScopeRail MutualScope = "rail"
	// MutualScopeHost pairs every RNIC of the host with a RoCEv2 GID, across
	// rails too. It assumes the rails route to each other.
	MutualScopeHost MutualScope = "host"
)

// MutualTransport is the verbs transport the live mutual probe uses.
type MutualTransport string

const (
	// MutualTransportRC is an RC SEND ping-pong, the default. RC retransmits a
	// lost packet for about half a second, so an isolated loss never shows.
	MutualTransportRC MutualTransport = "rc"
	// MutualTransportUD is a UD request/reply: one lost packet is one timed-out
	// probe (ToN'24 §IV-A-2). The scheduled probe requires it.
	MutualTransportUD MutualTransport = "ud"
)

// TimeoutRatio is the fraction of probes that got no reply, 1 − Recv/Sent
// (ToN'24 §IV-A-2). A pair that never connected timed out on every probe, so it
// is 1. ok is false when the pair connected but reports no probes sent: there
// is no rate to speak of, and 0 would read as a healthy pair.
func (m MutualMeasurement) TimeoutRatio() (ratio float64, ok bool) {
	if !m.Connected {
		return 1, true
	}
	if m.Sent == 0 {
		return 0, false
	}
	if m.Recv >= m.Sent {
		return 0, true
	}
	return 1 - float64(m.Recv)/float64(m.Sent), true
}

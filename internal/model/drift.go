package model

// DriftStatus is the verdict on a path against its own idle history
// (--drift-tracking): whether its readings have moved away from the level the
// host itself measured when tracking began. It is advisory and separate from
// PathStatus: a drifted path is still within its distance baseline, so it
// never feeds Algorithm 1 or a cause.
type DriftStatus int

const (
	// DriftUnknown: the history cannot say yet, or cannot say now. DriftReason
	// names why. Unknown is never "no drift".
	DriftUnknown DriftStatus = iota
	// DriftStable: every tracked signal reads within the drift threshold of
	// the path's reference.
	DriftStable
	// DriftDrifted: at least one tracked signal reads worse than the path's
	// reference by the drift threshold.
	DriftDrifted
)

// driftStatusNames spells DriftUnknown as "unknown", so the sentinel for a
// value outside the table is "invalid", as for LatencyStatus.
var driftStatusNames = [...]string{
	DriftUnknown: "unknown",
	DriftStable:  "stable",
	DriftDrifted: "drifted",
}

func (s DriftStatus) String() string { return enumName(driftStatusNames[:], int(s), "invalid") }

func (s DriftStatus) MarshalJSON() ([]byte, error) { return marshalName(s) }

// DriftReason says why a path's DriftStatus is DriftUnknown.
type DriftReason int

const (
	// DriftReasonNone is a judged path (stable or drifted).
	DriftReasonNone DriftReason = iota
	// DriftCollectingReference: the path has not yet been read on enough
	// days to form its reference.
	DriftCollectingReference
	// DriftFewRecentDays: the reference exists, but too few recent days
	// after it hold a reading: the tracking is young, the host was off or
	// busy, or the path keeps being left out of the history (it reads
	// abnormal, its probe is skipped).
	DriftFewRecentDays
	// DriftClockBehind: the wall clock reads earlier than the newest day of
	// the history, so "recent" cannot be told. Nothing is recorded until the
	// clock is past it again.
	DriftClockBehind
	// DriftNotTracked: the history is full (more paths than it holds).
	DriftNotTracked
	// DriftHistoryBlocked: the history on disk can be neither used nor
	// replaced without losing what may be a good one (a newer build wrote
	// it, or it cannot be read). Nothing is tracked until it is dealt with.
	DriftHistoryBlocked
)

var driftReasonNames = [...]string{
	DriftReasonNone:          "none",
	DriftCollectingReference: "collecting_reference",
	DriftFewRecentDays:       "few_recent_days",
	DriftClockBehind:         "clock_behind",
	DriftNotTracked:          "not_tracked",
	DriftHistoryBlocked:      "history_blocked",
}

func (r DriftReason) String() string { return enumName(driftReasonNames[:], int(r), "unknown") }

func (r DriftReason) MarshalJSON() ([]byte, error) { return marshalName(r) }

// DriftSignal names one tracked reading of a path.
type DriftSignal string

const (
	// DriftBandwidth is PathMeasurement.BandwidthBPS; lower is worse.
	DriftBandwidth DriftSignal = "bandwidth"
	// DriftLatencyP50 is PathMeasurement.SmallLatNsP50; higher is worse.
	DriftLatencyP50 DriftSignal = "latency_p50"
	// DriftLatencyP99 is PathMeasurement.SmallLatNsP99; higher is worse.
	DriftLatencyP99 DriftSignal = "latency_p99"
)

// DriftSignals is every tracked signal, in report order.
var DriftSignals = []DriftSignal{DriftBandwidth, DriftLatencyP50, DriftLatencyP99}

// DriftShape is how a drifted signal moved inside the window of daily values
// the history keeps.
type DriftShape string

const (
	// DriftShapeGradual: a monotonic trend over the window (aging, a slow
	// thermal rise).
	DriftShapeGradual DriftShape = "gradual"
	// DriftShapeStep: most of the change happened within a few days
	// (DriftFinding.StepOn), as after a firmware, driver or kernel update or
	// a hardware replacement.
	DriftShapeStep DriftShape = "step"
	// DriftShapeSustained: the signal reads worse than the reference
	// throughout the window, with neither a trend nor a step inside it: the
	// change is older than the window, or too slow to resolve in it.
	DriftShapeSustained DriftShape = "sustained"
	// DriftShapeUnknown: the window holds too few daily values to tell.
	DriftShapeUnknown DriftShape = "unknown"
)

// DriftTempStatus is what the temperature of a drifted path's devices did
// between the reference days and the recent days.
type DriftTempStatus string

const (
	// DriftTempRose: a device on the path reads hotter than in the reference
	// days by the thermal threshold. The drift is consistent with thermal
	// throttling, which reverses when the temperature does.
	DriftTempRose DriftTempStatus = "rose"
	// DriftTempSteady: every device on the path that has a sensor was read
	// on both sides, and none rose.
	DriftTempSteady DriftTempStatus = "steady"
	// DriftTempUnknown: a device on the path has no temperature on one of
	// the two sides. DriftTemperature.Reason names which.
	DriftTempUnknown DriftTempStatus = "unknown"
)

// Reasons of a DriftTempUnknown.
const (
	// DriftTempNoSensor: no temperature was read for the device at all.
	DriftTempNoSensor = "no_sensor"
	// DriftTempNoReference: the device was not read in the reference days.
	DriftTempNoReference = "no_reference_reading"
	// DriftTempNoRecent: the device was not read in the recent days.
	DriftTempNoRecent = "no_recent_reading"
)

// DriftSummary is the report's drift section: how many tracked paths are in
// each DriftStatus after this run, and one finding per drifted signal. Only a
// confirmed idle-trigger run of a daemon with --drift-tracking carries it.
type DriftSummary struct {
	Stable  int `json:"stable"`
	Drifted int `json:"drifted"`
	Unknown int `json:"unknown"`
	// UnknownReasons counts the unknown paths per reason, in reason order.
	UnknownReasons []DriftUnknownCount `json:"unknown_reasons,omitempty"`
	// Findings names the drifted signals, the largest change first. A
	// report holds a bounded number of them; FindingsOmitted counts the
	// rest, whose paths are still drifted in the metrics.
	Findings        []DriftFinding `json:"findings,omitempty"`
	FindingsOmitted int            `json:"findings_omitted,omitempty"`
}

// DriftUnknownCount is how many paths are unknown for one reason.
type DriftUnknownCount struct {
	Reason DriftReason `json:"reason"`
	Paths  int         `json:"paths"`
}

// DriftFinding is one signal of one path that reads worse than the path's
// own reference.
type DriftFinding struct {
	RNIC     string      `json:"rnic"`
	Endpoint string      `json:"endpoint"`
	Signal   DriftSignal `json:"signal"`
	Shape    DriftShape  `json:"shape"`
	// Change is how far Recent lies on the worse side of Reference, as a
	// fraction of Reference: 0.07 is a bandwidth 7% lower, or a latency 7%
	// higher.
	Change float64 `json:"change"`
	// Reference and Recent are in the signal's unit: bit/s for bandwidth, ns
	// for the latencies.
	Reference float64 `json:"reference"`
	Recent    float64 `json:"recent"`
	// ReferenceFrom..ReferenceTo and RecentFrom..RecentTo are the first and
	// last UTC day (YYYY-MM-DD) of the daily values behind each side.
	ReferenceFrom string `json:"reference_from"`
	ReferenceTo   string `json:"reference_to"`
	RecentFrom    string `json:"recent_from"`
	RecentTo      string `json:"recent_to"`
	// Since is the UTC day the signal was first judged drifted.
	Since string `json:"since"`
	// StepOn is the first UTC day at the new level of a DriftShapeStep.
	StepOn      string           `json:"step_on,omitempty"`
	Temperature DriftTemperature `json:"temperature"`
	// SoftwareChanges lists the kernel and RNIC firmware changes seen since
	// the reference days began, oldest first.
	SoftwareChanges []DriftSoftwareChange `json:"software_changes,omitempty"`
}

// DriftTemperature is the temperature evidence of a DriftFinding.
type DriftTemperature struct {
	Status DriftTempStatus `json:"status"`
	// Reason is why Status is DriftTempUnknown, as "<device>: <reason>".
	Reason string `json:"reason,omitempty"`
	// Devices holds the devices on the path read on both sides.
	Devices []DriftDeviceTemp `json:"devices,omitempty"`
}

// DriftDeviceTemp is one device's temperature in the reference days and in
// the recent days, in degrees Celsius.
type DriftDeviceTemp struct {
	// Device is the RNIC or GPU endpoint ID.
	Device     string  `json:"device"`
	ReferenceC float64 `json:"reference_c"`
	RecentC    float64 `json:"recent_c"`
}

// DriftSoftwareChange is one change of the kernel release or of an RNIC's
// firmware version that the drift history recorded.
type DriftSoftwareChange struct {
	// On is the UTC day (YYYY-MM-DD) of the first idle run that saw it.
	On string `json:"on"`
	// Component is "kernel" or "firmware <RNIC PCI address or device>".
	Component string `json:"component"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// PathDrift is the drift state of one tracked path after an idle run, for the
// metrics. The report carries only the DriftSummary built from these.
type PathDrift struct {
	RNIC     string
	Endpoint string
	Status   DriftStatus
	// Reason is set when Status is DriftUnknown.
	Reason DriftReason
	// Changes holds each signal's change against the reference (as
	// DriftFinding.Change, negative when the signal reads better), in
	// DriftSignals order; empty while Status is DriftUnknown.
	Changes []DriftChange
}

// DriftChange is one signal's change against the path's reference.
type DriftChange struct {
	Signal DriftSignal
	Change float64
}

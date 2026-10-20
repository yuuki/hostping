package model

import "errors"

// ErrNoGDR is returned when a GPU endpoint is requested but GPUDirect RDMA
// registration is not available. The engine skips that path instead of aborting.
var ErrNoGDR = errors.New("gpu endpoint requires GPUDirect RDMA")

// Live verbs failures that must not abort the rest of the host mesh.
var (
	ErrNoDevice = errors.New("no rdma device")
	ErrNoGID    = errors.New("no active rocev2 gid")
	ErrProbe    = errors.New("probe failed")
	// ErrNoLID is an ACTIVE InfiniBand port with LID 0: no subnet manager
	// has configured it, so it has no address to loop back through.
	ErrNoLID = errors.New("infiniband port has no lid")
	// ErrPortNotActive is an InfiniBand RNIC none of whose ports is ACTIVE.
	ErrPortNotActive = errors.New("infiniband port not active")
)

// ErrPortLoopback marks a loopback that failed at an InfiniBand port: its
// WRITEs completed with a port-level transport error (retry counter exceeded,
// a remote error, a response timeout) in every addressing mode the probe
// tried. Every other probe of that port would wait out the same hardware
// retry budget, so the engine stops probing the RNIC for the rest of the run.
// Wrap an error with PortLoopbackError rather than matching this directly.
var ErrPortLoopback = errors.New("loopback failed at the port")

// PortLoopbackError marks err as an ErrPortLoopback failure. The text is
// err's own, so skip reasons read as before; errors.Is matches both err's
// sentinels and ErrPortLoopback.
func PortLoopbackError(err error) error {
	if err == nil {
		return nil
	}
	return portLoopbackErr{err: err}
}

type portLoopbackErr struct{ err error }

func (e portLoopbackErr) Error() string   { return e.err.Error() }
func (e portLoopbackErr) Unwrap() []error { return []error{e.err, ErrPortLoopback} }

// Port addressing modes a live probe reports (PortAddressing.Mode).
const (
	// AddrRoCEv2GID addresses a RoCE port by a RoCEv2 GID, with a GRH.
	AddrRoCEv2GID = "rocev2_gid"
	// AddrIBLID addresses an InfiniBand port by its LID, with no GRH.
	AddrIBLID = "ib_lid"
	// AddrIBLIDGRH addresses an InfiniBand port by its LID plus a GRH over
	// GID index 0; tried only when the LID-only connect is refused.
	AddrIBLIDGRH = "ib_lid_grh"
)

// Link layers of an RNIC port, as sysfs ports/N/link_layer spells them.
const (
	LinkLayerEthernet   = "Ethernet"
	LinkLayerInfiniBand = "InfiniBand"
)

// PortAddressing is how a live loopback probe addressed its RNIC port. Mode
// is empty when no live probe ran (the simulator).
type PortAddressing struct {
	Mode string
	Port int
	// LID is the port's LID on InfiniBand, 0 on RoCE.
	LID int
}

// ErrComputeMode is the engine's reason for not probing a GPU whose compute
// mode is not Default: on an EXCLUSIVE_PROCESS GPU hostping's own context
// would either fail or lock out the job that owns the GPU, and a PROHIBITED
// GPU admits no context at all. The skip is reported like any other.
var ErrComputeMode = errors.New("compute_mode")

// GPU compute modes, spelled as nvidia-smi --query-gpu=compute_mode prints
// them. Exclusive_Thread is the pre-CUDA 8 mode, kept so an old driver is not
// read as unknown.
const (
	ComputeModeDefault          = "Default"
	ComputeModeExclusiveProcess = "Exclusive_Process"
	ComputeModeExclusiveThread  = "Exclusive_Thread"
	ComputeModeProhibited       = "Prohibited"
)

// ComputeModeBlocksProbe reports whether a GPU in mode must not be probed. An
// empty or unrecognised mode is unknown and does not block: a host without
// nvidia-smi, or a GPU without the notion (AMD), keeps being probed as before.
func ComputeModeBlocksProbe(mode string) bool {
	switch mode {
	case ComputeModeExclusiveProcess, ComputeModeExclusiveThread, ComputeModeProhibited:
		return true
	}
	return false
}

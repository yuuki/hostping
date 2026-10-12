package zigprobe

/*
#cgo CFLAGS: -I${SRCDIR}/../../include
#cgo linux LDFLAGS: -L${SRCDIR}/../../zig/zig-out/lib -lhostping_probe -Wl,-rpath,$ORIGIN -Wl,-rpath,$ORIGIN/../zig/zig-out/lib -Wl,-rpath,${SRCDIR}/../../zig/zig-out/lib -libverbs -lpthread -ldl -lm -lc
#cgo darwin LDFLAGS: -L${SRCDIR}/../../zig/zig-out/lib -lhostping_probe -Wl,-rpath,${SRCDIR}/../../zig/zig-out/lib
#include "hostping.h"
*/
import "C"
import (
	"context"
	"fmt"
	"time"
	"unsafe"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/sim"
	"github.com/yuuki/hostping/internal/topology"
)

// Prober calls the Zig C ABI (ibverbs on hardware, Eq.1 when Sim is set).
type Prober struct {
	Sim   *sim.World
	Graph topology.Graph
	// Iters is the loopback probe's WRITE pairs; 0 means 512.
	Iters     uint32
	TimeoutMS uint32
	// MutualIters is the round trips of one mutual burst; 0 means 64.
	MutualIters uint32
	// MemWarmup is --mem-warmup: how long a host-memory loopback probe
	// writes its buffers before the timed iterations. 0 is off; GPU
	// endpoints and the simulator ignore it.
	MemWarmup time.Duration
	// MutualMsg is the mutual probe's UD SEND payload in bytes; 0 means 1 B,
	// the ToN'24 size. The Zig probe's buffers hold at most 4096 B, and a UD
	// message cannot exceed the port MTU.
	MutualMsg uint32
	// MutualTimeoutMS is how long one mutual probe waits for its reply before
	// it counts as a timeout; 0 means 500 ms, ToN'24 §IV-A-2. It is separate
	// from TimeoutMS, which bounds a whole loopback probe.
	MutualTimeoutMS uint32
	// MutualTransport picks the live probe: rc (the zero value's default) or
	// ud. The simulator ignores it.
	MutualTransport model.MutualTransport
}

func (p Prober) mutualTransport() C.int32_t {
	if p.MutualTransport == model.MutualTransportUD {
		return C.HOSTPING_MUTUAL_UD
	}
	return C.HOSTPING_MUTUAL_RC
}

func (p Prober) mutualTimeoutMS() uint32 {
	if p.MutualTimeoutMS == 0 {
		return 500
	}
	return p.MutualTimeoutMS
}

func (p Prober) mutualMsg() uint32 {
	if p.MutualMsg == 0 {
		return 1
	}
	return p.MutualMsg
}

func (p Prober) iters() uint32 {
	if p.Iters == 0 {
		return 512
	}
	return p.Iters
}

func (p Prober) mutualIters() uint32 {
	if p.MutualIters == 0 {
		return 64
	}
	return p.MutualIters
}

func (p Prober) timeoutMS() uint32 {
	if p.TimeoutMS == 0 {
		return 5000
	}
	return p.TimeoutMS
}

// effectiveTimeoutMS is the timeout_ms handed to the C call. The C ABI has no
// cancellation hook, so a blocking probe can only be bounded up front: when ctx
// carries a deadline nearer than the configured timeout, the smaller value is
// used so the call cannot outlive the deadline. An already-cancelled or expired
// ctx yields an error instead of a probe. Zero means "library default" on the
// Zig side, so the clamp keeps a 1 ms floor.
func effectiveTimeoutMS(ctx context.Context, base uint32) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return base, nil
	}
	left := time.Until(dl)
	if left <= 0 {
		return 0, context.DeadlineExceeded
	}
	ms := (left.Nanoseconds() + int64(time.Millisecond) - 1) / int64(time.Millisecond) // round up, never to 0
	if ms >= int64(base) {
		return base, nil
	}
	return uint32(ms), nil
}

func cstr(dst *[64]C.char, s string) {
	for i := range dst {
		dst[i] = 0
	}
	n := len(s)
	if n > 63 {
		n = 63
	}
	for i := 0; i < n; i++ {
		dst[i] = C.char(s[i])
	}
}

func (p Prober) Loopback(ctx context.Context, rnic model.RNIC, ep model.Endpoint, small, large uint32) (model.PathMeasurement, error) {
	timeout, err := effectiveTimeoutMS(ctx, p.timeoutMS())
	if err != nil {
		return model.PathMeasurement{}, err
	}
	req, err := p.loopbackReq(rnic, ep, small, large, timeout)
	if err != nil {
		return model.PathMeasurement{}, err
	}
	var res C.hostping_loopback_res
	rc := C.hostping_loopback(&req, &res)
	if rc != 0 {
		err := mapProbeErr(int(rc), C.GoString(&res.err[0]))
		if res.port_fault != 0 {
			// The InfiniBand port answered no WRITE in any addressing mode:
			// the engine stops probing this RNIC for the rest of the run.
			err = model.PortLoopbackError(err)
		}
		return model.PathMeasurement{}, err
	}
	return model.PathMeasurement{
		RNIC:          rnic.ID,
		Endpoint:      ep.ID,
		SmallLatNsP50: uint64(res.small_lat_ns_p50),
		SmallLatNsP99: uint64(res.small_lat_ns_p99),
		LargeLatNsP50: uint64(res.large_lat_ns_p50),
		BandwidthBPS:  uint64(res.bandwidth_bps),
		Verbs: model.VerbNs{
			GPUAlloc:  uint64(res.gpu_alloc_ns),
			MRReg:     uint64(res.mr_reg_ns),
			QPSetup:   uint64(res.qp_setup_ns),
			MemWarmup: uint64(res.mem_warmup_ns),
		},
		Addressing: model.PortAddressing{
			Mode: addrMode(int(res.addr_mode)),
			Port: int(res.port_num),
			LID:  int(res.lid),
		},
	}, nil
}

func (p Prober) loopbackReq(rnic model.RNIC, ep model.Endpoint, small, large, timeout uint32) (C.hostping_loopback_req, error) {
	var req C.hostping_loopback_req
	cstr(&req.device, rnic.Device)
	cstr(&req.endpoint, ep.ID)
	if ep.Kind == model.EndpointGPU {
		req.gpu_index = C.int32_t(ep.Index)
		cstr(&req.gpu_bdf, ep.PCIeBDF)
		// NSDI'23 §4.1.1: source and destination both live in the GPU, so no
		// host DRAM is on the measured path and there is nothing to mbind.
		req.numa_node = -1
	} else {
		req.gpu_index = -1
		req.numa_node = C.int32_t(ep.NUMA)
		if p.MemWarmup > 0 {
			req.mem_warmup_ns = C.uint64_t(p.MemWarmup.Nanoseconds())
		}
	}
	req.small_size = C.uint32_t(small)
	req.large_size = C.uint32_t(large)
	req.iters = C.uint32_t(p.iters())
	req.timeout_ms = C.uint32_t(timeout)
	if p.Sim != nil {
		path, ok := p.Graph.Path(rnic.ID, ep.ID)
		if !ok {
			return req, fmt.Errorf("no path %s -> %s", rnic.ID, ep.ID)
		}
		req.sim = 1
		req.sim_path_bw_bps = C.uint64_t(p.Sim.PathBW(path))
		req.sim_path_lat_ns = C.uint64_t(p.Sim.PathLatNs(path))
		req.sim_tproc_ns = C.uint64_t(baseline.TprocNs)
		v := p.Sim.Verbs(ep.ID)
		req.sim_gpu_alloc_ns = C.uint64_t(v.GPUAlloc)
		req.sim_mr_reg_ns = C.uint64_t(v.MRReg)
		req.sim_qp_setup_ns = C.uint64_t(v.QPSetup)
	}
	return req, nil
}

func (p Prober) Mutual(ctx context.Context, src, dst model.RNIC) (model.MutualMeasurement, error) {
	timeout, err := effectiveTimeoutMS(ctx, p.mutualTimeoutMS())
	if err != nil {
		return model.MutualMeasurement{}, err
	}
	req := p.mutualReq(src, dst, timeout)
	var res C.hostping_mutual_res
	rc := C.hostping_mutual(&req, &res)
	if rc != 0 {
		// Only a connectivity-class failure is a real "these two RNICs cannot
		// reach each other" measurement. Setup failures (missing device, no
		// GID, allocation) become errors so the engine can skip the pair.
		if connectivityRC(rc) && res.connected == 0 {
			return model.MutualMeasurement{Src: src.ID, Dst: dst.ID, Connected: false, LossRatio: 1, Reliable: res.reliable != 0}, nil
		}
		return model.MutualMeasurement{}, mapProbeErr(int(rc), C.GoString(&res.err[0]))
	}
	return model.MutualMeasurement{
		Src:       src.ID,
		Dst:       dst.ID,
		Connected: res.connected != 0,
		RTTnsP50:  uint64(res.rtt_ns_p50),
		RTTnsP99:  uint64(res.rtt_ns_p99),
		LossRatio: float64(res.loss_ratio),
		Sent:      uint32(res.sent),
		Recv:      uint32(res.recvd),
		Reliable:  res.reliable != 0,
	}, nil
}

func (p Prober) mutualReq(src, dst model.RNIC, timeout uint32) C.hostping_mutual_req {
	var req C.hostping_mutual_req
	cstr(&req.src_device, src.Device)
	cstr(&req.dst_device, dst.Device)
	req.msg_size = C.uint32_t(p.mutualMsg())
	req.iters = C.uint32_t(p.mutualIters())
	req.timeout_ms = C.uint32_t(timeout)
	req.transport = p.mutualTransport()
	if p.Sim != nil {
		req.sim = 1
		m := p.Sim.Mutual(src.ID, dst.ID)
		req.sim_rtt_ns = C.uint64_t(m.RTTnsP50)
		if !m.Connected {
			req.sim_connect_fail = 1
		}
		if m.LossRatio > 0 {
			req.sim_loss_ppm = C.uint32_t(m.LossRatio * 1_000_000)
		}
	}
	return req
}

// addrMode names a HOSTPING_ADDR_* value; "" for HOSTPING_ADDR_NONE.
func addrMode(v int) string {
	switch v {
	case int(C.HOSTPING_ADDR_ROCEV2_GID):
		return model.AddrRoCEv2GID
	case int(C.HOSTPING_ADDR_IB_LID):
		return model.AddrIBLID
	case int(C.HOSTPING_ADDR_IB_LID_GRH):
		return model.AddrIBLIDGRH
	}
	return ""
}

// connectivityRC reports whether rc means the RoCE path itself failed (QP
// handshake, completion error or timeout) rather than local probe setup.
func connectivityRC(rc C.int) bool {
	return rc == C.HOSTPING_ERR_CONNECT || rc == C.HOSTPING_ERR_TIMEOUT
}

func mapProbeErr(rc int, msg string) error {
	if msg == "" {
		msg = "probe failed"
	}
	switch rc {
	case int(C.HOSTPING_ERR_NO_GDR):
		return fmt.Errorf("%w: %s", model.ErrNoGDR, msg)
	case int(C.HOSTPING_ERR_NO_DEVICE):
		return fmt.Errorf("%w: %s", model.ErrNoDevice, msg)
	case int(C.HOSTPING_ERR_PORT):
		return portErr(msg)
	default:
		return fmt.Errorf("%w: %s", model.ErrProbe, msg)
	}
}

// ReleaseGPU implements engine.GPUReleaser: it hands back the CUDA/HIP
// runtime primary contexts the run's GPU loopbacks created, which the runtime
// would otherwise keep for the life of the process. The simulator creates
// none.
func (p Prober) ReleaseGPU() {
	if p.Sim != nil {
		return
	}
	C.hostping_gpu_release()
}

func Version() string { return C.GoString(C.hostping_version()) }

// loopbackReqABISize is the C ABI size of hostping_loopback_req.
// Kept out of *_test.go because Go does not allow cgo in test files.
func loopbackReqABISize() int {
	return int(unsafe.Sizeof(C.hostping_loopback_req{}))
}

// loopbackResABISize is the C ABI size of hostping_loopback_res.
func loopbackResABISize() int {
	return int(unsafe.Sizeof(C.hostping_loopback_res{}))
}

// loopbackReqFields is what Loopback would hand to the C ABI for one path:
// iters and mem_warmup_ns. Kept out of *_test.go for the same reason as
// loopbackReqABISize.
func loopbackReqFields(p Prober, rnic model.RNIC, ep model.Endpoint) (iters uint32, memWarmupNs uint64, err error) {
	req, err := p.loopbackReq(rnic, ep, 1, 4096, p.timeoutMS())
	return uint32(req.iters), uint64(req.mem_warmup_ns), err
}

// mutualReqIters is the iters Mutual would hand to the C ABI.
func mutualReqIters(p Prober, src, dst model.RNIC) uint32 {
	req := p.mutualReq(src, dst, p.mutualTimeoutMS())
	return uint32(req.iters)
}

// mutualReqMsgSize is the msg_size Mutual would hand to the C ABI. Kept out of
// *_test.go for the same reason as loopbackReqABISize.
func mutualReqMsgSize(p Prober, src, dst model.RNIC) uint32 {
	req := p.mutualReq(src, dst, p.mutualTimeoutMS())
	return uint32(req.msg_size)
}

// mutualReqTimeoutMS is the per-probe timeout_ms Mutual would hand to the C
// ABI without a context deadline.
func mutualReqTimeoutMS(p Prober, src, dst model.RNIC) uint32 {
	req := p.mutualReq(src, dst, p.mutualTimeoutMS())
	return uint32(req.timeout_ms)
}

// mutualReqTransport is the transport Mutual would hand to the C ABI, as
// the header's constant name.
func mutualReqTransport(p Prober, src, dst model.RNIC) string {
	req := p.mutualReq(src, dst, p.mutualTimeoutMS())
	switch req.transport {
	case C.HOSTPING_MUTUAL_RC:
		return "HOSTPING_MUTUAL_RC"
	case C.HOSTPING_MUTUAL_UD:
		return "HOSTPING_MUTUAL_UD"
	}
	return fmt.Sprintf("unknown(%d)", int(req.transport))
}

func SimLatency(tproc, host, size, bw uint64) uint64 {
	return uint64(C.hostping_sim_latency_ns(C.uint64_t(tproc), C.uint64_t(host), C.uint64_t(size), C.uint64_t(bw)))
}

// Live hardware sampling is monitor.SysfsSampler: nothing this package reads
// needs libibverbs, so nothing but the probes themselves crosses the C ABI.

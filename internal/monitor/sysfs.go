package monitor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// SysfsSampler reads InfiniBand / netdev counters and GPU busy state.
// Pause duration is taken from vendor *pause*duration* files or ethtool -S
// (ConnectX tx_prioN_pause_duration, microseconds). Drops are whatever
// looksLikeDrop accepts: discards, dropped and out_of_buffer, never symbol or
// receive errors.
type SysfsSampler struct {
	Graph topology.Graph
	// GPUUtilInterval rate-limits nvidia-smi; zero means DefaultGPUUtilInterval.
	GPUUtilInterval time.Duration
	// Uncore reads memory-link and inter-socket utilization from the IMC
	// and UPI PMUs; nil when --uncore-util is off or the PMUs cannot be
	// read.
	Uncore *Uncore
}

// PollBus advances the uncore counters one poll; see BusPoller.
func (s SysfsSampler) PollBus(_ context.Context, reset bool) {
	s.Uncore.Poll(reset)
}

func (s SysfsSampler) RNIC(ctx context.Context, n model.RNIC) (model.RNICCounters, error) {
	return ReadRNICSysfs(ctx, n)
}

// ReadRNICSysfs reads n's counters from the live /sys; SysfsSampler.RNIC, the
// hardware sampler, goes through it.
func ReadRNICSysfs(ctx context.Context, n model.RNIC) (model.RNICCounters, error) {
	return ReadRNICSysfsFrom(ctx, "/sys", n)
}

// Counter source bits for RNICCounters.PauseSources and DropSources.
const (
	srcIBDiscards  uint8 = 1 << iota // counters/port_xmit_discards
	srcHWCounters                    // ports/N/hw_counters
	srcEthtool                       // ethtool -S
	srcNetdevStats                   // class/net/<netdev>/statistics
)

// ReadRNICSysfsFrom reads n's counters under sysroot (normally /sys) so tests
// can inject a tree.
//
// Pause duration comes from hw_counters, else ethtool -S. Drops are the sum of
// port_xmit_discards, hw_counters and ethtool -S, with the netdev statistics
// as a fallback when none of those has one. Each family records the sources
// it was read from, so a counter that could not be read is told apart from
// one that reads 0. An InfiniBand port has no PFC, so its pause duration is
// never looked for.
func ReadRNICSysfsFrom(ctx context.Context, sysroot string, n model.RNIC) (model.RNICCounters, error) {
	port := firstActiveIBPort(sysroot, n.Device)
	portDir := filepath.Join(sysroot, "class/infiniband", n.Device, "ports", port)
	base := filepath.Join(portDir, "counters")
	c := model.RNICCounters{Device: n.Device, Netdev: n.Netdev}
	ibRx, ibRxOK := readU64OK(filepath.Join(base, "port_rcv_data"))
	ibTx, ibTxOK := readU64OK(filepath.Join(base, "port_xmit_data"))
	usedIB := ibRxOK || ibTxOK
	if usedIB {
		c.RxBytes = ibRx * 4
		c.TxBytes = ibTx * 4
	} else if n.Netdev != "" {
		st := filepath.Join(sysroot, "class/net", n.Netdev, "statistics")
		c.RxBytes = readU64(filepath.Join(st, "rx_bytes"))
		c.TxBytes = readU64(filepath.Join(st, "tx_bytes"))
	}
	if v, ok := readU64OK(filepath.Join(base, "port_xmit_discards")); ok {
		c.TxDrops = v
		c.DropSources |= srcIBDiscards
	}
	ib := isInfiniBandLinkLayer(readFile(filepath.Join(portDir, "link_layer")))
	hwPause, hwDrops := scanCounterDir(filepath.Join(portDir, "hw_counters"), &c, !ib)
	if hwPause {
		c.PauseSources |= srcHWCounters
	}
	if hwDrops {
		c.DropSources |= srcHWCounters
	}
	var ethtoolWhy string
	switch {
	case ib:
		c.PauseGap = "link layer is InfiniBand, which has no PFC"
	case n.Netdev == "":
		if !hwPause {
			c.PauseGap = "no hw_counters *pause*duration*, no netdev for ethtool -S"
		}
	default:
		// Consulted even when hw_counters has the pause duration: the
		// *_discards_phy drops are only exposed through ethtool.
		res := ethtool.scan(ctx, n.Netdev, &c, !hwPause)
		if err := ctx.Err(); err != nil {
			// A read cut short by the caller is no reading at all.
			return c, err
		}
		if res.pause {
			c.PauseSources |= srcEthtool
		}
		if res.drops {
			c.DropSources |= srcEthtool
		}
		ethtoolWhy = res.why
		if c.PauseSources == 0 {
			c.PauseGap = "no hw_counters *pause*duration*, " + res.why
		}
	}
	if c.DropSources == 0 && n.Netdev != "" {
		if _, ok := scanCounterDir(filepath.Join(sysroot, "class/net", n.Netdev, "statistics"), &c, false); ok {
			c.DropSources |= srcNetdevStats
		}
	}
	if c.DropSources == 0 {
		c.DropGap = "no port_xmit_discards, hw_counters or netdev statistics drop counter"
		if ethtoolWhy != "" {
			c.DropGap += ", " + ethtoolWhy
		}
	}
	if !usedIB && c.RxBytes == 0 && c.TxBytes == 0 && c.TxPauseDurationUs == 0 {
		return c, errors.New("no RNIC counters readable for " + n.Device)
	}
	return c, nil
}

// isInfiniBandLinkLayer reads a ports/N/link_layer value. A missing file is
// not InfiniBand, so the pause sources are still looked for.
func isInfiniBandLinkLayer(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "InfiniBand")
}

func (s SysfsSampler) GPUs(ctx context.Context) ([]model.GPUStatus, error) {
	return ReadGPUs(ctx, s.Graph, s.GPUUtilInterval), nil
}

// Bus reads GPU PCIe utilization through nvidia-smi, refreshed at most once
// per GPUUtilInterval, and memory-link and inter-socket utilization from the
// uncore IMC and UPI counters over the last poll interval when Uncore is set.
// RNIC PCIe utilization comes from the monitor's counter delta
// (Decision.Util).
func (s SysfsSampler) Bus(ctx context.Context) map[string]float64 {
	out := s.Uncore.Util()
	if out == nil {
		out = map[string]float64{}
	}
	if hasGPU(s.Graph) {
		every := s.GPUUtilInterval
		if every <= 0 {
			every = DefaultGPUUtilInterval
		}
		for id, u := range gpuPCIeUtil(s.Graph, gpuPCIe.get(ctx, every)) {
			out[id] = u
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func hasGPU(g topology.Graph) bool {
	for _, e := range g.Endpoints {
		if e.Kind == model.EndpointGPU {
			return true
		}
	}
	return false
}

// ReadGPUs reports per-GPU busy state. The nvidia-smi query is refreshed at
// most once per gpuUtilInterval (zero selects DefaultGPUUtilInterval); the
// first call of the process always samples, so --once is never served stale.
func ReadGPUs(ctx context.Context, g topology.Graph, gpuUtilInterval time.Duration) []model.GPUStatus {
	if gpuUtilInterval <= 0 {
		gpuUtilInterval = DefaultGPUUtilInterval
	}
	smi := gpuUtil.sample(ctx, gpuUtilInterval)
	var out []model.GPUStatus
	for _, e := range g.Endpoints {
		if e.Kind != model.EndpointGPU {
			continue
		}
		// DRM (AMD) GPUs have no compute mode; theirs stays unknown.
		st := model.GPUStatus{ID: e.ID, Unknown: true, ComputeMode: smi.mode[normalizeBDF(e.PCIeBDF)]}
		if u, ok := smi.util[normalizeBDF(e.PCIeBDF)]; ok {
			st.Unknown = false
			st.Util = u
			st.Idle = u < 5
			out = append(out, st)
			continue
		}
		if u, ok := drmBusy(e.PCIeBDF); ok {
			st.Unknown = false
			st.Util = u
			st.Idle = u < 5
			out = append(out, st)
			continue
		}
		out = append(out, st)
	}
	return out
}

// nvidiaSample is one nvidia-smi query, keyed by normalized BDF. A GPU whose
// utilization or compute mode could not be parsed is absent from that map:
// unknown, never 0 or Default.
type nvidiaSample struct {
	util map[string]float64
	mode map[string]string
}

func nvidiaGPUsByBDF(ctx context.Context, run runner) nvidiaSample {
	b, err := run(ctx, "nvidia-smi", "--query-gpu=pci.bus_id,utilization.gpu,compute_mode", "--format=csv,noheader,nounits")
	if err != nil {
		return nvidiaSample{}
	}
	return parseNvidiaGPUs(string(b))
}

// parseNvidiaGPUs reads "bus_id, util, compute_mode" lines. Each column is
// parsed on its own, so a GPU whose utilization reads [N/A] still reports its
// compute mode and the reverse.
func parseNvidiaGPUs(out string) nvidiaSample {
	s := nvidiaSample{util: map[string]float64{}, mode: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		bdf := normalizeBDF(parts[0])
		if u, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
			s.util[bdf] = u
		}
		if len(parts) >= 3 {
			if m := computeMode(parts[2]); m != "" {
				s.mode[bdf] = m
			}
		}
	}
	return s
}

// computeMode canonicalizes an nvidia-smi compute_mode cell. Anything it does
// not recognise ([N/A], [Not Supported], garbage) is "" (unknown), which the
// engine treats as no reason to skip.
func computeMode(cell string) string {
	switch strings.ToLower(strings.TrimSpace(cell)) {
	case "default":
		return model.ComputeModeDefault
	case "exclusive_process":
		return model.ComputeModeExclusiveProcess
	case "exclusive_thread":
		return model.ComputeModeExclusiveThread
	case "prohibited":
		return model.ComputeModeProhibited
	}
	return ""
}

func drmBusy(bdf string) (float64, bool) {
	ents, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return 0, false
	}
	want := normalizeBDF(bdf)
	for _, e := range ents {
		dev := filepath.Join("/sys/class/drm", e.Name(), "device")
		real, err := filepath.EvalSymlinks(dev)
		if err != nil {
			continue
		}
		if normalizeBDF(filepath.Base(real)) != want {
			continue
		}
		if u, ok := readU64OK(filepath.Join(dev, "gpu_busy_percent")); ok {
			return float64(u), true
		}
	}
	return 0, false
}

func normalizeBDF(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	parts := strings.Split(s, ":")
	if len(parts) >= 3 {
		return strings.Join(parts[len(parts)-2:], ":")
	}
	return strings.TrimPrefix(s, "0000:")
}

func firstActiveIBPort(sysroot, ibdev string) string {
	for _, p := range []string{"1", "2", "3", "4"} {
		st := strings.ToUpper(readFile(filepath.Join(sysroot, "class/infiniband", ibdev, "ports", p, "state")))
		if strings.Contains(st, "ACTIVE") || strings.HasPrefix(strings.TrimSpace(st), "4:") {
			return p
		}
	}
	return "1"
}

// looksLikePauseDuration matches the vendor *pause*duration* counters, which
// are microseconds spent paused rather than a PFC frame count.
func looksLikePauseDuration(name string) bool {
	return strings.Contains(name, "pause") && strings.Contains(name, "duration")
}

// looksLikeDrop reports whether a counter counts toward RNICCounters drops.
//
// Included: out_of_buffer (no receive WQE was posted), anything containing
// "discard" (port_xmit_discards, the Mellanox rx_discards_phy /
// rx_prioN_buf_discard family), anything containing "dropped" (the netdev
// rx_dropped / tx_dropped statistics) and rx_missed_errors (the ring
// overflowed). These all mean a packet was thrown away for lack of a buffer
// or by policy, which is what the monitor's drop trigger is about.
//
// Excluded by name: symbol_error, port_rcv_errors and
// port_rcv_constraint_errors. Those count corrupt or malformed frames — a
// link-integrity fault, not congestion — and folding them in would fire a
// busy-mode run on a dirty cable forever, since the counter never resets.
//
// This is the union of what the Go and Zig readers used to count separately;
// the Zig reader is gone, so this is now the only definition of "a drop".
func looksLikeDrop(name string) bool {
	switch name {
	case "port_rcv_errors", "port_rcv_constraint_errors", "symbol_error":
		return false
	case "out_of_buffer", "rx_missed_errors":
		return true
	}
	return strings.Contains(name, "discard") || strings.Contains(name, "dropped")
}

// isTxCounter classifies a counter name as transmit-side; everything else is
// counted as receive-side.
func isTxCounter(name string) bool {
	return strings.HasPrefix(name, "tx_") ||
		strings.HasPrefix(name, "port_xmit") ||
		strings.Contains(name, "_tx_")
}

// pauseAccum keeps global and per-priority pause durations apart: a device
// that reports both would otherwise double-count, so the per-priority sum is
// only used when there is no global counter.
type pauseAccum struct{ txGlobal, txPrio, rxGlobal, rxPrio uint64 }

func (a *pauseAccum) add(name string, v uint64) {
	prio := strings.Contains(name, "prio")
	switch {
	case isTxCounter(name) || strings.Contains(name, "tx"):
		if prio {
			a.txPrio += v
		} else {
			a.txGlobal += v
		}
	case prio:
		a.rxPrio += v
	default:
		a.rxGlobal += v
	}
}

func (a *pauseAccum) flush(c *model.RNICCounters) {
	if a.txGlobal > 0 {
		c.TxPauseDurationUs += a.txGlobal
	} else {
		c.TxPauseDurationUs += a.txPrio
	}
	if a.rxGlobal > 0 {
		c.RxPauseDurationUs += a.rxGlobal
	} else {
		c.RxPauseDurationUs += a.rxPrio
	}
}

// scanCounterDir sums the drop counters, and the pause-duration counters when
// pause is set, in one sysfs directory, and reports which of the two families
// had at least one readable counter there. It serves both the InfiniBand
// hw_counters directory and the netdev statistics fallback.
func scanCounterDir(dir string, c *model.RNICCounters, pause bool) (pauseFound, dropFound bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false, false
	}
	var acc pauseAccum
	for _, e := range ents {
		name := strings.ToLower(e.Name())
		v, ok := readU64OK(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		switch {
		case looksLikePauseDuration(name):
			if pause {
				pauseFound = true
				acc.add(name, v)
			}
		case looksLikeDrop(name):
			dropFound = true
			if isTxCounter(name) {
				c.TxDrops += v
			} else {
				c.RxDrops += v
			}
		}
	}
	acc.flush(c)
	return pauseFound, dropFound
}

// ethtoolResult is what one ethtool.scan contributed. why says, when pause is
// false, why ethtool gave no pause duration; it ends up in the log.
type ethtoolResult struct {
	pause, drops bool
	why          string
}

const ethtoolGaveNone = "ethtool -S gave none"

// scan adds ethtool -S drops, and pause durations when pause is set, unless
// this netdev is in backoff after failing or reporting neither of them. Drops
// are read here because the Mellanox rx_discards_phy / tx_discards_phy family
// is only exposed through ethtool, never under hw_counters.
func (g *ethtoolGate) scan(ctx context.Context, netdev string, c *model.RNICCounters, pause bool) ethtoolResult {
	if netdev == "" {
		return ethtoolResult{why: "no netdev for ethtool -S"}
	}
	if !g.allow(netdev) {
		return ethtoolResult{why: "ethtool -S backed off (" + g.lastWhy(netdev) + ")"}
	}
	b, err := g.runner()(ctx, "ethtool", "-S", netdev)
	if err != nil && ctx.Err() != nil {
		// The caller canceled (shutdown) or ran out of time: that says
		// nothing about the netdev, so no backoff and no reason. Only the
		// command's own bound, with ctx still live, is a failure.
		return ethtoolResult{}
	}
	if err != nil {
		why := "ethtool -S failed: " + err.Error()
		if errors.Is(err, exec.ErrNotFound) {
			why = "ethtool not on PATH"
		}
		g.failed(netdev, why)
		return ethtoolResult{why: why}
	}
	var res ethtoolResult
	var acc pauseAccum
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		k, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(k))
		switch {
		case looksLikePauseDuration(name):
			res.pause = true
			acc.add(name, v)
		case looksLikeDrop(name):
			res.drops = true
			if isTxCounter(name) {
				c.TxDrops += v
			} else {
				c.RxDrops += v
			}
		}
	}
	if !res.pause && !res.drops {
		// Neither pause nor drop stats on this device; stop forking for it.
		g.failed(netdev, ethtoolGaveNone)
		return ethtoolResult{why: ethtoolGaveNone}
	}
	g.succeeded(netdev)
	if !res.pause {
		res.why = ethtoolGaveNone
	}
	if !pause {
		// hw_counters already has the pause duration; only drops are wanted.
		res.pause = false
		return res
	}
	acc.flush(c)
	return res
}

func readU64(path string) uint64 {
	v, _ := readU64OK(path)
	return v
}

func readU64OK(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

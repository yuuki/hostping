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
	"github.com/yuuki/hostping/internal/pci"
	"github.com/yuuki/hostping/internal/sysfs"
	"github.com/yuuki/hostping/internal/topology"
)

// SysfsSampler reads InfiniBand / netdev counters and GPU busy state.
// Pause duration is taken from vendor *pause*duration* files or ethtool -S
// (ConnectX tx_prioN_pause_duration, microseconds). Drops are whatever
// looksLikeDrop accepts: discards, dropped and out_of_buffer, never symbol or
// receive errors.
type SysfsSampler struct {
	Graph topology.Graph
	// Sysroot is the sysfs mount the counters are read under; empty means
	// /sys. Tests point it at a fixture tree.
	Sysroot string
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
	return ReadRNICSysfsFrom(ctx, s.sysroot(), n)
}

// sysroot is Sysroot, or /sys when unset.
func (s SysfsSampler) sysroot() string {
	if s.Sysroot != "" {
		return s.Sysroot
	}
	return "/sys"
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
	ibRx, ibRxOK := sysfs.ReadUint64OK(filepath.Join(base, "port_rcv_data"))
	ibTx, ibTxOK := sysfs.ReadUint64OK(filepath.Join(base, "port_xmit_data"))
	usedIB := ibRxOK || ibTxOK
	if usedIB {
		c.RxBytes = ibRx * 4
		c.TxBytes = ibTx * 4
	} else if n.Netdev != "" {
		st := filepath.Join(sysroot, "class/net", n.Netdev, "statistics")
		c.RxBytes, _ = sysfs.ReadUint64OK(filepath.Join(st, "rx_bytes"))
		c.TxBytes, _ = sysfs.ReadUint64OK(filepath.Join(st, "tx_bytes"))
	}
	if v, ok := sysfs.ReadUint64OK(filepath.Join(base, "port_xmit_discards")); ok {
		c.TxDrops = v
		c.DropSources |= srcIBDiscards
	}
	ib := isInfiniBandLinkLayer(sysfs.ReadString(filepath.Join(portDir, "link_layer")))
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
	return ReadGPUsFrom(ctx, s.sysroot(), s.Graph, s.GPUUtilInterval), nil
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

// ReadGPUs reports per-GPU busy state from the live /sys and nvidia-smi.
// The nvidia-smi query is refreshed at most once per gpuUtilInterval (zero
// selects DefaultGPUUtilInterval); the first call of the process always
// samples, so --once is never served stale.
func ReadGPUs(ctx context.Context, g topology.Graph, gpuUtilInterval time.Duration) []model.GPUStatus {
	return ReadGPUsFrom(ctx, "/sys", g, gpuUtilInterval)
}

// ReadGPUsFrom is ReadGPUs with the DRM busy counters read under sysroot
// (normally /sys) so tests can inject a tree.
func ReadGPUsFrom(ctx context.Context, sysroot string, g topology.Graph, gpuUtilInterval time.Duration) []model.GPUStatus {
	if gpuUtilInterval <= 0 {
		gpuUtilInterval = DefaultGPUUtilInterval
	}
	smi := gpuUtil.get(ctx, gpuUtilInterval)
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
		if u, ok := drmBusy(sysroot, e.PCIeBDF); ok {
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
		if bdf == "" {
			continue
		}
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

// drmBusy is the gpu_busy_percent of the DRM (AMD) GPU at bdf under sysroot:
// the class/drm entry whose device link resolves to that PCI address, matched
// on the full domain:bus:dev.fn so that two GPUs at the same bus:dev.fn in
// different PCI domains are told apart. A GPU without the file, or none at
// that address, is unknown.
func drmBusy(sysroot, bdf string) (float64, bool) {
	want := normalizeBDF(bdf)
	if want == "" {
		return 0, false
	}
	drm := filepath.Join(sysroot, "class/drm")
	ents, err := os.ReadDir(drm)
	if err != nil {
		return 0, false
	}
	for _, e := range ents {
		dev := filepath.Join(drm, e.Name(), "device")
		real, err := filepath.EvalSymlinks(dev)
		if err != nil {
			continue
		}
		if normalizeBDF(filepath.Base(real)) != want {
			continue
		}
		if u, ok := sysfs.ReadUint64OK(filepath.Join(dev, "gpu_busy_percent")); ok {
			return float64(u), true
		}
	}
	return 0, false
}

// normalizeBDF is pci.CanonicalBDF: the sysfs spelling of a PCI address,
// lower case with a four-digit domain, so nvidia-smi's "00000000:3C:00.0",
// sysfs's "0000:3c:00.0" and a bare "3c:00.0" (domain 0) all meet, while
// "0001:3c:00.0" stays apart from them. It is "" for anything that is not a
// BDF. The GPU lookups used to drop the domain, which made two GPUs at the
// same bus:dev.fn in different PCI domains collide.
func normalizeBDF(s string) string {
	return pci.CanonicalBDF(s)
}

// firstActiveIBPort is sysfs.FirstActiveIBPort for ibdev, the port topology
// discovery also picks (first ACTIVE by number, else the lowest-numbered),
// so the counters are read from the port whose rate and rail key the graph
// carries. It is "1" when the ports directory cannot be listed or is empty,
// so the read still attempts port 1 as it always has.
func firstActiveIBPort(sysroot, ibdev string) string {
	port, ok := sysfs.FirstActiveIBPort(sysroot, ibdev)
	if !ok {
		return "1"
	}
	return port
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

// classifyCounter files one counter (lower-cased name, value) with the family
// it belongs to: a pause duration into acc, a drop into c's Tx or Rx drops,
// anything else nowhere. It reports which family took it.
func classifyCounter(name string, v uint64, c *model.RNICCounters, acc *pauseAccum) (pause, drop bool) {
	switch {
	case looksLikePauseDuration(name):
		acc.add(name, v)
		return true, false
	case looksLikeDrop(name):
		if isTxCounter(name) {
			c.TxDrops += v
		} else {
			c.RxDrops += v
		}
		return false, true
	}
	return false, false
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
		v, ok := sysfs.ReadUint64OK(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		p, d := classifyCounter(strings.ToLower(e.Name()), v, c, &acc)
		pauseFound = pauseFound || (p && pause)
		dropFound = dropFound || d
	}
	if pause {
		acc.flush(c)
	}
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
	out, res, ok := g.stats(ctx, netdev)
	if !ok {
		return res
	}
	var acc pauseAccum
	res.pause, res.drops = parseEthtoolStats(string(out), c, &acc)
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

// stats is the output of ethtool -S netdev. When netdev is in backoff or the
// command fails, ok is false and res carries the reason, with a failure put
// into backoff; a read the caller cut short (ctx done) says nothing about
// the netdev, so it gets neither a backoff nor a reason.
func (g *ethtoolGate) stats(ctx context.Context, netdev string) (out []byte, res ethtoolResult, ok bool) {
	if !g.allow(netdev) {
		return nil, ethtoolResult{why: "ethtool -S backed off (" + g.lastWhy(netdev) + ")"}, false
	}
	b, err := g.runner()(ctx, "ethtool", "-S", netdev)
	if err != nil && ctx.Err() != nil {
		// The caller canceled (shutdown) or ran out of time: that says
		// nothing about the netdev, so no backoff and no reason. Only the
		// command's own bound, with ctx still live, is a failure.
		return nil, ethtoolResult{}, false
	}
	if err != nil {
		why := "ethtool -S failed: " + err.Error()
		if errors.Is(err, exec.ErrNotFound) {
			why = "ethtool not on PATH"
		}
		g.failed(netdev, why)
		return nil, ethtoolResult{why: why}, false
	}
	return b, ethtoolResult{}, true
}

// parseEthtoolStats files each "name: value" line of ethtool -S output with
// classifyCounter and reports which families the output had: drops go into
// c as they are read, pause durations into acc for the caller to flush.
func parseEthtoolStats(out string, c *model.RNICCounters, acc *pauseAccum) (pause, drops bool) {
	for _, line := range strings.Split(out, "\n") {
		k, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			continue
		}
		p, d := classifyCounter(strings.ToLower(strings.TrimSpace(k)), v, c, acc)
		pause = pause || p
		drops = drops || d
	}
	return pause, drops
}

// readTrim is the trimmed contents of path, "" when unreadable.
func readTrim(path string) string { return strings.TrimSpace(sysfs.ReadString(path)) }

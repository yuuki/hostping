package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yuuki/hostping/internal/baseline"
	"github.com/yuuki/hostping/internal/model"
)

// Thresholds and intervals from NSDI'23 §5 Implementation.
type Config struct {
	// ThpLowFraction of RNIC line rate; all RNICs below this (and GPUs idle) → idle engine run.
	ThpLowFraction float64
	// PFCHigh is Tx pause duration ratio that immediately starts the engine (0.03 = 30ms/s).
	PFCHigh float64
	// AbnormalTh is the fractional drop vs baseline (or vs sibling paths) that marks a path abnormal.
	AbnormalTh float64
	// LatencyFloor is the least excess over the sibling median, on top of
	// AbnormalTh, a path's small-message latency must show to be judged
	// abnormal against its siblings (the sweep, the paired recheck and the
	// locality shift all apply it). A user latency baseline has no floor.
	// The default suits platforms whose healthy paths jitter by a few
	// hundred ns; one whose paths move by more between runs needs it raised,
	// at the cost of missing rises smaller than the floor. At most
	// MaxLatencyFloor.
	LatencyFloor time.Duration
	// UtilHigh marks a bus as overloaded.
	UtilHigh float64
	// CPUBusyHigh is the CPU busy fraction of a NUMA node (from /proc/stat,
	// over the monitor poll) at or above which its CPU package counts as
	// loaded: a path through that package that reads abnormal is uncertain
	// with cpu_busy instead, since CPU load contends for the package fabric
	// the path crosses. Zero disables the check.
	CPUBusyHigh    float64
	SmallMsgBytes  uint32
	LargeMsgBytes  uint32
	MutualMsgBytes uint32 // mutual-probe UD SEND payload
	// Iters is the number of small/large WRITE pairs one loopback probe
	// times. Too few and a path between two root ports or sockets reads at a
	// different level from run to run; a probe's setup (QP, MR) costs far
	// more than 512 iterations, so the default barely changes a run's time.
	// The Zig probe takes at most 512.
	Iters uint32
	// MutualIters is the number of round trips in one mutual burst. It is
	// separate from Iters because a burst against a pair that does not answer
	// takes MutualIters x MutualTimeout, which has to stay inside a daemon
	// shutdown (see docs/operations.md).
	MutualIters uint32
	// MemWarmup, when positive, has every host-memory loopback probe write
	// its NUMA-bound buffers from the CPU for this long right before the
	// timed iterations (zig/src/memwarm.zig), so a memory subsystem that
	// idles at a lower operating point is carrying traffic when the
	// measurement starts. Zero (the default) is off; at most MaxMemWarmup.
	// GPU endpoints are unaffected.
	MemWarmup      time.Duration
	IdleInterval   time.Duration
	MetricInterval time.Duration
	// GPUUtilInterval rate-limits the external GPU utilization query
	// (nvidia-smi), which is far too slow to fork every MetricInterval.
	GPUUtilInterval time.Duration
	// BusyCooldown rate-limits PFC/drop triggers. The engine's own loopback and
	// mutual probes land in the next counter delta, so an unthrottled busy
	// trigger re-arms itself every poll. Zero disables the cooldown.
	BusyCooldown time.Duration
	// BusyInterval starts a busy-mode run on a loaded host that raised
	// neither PFC nor drops, at most once per interval (and never inside
	// BusyCooldown). NSDI'23 §4.2 only measures a loaded host on PFC/drops,
	// which a bottleneck that costs throughput without filling the RNIC
	// receive buffer never produces. This goes beyond the paper, so it is
	// opt-in: zero (the default) disables it. Each run costs the §4.3 reduced
	// probe set, repeated BusySamples times on loaded RNICs.
	BusyInterval time.Duration
	// BusySamples is how many loopback readings a path takes on a loaded RNIC
	// before the median of them is classified. Busy mode judges such a path
	// against its siblings at the same distance rather than against the idle
	// baseline (§4.3), and with a single reading each the comparison is as
	// noisy as one probe: per-sample spread alone clears AbnormalTh and marks
	// healthy siblings abnormal. The repeated probes cost time on an RNIC that
	// is already carrying service traffic, which §4.3 wants kept small, so the
	// default is the smallest count that has a median and 1 turns the
	// repetition off. See Engine.busyRepeated for where it is spent.
	BusySamples uint32
	FlapWindow  int
	Timeout     time.Duration
	// TimeoutTh is Timeout_th of ToN'24 §IV-C-2: an RNIC whose mutual probes
	// time out more often than this across its pairs is flagged.
	TimeoutTh float64
	// MutualScope selects the RNIC pairs the mutual probe measures: rail (the
	// default) pairs only RNICs sharing a rail, host pairs every RNIC of the
	// host, across rails too. Host scope only makes sense when the rails route
	// to each other through the ToR or fabric.
	MutualScope model.MutualScope
	// MutualInfiniBand lets RNICs whose port link layer is InfiniBand into the
	// mutual probe, paired by subnet prefix ("ib:" rail key) under rail scope.
	// Off by default in every scope: separate InfiniBand fabrics commonly all
	// keep the default subnet prefix, so the prefix does not tell them apart,
	// and pairs across them time out as false connectivity failures.
	MutualInfiniBand bool
	// MutualTransport is the live mutual probe's verbs transport. rc (the
	// default) is the RC ping-pong known to work on hardware, whose
	// retransmission hides isolated loss; ud is the lossy UD probe ToN'24
	// §IV-A-2 needs, not yet exercised on RDMA hardware. A UD bug would read
	// as timeouts on every pair — false connectivity and ToR causes — so it
	// is opt-in, and the schedule (MutualInterval) requires it.
	MutualTransport model.MutualTransport

	// MemCapacity and UPICapacity replace the nominal capacity discovery
	// assumes for memory (mem.N) and inter-socket (upi.A-B) links, so bus
	// utilization can be computed against them (NSDI'23 §4.1.2).
	MemCapacity LinkCapacity
	UPICapacity LinkCapacity
	// UncoreUtil reads memory traffic from the Intel uncore IMC PMUs and
	// inter-socket traffic from the UPI PMUs (perf_event_open) to give mem.N
	// and upi.A-B links a live utilization. Opt-in: it needs CAP_PERFMON or
	// perf_event_paranoid <= 0, and a MemCapacity / UPICapacity.
	UncoreUtil bool
	// UPIBytesPerFlit, when positive, is the bytes of data one UPI data-flit
	// count carries, for a processor whose ratio hostping has not measured.
	// Zero uses the measured per-model ratio; at most MaxUPIBytesPerFlit.
	UPIBytesPerFlit float64
	// MutualTimeout is how long one mutual probe waits for its reply before
	// it counts as a timeout (ToN'24 §IV-A-2: 500 ms). Timeout bounds a whole
	// loopback probe and is far too long for this.
	MutualTimeout time.Duration
	// MutualInterval is the daemon's mutual probe schedule: every RNIC probes
	// one random peer this often (ToN'24 §IV-A-2: 100 ms). Zero, the default,
	// leaves the schedule off, and the engine probes each pair in a burst per
	// run as the one-shot diagnose does. It is opt-in until the UD transport
	// has been exercised on RDMA hardware: switched on by default, a transport
	// bug would read as lost probes on every host, a standing stream of false
	// intermittent-timeout causes and alarm-triggered runs.
	MutualInterval time.Duration
	// MutualWindow is the sliding window each pair's timeout ratio is
	// aggregated over (ToN'24 §IV-A-2: 20 s).
	MutualWindow time.Duration

	OTLPEndpoint string
	// OTLPInsecure is --otlp-insecure, honoured only when OTLPInsecureSet: unset,
	// the transport follows the endpoint's scheme, OTEL_EXPORTER_OTLP_INSECURE,
	// or plaintext for loopback and TLS otherwise (see internal/telemetry).
	OTLPInsecure    bool
	OTLPInsecureSet bool
	PrometheusAddr  string
	// MetricsWebConfig is a Prometheus exporter-toolkit web config file
	// (TLS, basic auth) for the /metrics listener. Empty: plain HTTP.
	MetricsWebConfig string
	// MetricsVerbDurations exports hostping.verb.duration. Its 2-3 series per
	// RNIC→endpoint path grow with RNICs × endpoints, so a host with many
	// RNICs can turn it off; the probes still time the verbs, and the report
	// and the gpu_slow_mr_reg diagnosis keep using them.
	MetricsVerbDurations bool
	ServiceName          string

	// MetricsSet / OTLPSet record that the user asked for an exporter explicitly
	// (--metrics / --otlp, or OTEL_EXPORTER_OTLP_ENDPOINT). One-shot runs only
	// export when they are set; see TelemetryExporters.
	MetricsSet bool
	OTLPSet    bool

	// ExcludeRNICs are RDMA devices (IB device name or PCI address) sysfs
	// discovery drops: never probed, paired, linked or exported. For devices
	// that are not data-path NICs, such as the HCA a host manages its GPU
	// fabric through. Discovery fails on an entry that matches no device.
	ExcludeRNICs []string

	Sim          bool
	SimFault     string
	TopologyName string
	TopologyFile string
	Once         bool

	// BaselineFile is a JSON object of per-distance idle baselines.
	BaselineFile string
	// BaselineSpecs are repeatable --baseline KEY=BW[,LAT] values.
	BaselineSpecs []string

	// ReportFormat is how a confirmed report is written to stdout. Empty
	// means the default for the run kind; see EffectiveReportFormat.
	ReportFormat ReportFormat
	// ReportFile, when set, receives every confirmed report as one compact
	// JSON line, appended (whatever ReportFormat says about stdout).
	ReportFile string

	// StateDir, when set, is where the daemon keeps its flapping streaks and
	// idle cadence across a restart (internal/state). One-shot runs never
	// read or write it.
	StateDir string
}

func Defaults() Config {
	return Config{
		ThpLowFraction: 0.05,
		PFCHigh:        0.03,
		AbnormalTh:     0.20,
		LatencyFloor:   500 * time.Nanosecond,
		UtilHigh:       0.90,
		CPUBusyHigh:    0.5,
		SmallMsgBytes:  1,
		LargeMsgBytes:  256 * 1024, // 256 KiB for 200 Gb/s RNICs (ToN'24 §IV-A-1; NSDI'23 §4.1.1 used 128 KiB)
		MutualMsgBytes: 1,          // ToN'24 sends 1 B "to reduce service overhead"
		Iters:          512,
		MutualIters:    64,
		IdleInterval:   5 * time.Minute,
		MetricInterval: time.Second,
		// nvidia-smi costs hundreds of ms; GPU util only gates the idle run.
		GPUUtilInterval:      10 * time.Second,
		BusyCooldown:         30 * time.Second,
		BusySamples:          3,
		FlapWindow:           3,
		Timeout:              5 * time.Second,
		TimeoutTh:            0.1,
		MutualScope:          model.MutualScopeRail,
		MutualTransport:      model.MutualTransportRC,
		MutualTimeout:        500 * time.Millisecond,
		MutualWindow:         20 * time.Second,
		PrometheusAddr:       ":9464",
		MetricsVerbDurations: true,
		ServiceName:          "hostping",
		TopologyName:         "fig2",
		OTLPEndpoint:         os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		OTLPSet:              os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "",
	}
}

// Parse parses args into the flag set and records which telemetry flags the user
// set explicitly.
func (c *Config) Parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "metrics":
			c.MetricsSet = true
		case "otlp":
			c.OTLPSet = true
		case "otlp-insecure":
			c.OTLPInsecureSet = true
		}
	})
	return nil
}

// TelemetryExporters reports the exporters this run should start. A one-shot run
// (hostpingctl diagnose, hostping --once) has nobody to scrape /metrics and no
// time for a periodic OTLP push, so it exports only what the user asked for
// explicitly; the daemon keeps the configured defaults.
func (c Config) TelemetryExporters() (prometheusAddr, otlpEndpoint string) {
	if !c.Once {
		return c.PrometheusAddr, c.OTLPEndpoint
	}
	if c.MetricsSet {
		prometheusAddr = c.PrometheusAddr
	}
	if c.OTLPSet {
		otlpEndpoint = c.OTLPEndpoint
	}
	return prometheusAddr, otlpEndpoint
}

func (c *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.BoolVar(&c.Sim, "sim", c.Sim, "run against the in-process intra-host simulator")
	fs.StringVar(&c.SimFault, "fault", c.SimFault, "simulator fault (e.g. rnic_pcie:rnic0, gpu_pcie:gpu0, upi, mesh:0-1, acs, ats, mem_flap:mem0, connect_fail:rnic0, flap:rnic0, tor, isolate:rnic3, compute_mode:gpu3, fast_rp:rp.0,rp.1)")
	fs.StringVar(&c.TopologyName, "topology", c.TopologyName, "built-in topology name (fig2; fig2-rails: the same host with one rail per RNIC; fig2-snc: the same host with sub-NUMA clustering, two NUMA nodes per package)")
	fs.StringVar(&c.TopologyFile, "topology-file", c.TopologyFile, "JSON topology file")
	fs.Var((*rnicList)(&c.ExcludeRNICs), "exclude-rnic", "RDMA device to leave out of discovery, by IB device name (mlx5_3) or PCI address (0000:3c:00.1), repeatable or comma-separated: never probed, paired or exported (e.g. a fabric-management HCA). A name that matches no device fails start-up")
	fs.StringVar(&c.PrometheusAddr, "metrics", c.PrometheusAddr, "Prometheus /metrics listen address (empty to disable; one-shot runs listen only when this flag is given)")
	fs.StringVar(&c.MetricsWebConfig, "metrics-web-config", c.MetricsWebConfig, "Prometheus exporter-toolkit web config file enabling TLS and/or basic auth on the --metrics listener (empty: plain HTTP)")
	fs.BoolVar(&c.MetricsVerbDurations, "metrics-verb-durations", c.MetricsVerbDurations, "export hostping.verb.duration (2-3 series per RNIC→endpoint path); false drops the series from /metrics and OTLP, while reports and the gpu_slow_mr_reg diagnosis keep the timings")
	fs.StringVar(&c.OTLPEndpoint, "otlp", c.OTLPEndpoint, "OTLP gRPC endpoint (or OTEL_EXPORTER_OTLP_ENDPOINT): host:port, or http:// (plaintext) / https:// (TLS) URL")
	fs.BoolVar(&c.OTLPInsecure, "otlp-insecure", c.OTLPInsecure, "plaintext OTLP for a host:port endpoint (unset: OTEL_EXPORTER_OTLP_INSECURE, else plaintext for loopback and TLS with the system roots otherwise)")
	fs.DurationVar(&c.IdleInterval, "idle-interval", c.IdleInterval, "idle-host full-mesh interval")
	fs.DurationVar(&c.MetricInterval, "metric-interval", c.MetricInterval, "RNIC counter poll interval")
	fs.DurationVar(&c.GPUUtilInterval, "gpu-util-interval", c.GPUUtilInterval, "minimum period between nvidia-smi GPU utilization queries")
	fs.DurationVar(&c.BusyCooldown, "busy-cooldown", c.BusyCooldown, "minimum interval between PFC/drop-triggered busy runs (0 disables)")
	fs.DurationVar(&c.BusyInterval, "busy-interval", c.BusyInterval, "periodic busy-mode run on a loaded host without PFC/drops, at most once per interval (0 disables; beyond NSDI'23 §4.2)")
	fs.Var((*uint32Value)(&c.BusySamples), "busy-samples", "loopback readings per path on a loaded RNIC in busy mode; the median is classified, so a single noisy sample no longer marks a healthy sibling abnormal (1 disables the repeat)")
	fs.BoolVar(&c.Once, "once", c.Once, "run the engine once and exit")
	fs.DurationVar(&c.Timeout, "timeout", c.Timeout, "per-probe timeout")
	fs.Float64Var(&c.TimeoutTh, "timeout-th", c.TimeoutTh, "mutual probe timeout ratio above which an RNIC is flagged (ToN'24 Timeout_th)")
	fs.Var((*mutualScope)(&c.MutualScope), "mutual-scope", "RNIC pairs the mutual probe measures: rail (only RNICs sharing an IPv4 subnet or GID rail) or host (every RNIC, across rails too; only meaningful when the rails route to each other)")
	fs.BoolVar(&c.MutualInfiniBand, "mutual-infiniband", c.MutualInfiniBand, "include InfiniBand-link-layer RNICs in the mutual probe, paired by subnet prefix (off by default: separate fabrics sharing the default prefix fe80::/64 would pair and time out)")
	fs.Var((*mutualTransport)(&c.MutualTransport), "mutual-transport", "live mutual probe transport: rc (RC ping-pong; retransmission hides isolated loss) or ud (UD request/reply, one lost packet is one timeout; required by --mutual-interval)")
	fs.DurationVar(&c.MutualTimeout, "mutual-timeout", c.MutualTimeout, "wait for one mutual probe's reply before it counts as a timeout (ToN'24: 500ms)")
	fs.DurationVar(&c.MutualInterval, "mutual-interval", c.MutualInterval, "daemon: every RNIC probes one random peer this often (opt-in, 0 = off: pairs are probed in a burst per engine run; ToN'24 recommends 100ms)")
	fs.DurationVar(&c.MutualWindow, "mutual-window", c.MutualWindow, "daemon: sliding window of the scheduled mutual probe's timeout ratio (ToN'24: 20s)")
	fs.Float64Var(&c.AbnormalTh, "abnormal-th", c.AbnormalTh, "path abnormality threshold")
	fs.Var(&boundedDuration{p: &c.LatencyFloor, max: MaxLatencyFloor}, "latency-floor", "least excess over the sibling median, on top of --abnormal-th, for a path's small-message latency to be judged abnormal against its siblings (sweep, recheck and locality shift); raise it where healthy paths jitter by more between runs (at most 10us; a set baseline LAT has no floor)")
	fs.Float64Var(&c.PFCHigh, "pfc-high", c.PFCHigh, "Tx pause duration ratio that triggers busy diagnosis")
	fs.Float64Var(&c.UtilHigh, "util-high", c.UtilHigh, "bus utilization that marks a link overloaded")
	fs.Var(&boundedFloat{p: &c.CPUBusyHigh, max: 1}, "cpu-busy-high", "CPU busy fraction of a NUMA node (/proc/stat, over the poll interval) at or above which a path through its CPU package that reads abnormal is reported uncertain (cpu_busy) instead (0 disables, at most 1)")
	fs.Var(&linkCapacityFlag{c: &c.MemCapacity, prefix: "mem."}, "mem-channel-capacity", "memory bandwidth of each mem.N link (all channels of one NUMA node, reads + writes), replacing the assumed constant (repeatable). BW for every link or mem.N=BW for one; units bps/Gbps (bits) or GB/s/GiB/s (bytes)")
	fs.Var(&linkCapacityFlag{c: &c.UPICapacity, prefix: "upi."}, "upi-capacity", "capacity of each upi.A-B inter-socket link, replacing the assumed constant (repeatable). BW for every link or upi.A-B=BW for one; units as --mem-channel-capacity")
	fs.BoolVar(&c.UncoreUtil, "uncore-util", c.UncoreUtil, "read memory-channel and inter-socket utilization from Intel uncore IMC and UPI PMUs via perf_event_open (Linux; needs CAP_PERFMON or perf_event_paranoid <= 0, and --mem-channel-capacity / --upi-capacity)")
	fs.Var(&boundedFloat{p: &c.UPIBytesPerFlit, max: MaxUPIBytesPerFlit}, "upi-bytes-per-flit", "bytes of data one UPI data-flit count (UNC_UPI_TxL_FLITS.ALL_DATA) carries, for a processor whose ratio hostping has not measured; measure it against a known cross-socket load (0 = the measured per-model ratio, at most 64)")
	fs.Float64Var(&c.ThpLowFraction, "thp-low", c.ThpLowFraction, "fraction of RNIC line rate below which the host counts as idle")
	fs.Var((*uint32Value)(&c.Iters), "iters", "loopback probe iterations (small/large WRITE pairs) per measurement, at most 512")
	fs.Var((*uint32Value)(&c.MutualIters), "mutual-iters", "round trips per mutual probe burst (a pair that does not answer costs this many --mutual-timeout)")
	fs.Var(&boundedDuration{p: &c.MemWarmup, max: MaxMemWarmup}, "mem-warmup", "before each host-memory loopback probe, write the probe's NUMA-bound buffers from the CPU for this long so the memory subsystem is awake when measuring (0 = off, at most 1s; GPU endpoints unaffected)")
	fs.Var((*uint32Value)(&c.SmallMsgBytes), "small-msg", "small-message size in bytes (latency probe)")
	fs.Var(&boundedUint32{p: &c.LargeMsgBytes, min: 1, max: MaxLargeMsgBytes}, "large-msg", "large-message size in bytes (bandwidth probe), at most 16 MiB")
	fs.Var(&boundedUint32{p: &c.MutualMsgBytes, min: 1, max: MaxMutualMsgBytes}, "mutual-msg", "mutual-probe UD SEND payload in bytes, 1 to 4096 and at most the port MTU")
	fs.IntVar(&c.FlapWindow, "flap-window", c.FlapWindow, "consecutive gray tests on a link that diagnose flapping")
	fs.Var((*baselineSpecs)(&c.BaselineSpecs), "baseline", "per-distance idle baseline (repeatable). KEY=BW[,LAT] e.g. local_mem=270Gbps,1400ns. Keys: same_switch, multi_switch, cross_root, cross_socket, local_mem, remote_mem. Units: bps/Gbps, ns/us. Path status is bandwidth-only; a LAT you set judges that distance's advisory idle latency_status (unset: same-distance siblings; the Fig.4 latencies are never used for it); same_switch latency also gates ACS/ATS. Unset distances keep Fig.4×line-rate defaults.")
	fs.Var((*ReportFormat)(&c.ReportFormat), "report-format", "how each report is written to stdout: json (one compact object per line), pretty (indented JSON) or none (default: pretty for one-shot runs, json for the daemon)")
	fs.StringVar(&c.ReportFile, "report-file", c.ReportFile, "append each report to this file as one JSON line (created 0640; reopened per report, so logrotate needs no signal)")
	fs.StringVar(&c.StateDir, "state-dir", c.StateDir, "daemon: keep the flapping streaks (--flap-window) and the idle-run cadence in DIR/state.json across a restart of the same boot and topology (created 0700 if missing; empty: kept in memory only; one-shot runs never touch it)")
	fs.StringVar(&c.BaselineFile, "baseline-file", c.BaselineFile, "JSON object of idle baselines (same keys as --baseline). Fields: bandwidth_bps or bandwidth (e.g. 270Gbps), latency_ns or latency (e.g. 1400ns). An optional \"tables\" object holds per-Fig.4-table sections (key line_rate=<N>Gbps[,pcie_hop=<N>Gbps]) that replace the top level for RNICs on that table. Unknown fields are rejected. Flags override the file, sections included.")
}

// EffectiveReportFormat is the stdout report format of this run: the one
// asked for, else pretty for a one-shot run (an operator reading a terminal)
// and json for the daemon (a log pipeline reading one entry per line). The
// default follows the run kind, not whether stdout is a terminal, so the same
// command line produces the same output under systemd, a pipe or ssh.
func (c Config) EffectiveReportFormat() ReportFormat {
	if c.ReportFormat != "" {
		return c.ReportFormat
	}
	if c.Once {
		return ReportFormatPretty
	}
	return ReportFormatJSON
}

// ValidateMutual rejects a mutual probe schedule that cannot work: the window
// must hold at least one round, a probe must be allowed to time out, and the
// schedule needs the UD transport. The combination with rc is rejected rather
// than silently switched to ud, so choosing the unvalidated transport stays an
// explicit decision.
func (c Config) ValidateMutual() error {
	if c.MutualTimeout <= 0 {
		return fmt.Errorf("--mutual-timeout must be positive, got %v", c.MutualTimeout)
	}
	if c.MutualInterval < 0 {
		return fmt.Errorf("--mutual-interval must not be negative, got %v", c.MutualInterval)
	}
	if c.MutualInterval > 0 && c.MutualTransport != model.MutualTransportUD {
		return fmt.Errorf("--mutual-interval needs --mutual-transport=ud: RC retransmission hides the isolated losses the scheduled probe counts (got --mutual-transport=%s)", c.MutualTransport)
	}
	if c.MutualInterval > 0 && c.MutualWindow < c.MutualInterval {
		return fmt.Errorf("--mutual-window %v is shorter than --mutual-interval %v", c.MutualWindow, c.MutualInterval)
	}
	return nil
}

// Overrides is the user-supplied baseline table (partial). Empty when nothing is set.
func (c Config) Overrides() (baseline.Overlay, error) {
	return baseline.Load(c.BaselineFile, c.BaselineSpecs)
}

// uint32Value is the probe-sized counterpart of flag's Uint64Var: the probe ABI
// takes uint32 iteration counts and message sizes, so an out-of-range value is
// a flag error, not a silent truncation.
type uint32Value uint32

func (v *uint32Value) String() string {
	if v == nil {
		return "0"
	}
	return strconv.FormatUint(uint64(*v), 10)
}

func (v *uint32Value) Set(s string) error {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return err
	}
	*v = uint32Value(n)
	return nil
}

// MaxLargeMsgBytes bounds --large-msg. Every loopback probe allocates and
// registers two buffers of this size (host DRAM, or GPU memory for a GDR
// endpoint) per path, so an unbounded value turns a typo into a large pinned
// allocation on every run. 16 MiB is 64x the default, far more than a 400 Gb/s
// transfer needs to dominate Tproc + Lat_host in Eq.1 - Eq.2.
const MaxLargeMsgBytes = 16 << 20

// MaxMutualMsgBytes bounds --mutual-msg: the Zig mutual probe sizes its
// registered buffers for at most 4 KiB (zig/src/verbs.zig), the largest RoCE
// MTU a single UD message can fill.
const MaxMutualMsgBytes = 4096

// boundedUint32 is a uint32Value that also rejects values outside [min, max],
// for probe sizes whose buffers have a fixed or sane upper limit.
type boundedUint32 struct {
	p        *uint32
	min, max uint32
}

func (v *boundedUint32) String() string {
	if v == nil || v.p == nil {
		return "0"
	}
	return strconv.FormatUint(uint64(*v.p), 10)
}

func (v *boundedUint32) Set(s string) error {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return err
	}
	if n < uint64(v.min) || n > uint64(v.max) {
		return fmt.Errorf("%d out of range [%d, %d]", n, v.min, v.max)
	}
	*v.p = uint32(n)
	return nil
}

// MaxMemWarmup bounds --mem-warmup: the warm-up runs once per host-memory
// path of every run, so a typo must not turn into seconds of CPU and memory
// traffic per path. The Zig probe applies the same cap.
const MaxMemWarmup = time.Second

// MaxLatencyFloor bounds --latency-floor. A small-message p50 is a few µs,
// so a floor this high already turns the sibling latency verdict off.
const MaxLatencyFloor = 10 * time.Microsecond

// MaxUPIBytesPerFlit bounds --upi-bytes-per-flit: one count never carries more
// than a 64-byte cache line.
const MaxUPIBytesPerFlit = 64

// boundedFloat is a float64 flag that rejects values outside [0, max].
type boundedFloat struct {
	p   *float64
	max float64
}

func (v *boundedFloat) String() string {
	if v == nil || v.p == nil {
		return "0"
	}
	return strconv.FormatFloat(*v.p, 'g', -1, 64)
}

func (v *boundedFloat) Set(s string) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	if !(f >= 0 && f <= v.max) {
		return fmt.Errorf("%v out of range [0, %v]", f, v.max)
	}
	*v.p = f
	return nil
}

// boundedDuration is a time.Duration flag that rejects values outside
// [0, max].
type boundedDuration struct {
	p   *time.Duration
	max time.Duration
}

func (v *boundedDuration) String() string {
	if v == nil || v.p == nil {
		return "0s"
	}
	return v.p.String()
}

func (v *boundedDuration) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if d < 0 || d > v.max {
		return fmt.Errorf("%v out of range [0s, %v]", d, v.max)
	}
	*v.p = d
	return nil
}

// mutualScope is the --mutual-scope flag value, restricted to the known scopes.
type mutualScope model.MutualScope

func (v *mutualScope) String() string {
	if v == nil {
		return ""
	}
	return string(*v)
}

func (v *mutualScope) Set(s string) error {
	switch model.MutualScope(s) {
	case model.MutualScopeRail, model.MutualScopeHost:
		*v = mutualScope(s)
		return nil
	}
	return fmt.Errorf("unknown mutual scope %q (want %s or %s)", s, model.MutualScopeRail, model.MutualScopeHost)
}

type mutualTransport model.MutualTransport

func (v *mutualTransport) String() string {
	if v == nil {
		return ""
	}
	return string(*v)
}

func (v *mutualTransport) Set(s string) error {
	switch model.MutualTransport(s) {
	case model.MutualTransportRC, model.MutualTransportUD:
		*v = mutualTransport(s)
		return nil
	}
	return fmt.Errorf("unknown mutual transport %q (want %s or %s)", s, model.MutualTransportRC, model.MutualTransportUD)
}

// rnicList is the repeatable --exclude-rnic flag; a value may also list
// several devices separated by commas.
type rnicList []string

func (l *rnicList) String() string {
	if l == nil {
		return ""
	}
	return strings.Join(*l, ",")
}

func (l *rnicList) Set(v string) error {
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			return fmt.Errorf("empty RNIC name in %q", v)
		}
		*l = append(*l, e)
	}
	return nil
}

type baselineSpecs []string

func (s *baselineSpecs) String() string {
	return strings.Join(*s, "; ")
}

func (s *baselineSpecs) Set(v string) error {
	if _, err := baseline.ParseSpecs(v); err != nil {
		return err
	}
	*s = append(*s, v)
	return nil
}

// ReportFormat is the --report-format flag value.
type ReportFormat string

const (
	// ReportFormatJSON is one compact JSON object per line.
	ReportFormatJSON ReportFormat = "json"
	// ReportFormatPretty is the indented JSON object.
	ReportFormatPretty ReportFormat = "pretty"
	// ReportFormatNone writes no report to stdout.
	ReportFormatNone ReportFormat = "none"
)

func (v *ReportFormat) String() string {
	if v == nil {
		return ""
	}
	return string(*v)
}

func (v *ReportFormat) Set(s string) error {
	switch ReportFormat(s) {
	case ReportFormatJSON, ReportFormatPretty, ReportFormatNone:
		*v = ReportFormat(s)
		return nil
	}
	return fmt.Errorf("unknown report format %q (want %s, %s or %s)", s, ReportFormatJSON, ReportFormatPretty, ReportFormatNone)
}

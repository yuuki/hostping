package telemetry

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/yuuki/hostping/internal/buildinfo"
	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
)

type Telemetry struct {
	provider *sdkmetric.MeterProvider
	http     *http.Server
	ln       net.Listener
	meter    metric.Meter

	loopLat  metric.Float64Gauge
	loopBW   metric.Float64Gauge
	pathAbn  metric.Int64Gauge
	pathStat metric.Int64Gauge
	pathWhy  metric.Int64Gauge
	pathLat  metric.Int64Gauge
	latWhy   metric.Int64Gauge
	latRuns  metric.Int64Gauge
	linkAbn  metric.Int64Gauge
	linkStat metric.Int64Gauge
	pause    metric.Float64Gauge
	drops    metric.Int64Gauge
	thp      metric.Float64Gauge
	cpuBusy  metric.Float64Gauge
	bus      metric.Float64Gauge
	mutualR  metric.Float64Gauge
	mutualL  metric.Float64Gauge
	mutualT  metric.Float64Gauge
	skipped  metric.Int64Gauge
	verbDur  metric.Float64Gauge
	// verbsOff drops hostping.verb.duration (--metrics-verb-durations=false).
	verbsOff bool
	diag     metric.Int64Counter
	runs     metric.Int64Counter
	idle     metric.Int64Gauge

	lastRun     metric.Float64Gauge
	lastSuccess metric.Float64Gauge
	runFailed   metric.Int64Counter
	runDiscard  metric.Int64Counter
	gateSkip    metric.Int64Counter
	pollErrors  metric.Int64Counter
	lastPoll    metric.Float64Gauge
	monIdle     metric.Int64Gauge
}

// Values of hostping.engine.run.failed's stage attribute.
const (
	// FailedEngine: the engine run returned an error.
	FailedEngine = "engine"
	// FailedReport: a confirmed report could not be written to every output.
	FailedReport = "report"
)

// Values of hostping.engine.run.discarded's reason attribute: what the clean
// poll after a held idle-mode run found.
const (
	// DiscardHostBusy: an RNIC or GPU was busy, or PFC pauses or drops moved.
	DiscardHostBusy = "host_busy"
	// DiscardIdleUnknown: idle state could not be read.
	DiscardIdleUnknown = "idle_unknown"
)

// Values of hostping.engine.idle_gate.skipped's reason attribute: why
// --idle-gate-command kept an idle run from starting.
const (
	// GateDenied: the command exited 1, the site's "not now".
	GateDenied = "denied"
	// GateTimeout: the command did not exit within --idle-gate-timeout.
	GateTimeout = "timeout"
	// GateError: the command exited with another non-zero status (it could
	// not decide, e.g. the scheduler could not be queried), could not be
	// started, or was killed by a signal.
	GateError = "error"
)

// gateReasons is the fixed attribute set of hostping.engine.idle_gate.skipped.
var gateReasons = []string{GateDenied, GateTimeout, GateError}

// failStages and discardReasons are the fixed attribute sets of the run
// outcome counters. Each series is written as 0 at start-up, so a rate() over
// it is defined from the first scrape rather than only after the first
// failure.
var (
	failStages     = []string{FailedEngine, FailedReport}
	discardReasons = []string{DiscardHostBusy, DiscardIdleUnknown}
)

// Option configures Start.
type Option func(*options)

type options struct {
	live      *Liveness
	rnics     []model.RNIC
	endpoints []model.Endpoint
	nodes     []model.TopologyNode
	edges     []model.TopologyEdge
}

// WithLiveness makes /healthz report l: 503 while the daemon's poll loop is
// stalled. Without it /healthz is 200 while the process serves.
func WithLiveness(l *Liveness) Option {
	return func(o *options) { o.live = l }
}

// endpointKinds is every model.EndpointKind. hostping.endpoint.skipped writes
// one sample per kind on every report, so a kind that stops being skipped falls
// back to 0 instead of leaving its last non-zero value behind. The failure
// reason is not an attribute: it is free-form probe text, so it would both blow
// up cardinality and strand a series on every reason that gets fixed. The
// reason is logged and carried in the report instead.
var endpointKinds = []model.EndpointKind{model.EndpointGPU, model.EndpointMemory}

// WithTopology exports hostping.rnic.info and hostping.endpoint.info for the
// discovered RNICs and endpoints. The slices are copied: the info series are
// fixed at start-up and every collection writes the same values.
func WithTopology(rnics []model.RNIC, endpoints []model.Endpoint) Option {
	return func(o *options) {
		o.rnics = append([]model.RNIC(nil), rnics...)
		o.endpoints = append([]model.Endpoint(nil), endpoints...)
	}
}

// WithTopologyDiagram exports hostping.topology.node.info and
// hostping.topology.edge.info for the host topology diagram (see
// topology.Graph.Diagram). Like WithTopology the slices are copied and the
// series fixed at start-up.
func WithTopologyDiagram(nodes []model.TopologyNode, edges []model.TopologyEdge) Option {
	return func(o *options) {
		o.nodes = append([]model.TopologyNode(nil), nodes...)
		o.edges = append([]model.TopologyEdge(nil), edges...)
	}
}

// Start builds the meter provider and its exporters. build is exported as
// hostping.build.info.
func Start(ctx context.Context, cfg config.Config, build buildinfo.Info, opts ...Option) (*Telemetry, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithAttributes(semconv.ServiceName(cfg.ServiceName)),
	)
	if err != nil {
		return nil, err
	}

	promExp, err := prometheus.New()
	if err != nil {
		return nil, err
	}
	popts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExp),
	}
	if cfg.OTLPEndpoint != "" {
		var flag *bool
		if cfg.OTLPInsecureSet {
			flag = &cfg.OTLPInsecure
		}
		tr, err := resolveOTLPTransport(cfg.OTLPEndpoint, flag, os.Getenv)
		if err != nil {
			return nil, err
		}
		otlp, err := otlpmetricgrpc.New(ctx, tr.options(os.Getenv)...)
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		log.Printf("hostping otlp exporter endpoint=%s tls=%v (%s)", tr.endpoint, !tr.insecure, tr.reason)
		popts = append(popts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(otlp, sdkmetric.WithInterval(10*time.Second))))
	}
	provider := sdkmetric.NewMeterProvider(popts...)

	fail := func(err error) (*Telemetry, error) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = provider.Shutdown(sctx)
		return nil, err
	}
	t, err := New(provider)
	if err != nil {
		return fail(err)
	}
	t.verbsOff = !cfg.MetricsVerbDurations
	if cfg.IdleGateCommand != "" && !cfg.Once {
		t.initIdleGateSeries()
	}
	if err := t.registerBuildInfo(build); err != nil {
		return fail(err)
	}
	if err := t.registerTopologyInfo(o.rnics, o.endpoints); err != nil {
		return fail(err)
	}
	if err := t.registerDiagramInfo(o.nodes, o.edges); err != nil {
		return fail(err)
	}
	if cfg.PrometheusAddr != "" {
		// The web config (TLS, basic auth) and the address are both checked
		// here rather than in the serving goroutine: a bad file or an address
		// that cannot be bound fails the start (and systemd restarts the
		// unit) instead of leaving a daemon running without its main output.
		if err := web.Validate(cfg.MetricsWebConfig); err != nil {
			return fail(fmt.Errorf("--metrics-web-config %s: %w", cfg.MetricsWebConfig, err))
		}
		ln, err := net.Listen("tcp", cfg.PrometheusAddr)
		if err != nil {
			return fail(fmt.Errorf("metrics listener: %w", err))
		}
		t.ln = ln
		t.http = newMetricsServer(o.live)
		webConfig := cfg.MetricsWebConfig
		go func() {
			err := web.Serve(ln, t.http, &web.FlagConfig{WebConfigFile: &webConfig}, slog.Default())
			if err != nil && err != http.ErrServerClosed {
				log.Printf("hostping metrics server %s: %v", cfg.PrometheusAddr, err)
			}
		}()
	}
	// Installed last, so a failed start leaves the global provider alone.
	otel.SetMeterProvider(provider)
	return t, nil
}

// Metrics server timeouts. A slow or stalled client cannot hold a connection
// open indefinitely (gosec G112), while a large /metrics scrape still has a
// minute to be written.
const (
	metricsReadHeaderTimeout = 10 * time.Second
	metricsReadTimeout       = 30 * time.Second
	metricsWriteTimeout      = time.Minute
	metricsIdleTimeout       = 2 * time.Minute
)

// New records to provider's readers without starting any exporter or
// listener: Start's instruments, for a caller (or a test) that owns the
// provider. Shutting the provider down is the caller's.
func New(provider *sdkmetric.MeterProvider) (*Telemetry, error) {
	t := &Telemetry{provider: provider, meter: provider.Meter("github.com/yuuki/hostping")}
	if err := t.initInstruments(); err != nil {
		return nil, err
	}
	return t, nil
}

// newMetricsServer serves /metrics and /healthz. live may be nil (a one-shot
// run, which has no poll loop): /healthz is then 200 while the process serves.
func newMetricsServer(live *Liveness) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/healthz", live)
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
		ReadTimeout:       metricsReadTimeout,
		WriteTimeout:      metricsWriteTimeout,
		IdleTimeout:       metricsIdleTimeout,
	}
}

// Addr is the metrics listener's bound address, or "" without one. With
// --metrics :0 it carries the port the kernel picked.
func (t *Telemetry) Addr() string {
	if t == nil || t.ln == nil {
		return ""
	}
	return t.ln.Addr().String()
}

func (t *Telemetry) initInstruments() error {
	var err error
	// The probe produces exactly one summary value per path per run, so a
	// histogram would only ever bucket p50s (its count is a run count, not a
	// sample count). A gauge reports the last measured p50 honestly.
	t.loopLat, err = t.meter.Float64Gauge("hostping.loopback.latency", metric.WithUnit("us"), metric.WithDescription("Intra-host loopback latency, small-message p50"))
	if err != nil {
		return err
	}
	t.loopBW, err = t.meter.Float64Gauge("hostping.loopback.bandwidth", metric.WithUnit("bit/s"), metric.WithDescription("Intra-host loopback bandwidth from Eq.1"))
	if err != nil {
		return err
	}
	t.pathAbn, err = t.meter.Int64Gauge("hostping.path.abnormal", metric.WithDescription("1 if RNIC→endpoint path is abnormal"))
	if err != nil {
		return err
	}
	// hostping.path.abnormal is binary, so normal and uncertain are both 0 and
	// a path hostping never judged is indistinguishable from a healthy one. It
	// keeps its meaning (alerts are built on the name); the full status, and
	// why an uncertain path is uncertain, live in the code gauges below. Each
	// is one series per path holding a code from statuscode.go, rewritten on
	// every report, so a state change replaces the value instead of leaving a
	// stale series behind.
	t.pathStat, err = t.meter.Int64Gauge("hostping.path.status_code", metric.WithDescription("Code of the path's current status (1 normal, 2 uncertain, 3 abnormal); hostping.status_code.info names every code"))
	if err != nil {
		return err
	}
	t.pathWhy, err = t.meter.Int64Gauge("hostping.path.uncertain_reason_code", metric.WithDescription("Code of why the path is uncertain, 0 (none) when it is not; hostping.status_code.info names every code"))
	if err != nil {
		return err
	}
	// The latency verdict is advisory and separate from path.status_code,
	// which stays the bandwidth verdict Algorithm 1 reads.
	t.pathLat, err = t.meter.Int64Gauge("hostping.path.latency_status_code", metric.WithDescription("Code of the path's current small-message latency verdict (1 normal, 2 unknown, 3 abnormal); hostping.status_code.info names every code"))
	if err != nil {
		return err
	}
	t.latWhy, err = t.meter.Int64Gauge("hostping.path.latency_reason_code", metric.WithDescription("Code of what the latency verdict was judged against, or why it was not judged; hostping.status_code.info names every code"))
	if err != nil {
		return err
	}
	// latency_status_code is per run, so one healthy path reading high in a
	// single run flips it. The streak lets a dashboard or alert wait for the
	// persistence the analyzer itself requires before it names a cause.
	t.latRuns, err = t.meter.Int64Gauge("hostping.path.latency_streak", metric.WithDescription("Consecutive idle runs, this one included, that read the path's latency abnormal against its siblings, 0 when this run did not"))
	if err != nil {
		return err
	}
	t.linkAbn, err = t.meter.Int64Gauge("hostping.link.abnormal_count", metric.WithDescription("Algorithm 1 abnormal_cnt per hop"))
	if err != nil {
		return err
	}
	t.linkStat, err = t.meter.Int64Gauge("hostping.link.status_code", metric.WithDescription("Code of the link's current Algorithm 1 status (1 normal, 2 uncertain, 3 abnormal, 4 gray); hostping.status_code.info names every code"))
	if err != nil {
		return err
	}
	t.pause, err = t.meter.Float64Gauge("hostping.rnic.pause_duration_ratio", metric.WithDescription("Tx pause duration ratio (seconds paused per second)"))
	if err != nil {
		return err
	}
	t.drops, err = t.meter.Int64Gauge("hostping.rnic.drop.delta", metric.WithDescription("Packet drop counter delta"))
	if err != nil {
		return err
	}
	t.thp, err = t.meter.Float64Gauge("hostping.rnic.throughput", metric.WithUnit("bit/s"))
	if err != nil {
		return err
	}
	t.cpuBusy, err = t.meter.Float64Gauge("hostping.cpu.busy", metric.WithDescription("CPU busy fraction of a NUMA node over the monitor poll interval, from /proc/stat"))
	if err != nil {
		return err
	}
	t.bus, err = t.meter.Float64Gauge("hostping.bus.utilization", metric.WithDescription("Intra-host bus utilization"))
	if err != nil {
		return err
	}
	// One summary value per RNIC pair per run, same as loopback latency.
	t.mutualR, err = t.meter.Float64Gauge("hostping.mutual.rtt", metric.WithUnit("us"), metric.WithDescription("Same-rail RNIC-to-RNIC RTT p50"))
	if err != nil {
		return err
	}
	t.mutualL, err = t.meter.Float64Gauge("hostping.mutual.loss_ratio", metric.WithDescription("Same-rail RNIC-to-RNIC loss ratio"))
	if err != nil {
		return err
	}
	t.mutualT, err = t.meter.Float64Gauge("hostping.mutual.timeout_ratio", metric.WithDescription("Same-rail RNIC-to-RNIC probe timeout ratio, 1 - recv/sent"))
	if err != nil {
		return err
	}
	t.skipped, err = t.meter.Int64Gauge("hostping.endpoint.skipped", metric.WithDescription("Endpoints whose loopback probes were skipped because registration failed or the GPU compute mode is not Default"))
	if err != nil {
		return err
	}
	// Like loopback latency, one value per path per run: the probe calls each
	// setup verb once, so a histogram would bucket runs rather than calls.
	t.verbDur, err = t.meter.Float64Gauge("hostping.verb.duration", metric.WithUnit("us"), metric.WithDescription("Execution time of a loopback probe's setup verbs (ToN'24 §VI-C)"))
	if err != nil {
		return err
	}
	t.diag, err = t.meter.Int64Counter("hostping.diagnosis.count", metric.WithDescription("Root-cause diagnoses emitted"))
	if err != nil {
		return err
	}
	t.runs, err = t.meter.Int64Counter("hostping.engine.run.count")
	if err != nil {
		return err
	}
	t.idle, err = t.meter.Int64Gauge("hostping.host.idle")
	if err != nil {
		return err
	}
	if err := t.registerStatusCodeInfo(); err != nil {
		return err
	}
	return t.initLivenessInstruments()
}

// initLivenessInstruments creates the instruments that tell a working agent
// from a stuck one, and writes the outcome counters' fixed series as 0. The
// timestamps stay absent until their first event.
func (t *Telemetry) initLivenessInstruments() error {
	var err error
	t.lastRun, err = t.meter.Float64Gauge("hostping.engine.last_run_timestamp", metric.WithUnit("s"), metric.WithDescription("Unix time the last engine run ended, whatever its outcome"))
	if err != nil {
		return err
	}
	t.lastSuccess, err = t.meter.Float64Gauge("hostping.engine.last_success_timestamp", metric.WithUnit("s"), metric.WithDescription("Unix time the last engine run whose report was confirmed and written ended"))
	if err != nil {
		return err
	}
	t.runFailed, err = t.meter.Int64Counter("hostping.engine.run.failed", metric.WithDescription("Engine runs that returned an error (stage=engine) or whose confirmed report could not be written (stage=report)"))
	if err != nil {
		return err
	}
	t.runDiscard, err = t.meter.Int64Counter("hostping.engine.run.discarded", metric.WithDescription("Idle-mode runs discarded because the clean poll after them found the host busy or unclassified"))
	if err != nil {
		return err
	}
	t.gateSkip, err = t.meter.Int64Counter("hostping.engine.idle_gate.skipped", metric.WithDescription("Idle runs the --idle-gate-command kept from starting: it exited 1 (reason=denied), timed out (reason=timeout), or exited with another non-zero status or could not run (reason=error)"))
	if err != nil {
		return err
	}
	t.pollErrors, err = t.meter.Int64Counter("hostping.monitor.poll.errors", metric.WithDescription("Monitor polls that returned an error"))
	if err != nil {
		return err
	}
	t.lastPoll, err = t.meter.Float64Gauge("hostping.monitor.last_poll_timestamp", metric.WithUnit("s"), metric.WithDescription("Unix time of the last monitor poll that completed without error"))
	if err != nil {
		return err
	}
	// hostping.host.idle is the last report's classification, so it keeps
	// saying idle on a host that turned busy and has not been diagnosed since.
	// A staleness alert needs the monitor's current view.
	t.monIdle, err = t.meter.Int64Gauge("hostping.monitor.host_idle", metric.WithDescription("1 when the last clean monitor poll found the host idle, 0 when busy or unclassified"))
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, s := range failStages {
		t.runFailed.Add(ctx, 0, metric.WithAttributes(attribute.String("stage", s)))
	}
	for _, r := range discardReasons {
		t.runDiscard.Add(ctx, 0, metric.WithAttributes(attribute.String("reason", r)))
	}
	t.pollErrors.Add(ctx, 0)
	return nil
}

func unixSeconds(at time.Time) float64 {
	return float64(at.UnixNano()) / 1e9
}

// RecordPoll writes the outcome of one monitor poll that returned at at:
// hostping.monitor.last_poll_timestamp on success, hostping.monitor.poll.errors
// on an error.
func (t *Telemetry) RecordPoll(ctx context.Context, at time.Time, err error) {
	if t == nil {
		return
	}
	if err != nil {
		t.pollErrors.Add(ctx, 1)
		return
	}
	t.lastPoll.Record(ctx, unixSeconds(at))
}

// RecordMonitorIdle writes hostping.monitor.host_idle from a clean (Ready)
// poll: idle is true only when the poll classified the host as idle.
func (t *Telemetry) RecordMonitorIdle(ctx context.Context, idle bool) {
	if t == nil {
		return
	}
	v := int64(0)
	if idle {
		v = 1
	}
	t.monIdle.Record(ctx, v)
}

// RecordRunEnd writes hostping.engine.last_run_timestamp: an engine run ended
// at at, whatever its outcome.
func (t *Telemetry) RecordRunEnd(ctx context.Context, at time.Time) {
	if t == nil {
		return
	}
	t.lastRun.Record(ctx, unixSeconds(at))
}

// RecordRunFailed counts a failed run; stage is FailedEngine or FailedReport.
func (t *Telemetry) RecordRunFailed(ctx context.Context, stage string) {
	if t == nil {
		return
	}
	t.runFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", stage)))
}

// RecordSuccess writes hostping.engine.last_success_timestamp: the run that
// ended at at produced a report that was confirmed and written.
func (t *Telemetry) RecordSuccess(ctx context.Context, at time.Time) {
	if t == nil {
		return
	}
	t.lastSuccess.Record(ctx, unixSeconds(at))
}

// RecordDiscarded counts a discarded idle-mode run; reason is
// DiscardHostBusy or DiscardIdleUnknown.
func (t *Telemetry) RecordDiscarded(ctx context.Context, reason string) {
	if t == nil {
		return
	}
	t.runDiscard.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// RecordIdleGateSkipped counts an idle run that --idle-gate-command kept
// from starting; reason is GateDenied, GateTimeout or GateError.
func (t *Telemetry) RecordIdleGateSkipped(ctx context.Context, reason string) {
	if t == nil {
		return
	}
	t.gateSkip.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// initIdleGateSeries writes hostping.engine.idle_gate.skipped's fixed series
// as 0, so a rate() over them is defined from the first scrape. Only a daemon
// that runs a gate has them: elsewhere the counter could never move.
func (t *Telemetry) initIdleGateSeries() {
	ctx := context.Background()
	for _, r := range gateReasons {
		t.gateSkip.Add(ctx, 0, metric.WithAttributes(attribute.String("reason", r)))
	}
}

// registerBuildInfo exports hostping.build.info: a constant 1 whose
// attributes name the running build, so dashboards can group results by build.
// It is observed on every collection, so it is on /metrics from the first
// scrape, before any engine run has reported.
func (t *Telemetry) registerBuildInfo(build buildinfo.Info) error {
	attrs := metric.WithAttributes(
		attribute.String("version", build.Version),
		attribute.String("commit", build.Commit),
		attribute.String("zig", build.Zig),
		attribute.String("go", build.Go),
	)
	_, err := t.meter.Int64ObservableGauge("hostping.build.info",
		metric.WithDescription("Always 1; the attributes identify the running build"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, attrs)
			return nil
		}))
	return err
}

// unknownAttr is the value of an identity attribute discovery could not
// fill, as topology discovery spells an unknown fabric.
const unknownAttr = "unknown"

func orUnknown(v string) string {
	if v == "" {
		return unknownAttr
	}
	return v
}

// rnicInfoAttrs is the attribute set of one hostping.rnic.info series. rnic is
// the RNIC ID every other RNIC series carries, so a query joins on it to read
// the device name and PCI address.
func rnicInfoAttrs(n model.RNIC) attribute.Set {
	return attribute.NewSet(
		attribute.String("rnic", n.ID),
		attribute.String("device", orUnknown(n.Device)),
		attribute.String("pcie_bdf", orUnknown(n.PCIeBDF)),
		attribute.String("netdev", orUnknown(n.Netdev)),
		attribute.String("link_layer", orUnknown(n.LinkLayer)),
		attribute.String("fabric", orUnknown(n.Fabric)),
		attribute.String("socket", strconv.Itoa(n.Socket)),
		attribute.String("numa", strconv.Itoa(n.NUMA)),
	)
}

// endpointInfoAttrs is the attribute set of one hostping.endpoint.info series.
// A memory endpoint has no PCI function, so it carries no pcie_bdf; root_port
// is left out where the endpoint has none (memory). A GPU whose address was not
// read says "unknown".
func endpointInfoAttrs(e model.Endpoint) attribute.Set {
	kv := []attribute.KeyValue{
		attribute.String("endpoint", e.ID),
		attribute.String("kind", string(e.Kind)),
		attribute.String("index", strconv.Itoa(e.Index)),
		attribute.String("socket", strconv.Itoa(e.Socket)),
		attribute.String("numa", strconv.Itoa(e.NUMA)),
	}
	if e.Kind != model.EndpointMemory {
		kv = append(kv, attribute.String("pcie_bdf", orUnknown(e.PCIeBDF)))
	}
	if e.RootPort != "" {
		kv = append(kv, attribute.String("root_port", e.RootPort))
	}
	return attribute.NewSet(kv...)
}

// registerTopologyInfo exports hostping.rnic.info and hostping.endpoint.info:
// a constant 1 per RNIC and per endpoint whose attributes carry the physical
// identifiers (IB device, PCI BDF, NUMA placement) the measurement series
// leave out. Like hostping.build.info they are observed on every collection,
// so they are on /metrics from the first scrape. RNICs dropped by
// --exclude-rnic are not in rnics and get no series.
func (t *Telemetry) registerTopologyInfo(rnics []model.RNIC, endpoints []model.Endpoint) error {
	rnicAttrs := make([]metric.ObserveOption, len(rnics))
	for i, n := range rnics {
		rnicAttrs[i] = metric.WithAttributeSet(rnicInfoAttrs(n))
	}
	epAttrs := make([]metric.ObserveOption, len(endpoints))
	for i, e := range endpoints {
		epAttrs[i] = metric.WithAttributeSet(endpointInfoAttrs(e))
	}
	_, err := t.meter.Int64ObservableGauge("hostping.rnic.info",
		metric.WithDescription("Always 1; the attributes identify the RNIC (IB device, PCI BDF, netdev, link layer, fabric, NUMA placement)"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, a := range rnicAttrs {
				o.Observe(1, a)
			}
			return nil
		}))
	if err != nil {
		return err
	}
	_, err = t.meter.Int64ObservableGauge("hostping.endpoint.info",
		metric.WithDescription("Always 1; the attributes identify the loopback endpoint (kind, index, PCI BDF, root port, NUMA placement)"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, a := range epAttrs {
				o.Observe(1, a)
			}
			return nil
		}))
	return err
}

// diagramNodeAttrs is the attribute set of one hostping.topology.node.info
// series. node is the ID the edges' source and target name. numa is left out
// where the node has no single NUMA node (a package under sub-NUMA
// clustering), and pcie_bdf where it is no PCI function (package, NUMA node,
// memory); a PCI node whose address is not known says "unknown".
func diagramNodeAttrs(n model.TopologyNode) attribute.Set {
	kv := []attribute.KeyValue{
		attribute.String("node", n.ID),
		attribute.String("kind", n.Kind),
		attribute.String("title", orUnknown(n.Title)),
		attribute.String("socket", strconv.Itoa(n.Socket)),
	}
	if n.NUMA >= 0 {
		kv = append(kv, attribute.String("numa", strconv.Itoa(n.NUMA)))
	}
	switch n.Kind {
	case model.TopoNodeSocket, model.TopoNodeNUMA, model.TopoNodeMemory:
	default:
		kv = append(kv, attribute.String("pcie_bdf", orUnknown(n.PCIeBDF)))
	}
	return attribute.NewSet(kv...)
}

// diagramEdgeAttrs is the attribute set of one hostping.topology.edge.info
// series. link is the link attribute of hostping.link.status_code and
// hostping.bus.utilization, so a query joins the edge to its state, or "none"
// for a structural edge.
func diagramEdgeAttrs(e model.TopologyEdge) attribute.Set {
	return attribute.NewSet(
		attribute.String("edge", e.ID),
		attribute.String("link", e.Link),
		attribute.String("source", e.Source),
		attribute.String("target", e.Target),
		attribute.String("kind", e.Kind),
		attribute.String("capacity_bps", strconv.FormatUint(e.CapacityBPS, 10)),
		attribute.String("negotiated_bps", strconv.FormatUint(e.NegotiatedBPS, 10)),
		attribute.String("capacity_assumed", strconv.FormatBool(e.CapacityAssumed)),
	)
}

// registerDiagramInfo exports hostping.topology.node.info and
// hostping.topology.edge.info: a constant 1 per node and per edge of the host
// topology diagram, for a dashboard's node graph. They are observed on every
// collection, like the other info series.
func (t *Telemetry) registerDiagramInfo(nodes []model.TopologyNode, edges []model.TopologyEdge) error {
	nodeAttrs := make([]metric.ObserveOption, len(nodes))
	for i, n := range nodes {
		nodeAttrs[i] = metric.WithAttributeSet(diagramNodeAttrs(n))
	}
	edgeAttrs := make([]metric.ObserveOption, len(edges))
	for i, e := range edges {
		edgeAttrs[i] = metric.WithAttributeSet(diagramEdgeAttrs(e))
	}
	_, err := t.meter.Int64ObservableGauge("hostping.topology.node.info",
		metric.WithDescription("Always 1; one series per node of the host topology diagram (CPU package, NUMA node, memory, root port, PCIe switch, RNIC, GPU)"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, a := range nodeAttrs {
				o.Observe(1, a)
			}
			return nil
		}))
	if err != nil {
		return err
	}
	_, err = t.meter.Int64ObservableGauge("hostping.topology.edge.info",
		metric.WithDescription("Always 1; one series per edge of the host topology diagram, source upstream of target; link names the link of hostping.link.status_code"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for _, a := range edgeAttrs {
				o.Observe(1, a)
			}
			return nil
		}))
	return err
}

// RecordReport writes one report to the instruments. It holds no lock: every
// OTel instrument is safe for concurrent use and RecordReport keeps no state
// of its own.
func (t *Telemetry) RecordReport(ctx context.Context, rep model.Report) {
	if t == nil {
		return
	}
	t.runs.Add(ctx, 1, metric.WithAttributes(attribute.String("trigger", string(rep.Trigger)), attribute.String("mode", string(rep.Mode))))
	idle := int64(0)
	if rep.HostIdle {
		idle = 1
	}
	t.idle.Record(ctx, idle)
	for _, p := range rep.Paths {
		attrs := metric.WithAttributes(attribute.String("rnic", p.RNIC), attribute.String("endpoint", p.Endpoint))
		t.loopLat.Record(ctx, float64(p.SmallLatNsP50)/1000.0, attrs)
		t.loopBW.Record(ctx, float64(p.BandwidthBPS), attrs)
		abn := int64(0)
		if p.Status == model.PathAbnormal {
			abn = 1
		}
		t.pathAbn.Record(ctx, abn, attrs)
		t.recordVerbs(ctx, p)
		// Only PathUncertain carries a reason; one left on a decided path is
		// not exported, so a normal or abnormal path always reads none.
		reason := p.UncertainReason
		if p.Status != model.PathUncertain {
			reason = model.UncertainNone
		}
		t.pathStat.Record(ctx, pathStatusCode(p.Status), attrs)
		t.pathWhy.Record(ctx, uncertainReasonCode(reason), attrs)
		t.pathLat.Record(ctx, latencyStatusCode(p.LatencyStatus), attrs)
		t.latWhy.Record(ctx, latencyReasonCode(p.LatencyReason), attrs)
		t.latRuns.Record(ctx, int64(p.LatencyStreak), attrs)
	}
	for _, l := range rep.Links {
		attrs := metric.WithAttributes(attribute.String("link", l.ID))
		t.linkAbn.Record(ctx, int64(l.AbnormalCount), attrs)
		// An unread bus writes no sample rather than a 0 that would read as
		// an idle link.
		if u, ok := l.Util(); ok {
			t.bus.Record(ctx, u, attrs)
		}
		t.linkStat.Record(ctx, linkStatusCode(l.Status), attrs)
	}
	for _, k := range endpointKinds {
		t.skipped.Record(ctx, int64(rep.SkippedEndpointCount(k)), metric.WithAttributes(attribute.String("kind", string(k))))
	}
	t.recordMutual(ctx, rep.Mutual)
	for _, c := range rep.Causes {
		t.diag.Add(ctx, 1, metric.WithAttributes(attribute.String("code", c.Code), attribute.String("confidence", c.Confidence)))
	}
}

// RecordMutual writes the scheduled mutual probe's window between engine runs,
// so hostping.mutual.* follows the probe (every --metric-interval) rather than
// the engine's run schedule.
func (t *Telemetry) RecordMutual(ctx context.Context, ms []model.MutualMeasurement) {
	if t == nil {
		return
	}
	t.recordMutual(ctx, ms)
}

func (t *Telemetry) recordMutual(ctx context.Context, ms []model.MutualMeasurement) {
	for _, m := range ms {
		attrs := metric.WithAttributes(attribute.String("src", m.Src), attribute.String("dst", m.Dst))
		t.mutualR.Record(ctx, float64(m.RTTnsP50)/1000.0, attrs)
		t.mutualL.Record(ctx, m.LossRatio, attrs)
		if r, ok := m.TimeoutRatio(); ok {
			t.mutualT.Record(ctx, r, attrs)
		}
	}
}

// recordVerbs writes hostping.verb.duration for one path. A probe that
// reported no timings (VerbNs zero) writes nothing rather than a false 0 µs.
// gpu_alloc exists only for GPU endpoints, so a memory path never gets that
// series; the rest are written on every report, so none goes stale. With
// --metrics-verb-durations=false nothing is written at all.
func (t *Telemetry) recordVerbs(ctx context.Context, p model.PathMeasurement) {
	if t.verbsOff || p.Verbs == (model.VerbNs{}) {
		return
	}
	rec := func(verb string, ns uint64) {
		t.verbDur.Record(ctx, float64(ns)/1000.0, metric.WithAttributes(
			attribute.String("rnic", p.RNIC),
			attribute.String("endpoint", p.Endpoint),
			attribute.String("endpoint_kind", string(p.EndpointKind)),
			attribute.String("verb", verb),
		))
	}
	if p.EndpointKind == model.EndpointGPU {
		rec("gpu_alloc", p.Verbs.GPUAlloc)
	}
	rec("mr_reg", p.Verbs.MRReg)
	rec("qp_setup", p.Verbs.QPSetup)
	// Only with --mem-warmup, and then on every memory path of the report.
	if p.Verbs.MemWarmup > 0 {
		rec("mem_warmup", p.Verbs.MemWarmup)
	}
}

// RecordRates writes one monitor poll to the RNIC counter instruments. A pause
// ratio or drop delta that was not measured on this poll (no source, unread,
// or re-baselining) is not recorded: an absent series, never a healthy 0.
func (t *Telemetry) RecordRates(ctx context.Context, rates []model.RNICRates) {
	if t == nil {
		return
	}
	for _, r := range rates {
		attrs := metric.WithAttributes(attribute.String("rnic", r.Device))
		t.thp.Record(ctx, float64(r.ThroughputBPS), attrs)
		if r.PauseKnown {
			t.pause.Record(ctx, r.PauseDurationRatio, attrs)
		}
		if r.DropKnown {
			t.drops.Record(ctx, int64(r.DropDelta), attrs)
		}
	}
}

// RecordCPULoad records each NUMA node's CPU busy fraction from a monitor
// poll. A node whose load was not read records nothing.
func (t *Telemetry) RecordCPULoad(ctx context.Context, loads []model.CPULoad) {
	if t == nil {
		return
	}
	for _, l := range loads {
		t.cpuBusy.Record(ctx, l.Busy, metric.WithAttributes(
			attribute.String("numa", strconv.Itoa(l.NUMA)),
			attribute.String("socket", strconv.Itoa(l.Socket)),
		))
	}
}

// Shutdown is a no-op on a nil Telemetry, which is what a one-shot run without
// exporters gets.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if t.http != nil {
		_ = t.http.Shutdown(ctx)
	}
	if t.provider != nil {
		return t.provider.Shutdown(ctx)
	}
	return nil
}

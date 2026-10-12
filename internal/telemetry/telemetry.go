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
	pathLat  metric.Int64Gauge
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
	diag     metric.Int64Counter
	runs     metric.Int64Counter
	idle     metric.Int64Gauge

	lastRun     metric.Float64Gauge
	lastSuccess metric.Float64Gauge
	runFailed   metric.Int64Counter
	runDiscard  metric.Int64Counter
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
	live *Liveness
}

// WithLiveness makes /healthz report l: 503 while the daemon's poll loop is
// stalled. Without it /healthz is 200 while the process serves.
func WithLiveness(l *Liveness) Option {
	return func(o *options) { o.live = l }
}

// linkStatuses is every model.LinkStatus. hostping.link.status writes one
// 0/1 sample per status per link on every report, so a link that changes
// state overwrites all of its own series instead of leaving a stale one
// behind (which is what a status attribute on a gauge would do).
var linkStatuses = []model.LinkStatus{
	model.LinkUncertain,
	model.LinkNormal,
	model.LinkAbnormal,
	model.LinkGray,
}

// pathSeries is the (status, reason) pairs hostping.path.status writes for
// every path on every report, following linkStatuses: the set is fixed and
// independent of what a path actually measured, so a path that changes status
// or reason overwrites all of its own series instead of stranding the last one
// at 1.
//
// A reason only narrows model.PathUncertain, so the decided statuses carry
// UncertainNone rather than a full status × reason product. (uncertain, none)
// is written too: it is 0 on a classified report, and turns into the alert that
// some uncertain branch forgot to record why.
var pathSeries = []struct {
	status model.PathStatus
	reason model.PathUncertainReason
}{
	{model.PathUncertain, model.UncertainNone},
	{model.PathUncertain, model.UncertainNoPath},
	{model.PathUncertain, model.UncertainRNICBusy},
	{model.PathUncertain, model.UncertainNonAffinitive},
	{model.PathUncertain, model.UncertainBelowCeiling},
	{model.PathUncertain, model.UncertainNoCeiling},
	{model.PathUncertain, model.UncertainHostShift},
	{model.PathUncertain, model.UncertainCPUBusy},
	{model.PathNormal, model.UncertainNone},
	{model.PathAbnormal, model.UncertainNone},
}

// latencySeries is the (status, reason) pairs hostping.path.latency_status
// writes for every path on every report, fixed for the same reason as
// pathSeries. A decided verdict carries what it was judged against; an unknown
// one why it was not judged. (unknown, none) is 0 on a classified report.
var latencySeries = []struct {
	status model.LatencyStatus
	reason model.LatencyReason
}{
	{model.LatencyUnknown, model.LatencyReasonNone},
	{model.LatencyUnknown, model.LatencyBusy},
	{model.LatencyUnknown, model.LatencyFewSiblings},
	{model.LatencyUnknown, model.LatencyNoPath},
	{model.LatencyUnknown, model.LatencyNoReading},
	{model.LatencyUnknown, model.LatencyCPUBusy},
	{model.LatencyNormal, model.LatencyByBaseline},
	{model.LatencyNormal, model.LatencyBySiblings},
	{model.LatencyNormal, model.LatencyRecheckCleared},
	{model.LatencyNormal, model.LatencyLocalityShift},
	{model.LatencyAbnormal, model.LatencyByBaseline},
	{model.LatencyAbnormal, model.LatencyBySiblings},
}

// endpointKinds is every model.EndpointKind. hostping.endpoint.skipped writes
// one sample per kind on every report, so a kind that stops being skipped falls
// back to 0 instead of leaving its last non-zero value behind. The failure
// reason is not an attribute: it is free-form probe text, so it would both blow
// up cardinality and strand a series on every reason that gets fixed. The
// reason is logged and carried in the report instead.
var endpointKinds = []model.EndpointKind{model.EndpointGPU, model.EndpointMemory}

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
	if err := t.registerBuildInfo(build); err != nil {
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
	// why an uncertain path is uncertain, live here.
	t.pathStat, err = t.meter.Int64Gauge("hostping.path.status", metric.WithDescription("1 for the path's current status and uncertain reason, 0 for the others"))
	if err != nil {
		return err
	}
	// The latency verdict is advisory and separate from path.status, which
	// stays the bandwidth verdict Algorithm 1 reads.
	t.pathLat, err = t.meter.Int64Gauge("hostping.path.latency_status", metric.WithDescription("1 for the path's current small-message latency verdict and its reason, 0 for the others"))
	if err != nil {
		return err
	}
	t.linkAbn, err = t.meter.Int64Gauge("hostping.link.abnormal_count", metric.WithDescription("Algorithm 1 abnormal_cnt per hop"))
	if err != nil {
		return err
	}
	t.linkStat, err = t.meter.Int64Gauge("hostping.link.status", metric.WithDescription("1 for the link's current Algorithm 1 status, 0 for the others"))
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
		// A reason on a decided path would not match any series below, which
		// would leave the path with no 1 at all. Only PathUncertain carries one.
		reason := p.UncertainReason
		if p.Status != model.PathUncertain {
			reason = model.UncertainNone
		}
		for _, s := range pathSeries {
			v := int64(0)
			if s.status == p.Status && s.reason == reason {
				v = 1
			}
			t.pathStat.Record(ctx, v, metric.WithAttributes(
				attribute.String("rnic", p.RNIC),
				attribute.String("endpoint", p.Endpoint),
				attribute.String("status", s.status.String()),
				attribute.String("reason", s.reason.String()),
			))
		}
		for _, s := range latencySeries {
			v := int64(0)
			if s.status == p.LatencyStatus && s.reason == p.LatencyReason {
				v = 1
			}
			t.pathLat.Record(ctx, v, metric.WithAttributes(
				attribute.String("rnic", p.RNIC),
				attribute.String("endpoint", p.Endpoint),
				attribute.String("status", s.status.String()),
				attribute.String("reason", s.reason.String()),
			))
		}
	}
	for _, l := range rep.Links {
		attrs := metric.WithAttributes(attribute.String("link", l.ID))
		t.linkAbn.Record(ctx, int64(l.AbnormalCount), attrs)
		// An unread bus writes no sample rather than a 0 that would read as
		// an idle link.
		if u, ok := l.Util(); ok {
			t.bus.Record(ctx, u, attrs)
		}
		for _, s := range linkStatuses {
			v := int64(0)
			if s == l.Status {
				v = 1
			}
			t.linkStat.Record(ctx, v, metric.WithAttributes(
				attribute.String("link", l.ID),
				attribute.String("status", s.String()),
			))
		}
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
// series; the rest are written on every report, so none goes stale.
func (t *Telemetry) recordVerbs(ctx context.Context, p model.PathMeasurement) {
	if p.Verbs == (model.VerbNs{}) {
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

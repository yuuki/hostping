package config

import (
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

func TestBaselineFlags(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{
		"--baseline", "local_mem=270Gbps,1400ns",
		"--baseline", "remote_mem=265Gbps",
	}); err != nil {
		t.Fatal(err)
	}
	ov, err := cfg.Overrides()
	if err != nil {
		t.Fatal(err)
	}
	if ov.Distances[model.DistanceLocalMem].BandwidthBPS != 270_000_000_000 || ov.Distances[model.DistanceLocalMem].LatencyNs != 1400 {
		t.Fatalf("local %+v", ov.Distances[model.DistanceLocalMem])
	}
	if ov.Distances[model.DistanceRemoteMem].BandwidthBPS != 265_000_000_000 {
		t.Fatalf("remote %+v", ov.Distances[model.DistanceRemoteMem])
	}
	if _, ok := ov.Distances[model.DistanceSameSwitch]; ok {
		t.Fatal("unset same_switch must not appear in overrides")
	}

	help := fs.Lookup("baseline").Usage
	for _, want := range []string{"local_mem", "Fig.4", "same_switch latency", "bps/Gbps"} {
		if !strings.Contains(help, want) {
			t.Fatalf("flag help missing %q: %s", want, help)
		}
	}
	fileHelp := fs.Lookup("baseline-file").Usage
	if !strings.Contains(fileHelp, "JSON") || strings.Contains(fileHelp, "YAML") {
		t.Fatalf("baseline-file help must claim JSON only: %s", fileHelp)
	}
	if !strings.Contains(fileHelp, "bandwidth_bps") || !strings.Contains(fileHelp, "Unknown fields") {
		t.Fatalf("baseline-file help missing fields: %s", fileHelp)
	}
}

func TestBaselineFlagRejectsBadSpec(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var buf strings.Builder
	fs.SetOutput(&buf)
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{"--baseline", "nope=270Gbps"}); err == nil {
		t.Fatal("unknown key must fail")
	}
}

// Every probe parameter is reachable from the command line, and the defaults
// are what Defaults() says with no flags given.
func TestProbeFlags(t *testing.T) {
	def := Defaults()
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, def) {
		t.Fatalf("parsing no flags changed the defaults:\n got %+v\nwant %+v", cfg, def)
	}

	cfg = Defaults()
	fs = flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{
		"--iters", "128",
		"--timeout", "2s",
		"--util-high", "0.75",
		"--thp-low", "0.10",
		"--small-msg", "8",
		"--large-msg", "524288",
		"--mutual-msg", "64",
		"--flap-window", "5",
		"--timeout-th", "0.25",
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Iters != 128 {
		t.Errorf("Iters = %d, want 128", cfg.Iters)
	}
	if cfg.Timeout != 2*time.Second {
		t.Errorf("Timeout = %v, want 2s", cfg.Timeout)
	}
	if cfg.UtilHigh != 0.75 {
		t.Errorf("UtilHigh = %v, want 0.75", cfg.UtilHigh)
	}
	if cfg.ThpLowFraction != 0.10 {
		t.Errorf("ThpLowFraction = %v, want 0.1", cfg.ThpLowFraction)
	}
	if cfg.SmallMsgBytes != 8 {
		t.Errorf("SmallMsgBytes = %d, want 8", cfg.SmallMsgBytes)
	}
	if cfg.LargeMsgBytes != 524288 {
		t.Errorf("LargeMsgBytes = %d, want 524288", cfg.LargeMsgBytes)
	}
	if cfg.MutualMsgBytes != 64 {
		t.Errorf("MutualMsgBytes = %d, want 64", cfg.MutualMsgBytes)
	}
	if cfg.FlapWindow != 5 {
		t.Errorf("FlapWindow = %d, want 5", cfg.FlapWindow)
	}
	if cfg.TimeoutTh != 0.25 {
		t.Errorf("TimeoutTh = %v, want 0.25", cfg.TimeoutTh)
	}
}

// ToN'24 §IV-C-2 sets Timeout_th to 10%.
func TestTimeoutThDefault(t *testing.T) {
	if got := Defaults().TimeoutTh; got != 0.1 {
		t.Fatalf("TimeoutTh default = %v, want 0.1", got)
	}
}

// --mutual-infiniband is off by default, so InfiniBand RNICs stay out of the
// mutual probe unless the operator asks for them.
func TestMutualInfiniBandFlag(t *testing.T) {
	if Defaults().MutualInfiniBand {
		t.Fatal("MutualInfiniBand must default to false")
	}
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{"--mutual-infiniband"}); err != nil {
		t.Fatal(err)
	}
	if !cfg.MutualInfiniBand {
		t.Fatal("--mutual-infiniband did not set MutualInfiniBand")
	}
}

// hostping.verb.duration is exported by default; --metrics-verb-durations=false
// turns it off.
func TestMetricsVerbDurationsFlag(t *testing.T) {
	if !Defaults().MetricsVerbDurations {
		t.Fatal("MetricsVerbDurations must default to true")
	}
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{"--metrics-verb-durations=false"}); err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsVerbDurations {
		t.Fatal("--metrics-verb-durations=false did not clear MetricsVerbDurations")
	}
}

// --exclude-rnic is repeatable and also takes a comma-separated list; an
// empty name is an error rather than an entry that matches nothing.
func TestExcludeRNICFlag(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{"--exclude-rnic", "mlx5_2", "--exclude-rnic", "mlx5_3, 0000:3c:00.1"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"mlx5_2", "mlx5_3", "0000:3c:00.1"}; !reflect.DeepEqual(cfg.ExcludeRNICs, want) {
		t.Fatalf("ExcludeRNICs = %q, want %q", cfg.ExcludeRNICs, want)
	}
	for _, bad := range []string{"", "mlx5_2,", ",mlx5_2"} {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		cfg.RegisterFlags(fs)
		if err := fs.Parse([]string{"--exclude-rnic", bad}); err == nil {
			t.Errorf("--exclude-rnic %q accepted", bad)
		}
	}
}

// --mutual-scope defaults to rail, accepts host and rejects anything else, so
// a typo cannot silently fall back to one of the two.
func TestMutualScopeFlag(t *testing.T) {
	if got := Defaults().MutualScope; got != model.MutualScopeRail {
		t.Fatalf("MutualScope default = %q, want rail", got)
	}
	for _, tc := range []struct {
		arg  string
		want model.MutualScope
		ok   bool
	}{
		{"rail", model.MutualScopeRail, true},
		{"host", model.MutualScopeHost, true},
		{"hosts", "", false},
		{"", "", false},
	} {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		cfg.RegisterFlags(fs)
		err := fs.Parse([]string{"--mutual-scope", tc.arg})
		if (err == nil) != tc.ok {
			t.Fatalf("--mutual-scope %q: err = %v, want ok=%v", tc.arg, err, tc.ok)
		}
		if tc.ok && cfg.MutualScope != tc.want {
			t.Fatalf("--mutual-scope %q: got %q", tc.arg, cfg.MutualScope)
		}
	}
}

// --mutual-transport defaults to rc, accepts ud and rejects anything else; the
// schedule needs ud and is refused, not silently switched, with rc.
func TestMutualTransportFlag(t *testing.T) {
	if got := Defaults().MutualTransport; got != model.MutualTransportRC {
		t.Fatalf("MutualTransport default = %q, want rc", got)
	}
	for _, tc := range []struct {
		arg  string
		want model.MutualTransport
		ok   bool
	}{
		{"rc", model.MutualTransportRC, true},
		{"ud", model.MutualTransportUD, true},
		{"UD", "", false},
		{"", "", false},
	} {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		cfg.RegisterFlags(fs)
		err := fs.Parse([]string{"--mutual-transport", tc.arg})
		if (err == nil) != tc.ok {
			t.Fatalf("--mutual-transport %q: err = %v, want ok=%v", tc.arg, err, tc.ok)
		}
		if tc.ok && cfg.MutualTransport != tc.want {
			t.Fatalf("--mutual-transport %q: got %q", tc.arg, cfg.MutualTransport)
		}
	}
	cfg := Defaults()
	cfg.MutualInterval = 100 * time.Millisecond
	err := cfg.ValidateMutual()
	if err == nil || !strings.Contains(err.Error(), "--mutual-transport=ud") {
		t.Fatalf("the schedule with rc must be refused naming ud, got %v", err)
	}
	cfg.MutualTransport = model.MutualTransportUD
	if err := cfg.ValidateMutual(); err != nil {
		t.Fatal(err)
	}
}

// ToN'24 §IV-A-2 probes every 100 ms with a 500 ms probe timeout over a 20 s
// window. The timeout and window are the defaults; the schedule itself is
// opt-in, so the default is the per-run burst.
func TestMutualScheduleDefaultsAndFlags(t *testing.T) {
	d := Defaults()
	if d.MutualInterval != 0 || d.MutualTimeout != 500*time.Millisecond || d.MutualWindow != 20*time.Second {
		t.Fatalf("defaults: interval=%v timeout=%v window=%v", d.MutualInterval, d.MutualTimeout, d.MutualWindow)
	}
	if err := d.ValidateMutual(); err != nil {
		t.Fatal(err)
	}
	c := Defaults()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c.RegisterFlags(fs)
	if err := c.Parse(fs, []string{"--mutual-transport", "ud", "--mutual-interval", "100ms", "--mutual-timeout", "250ms", "--mutual-window", "10s"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateMutual(); err != nil {
		t.Fatal(err)
	}
	if c.MutualInterval != 100*time.Millisecond || c.MutualTimeout != 250*time.Millisecond || c.MutualWindow != 10*time.Second {
		t.Fatalf("flags: %+v", c)
	}
	if c.Timeout != d.Timeout {
		t.Fatal("--mutual-timeout must not change the loopback --timeout")
	}
	ud := model.MutualTransportUD
	for _, bad := range []Config{
		{MutualTransport: ud, MutualTimeout: 0, MutualInterval: time.Second, MutualWindow: time.Minute},
		{MutualTransport: ud, MutualTimeout: time.Second, MutualInterval: -1, MutualWindow: time.Minute},
		{MutualTransport: ud, MutualTimeout: time.Second, MutualInterval: time.Minute, MutualWindow: time.Second},
	} {
		if bad.ValidateMutual() == nil {
			t.Fatalf("want an error for %+v", bad)
		}
	}
}

// The probe ABI takes uint32, so a value that does not fit is a flag error
// rather than a silently truncated probe.
func TestUint32FlagsRejectOutOfRange(t *testing.T) {
	for _, name := range []string{"iters", "mutual-iters", "small-msg", "large-msg", "mutual-msg"} {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		cfg.RegisterFlags(fs)
		if err := fs.Parse([]string{"--" + name, "4294967296"}); err == nil {
			t.Errorf("--%s accepted a value larger than uint32", name)
		}
	}
}

// PrintDefaults must show the real defaults, not zero.
func TestProbeFlagDefaultsInUsage(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	for name, want := range map[string]string{
		"iters":              "512",
		"mutual-iters":       "64",
		"mem-warmup":         "0s",
		"upi-bytes-per-flit": "0",
		"small-msg":          "1",
		"large-msg":          "262144",
		"mutual-msg":         "1",
		"flap-window":        "3",
		"util-high":          "0.9",
		"thp-low":            "0.05",
		"timeout":            "5s",
	} {
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("flag --%s is not registered", name)
		}
		if f.DefValue != want {
			t.Errorf("--%s default = %q, want %q", name, f.DefValue, want)
		}
	}
}

func TestTelemetryExportersOneShotNeedsExplicitFlags(t *testing.T) {
	parse := func(args ...string) Config {
		t.Helper()
		cfg := Defaults()
		cfg.OTLPEndpoint, cfg.OTLPSet = "", false // ignore any ambient OTEL env
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		cfg.RegisterFlags(fs)
		if err := cfg.Parse(fs, args); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	daemon := parse()
	if addr, otlp := daemon.TelemetryExporters(); addr != ":9464" || otlp != "" {
		t.Fatalf("daemon defaults must keep the listener: addr=%q otlp=%q", addr, otlp)
	}
	once := parse("--once")
	if addr, otlp := once.TelemetryExporters(); addr != "" || otlp != "" {
		t.Fatalf("one-shot must export nothing by default: addr=%q otlp=%q", addr, otlp)
	}
	onceMetrics := parse("--once", "--metrics", "127.0.0.1:9999")
	if addr, otlp := onceMetrics.TelemetryExporters(); addr != "127.0.0.1:9999" || otlp != "" {
		t.Fatalf("explicit --metrics must be honoured: addr=%q otlp=%q", addr, otlp)
	}
	onceOTLP := parse("--once", "--otlp", "localhost:4317")
	if addr, otlp := onceOTLP.TelemetryExporters(); addr != "" || otlp != "localhost:4317" {
		t.Fatalf("--otlp alone must not open the listener: addr=%q otlp=%q", addr, otlp)
	}
}

// --otlp-insecure is tri-state: only an explicit flag overrides the
// endpoint-based default, so Parse must record that it was given.
func TestOTLPInsecureRecordsExplicitFlag(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		set, insecure bool
	}{
		{nil, false, false},
		{[]string{"--otlp-insecure"}, true, true},
		{[]string{"--otlp-insecure=false"}, true, false},
	} {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		cfg.RegisterFlags(fs)
		if err := cfg.Parse(fs, tc.args); err != nil {
			t.Fatal(err)
		}
		if cfg.OTLPInsecureSet != tc.set || cfg.OTLPInsecure != tc.insecure {
			t.Errorf("%v: set=%v insecure=%v, want set=%v insecure=%v", tc.args, cfg.OTLPInsecureSet, cfg.OTLPInsecure, tc.set, tc.insecure)
		}
	}
}

func TestOTLPEnvCountsAsExplicit(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	cfg := Defaults()
	cfg.Once = true
	if !cfg.OTLPSet {
		t.Fatal("OTEL_EXPORTER_OTLP_ENDPOINT must mark OTLP as requested")
	}
	if addr, otlp := cfg.TelemetryExporters(); addr != "" || otlp != "localhost:4317" {
		t.Fatalf("env endpoint: addr=%q otlp=%q", addr, otlp)
	}
}

// --busy-samples is how many readings a comparable path takes on a loaded RNIC
// before the median is classified. The default has to be at least 2 or there is
// no median and busy mode is back to judging single readings against each
// other; the flag is how an operator trades probe time on a busy RNIC for it.
func TestBusySamplesFlag(t *testing.T) {
	cfg := Defaults()
	if cfg.BusySamples < 2 {
		t.Fatalf("default --busy-samples = %d, too few to have a median", cfg.BusySamples)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg.RegisterFlags(fs)
	if err := fs.Parse([]string{"--busy-samples", "5"}); err != nil {
		t.Fatal(err)
	}
	if cfg.BusySamples != 5 {
		t.Fatalf("--busy-samples = %d, want 5", cfg.BusySamples)
	}

	// A probe count is never negative, and a flag that silently turned one into
	// a sample count would be worse than an error.
	neg := Defaults()
	nfs := flag.NewFlagSet("test", flag.ContinueOnError)
	nfs.SetOutput(&strings.Builder{})
	neg.RegisterFlags(nfs)
	if err := nfs.Parse([]string{"--busy-samples", "-1"}); err == nil {
		t.Fatal("a negative sample count must fail")
	}
}

// Probe sizes whose buffers have a fixed or sane limit reject values outside
// it at parse time instead of failing (or allocating) inside every probe. The
// mutual probe's registered buffers are 4 KiB, and a SEND longer than that
// would complete in error and read as "not connected" rather than as a bad
// flag.
func TestProbeSizeFlagBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  string
		ok   bool
	}{
		{"large-msg", "0", false},
		{"large-msg", "1", true},
		{"large-msg", "16777216", true},
		{"large-msg", "16777217", false},
		{"mutual-msg", "0", false},
		{"mutual-msg", "1", true},
		{"mutual-msg", "4096", true},
		{"mutual-msg", "4097", false},
		{"mutual-msg", "-1", false},
	} {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		cfg.RegisterFlags(fs)
		err := fs.Parse([]string{"--" + tc.name, tc.val})
		if (err == nil) != tc.ok {
			t.Errorf("--%s %s: err=%v, want ok=%v", tc.name, tc.val, err, tc.ok)
		}
	}
}

// The default stdout report format follows the run kind, not the terminal:
// pretty for one-shot runs, compact json for the daemon.
func TestReportFormatDefaultsAndFlag(t *testing.T) {
	parse := func(args ...string) (Config, error) {
		t.Helper()
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		cfg.RegisterFlags(fs)
		err := cfg.Parse(fs, args)
		return cfg, err
	}
	for _, tc := range []struct {
		args []string
		want ReportFormat
	}{
		{nil, ReportFormatJSON},
		{[]string{"--once"}, ReportFormatPretty},
		{[]string{"--report-format", "pretty"}, ReportFormatPretty},
		{[]string{"--once", "--report-format=json"}, ReportFormatJSON},
		{[]string{"--report-format=none"}, ReportFormatNone},
	} {
		cfg, err := parse(tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got := cfg.EffectiveReportFormat(); got != tc.want {
			t.Errorf("%v: format = %q, want %q", tc.args, got, tc.want)
		}
	}
	if _, err := parse("--report-format=yaml"); err == nil {
		t.Fatal("unknown --report-format must be rejected")
	}
	cfg, err := parse("--report-file", "/var/log/hostping/reports.jsonl")
	if err != nil || cfg.ReportFile != "/var/log/hostping/reports.jsonl" {
		t.Fatalf("--report-file = %q err=%v", cfg.ReportFile, err)
	}
}

// The loopback default is 512 WRITE pairs (the Zig probe's maximum); the
// mutual burst keeps 64 round trips, so a pair that does not answer still
// costs at most 64 x --mutual-timeout. The memory warm-up is off.
func TestIterDefaults(t *testing.T) {
	d := Defaults()
	if d.Iters != 512 {
		t.Errorf("Iters = %d, want 512", d.Iters)
	}
	if d.MutualIters != 64 {
		t.Errorf("MutualIters = %d, want 64", d.MutualIters)
	}
	if d.MemWarmup != 0 {
		t.Errorf("MemWarmup = %v, want 0 (off)", d.MemWarmup)
	}
}

func TestUPIBytesPerFlitFlag(t *testing.T) {
	parse := func(v string) (Config, error) {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		cfg.RegisterFlags(fs)
		return cfg, fs.Parse([]string{"--upi-bytes-per-flit", v})
	}
	for v, want := range map[string]float64{"0": 0, "5.333": 5.333, "64": 64} {
		cfg, err := parse(v)
		if err != nil || cfg.UPIBytesPerFlit != want {
			t.Errorf("--upi-bytes-per-flit %s = %v %v, want %v", v, cfg.UPIBytesPerFlit, err, want)
		}
	}
	for _, v := range []string{"-1", "65", "NaN", "many"} {
		if _, err := parse(v); err == nil {
			t.Errorf("--upi-bytes-per-flit %s accepted", v)
		}
	}
}

func TestMemWarmupFlag(t *testing.T) {
	parse := func(v string) (Config, error) {
		cfg := Defaults()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(&strings.Builder{})
		cfg.RegisterFlags(fs)
		return cfg, fs.Parse([]string{"--mem-warmup", v})
	}
	for v, want := range map[string]time.Duration{
		"0":     0,
		"50ms":  50 * time.Millisecond,
		"1s":    time.Second,
		"250us": 250 * time.Microsecond,
	} {
		cfg, err := parse(v)
		if err != nil {
			t.Errorf("--mem-warmup %s: %v", v, err)
			continue
		}
		if cfg.MemWarmup != want {
			t.Errorf("--mem-warmup %s = %v, want %v", v, cfg.MemWarmup, want)
		}
	}
	for _, v := range []string{"-1ms", "1001ms", "5s", "fast"} {
		if _, err := parse(v); err == nil {
			t.Errorf("--mem-warmup %s accepted", v)
		}
	}
}

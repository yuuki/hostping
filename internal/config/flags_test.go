package config

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

// The flags, their defaults, their help text and the errors a bad value
// produces are a user-visible contract shared by hostping and hostpingctl:
// both call Config.RegisterFlags on Defaults() and add only their own flags
// afterwards (hostping: --version; hostpingctl calibrate: --runs,
// --interval, --max-discards, --out, --latency). This file snapshots that
// shared set and pins every flag.Value failure path, so a refactor of the
// flag types or of how the help texts are built shows up as a diff here and
// not as a changed --help or a changed error on an operator's terminal.
//
// Regenerate the snapshots after an intended change with
//
//	go test ./internal/config -run TestFlagsGolden -update

var updateGolden = flag.Bool("update", false, "rewrite internal/config/testdata/*.golden from the current flags")

// newFlagSet registers the flags exactly as both binaries do: on Defaults(),
// into a fresh flag.FlagSet. The OTLP endpoint default comes from the
// environment, so it is pinned to empty for a deterministic snapshot.
func newFlagSet(t *testing.T) (*Config, *flag.FlagSet) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg := Defaults()
	fs := flag.NewFlagSet("hostping", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg.RegisterFlags(fs)
	return &cfg, fs
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) == got {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Errorf("%s line %d differs:\n got: %q\nwant: %q", path, i+1, g, w)
		}
	}
	t.Fatalf("%s is stale (run with -update after an intended change)", path)
}

// TestFlagsGolden snapshots every registered flag as `name | DefValue |
// Usage`, sorted by name, and the PrintDefaults text that --help prints.
func TestFlagsGolden(t *testing.T) {
	_, fs := newFlagSet(t)
	var lines []string
	fs.VisitAll(func(f *flag.Flag) {
		lines = append(lines, f.Name+" | "+f.DefValue+" | "+f.Usage)
	})
	sort.Strings(lines)
	checkGolden(t, "flags.golden", strings.Join(lines, "\n")+"\n")

	var help strings.Builder
	fs.SetOutput(&help)
	fs.PrintDefaults()
	checkGolden(t, "flags_help.golden", help.String())
}

// Each binary's own flags live in cmd/ and are not part of the snapshot;
// this pins that the shared set is the whole of RegisterFlags, by count, so
// a flag added or removed there is noticed even if the golden is updated
// without reading the diff.
func TestFlagsGoldenCount(t *testing.T) {
	_, fs := newFlagSet(t)
	n := 0
	fs.VisitAll(func(*flag.Flag) { n++ })
	if n != 49 {
		t.Fatalf("RegisterFlags registers %d flags, want 49 (update this test and the goldens together with an intended change)", n)
	}
}

// zeroString is what the flag package compares DefValue against to decide
// whether PrintDefaults shows "(default ...)": the String() of a freshly
// allocated zero value of the flag's type.
func zeroString(v flag.Value) string {
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Pointer {
		return reflect.New(typ.Elem()).Interface().(flag.Value).String()
	}
	return reflect.Zero(typ).Interface().(flag.Value).String()
}

// TestFlagValueZeroString pins what each custom flag type prints for a zero
// value, which (together with DefValue) decides whether --help shows a
// "(default ...)" suffix for that flag.
func TestFlagValueZeroString(t *testing.T) {
	_, fs := newFlagSet(t)
	for name, want := range map[string]string{
		"iters":                "0",
		"mutual-iters":         "0",
		"small-msg":            "0",
		"busy-samples":         "0",
		"large-msg":            "0",
		"mutual-msg":           "0",
		"cpu-busy-high":        "0",
		"upi-bytes-per-flit":   "0",
		"idle-gate-timeout":    "0s",
		"latency-floor":        "0s",
		"mem-warmup":           "0s",
		"mutual-scope":         "",
		"mutual-transport":     "",
		"report-format":        "",
		"exclude-rnic":         "",
		"baseline":             "",
		"mem-channel-capacity": "",
		"upi-capacity":         "",
	} {
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("flag --%s is not registered", name)
		}
		if got := zeroString(f.Value); got != want {
			t.Errorf("--%s zero String() = %q, want %q", name, got, want)
		}
	}
}

// TestFlagValueSetErrors pins the exact error of every flag.Value Set
// failure path: unknown enum values, out-of-range and unparsable bounded
// values, and the list and capacity parsers.
func TestFlagValueSetErrors(t *testing.T) {
	cases := []struct {
		flag, arg, want string
	}{
		// Enum values: unknown, empty and wrong case are all rejected, naming
		// the accepted values.
		{"mutual-scope", "bogus", `unknown mutual scope "bogus" (want rail or host)`},
		{"mutual-scope", "", `unknown mutual scope "" (want rail or host)`},
		{"mutual-scope", "Rail", `unknown mutual scope "Rail" (want rail or host)`},
		{"mutual-transport", "bogus", `unknown mutual transport "bogus" (want rc or ud)`},
		{"mutual-transport", "", `unknown mutual transport "" (want rc or ud)`},
		{"mutual-transport", "UD", `unknown mutual transport "UD" (want rc or ud)`},
		{"report-format", "bogus", `unknown report format "bogus" (want json, pretty or none)`},
		{"report-format", "", `unknown report format "" (want json, pretty or none)`},
		{"report-format", "JSON", `unknown report format "JSON" (want json, pretty or none)`},

		// uint32 (unbounded): the strconv error is passed through.
		{"iters", "4294967296", `strconv.ParseUint: parsing "4294967296": value out of range`},
		{"iters", "-1", `strconv.ParseUint: parsing "-1": invalid syntax`},
		{"iters", "many", `strconv.ParseUint: parsing "many": invalid syntax`},
		{"iters", "", `strconv.ParseUint: parsing "": invalid syntax`},
		{"iters", "1.5", `strconv.ParseUint: parsing "1.5": invalid syntax`},
		{"iters", "+7", `strconv.ParseUint: parsing "+7": invalid syntax`},
		{"mutual-iters", "4294967296", `strconv.ParseUint: parsing "4294967296": value out of range`},
		{"small-msg", "4294967296", `strconv.ParseUint: parsing "4294967296": value out of range`},
		{"busy-samples", "-1", `strconv.ParseUint: parsing "-1": invalid syntax`},

		// Bounded uint32: parse errors first, then the [min, max] check.
		{"large-msg", "0", `0 out of range [1, 16777216]`},
		{"large-msg", "16777217", `16777217 out of range [1, 16777216]`},
		{"large-msg", "4294967296", `strconv.ParseUint: parsing "4294967296": value out of range`},
		{"large-msg", "-1", `strconv.ParseUint: parsing "-1": invalid syntax`},
		{"large-msg", "big", `strconv.ParseUint: parsing "big": invalid syntax`},
		{"mutual-msg", "0", `0 out of range [1, 4096]`},
		{"mutual-msg", "4097", `4097 out of range [1, 4096]`},
		{"mutual-msg", "-1", `strconv.ParseUint: parsing "-1": invalid syntax`},

		// Bounded float: [0, max]; NaN and the infinities are out of range.
		{"cpu-busy-high", "-0.1", `-0.1 out of range [0, 1]`},
		{"cpu-busy-high", "1.5", `1.5 out of range [0, 1]`},
		{"cpu-busy-high", "NaN", `NaN out of range [0, 1]`},
		{"cpu-busy-high", "Inf", `+Inf out of range [0, 1]`},
		{"cpu-busy-high", "-Inf", `-Inf out of range [0, 1]`},
		{"cpu-busy-high", "half", `strconv.ParseFloat: parsing "half": invalid syntax`},
		{"cpu-busy-high", "", `strconv.ParseFloat: parsing "": invalid syntax`},
		{"upi-bytes-per-flit", "-1", `-1 out of range [0, 64]`},
		{"upi-bytes-per-flit", "65", `65 out of range [0, 64]`},
		{"upi-bytes-per-flit", "64.0001", `64.0001 out of range [0, 64]`},
		{"upi-bytes-per-flit", "NaN", `NaN out of range [0, 64]`},
		{"upi-bytes-per-flit", "many", `strconv.ParseFloat: parsing "many": invalid syntax`},

		// Bounded duration: [0s, max]; the duration is printed as
		// time.Duration formats it, so 1m reads 1m0s and 10001ns 10.001µs.
		{"idle-gate-timeout", "-1s", `-1s out of range [0s, 20s]`},
		{"idle-gate-timeout", "21s", `21s out of range [0s, 20s]`},
		{"idle-gate-timeout", "20001ms", `20.001s out of range [0s, 20s]`},
		{"idle-gate-timeout", "1m", `1m0s out of range [0s, 20s]`},
		{"idle-gate-timeout", "soon", `time: invalid duration "soon"`},
		{"idle-gate-timeout", "5", `time: missing unit in duration "5"`},
		{"idle-gate-timeout", "", `time: invalid duration ""`},
		{"latency-floor", "-1ns", `-1ns out of range [0s, 10µs]`},
		{"latency-floor", "10001ns", `10.001µs out of range [0s, 10µs]`},
		{"latency-floor", "1ms", `1ms out of range [0s, 10µs]`},
		{"latency-floor", "high", `time: invalid duration "high"`},
		{"mem-warmup", "-1ms", `-1ms out of range [0s, 1s]`},
		{"mem-warmup", "1001ms", `1.001s out of range [0s, 1s]`},
		{"mem-warmup", "5s", `5s out of range [0s, 1s]`},
		{"mem-warmup", "fast", `time: invalid duration "fast"`},

		// --exclude-rnic: an empty entry anywhere in the list.
		{"exclude-rnic", "", `empty RNIC name in ""`},
		{"exclude-rnic", "mlx5_2,", `empty RNIC name in "mlx5_2,"`},
		{"exclude-rnic", ",mlx5_2", `empty RNIC name in ",mlx5_2"`},
		{"exclude-rnic", "mlx5_2, ,mlx5_3", `empty RNIC name in "mlx5_2, ,mlx5_3"`},

		// --baseline: the spec parser's errors (internal/baseline) pass
		// through unchanged.
		{"baseline", "", `empty baseline spec`},
		{"baseline", " ; ", `empty baseline spec`},
		{"baseline", "local_mem", `baseline spec "local_mem": want KEY=BW[,LAT]`},
		{"baseline", "local_mem=", `baseline spec "local_mem=": want KEY=BW[,LAT]`},
		{"baseline", "=270Gbps", `baseline spec "=270Gbps": want KEY=BW[,LAT]`},
		{"baseline", "nope=270Gbps", `unknown distance "nope" (want same_switch, multi_switch, cross_root, cross_socket, local_mem, remote_mem)`},
		{"baseline", "local_mem=270", `local_mem: bandwidth "270": need unit bps or Gbps`},
		{"baseline", "local_mem=xGbps", `local_mem: bandwidth "xGbps": invalid number`},
		{"baseline", "local_mem=0Gbps", `local_mem: bandwidth "0Gbps": invalid number`},
		{"baseline", "local_mem=270Gbps,1.2.3ns", `local_mem: latency "1.2.3ns": invalid number`},
		{"baseline", "local_mem=270Gbps,-1ns", `local_mem: latency "-1ns": invalid number`},
		// A part that ends in a unit but has no digit before it is read as a
		// bandwidth, and the splitter drops an empty trailing part, so the
		// quoted spec is the rewritten one, not what the operator typed.
		{"baseline", "local_mem=270Gbps,xns", `local_mem: bandwidth "xns": need unit bps or Gbps`},
		{"baseline", "local_mem=,", `baseline spec "local_mem=": want KEY=BW[,LAT]`},

		// --mem-channel-capacity / --upi-capacity: the capacity parser's
		// errors, and a link ID that does not carry the flag's prefix.
		{"mem-channel-capacity", "200GB", `capacity "200GB": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`},
		{"mem-channel-capacity", " 200_GB ", `capacity "200GB": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`},
		{"mem-channel-capacity", "", `capacity "": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`},
		{"mem-channel-capacity", "0Gbps", `capacity "0Gbps": invalid number`},
		{"mem-channel-capacity", "-1GB/s", `capacity "-1GB/s": invalid number`},
		{"mem-channel-capacity", "xGbps", `capacity "xGbps": invalid number`},
		{"mem-channel-capacity", "Gbps", `capacity "Gbps": invalid number`},
		{"mem-channel-capacity", "1e30TB/s", `capacity "1e30TB/s": out of range`},
		{"mem-channel-capacity", "0.1bps", `capacity "0.1bps": out of range`},
		{"mem-channel-capacity", "upi.0-1=200GB/s", "link \"upi.0-1\": want a mem.N link ID from `hostpingctl topology`"},
		{"mem-channel-capacity", "mem.=200GB/s", "link \"mem.\": want a mem.N link ID from `hostpingctl topology`"},
		{"mem-channel-capacity", "=200GB/s", "link \"\": want a mem.N link ID from `hostpingctl topology`"},
		{"mem-channel-capacity", "mem.1=200GB", `capacity "200GB": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`},
		{"mem-channel-capacity", "mem.1=", `capacity "": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`},
		{"upi-capacity", "mem.0=200GB/s", "link \"mem.0\": want a upi.N link ID from `hostpingctl topology`"},
		{"upi-capacity", "upi.=384Gbps", "link \"upi.\": want a upi.N link ID from `hostpingctl topology`"},
		{"upi-capacity", "384G", `capacity "384G": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`},
	}
	for _, tc := range cases {
		_, fs := newFlagSet(t)
		f := fs.Lookup(tc.flag)
		if f == nil {
			t.Fatalf("flag --%s is not registered", tc.flag)
		}
		err := f.Value.Set(tc.arg)
		if err == nil {
			t.Errorf("--%s %q: accepted, want error %q", tc.flag, tc.arg, tc.want)
			continue
		}
		if got := err.Error(); got != tc.want {
			t.Errorf("--%s %q:\n got %q\nwant %q", tc.flag, tc.arg, got, tc.want)
		}
	}
}

// TestFlagParseErrorWrapsSetError pins what the operator sees: the flag
// package wraps the Set error with the value and the flag name.
func TestFlagParseErrorWrapsSetError(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--mutual-scope=bogus"}, `invalid value "bogus" for flag -mutual-scope: unknown mutual scope "bogus" (want rail or host)`},
		{[]string{"--mutual-transport", "bogus"}, `invalid value "bogus" for flag -mutual-transport: unknown mutual transport "bogus" (want rc or ud)`},
		{[]string{"--report-format", "yaml"}, `invalid value "yaml" for flag -report-format: unknown report format "yaml" (want json, pretty or none)`},
		{[]string{"--idle-gate-timeout", "1m"}, `invalid value "1m" for flag -idle-gate-timeout: 1m0s out of range [0s, 20s]`},
		{[]string{"--large-msg", "0"}, `invalid value "0" for flag -large-msg: 0 out of range [1, 16777216]`},
		{[]string{"--cpu-busy-high", "2"}, `invalid value "2" for flag -cpu-busy-high: 2 out of range [0, 1]`},
		{[]string{"--baseline", "nope=1Gbps"}, `invalid value "nope=1Gbps" for flag -baseline: unknown distance "nope" (want same_switch, multi_switch, cross_root, cross_socket, local_mem, remote_mem)`},
		{[]string{"--no-such-flag"}, `flag provided but not defined: -no-such-flag`},
	} {
		cfg, fs := newFlagSet(t)
		err := cfg.Parse(fs, tc.args)
		if err == nil {
			t.Errorf("%v: accepted", tc.args)
			continue
		}
		if got := err.Error(); got != tc.want {
			t.Errorf("%v:\n got %q\nwant %q", tc.args, got, tc.want)
		}
	}
	// -h is not an error the operator sees as one.
	cfg, fs := newFlagSet(t)
	if err := cfg.Parse(fs, []string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: err = %v, want flag.ErrHelp", err)
	}
}

// TestFlagValueString pins String() of every custom flag.Value after a
// successful Set (what `flag` would print as the value, and what a
// repeatable flag shows for several values), and that Set stored the value.
func TestFlagValueString(t *testing.T) {
	for _, tc := range []struct {
		flag string
		args []string
		want string
	}{
		{"mutual-scope", []string{"host"}, "host"},
		{"mutual-scope", []string{"rail"}, "rail"},
		{"mutual-transport", []string{"ud"}, "ud"},
		{"mutual-transport", []string{"rc"}, "rc"},
		{"report-format", []string{"json"}, "json"},
		{"report-format", []string{"pretty"}, "pretty"},
		{"report-format", []string{"none"}, "none"},
		{"iters", []string{"128"}, "128"},
		{"iters", []string{"0"}, "0"},
		{"iters", []string{"4294967295"}, "4294967295"},
		{"iters", []string{"007"}, "7"},
		{"mutual-iters", []string{"8"}, "8"},
		{"small-msg", []string{"64"}, "64"},
		{"busy-samples", []string{"5"}, "5"},
		{"large-msg", []string{"524288"}, "524288"},
		{"large-msg", []string{"1"}, "1"},
		{"large-msg", []string{"16777216"}, "16777216"},
		{"mutual-msg", []string{"4096"}, "4096"},
		{"cpu-busy-high", []string{"0.75"}, "0.75"},
		{"cpu-busy-high", []string{"1"}, "1"},
		{"cpu-busy-high", []string{"0"}, "0"},
		{"cpu-busy-high", []string{"5e-1"}, "0.5"},
		{"cpu-busy-high", []string{".25"}, "0.25"},
		{"upi-bytes-per-flit", []string{"5.333"}, "5.333"},
		{"upi-bytes-per-flit", []string{"64"}, "64"},
		{"idle-gate-timeout", []string{"1500ms"}, "1.5s"},
		{"idle-gate-timeout", []string{"20s"}, "20s"},
		{"idle-gate-timeout", []string{"0"}, "0s"},
		{"latency-floor", []string{"1.5us"}, "1.5µs"},
		{"latency-floor", []string{"10us"}, "10µs"},
		{"latency-floor", []string{"800ns"}, "800ns"},
		{"mem-warmup", []string{"250us"}, "250µs"},
		{"mem-warmup", []string{"1s"}, "1s"},
		{"mem-warmup", []string{"50ms"}, "50ms"},
		{"exclude-rnic", []string{"mlx5_2"}, "mlx5_2"},
		{"exclude-rnic", []string{"mlx5_2", "mlx5_3, 0000:3c:00.1"}, "mlx5_2,mlx5_3,0000:3c:00.1"},
		{"baseline", []string{"local_mem=270Gbps,1400ns"}, "local_mem=270Gbps,1400ns"},
		{"baseline", []string{"local_mem=270Gbps,1400ns", "remote_mem=265Gbps"}, "local_mem=270Gbps,1400ns; remote_mem=265Gbps"},
		{"mem-channel-capacity", []string{"200GB/s"}, "1600000000000bps"},
		{"mem-channel-capacity", []string{"mem.1=100GB/s"}, "mem.1=800000000000bps"},
		{"mem-channel-capacity", []string{"200GB/s", "mem.1=100GB/s", "mem.0=50GB/s"}, "1600000000000bps,mem.0=400000000000bps,mem.1=800000000000bps"},
		{"mem-channel-capacity", []string{"mem.1=100GB/s", "mem.1=50GB/s"}, "mem.1=400000000000bps"},
		{"upi-capacity", []string{"upi.0-1=384Gbps"}, "upi.0-1=384000000000bps"},
		{"upi-capacity", []string{"1GiB/s"}, "8589934592bps"},
	} {
		_, fs := newFlagSet(t)
		f := fs.Lookup(tc.flag)
		if f == nil {
			t.Fatalf("flag --%s is not registered", tc.flag)
		}
		for _, a := range tc.args {
			if err := f.Value.Set(a); err != nil {
				t.Fatalf("--%s %q: %v", tc.flag, a, err)
			}
		}
		if got := f.Value.String(); got != tc.want {
			t.Errorf("--%s %q: String() = %q, want %q", tc.flag, tc.args, got, tc.want)
		}
	}
}

// TestFlagValueStringIsDefault: before any Set, String() is DefValue, so a
// flag type whose String() does not round-trip its default would show a
// wrong "(default ...)" in --help.
func TestFlagValueStringIsDefault(t *testing.T) {
	_, fs := newFlagSet(t)
	fs.VisitAll(func(f *flag.Flag) {
		if got := f.Value.String(); got != f.DefValue {
			t.Errorf("--%s: String() = %q before any Set, DefValue = %q", f.Name, got, f.DefValue)
		}
	})
}

// TestFlagSetWritesConfig: Set on every custom flag.Value lands in the Config
// field it was registered for (the pointer plumbing of the bounded and enum
// types), with the value String() reports.
func TestFlagSetWritesConfig(t *testing.T) {
	cfg, fs := newFlagSet(t)
	if err := cfg.Parse(fs, []string{
		"--mutual-scope=host",
		"--mutual-transport=ud",
		"--report-format=none",
		"--iters=128",
		"--mutual-iters=8",
		"--small-msg=64",
		"--busy-samples=5",
		"--large-msg=524288",
		"--mutual-msg=4096",
		"--cpu-busy-high=0.75",
		"--upi-bytes-per-flit=5.333",
		"--idle-gate-timeout=1500ms",
		"--latency-floor=1.5us",
		"--mem-warmup=250us",
		"--exclude-rnic=mlx5_2",
		"--baseline=local_mem=270Gbps,1400ns",
		"--mem-channel-capacity=200GB/s",
		"--upi-capacity=upi.0-1=384Gbps",
	}); err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	want.OTLPEndpoint, want.OTLPSet = "", false
	want.MutualScope = model.MutualScopeHost
	want.MutualTransport = model.MutualTransportUD
	want.ReportFormat = ReportFormatNone
	want.Iters = 128
	want.MutualIters = 8
	want.SmallMsgBytes = 64
	want.BusySamples = 5
	want.LargeMsgBytes = 524288
	want.MutualMsgBytes = 4096
	want.CPUBusyHigh = 0.75
	want.UPIBytesPerFlit = 5.333
	want.IdleGateTimeout = 1500 * time.Millisecond
	want.LatencyFloor = 1500 * time.Nanosecond
	want.MemWarmup = 250 * time.Microsecond
	want.ExcludeRNICs = []string{"mlx5_2"}
	want.BaselineSpecs = []string{"local_mem=270Gbps,1400ns"}
	want.MemCapacity = LinkCapacity{All: 1600e9}
	want.UPICapacity = LinkCapacity{ByLink: map[string]uint64{"upi.0-1": 384e9}}
	if !reflect.DeepEqual(*cfg, want) {
		t.Fatalf("parsed config differs:\n got %+v\nwant %+v", *cfg, want)
	}
}

// TestBoundedFlagMaxConstants pins the Max* constants the bounds and the
// help texts are built from. The help text itself is in flags.golden; this
// keeps the number in the error message and the one in the help text tied
// to the same constant.
func TestBoundedFlagMaxConstants(t *testing.T) {
	if MaxLargeMsgBytes != 16<<20 || MaxMutualMsgBytes != 4096 {
		t.Errorf("MaxLargeMsgBytes=%d MaxMutualMsgBytes=%d", MaxLargeMsgBytes, MaxMutualMsgBytes)
	}
	if MaxMemWarmup != time.Second || MaxIdleGateTimeout != 20*time.Second || MaxLatencyFloor != 10*time.Microsecond {
		t.Errorf("MaxMemWarmup=%v MaxIdleGateTimeout=%v MaxLatencyFloor=%v", MaxMemWarmup, MaxIdleGateTimeout, MaxLatencyFloor)
	}
	if MaxUPIBytesPerFlit != 64 {
		t.Errorf("MaxUPIBytesPerFlit=%v", MaxUPIBytesPerFlit)
	}
	_, fs := newFlagSet(t)
	for name, want := range map[string]string{
		"idle-gate-timeout":  "(at most 20s)",
		"latency-floor":      "(at most 10us;",
		"mem-warmup":         "at most 1s;",
		"large-msg":          "at most 16 MiB",
		"mutual-msg":         "1 to 4096",
		"upi-bytes-per-flit": "at most 64)",
		"cpu-busy-high":      "at most 1)",
		"iters":              "at most 512",
	} {
		if u := fs.Lookup(name).Usage; !strings.Contains(u, want) {
			t.Errorf("--%s help lacks %q: %s", name, want, u)
		}
	}
}

// TestValidateMutualMessages pins the exact text of each ValidateMutual
// rejection, and the order the checks run in when several apply.
func TestValidateMutualMessages(t *testing.T) {
	ud, rc := model.MutualTransportUD, model.MutualTransportRC
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"zero timeout", Config{MutualTransport: ud, MutualTimeout: 0, MutualWindow: time.Minute},
			"--mutual-timeout must be positive, got 0s"},
		{"negative timeout", Config{MutualTransport: ud, MutualTimeout: -time.Second, MutualWindow: time.Minute},
			"--mutual-timeout must be positive, got -1s"},
		{"negative interval", Config{MutualTransport: ud, MutualTimeout: time.Second, MutualInterval: -1, MutualWindow: time.Minute},
			"--mutual-interval must not be negative, got -1ns"},
		{"negative interval ms", Config{MutualTransport: ud, MutualTimeout: time.Second, MutualInterval: -100 * time.Millisecond, MutualWindow: time.Minute},
			"--mutual-interval must not be negative, got -100ms"},
		{"schedule with rc", Config{MutualTransport: rc, MutualTimeout: 500 * time.Millisecond, MutualInterval: 100 * time.Millisecond, MutualWindow: 20 * time.Second},
			"--mutual-interval needs --mutual-transport=ud: RC retransmission hides the isolated losses the scheduled probe counts (got --mutual-transport=rc)"},
		{"schedule with no transport", Config{MutualTimeout: 500 * time.Millisecond, MutualInterval: 100 * time.Millisecond, MutualWindow: 20 * time.Second},
			"--mutual-interval needs --mutual-transport=ud: RC retransmission hides the isolated losses the scheduled probe counts (got --mutual-transport=)"},
		{"window shorter than interval", Config{MutualTransport: ud, MutualTimeout: time.Second, MutualInterval: time.Minute, MutualWindow: time.Second},
			"--mutual-window 1s is shorter than --mutual-interval 1m0s"},
		{"zero window", Config{MutualTransport: ud, MutualTimeout: time.Second, MutualInterval: 100 * time.Millisecond},
			"--mutual-window 0s is shorter than --mutual-interval 100ms"},
		// Timeout is checked first, then the interval sign, then the transport.
		{"timeout before interval", Config{MutualTransport: rc, MutualTimeout: 0, MutualInterval: -1},
			"--mutual-timeout must be positive, got 0s"},
		{"interval sign before transport", Config{MutualTransport: rc, MutualTimeout: time.Second, MutualInterval: -1},
			"--mutual-interval must not be negative, got -1ns"},
		{"transport before window", Config{MutualTransport: rc, MutualTimeout: time.Second, MutualInterval: time.Minute, MutualWindow: time.Second},
			"--mutual-interval needs --mutual-transport=ud: RC retransmission hides the isolated losses the scheduled probe counts (got --mutual-transport=rc)"},
	} {
		err := tc.cfg.ValidateMutual()
		if err == nil {
			t.Errorf("%s: accepted, want %q", tc.name, tc.want)
			continue
		}
		if got := err.Error(); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	// The default, the schedule with ud, and a window equal to the interval
	// pass; so does rc with no schedule and a zero window.
	for _, ok := range []Config{
		Defaults(),
		{MutualTransport: ud, MutualTimeout: time.Second, MutualInterval: 100 * time.Millisecond, MutualWindow: 100 * time.Millisecond},
		{MutualTransport: rc, MutualTimeout: time.Nanosecond},
	} {
		if err := ok.ValidateMutual(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}

// TestValidateIdleGateMessages pins the ValidateIdleGate rejection text.
func TestValidateIdleGateMessages(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{IdleGateCommand: "/bin/true", IdleGateTimeout: 0}, "--idle-gate-timeout must be positive with --idle-gate-command, got 0s"},
		{Config{IdleGateCommand: "/bin/true", IdleGateTimeout: -time.Second}, "--idle-gate-timeout must be positive with --idle-gate-command, got -1s"},
	} {
		err := tc.cfg.ValidateIdleGate()
		if err == nil {
			t.Errorf("%+v: accepted, want %q", tc.cfg, tc.want)
			continue
		}
		if got := err.Error(); got != tc.want {
			t.Errorf("%+v:\n got %q\nwant %q", tc.cfg, got, tc.want)
		}
	}
	for _, ok := range []Config{
		{},
		{IdleGateTimeout: 0},
		{IdleGateTimeout: -time.Second},
		{IdleGateCommand: "/bin/true", IdleGateTimeout: time.Nanosecond},
	} {
		if err := ok.ValidateIdleGate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}

// TestEffectiveReportFormatTable pins the stdout format for every
// (ReportFormat, Once) combination.
func TestEffectiveReportFormatTable(t *testing.T) {
	for _, tc := range []struct {
		format ReportFormat
		once   bool
		want   ReportFormat
	}{
		{"", false, ReportFormatJSON},
		{"", true, ReportFormatPretty},
		{ReportFormatJSON, false, ReportFormatJSON},
		{ReportFormatJSON, true, ReportFormatJSON},
		{ReportFormatPretty, false, ReportFormatPretty},
		{ReportFormatPretty, true, ReportFormatPretty},
		{ReportFormatNone, false, ReportFormatNone},
		{ReportFormatNone, true, ReportFormatNone},
	} {
		c := Config{ReportFormat: tc.format, Once: tc.once}
		if got := c.EffectiveReportFormat(); got != tc.want {
			t.Errorf("format=%q once=%v: %q, want %q", tc.format, tc.once, got, tc.want)
		}
	}
}

// TestParseCapacityErrors pins ParseCapacity's error texts directly (the
// flag tests above cover them through the flag; this covers the exported
// parser other packages may call).
func TestParseCapacityErrors(t *testing.T) {
	for in, want := range map[string]string{
		"":         `capacity "": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`,
		"400":      `capacity "400": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`,
		"50GB":     `capacity "50GB": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`,
		"400G":     `capacity "400G": need a unit, bits (bps, Gbps, Gb/s) or bytes (GB/s, GiB/s)`,
		"0Gbps":    `capacity "0Gbps": invalid number`,
		"-1GB/s":   `capacity "-1GB/s": invalid number`,
		"xGbps":    `capacity "xGbps": invalid number`,
		"NaNGbps":  `capacity "NaNGbps": invalid number`,
		"InfGbps":  `capacity "InfGbps": invalid number`,
		"1e30TB/s": `capacity "1e30TB/s": out of range`,
		"0.5bps":   `capacity "0.5bps": out of range`,
	} {
		_, err := ParseCapacity(in)
		if err == nil {
			t.Errorf("ParseCapacity(%q): accepted, want %q", in, want)
			continue
		}
		if got := err.Error(); got != want {
			t.Errorf("ParseCapacity(%q):\n got %q\nwant %q", in, got, want)
		}
	}
}

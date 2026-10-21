package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// mkRoCEPort lays out one active Ethernet-link-layer port with byte counters
// and port_xmit_discards.
func mkRoCEPort(t *testing.T, root, dev string) {
	t.Helper()
	mkIBPort(t, root, dev, "1")
	base := filepath.Join("class/infiniband", dev, "ports/1")
	mkFile(t, root, filepath.Join(base, "link_layer"), "Ethernet")
	mkFile(t, root, filepath.Join(base, "counters/port_xmit_discards"), "0")
}

func noEthtool(context.Context, string, ...string) ([]byte, error) {
	return nil, &exec.Error{Name: "ethtool", Err: exec.ErrNotFound}
}

func TestReadRNICSysfsInfiniBandHasNoPauseSourceWithoutEthtool(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("ethtool must not run for an InfiniBand link layer")
		return nil, nil
	})
	mkIBPort(t, root, "dev0", "1")
	mkFile(t, root, "class/infiniband/dev0/ports/1/link_layer", "InfiniBand")
	mkFile(t, root, "class/infiniband/dev0/ports/1/counters/port_xmit_discards", "0")
	// A pause-looking hw counter on an IB port is not PFC and is not read.
	mkFile(t, root, "class/infiniband/dev0/ports/1/hw_counters/tx_pause_duration", "9")

	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "dev0", Netdev: "ib0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PauseSources != 0 || c.TxPauseDurationUs != 0 || !strings.Contains(c.PauseGap, "InfiniBand") {
		t.Fatalf("pause sources=%b tx=%d gap=%q, want none for InfiniBand", c.PauseSources, c.TxPauseDurationUs, c.PauseGap)
	}
	if c.DropSources&srcIBDiscards == 0 {
		t.Fatalf("drop sources=%b, want port_xmit_discards", c.DropSources)
	}
}

func TestReadRNICSysfsRoCEPauseFileReadingZeroIsKnown(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, noEthtool)
	mkRoCEPort(t, root, "dev0")
	mkFile(t, root, "class/infiniband/dev0/ports/1/hw_counters/tx_prio3_pause_duration", "0")

	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "dev0", Netdev: "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PauseSources != srcHWCounters || c.PauseGap != "" || c.TxPauseDurationUs != 0 {
		t.Fatalf("pause sources=%b gap=%q tx=%d, want hw_counters reading 0", c.PauseSources, c.PauseGap, c.TxPauseDurationUs)
	}
	if c.DropSources != srcIBDiscards {
		t.Fatalf("drop sources=%b, want port_xmit_discards only", c.DropSources)
	}
}

func TestReadRNICSysfsRoCEWithoutPauseSourceNamesMissingEthtool(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, noEthtool)
	mkRoCEPort(t, root, "dev0")

	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "dev0", Netdev: "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PauseSources != 0 || !strings.Contains(c.PauseGap, "ethtool not on PATH") {
		t.Fatalf("pause sources=%b gap=%q, want none naming ethtool", c.PauseSources, c.PauseGap)
	}
	// Still backed off on the next poll; the reason is kept.
	c, _ = ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "dev0", Netdev: "eth0"})
	if !strings.Contains(c.PauseGap, "ethtool not on PATH") {
		t.Fatalf("gap=%q, want the backoff to keep the reason", c.PauseGap)
	}
}

func TestReadRNICSysfsEthtoolSuppliesPauseAndDrops(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte(ethtoolPauseOut), nil
	})
	mkRoCEPort(t, root, "dev0")
	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "dev0", Netdev: "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PauseSources != srcEthtool || c.TxPauseDurationUs != 7 {
		t.Fatalf("pause sources=%b tx=%d, want ethtool 7", c.PauseSources, c.TxPauseDurationUs)
	}
	if c.DropSources != srcIBDiscards|srcEthtool || c.Drops["rx_discards_phy"] != 1 {
		t.Fatalf("drop sources=%b drops=%v", c.DropSources, c.Drops)
	}
}

// With a pause counter under hw_counters, ethtool still supplies the
// *_discards_phy drops but not a second pause duration.
func TestReadRNICSysfsHWPauseKeepsEthtoolDrops(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte(ethtoolPauseOut), nil
	})
	mkRoCEPort(t, root, "dev0")
	mkFile(t, root, "class/infiniband/dev0/ports/1/hw_counters/tx_pause_duration", "40")
	c, err := ReadRNICSysfsFrom(context.Background(), root, model.RNIC{Device: "dev0", Netdev: "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if c.PauseSources != srcHWCounters || c.TxPauseDurationUs != 40 {
		t.Fatalf("pause sources=%b tx=%d, want hw_counters 40 only", c.PauseSources, c.TxPauseDurationUs)
	}
	if c.DropSources&srcEthtool == 0 || c.Drops["rx_discards_phy"] != 1 {
		t.Fatalf("drop sources=%b drops=%v, want ethtool drops", c.DropSources, c.Drops)
	}
}

// sysrootSampler reads RNIC counters from a fixture tree.
type sysrootSampler struct{ root string }

func (s sysrootSampler) RNIC(ctx context.Context, n model.RNIC) (model.RNICCounters, error) {
	return ReadRNICSysfsFrom(ctx, s.root, n)
}
func (sysrootSampler) GPUs(context.Context) ([]model.GPUStatus, error) { return nil, nil }
func (sysrootSampler) Bus(context.Context) map[string]float64          { return nil }

type logSink struct{ lines []string }

func (l *logSink) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) count(sub string) int {
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func newFixtureMonitor(t *testing.T, root string) (*Monitor, *logSink) {
	t.Helper()
	g := topology.Graph{RNICs: []model.RNIC{{ID: "rnic3", Device: "dev0", Netdev: "eth0", LineRate: 200e9}}}
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	cfg.BusyCooldown = 0
	m := New(cfg, g, sysrootSampler{root: root})
	start := time.Unix(1000, 0)
	tick := 0
	m.SetClock(func() time.Time { tick++; return start.Add(time.Duration(tick) * time.Second) })
	sink := &logSink{}
	m.Logf = sink.logf
	return m, sink
}

func poll(t *testing.T, m *Monitor) Decision {
	t.Helper()
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMonitorNoPauseSourceIsNotApplicable(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, noEthtool)
	mkRoCEPort(t, root, "dev0")
	m, sink := newFixtureMonitor(t, root)
	poll(t, m)
	d := poll(t, m)
	if !d.HostIdle || !d.IdleKnown || len(d.UnreadCounters) != 0 {
		t.Fatalf("a never-present pause source must not block idle: %+v", d)
	}
	if d.Rates[0].PauseKnown || !d.Rates[0].DropKnown {
		t.Fatalf("rates %+v, want pause unmeasured, drops measured", d.Rates[0])
	}
	poll(t, m)
	want := "rnic3 has no PFC pause-duration source (no hw_counters *pause*duration*, ethtool not on PATH); the PFC trigger cannot fire for it"
	if sink.count(want) != 1 || len(sink.lines) != 1 {
		t.Fatalf("log %q, want exactly %q", sink.lines, want)
	}
}

func TestMonitorInfiniBandPauseIsNotApplicable(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("ethtool must not run for an InfiniBand link layer")
		return nil, nil
	})
	mkIBPort(t, root, "dev0", "1")
	mkFile(t, root, "class/infiniband/dev0/ports/1/link_layer", "InfiniBand")
	mkFile(t, root, "class/infiniband/dev0/ports/1/counters/port_xmit_discards", "0")
	m, sink := newFixtureMonitor(t, root)
	poll(t, m)
	if d := poll(t, m); !d.HostIdle || !d.IdleKnown {
		t.Fatalf("InfiniBand without PFC must be idle: %+v", d)
	}
	if sink.count("rnic3 has no PFC pause-duration source (link layer is InfiniBand") != 1 {
		t.Fatalf("log %q", sink.lines)
	}
}

// The full lifecycle of a pause source: known (reading 0), vanished
// (unknown: not idle, not measured, logged once), back (re-baselined, so the
// jump does not trigger), then measured again.
func TestMonitorPauseSourceVanishesAndReturns(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, noEthtool)
	mkRoCEPort(t, root, "dev0")
	pausePath := filepath.Join(root, "class/infiniband/dev0/ports/1/hw_counters/tx_pause_duration")
	mkFile(t, root, "class/infiniband/dev0/ports/1/hw_counters/tx_pause_duration", "0")
	m, sink := newFixtureMonitor(t, root)

	poll(t, m)
	d := poll(t, m)
	if !d.HostIdle || !d.IdleKnown || !d.Rates[0].PauseKnown || d.Rates[0].PauseDurationRatio != 0 {
		t.Fatalf("a present pause counter reading 0 is known and idle: %+v", d)
	}

	if err := os.Remove(pausePath); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		d = poll(t, m)
		if d.HostIdle || d.IdleKnown || d.Run || d.Rates[0].PauseKnown {
			t.Fatalf("poll %d: unreadable pause must block idle and not be measured: %+v", i, d)
		}
		if len(d.UnreadCounters) != 1 || d.UnreadCounters[0] != "rnic3" {
			t.Fatalf("UnreadCounters=%v", d.UnreadCounters)
		}
	}
	if n := sink.count("rnic3 PFC pause-duration counter could not be read"); n != 1 {
		t.Fatalf("unknown logged %d times, want once: %q", n, sink.lines)
	}

	// Back with a large value: 30 s of pause would be a ratio far above
	// PFCHigh if it were differenced against the last reading.
	mkFile(t, root, "class/infiniband/dev0/ports/1/hw_counters/tx_pause_duration", "30000000")
	d = poll(t, m)
	if d.Run || d.Rates[0].PauseKnown || d.HostIdle {
		t.Fatalf("the first sample after the source returns only re-baselines: %+v", d)
	}
	if sink.count("rnic3 PFC pause-duration counter reads again; re-baselined") != 1 {
		t.Fatalf("log %q", sink.lines)
	}
	d = poll(t, m)
	if d.Run || !d.Rates[0].PauseKnown || !d.HostIdle || !d.IdleKnown {
		t.Fatalf("measured again against the new baseline: %+v", d)
	}
	mkFile(t, root, "class/infiniband/dev0/ports/1/hw_counters/tx_pause_duration", "30500000")
	d = poll(t, m)
	if !d.Run || d.Trigger != model.TriggerPFC {
		t.Fatalf("a real pause after the re-baseline must trigger: %+v", d)
	}
	if len(sink.lines) != 2 {
		t.Fatalf("log %q, want one line per change", sink.lines)
	}
}

func TestMonitorEthtoolPauseSourceFailingIsUnknown(t *testing.T) {
	root := t.TempDir()
	answer := true
	stubEthtool(t, func(context.Context, string, ...string) ([]byte, error) {
		if answer {
			return []byte(ethtoolPauseOut), nil
		}
		return nil, errors.New("signal: killed")
	})
	mkRoCEPort(t, root, "dev0")
	m, sink := newFixtureMonitor(t, root)
	poll(t, m)
	if d := poll(t, m); !d.HostIdle || !d.Rates[0].PauseKnown || !d.Rates[0].DropKnown {
		t.Fatalf("ethtool answering: %+v", d)
	}
	answer = false
	d := poll(t, m)
	if d.HostIdle || d.IdleKnown || d.Rates[0].PauseKnown || d.Rates[0].DropKnown {
		t.Fatalf("ethtool that answered before and now fails is unknown: %+v", d)
	}
	// In backoff the next poll does not fork, and is still unknown.
	if d = poll(t, m); d.HostIdle || d.Rates[0].PauseKnown {
		t.Fatalf("backed-off ethtool is unknown: %+v", d)
	}
	if sink.count("rnic3 PFC pause-duration counter could not be read (no hw_counters *pause*duration*, ethtool -S failed: signal: killed)") != 1 {
		t.Fatalf("log %q", sink.lines)
	}
	if sink.count("rnic3 packet-drop counter could not be read") != 1 {
		t.Fatalf("log %q", sink.lines)
	}
}

// Monitor-level checks on the StaticSampler: a counter family with no source
// (a gap and no source bits) never blocks idle and never triggers, while one
// whose source disappears blocks idle until it reads again.
func TestMonitorUnknownDropsBlockIdleNotApplicableDoNot(t *testing.T) {
	g := topology.Graph{RNICs: []model.RNIC{{ID: "rnic3", Device: "dev0", LineRate: 200e9}}}
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	s := StaticSampler{Counters: map[string]model.RNICCounters{
		"rnic3": {DropGap: "none", PauseGap: "none"},
	}}
	m := New(cfg, g, s)
	m.Logf = func(string, ...any) {}
	poll(t, m)
	s.Counters["rnic3"] = model.RNICCounters{DropGap: "none", PauseGap: "none", Drops: map[string]uint64{"rx_discards_phy": 5}, TxPauseDurationUs: 1 << 40}
	d := poll(t, m)
	if !d.HostIdle || !d.IdleKnown || d.Trigger != model.TriggerIdle || len(d.DropDelta) != 0 {
		t.Fatalf("not-applicable counters must neither trigger nor block idle: %+v", d)
	}

	// Drops readable, then not.
	m = New(cfg, g, s)
	m.Logf = func(string, ...any) {}
	s.Counters["rnic3"] = model.RNICCounters{}
	poll(t, m)
	s.Counters["rnic3"] = model.RNICCounters{DropGap: "gone"}
	d = poll(t, m)
	if d.HostIdle || d.IdleKnown || d.Run || d.Rates[0].DropKnown {
		t.Fatalf("unknown drops must block idle: %+v", d)
	}
	if _, ok := d.DropDelta["rnic3"]; ok {
		t.Fatalf("unknown drops must have no delta: %+v", d.DropDelta)
	}
}

// A shutdown cancels the poll in flight. That is not a counter source failing:
// no state changes, nothing is logged, ethtool is not backed off, and the next
// live poll measures against the last good sample.
func TestMonitorCanceledPollIsNotAnUnreadableSource(t *testing.T) {
	root := t.TempDir()
	var cancel context.CancelFunc
	calls := 0
	stubEthtool(t, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		calls++
		if cancel != nil {
			cancel() // SIGTERM arrives while ethtool runs
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []byte(ethtoolPauseOut), nil
	})
	mkRoCEPort(t, root, "dev0")
	m, sink := newFixtureMonitor(t, root)
	poll(t, m)
	poll(t, m)

	var ctx context.Context
	ctx, cancel = context.WithCancel(context.Background())
	if _, err := m.Poll(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled poll err=%v, want context.Canceled", err)
	}
	if len(sink.lines) != 0 {
		t.Fatalf("a canceled poll must log nothing: %q", sink.lines)
	}
	if !ethtool.allow("eth0") || ethtool.lastWhy("eth0") != "" {
		t.Fatalf("a canceled ethtool must not back off (why=%q)", ethtool.lastWhy("eth0"))
	}
	if st := m.pauseSrc["rnic3"].state; st != sourceKnown {
		t.Fatalf("pause state=%v, want still known", st)
	}
	if st := m.dropSrc["rnic3"].state; st != sourceKnown {
		t.Fatalf("drop state=%v, want still known", st)
	}
	// An already-canceled ctx does not even fork.
	before := calls
	if _, err := m.Poll(ctx); err == nil {
		t.Fatal("poll on a done ctx must fail")
	}
	if calls != before {
		t.Fatalf("ethtool forked on a done ctx")
	}

	cancel = nil
	d := poll(t, m)
	if !d.Rates[0].PauseKnown || !d.Rates[0].DropKnown || !d.HostIdle || len(sink.lines) != 0 {
		t.Fatalf("after the canceled poll the counters stay measured: %+v log=%q", d, sink.lines)
	}
}

// ethtool's own bound expiring while the caller's ctx is live is a real
// failure, unlike a cancellation from the caller.
func TestEthtoolScanOwnTimeoutIsAFailure(t *testing.T) {
	g := &ethtoolGate{run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}}
	var c model.RNICCounters
	res := g.scan(context.Background(), "eth0", &c, true)
	if res.why == "" || g.allow("eth0") {
		t.Fatalf("own timeout must back off with a reason: %+v", res)
	}
}

// The engine-run re-baseline is unaffected: the counters it forgets come back
// as a new baseline, not as an unknown source.
func TestMonitorMarkRunKeepsSourcesKnown(t *testing.T) {
	root := t.TempDir()
	stubEthtool(t, noEthtool)
	mkRoCEPort(t, root, "dev0")
	mkFile(t, root, "class/infiniband/dev0/ports/1/hw_counters/tx_pause_duration", "0")
	m, sink := newFixtureMonitor(t, root)
	poll(t, m)
	poll(t, m)
	m.MarkRun(time.Unix(0, 0))
	if d := poll(t, m); d.Ready || d.Run {
		t.Fatalf("post-run sample re-baselines: %+v", d)
	}
	if d := poll(t, m); !d.HostIdle || !d.IdleKnown || !d.Rates[0].PauseKnown {
		t.Fatalf("after the run the counters are measured again: %+v", d)
	}
	if len(sink.lines) != 0 {
		t.Fatalf("log %q, want nothing", sink.lines)
	}
}

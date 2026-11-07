package monitor

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/topology"
)

// A counter that goes down wrapped only from the upper half of its 32- or
// 64-bit range. Any other decrease is a reset, whose delta is unknown: read
// as a wrap, a reset from 100 to 0 was a delta of 2^32.
func TestCounterDelta(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prev, cur uint64
		want      uint64
		ok        bool
	}{
		{"increase", 7, 9, 2, true},
		{"equal", 42, 42, 0, true},
		{"equal at 0", 0, 0, 0, true},
		{"equal at the 64-bit max", math.MaxUint64, math.MaxUint64, 0, true},
		{"32-bit wrap from the upper half", 0xfffffff0, 0x10, 0x20, true},
		{"32-bit wrap from half the range", 1 << 31, 0, 1 << 31, true},
		{"32-bit wrap from the max", math.MaxUint32, 0, 1, true},
		{"64-bit wrap from the upper half", 0xfffffffffffffff0, 0x10, 0x20, true},
		{"64-bit wrap from half the range", 1 << 63, 0, 1 << 63, true},
		{"reset to 0 from the 32-bit lower half", 100, 0, 0, false},
		{"reset to a smaller value", 100, 40, 0, false},
		{"reset just below the 32-bit upper half", 1<<31 - 1, 0x10, 0, false},
		{"reset in the 64-bit lower half", 1 << 40, 5, 0, false},
		{"reset just below the 64-bit upper half", 1<<63 - 1, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := counterDelta(tc.cur, tc.prev)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("counterDelta(%#x, %#x) = %#x, %v; want %#x, %v", tc.cur, tc.prev, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A reset byte counter says nothing about the interval's traffic: the RNIC is
// neither busy (a delta near 2^64 against the line rate) nor idle (a delta of
// 0), and the host cannot be idle on that poll. The other counters of the
// same poll are still measured.
func TestRNICRatesByteCounterResetIsNeitherBusyNorIdle(t *testing.T) {
	n := model.RNIC{ID: "rnic0", LineRate: 200e9}
	prev := model.RNICCounters{RxBytes: 5 << 40, TxBytes: 3 << 40, TxPauseDurationUs: 1000, PauseSources: 1, DropSources: 1}
	for _, tc := range []struct {
		name   string
		rx, tx uint64
	}{
		{"both reset", 0, 0},
		{"rx reset", 4096, 3<<40 + 1<<20},
		{"tx reset", 5<<40 + 1<<20, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := prev
			cur.RxBytes, cur.TxBytes = tc.rx, tc.tx
			r, f, _ := rnicRates(n, prev, cur, 1, sourceKnown, sourceKnown, 0.05, 0.03)
			if !r.BytesReset || r.ThroughputBPS != 0 || r.Idle {
				t.Fatalf("rates %+v: want throughput unknown (BytesReset), not idle", r)
			}
			if f.busy || f.pfc || f.unread || f.lineRateUnknown {
				t.Fatalf("flags %+v: a reset must not read as load or as an unread pause/drop counter", f)
			}
			if !f.bytesReset || f.classified() || f.idle() {
				t.Fatalf("flags %+v: a reset RNIC is unclassified, so the host is not idle", f)
			}
			if !r.PauseKnown || r.PauseDurationRatio != 0 || !r.DropKnown {
				t.Fatalf("rates %+v: pause and drops are still measured", r)
			}
		})
	}

	// A 64-bit wrap is still traffic: 2^31 bytes in one second is above
	// Thplow (5% of 200 Gb/s).
	cur := prev
	cur.RxBytes = 1 << 30
	wrapped := prev
	wrapped.RxBytes = math.MaxUint64 - 1<<30 + 1
	r, f, _ := rnicRates(n, wrapped, cur, 1, sourceKnown, sourceKnown, 0.05, 0.03)
	if r.BytesReset || r.ThroughputBPS != 8<<31 || r.Idle || !f.busy || !f.classified() {
		t.Fatalf("rates %+v flags %+v: a wrap from the upper half is a delta of 2^31 bytes, busy", r, f)
	}
}

// A reset pause counter leaves the ratio unmeasured, like a re-baseline:
// neither a ratio in the thousands (a 32-bit wrap from 30 s of pause) that
// fires the PFC trigger, nor a measured 0.
func TestRNICRatesPauseCounterResetIsUnknown(t *testing.T) {
	n := model.RNIC{ID: "rnic0", LineRate: 200e9}
	prev := model.RNICCounters{TxPauseDurationUs: 30_000_000, PauseSources: 1, DropSources: 1}
	cur := prev
	cur.TxPauseDurationUs = 40_000
	r, f, _ := rnicRates(n, prev, cur, 1, sourceKnown, sourceKnown, 0.05, 0.03)
	if r.PauseKnown || r.PauseDurationRatio != 0 {
		t.Fatalf("rates %+v: a reset pause counter is unmeasured on this poll", r)
	}
	if f.pfc || f.busy {
		t.Fatalf("flags %+v: a reset must not fire the PFC trigger", f)
	}
	if !f.unread || !f.pauseReset || f.idle() {
		t.Fatalf("flags %+v: an unmeasured pause counter that has a source is unread, not idle", f)
	}
	if r.BytesReset || !r.Idle || !r.DropKnown {
		t.Fatalf("rates %+v: throughput and drops are still measured", r)
	}

	// A 32-bit wrap from the upper half is still pause: 0x20000 µs (131 ms)
	// in one second is above PFCHigh.
	wrapped := prev
	wrapped.TxPauseDurationUs = 0xffff0000
	cur.TxPauseDurationUs = 0x10000
	r, f, _ = rnicRates(n, wrapped, cur, 1, sourceKnown, sourceKnown, 0.05, 0.03)
	if !r.PauseKnown || r.PauseDurationRatio != 0x20000/1e6 || !f.pfc || f.unread {
		t.Fatalf("rates %+v flags %+v: a 32-bit wrap is a measured pause above PFCHigh", r, f)
	}
}

// resetMonitor is a Fig.2 monitor over s whose clock moves one second per
// poll and whose log goes to the returned sink.
func resetMonitor(cfg config.Config, s StaticSampler) (*Monitor, *logSink) {
	m := New(cfg, topology.Fig2(), s)
	start := time.Unix(1000, 0)
	tick := 0
	m.SetClock(func() time.Time { tick++; return start.Add(time.Duration(tick) * time.Second) })
	sink := &logSink{}
	m.Logf = sink.logf
	return m, sink
}

// Through Poll, a byte-counter reset (a driver reload) makes that one poll
// unknown: no idle run, the RNIC is not busy, its hop utilization is
// unknown, the RNIC is named and the reset logged. The next poll measures
// against the new baseline.
func TestPollByteCounterResetIsUnknownForOnePoll(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	s := StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {RxBytes: 5 << 40, TxBytes: 5 << 40}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m, sink := resetMonitor(cfg, s)
	poll(t, m)

	s.Counters["rnic0"] = model.RNICCounters{RxBytes: 1 << 20}
	d := poll(t, m)
	if d.Run || d.HostIdle || d.IdleKnown || d.BusyRNICs["rnic0"] {
		t.Fatalf("a byte-counter reset is neither idle nor busy: %+v", d)
	}
	if !slices.Equal(d.BytesReset, []string{"rnic0"}) || len(d.UnreadCounters) != 0 {
		t.Fatalf("BytesReset=%v UnreadCounters=%v, want [rnic0] and none", d.BytesReset, d.UnreadCounters)
	}
	if !d.Rates[0].BytesReset || d.Rates[0].Idle || !d.Rates[0].PauseKnown {
		t.Fatalf("rates %+v", d.Rates[0])
	}
	if _, ok := d.Util["pcie.rnic0"]; ok {
		t.Fatalf("hop utilization of a reset RNIC must be unknown: %v", d.Util)
	}
	if _, ok := d.Util["pcie.rnic1"]; !ok {
		t.Fatalf("other RNICs keep theirs: %v", d.Util)
	}
	if n := sink.count("rnic0 byte counters went down (a reset, not a wrap)"); n != 1 || len(sink.lines) != 1 {
		t.Fatalf("log %q, want the reset once", sink.lines)
	}

	s.Counters["rnic0"] = model.RNICCounters{RxBytes: 1<<20 + 4096}
	d = poll(t, m)
	if !d.Run || d.Trigger != model.TriggerIdle || !d.HostIdle || !d.IdleKnown || len(d.BytesReset) != 0 {
		t.Fatalf("measured again against the new baseline: %+v", d)
	}
	if len(sink.lines) != 1 {
		t.Fatalf("log %q, want one line per reset", sink.lines)
	}
}

// The busy RNIC set of a --busy-interval run cannot be classified on the poll
// a byte counter was reset: the run waits for the next poll.
func TestPollByteCounterResetDefersPeriodicRun(t *testing.T) {
	cfg := config.Defaults()
	cfg.BusyInterval = time.Nanosecond
	cfg.BusyCooldown = 0
	s := StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {RxBytes: 5 << 40}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: false}},
	}
	m, _ := resetMonitor(cfg, s)
	poll(t, m)
	if d := poll(t, m); d.Run {
		t.Fatalf("the periodic interval starts at the first ready poll: %+v", d)
	}
	s.Counters["rnic0"] = model.RNICCounters{RxBytes: 1 << 20}
	if d := poll(t, m); d.Run {
		t.Fatalf("a reset RNIC is unclassified, no periodic run: %+v", d)
	}
	if d := poll(t, m); !d.Run || d.Trigger != model.TriggerPeriodic {
		t.Fatalf("the next poll classifies it again: %+v", d)
	}
}

// Through Poll, a pause-counter reset is unread for one poll: no PFC run (a
// 32-bit wrap would read thousands of seconds of pause per second), not
// idle. The next pause is measured against the new baseline and triggers.
func TestPollPauseCounterResetIsUnreadForOnePoll(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = time.Hour
	cfg.BusyCooldown = 0
	s := StaticSampler{
		Counters: map[string]model.RNICCounters{"rnic0": {TxPauseDurationUs: 30_000_000}},
		GPU:      []model.GPUStatus{{ID: "gpu0", Idle: true}},
	}
	m, sink := resetMonitor(cfg, s)
	poll(t, m)

	s.Counters["rnic0"] = model.RNICCounters{TxPauseDurationUs: 1000}
	d := poll(t, m)
	if d.Run || d.HostIdle || d.IdleKnown || d.BusyRNICs["rnic0"] || d.Rates[0].PauseKnown {
		t.Fatalf("a pause-counter reset is unread, never a PFC run: %+v", d)
	}
	if !slices.Equal(d.UnreadCounters, []string{"rnic0"}) || len(d.BytesReset) != 0 {
		t.Fatalf("UnreadCounters=%v BytesReset=%v, want [rnic0] and none", d.UnreadCounters, d.BytesReset)
	}
	if sink.count("rnic0 PFC pause-duration counter went down (a reset, not a wrap)") != 1 || len(sink.lines) != 1 {
		t.Fatalf("log %q, want the reset once", sink.lines)
	}

	s.Counters["rnic0"] = model.RNICCounters{TxPauseDurationUs: 41_000}
	d = poll(t, m)
	if !d.Run || d.Trigger != model.TriggerPFC || !d.Rates[0].PauseKnown || len(d.UnreadCounters) != 0 {
		t.Fatalf("40 ms of pause after the reset is measured and triggers: %+v", d)
	}
}

package monitor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/config"
	"github.com/yuuki/hostping/internal/model"
	"github.com/yuuki/hostping/internal/monitor"
	"github.com/yuuki/hostping/internal/topology"
)

// cpuSampler is a StaticSampler with a CPU signal: each CPU call returns the
// next reading of ticks, or err.
type cpuSampler struct {
	monitor.StaticSampler
	ticks []map[int]monitor.CPUTicks
	err   error
	calls int
}

func (s *cpuSampler) CPU(context.Context) (map[int]monitor.CPUTicks, error) {
	if s.err != nil {
		return nil, s.err
	}
	t := s.ticks[min(s.calls, len(s.ticks)-1)]
	s.calls++
	return t, nil
}

func idleGPU() []model.GPUStatus { return []model.GPUStatus{{ID: "gpu0", Idle: true}} }

// TestCPULoadPerNUMANode: the busy fraction is the delta of busy over the
// delta of total ticks since the previous poll, per NUMA node, and High is
// set at --cpu-busy-high, the threshold included.
func TestCPULoadPerNUMANode(t *testing.T) {
	g := topology.Fig2()
	cfg := config.Defaults()
	cfg.CPUBusyHigh = 0.5
	s := &cpuSampler{StaticSampler: monitor.StaticSampler{GPU: idleGPU()}, ticks: []map[int]monitor.CPUTicks{
		{0: {Busy: 100, Total: 1000}, 1: {Busy: 100, Total: 1000}},
		{0: {Busy: 600, Total: 2000}, 1: {Busy: 599, Total: 2000}},
	}}
	m := monitor.New(cfg, g, s)
	d0, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d0.CPULoad != nil || d0.CPUUnknown {
		t.Fatalf("warmup poll carries no CPU load: %+v", d0)
	}
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []model.CPULoad{
		{NUMA: 0, Socket: 0, Busy: 0.5, High: true},
		{NUMA: 1, Socket: 1, Busy: 0.499, High: false},
	}
	if len(d.CPULoad) != len(want) {
		t.Fatalf("CPULoad = %+v, want %+v", d.CPULoad, want)
	}
	for i := range want {
		if d.CPULoad[i] != want[i] {
			t.Errorf("CPULoad[%d] = %+v, want %+v", i, d.CPULoad[i], want[i])
		}
	}
	// CPU load does not make the host busy: the RNICs and GPUs decide that.
	if !d.HostIdle || !d.IdleKnown || d.Mode != model.ModeIdle {
		t.Fatalf("a CPU-loaded host is still idle by its RNICs and GPUs: %+v", d)
	}
}

func TestCPUBusyHighZeroDisables(t *testing.T) {
	cfg := config.Defaults()
	cfg.CPUBusyHigh = 0
	s := &cpuSampler{StaticSampler: monitor.StaticSampler{GPU: idleGPU()}, ticks: []map[int]monitor.CPUTicks{
		{0: {Busy: 0, Total: 100}},
		{0: {Busy: 100, Total: 200}},
	}}
	m := monitor.New(cfg, topology.Fig2(), s)
	_, _ = m.Poll(context.Background())
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.CPULoad) != 1 || d.CPULoad[0].Busy != 1 || d.CPULoad[0].High {
		t.Fatalf("CPULoad = %+v, want node 0 fully busy and not High", d.CPULoad)
	}
}

// TestUnknownCPULoadIsNotIdle: a CPU signal that cannot be read is neither
// idle nor busy. The host is not idle and the decision says the state is
// unknown, as for an unread GPU.
func TestUnknownCPULoadIsNotIdle(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	var logs []string
	s := &cpuSampler{StaticSampler: monitor.StaticSampler{GPU: idleGPU()}, err: errors.New("no /proc")}
	m := monitor.New(cfg, topology.Fig2(), s)
	m.Logf = func(f string, _ ...any) { logs = append(logs, f) }
	_, _ = m.Poll(context.Background())
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.CPUUnknown || d.HostIdle || d.IdleKnown || d.Trigger == model.TriggerIdle {
		t.Fatalf("unread CPU load must not look idle: %+v", d)
	}
	if len(d.BusyRNICs) != 0 {
		t.Fatalf("unread CPU load must not make an RNIC busy: %+v", d.BusyRNICs)
	}
	if len(logs) != 1 {
		t.Fatalf("the failure is logged once, got %q", logs)
	}

	// Reading again re-baselines: one poll with no load, then a reading.
	s.err = nil
	s.ticks = []map[int]monitor.CPUTicks{{0: {Busy: 0, Total: 100}}, {0: {Busy: 10, Total: 200}}}
	d, _ = m.Poll(context.Background())
	if !d.CPUUnknown {
		t.Fatalf("the first reading after a failure is a baseline: %+v", d)
	}
	d, _ = m.Poll(context.Background())
	if d.CPUUnknown || !d.IdleKnown || len(d.CPULoad) != 1 {
		t.Fatalf("CPU load reads again: %+v", d)
	}
}

// TestNoCPUSignalLeavesIdleAlone: a sampler without a CPU signal (the
// simulator) decides as before.
func TestNoCPUSignalLeavesIdleAlone(t *testing.T) {
	cfg := config.Defaults()
	cfg.IdleInterval = 0
	m := monitor.New(cfg, topology.Fig2(), monitor.StaticSampler{GPU: idleGPU()})
	_, _ = m.Poll(context.Background())
	d, err := m.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.CPULoad != nil || d.CPUUnknown || !d.IdleKnown || !d.HostIdle {
		t.Fatalf("decision %+v", d)
	}
}

// TestCPULoadRebaselinedAfterRun: the first poll after a run reads no CPU
// load, so the run's own probes are never read back as host load.
func TestCPULoadRebaselinedAfterRun(t *testing.T) {
	cfg := config.Defaults()
	s := &cpuSampler{StaticSampler: monitor.StaticSampler{GPU: idleGPU()}, ticks: []map[int]monitor.CPUTicks{
		{0: {Busy: 0, Total: 100}},
		{0: {Busy: 10, Total: 200}},
		{0: {Busy: 200, Total: 300}},
		{0: {Busy: 210, Total: 400}},
	}}
	m := monitor.New(cfg, topology.Fig2(), s)
	_, _ = m.Poll(context.Background())
	_, _ = m.Poll(context.Background())
	m.MarkRun(time.Now())
	d, _ := m.Poll(context.Background())
	if d.Ready || d.CPULoad != nil {
		t.Fatalf("re-baseline poll: %+v", d)
	}
	d, _ = m.Poll(context.Background())
	if len(d.CPULoad) != 1 || d.CPULoad[0].Busy != 0.1 {
		t.Fatalf("CPULoad after re-baseline = %+v, want 0.1", d.CPULoad)
	}
}

func TestReadCPUFrom(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	sys := filepath.Join(root, "sys")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// user nice system idle iowait irq softirq steal guest guest_nice
	write(filepath.Join(proc, "stat"), `cpu  40 0 0 40 0 0 0 0 0 0
cpu0 10 1 2 50 7 0 0 0 99 99
cpu1 20 0 0 30 0 0 0 0 0 0
cpu2 5 0 0 5 0 0 0 0 0 0
intr 1 2 3
`)
	write(filepath.Join(sys, "devices/system/node/node0/cpulist"), "0-1\n")
	write(filepath.Join(sys, "devices/system/node/node1/cpulist"), "2,3\n")
	write(filepath.Join(sys, "devices/system/node/node2/cpulist"), "\n")
	got, err := monitor.ReadCPUFrom(proc, sys)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]monitor.CPUTicks{
		0: {Busy: 10 + 1 + 2 + 20, Total: 70 + 50},
		1: {Busy: 5, Total: 10}, // cpu3 is offline: absent from /proc/stat
		2: {},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("node %d = %+v, want %+v", n, got[n], w)
		}
	}

	// No NUMA nodes in sysfs: every CPU is on node 0.
	got, err = monitor.ReadCPUFrom(proc, filepath.Join(root, "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (monitor.CPUTicks{Busy: 38, Total: 130}) {
		t.Fatalf("flat host = %+v", got)
	}
}

// A node directory without a cpulist is a node without CPUs (zero ticks, no
// error), unlike topology's package lookup, which fails on it. A malformed
// cpulist is an error naming the node.
func TestReadCPUFromCPUListMissingOrMalformed(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	sys := filepath.Join(root, "sys")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proc, "stat"), "cpu  1 0 0 1 0 0 0 0 0 0\ncpu0 1 0 0 1 0 0 0 0 0 0\n")
	write(filepath.Join(sys, "devices/system/node/node0/cpulist"), "0\n")
	if err := os.MkdirAll(filepath.Join(sys, "devices/system/node/node1"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := monitor.ReadCPUFrom(proc, sys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (monitor.CPUTicks{Busy: 1, Total: 2}) || got[1] != (monitor.CPUTicks{}) {
		t.Fatalf("got %+v, want node0 {1 2} and an empty node1", got)
	}
	write(filepath.Join(sys, "devices/system/node/node1/cpulist"), "0-\n")
	if _, err := monitor.ReadCPUFrom(proc, sys); err == nil || !strings.Contains(err.Error(), "node1 cpulist") {
		t.Fatalf("malformed cpulist: err = %v, want one naming node1 cpulist", err)
	}
}

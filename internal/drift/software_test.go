package drift

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

func temps(rnic, ep float64) Reading {
	r := Reading{BandwidthBPS: baseBW, LatP50Ns: baseP50, LatP99Ns: baseP99}
	if rnic != 0 {
		r.TempRNIC = &rnic
	}
	if ep != 0 {
		r.TempEndpoint = &ep
	}
	return r
}

// The temperature of the path's devices is compared between the reference
// days and the recent days, and only what was read on both sides counts. A
// device is hotter from 5 degrees Celsius on.
func TestTemperature(t *testing.T) {
	cases := []struct {
		name string
		p    PathIdent
		// ref and recent are the RNIC and endpoint temperature (0: unread)
		// of the reference days and of the days after them.
		ref, recent [2]float64
		want        model.DriftTempStatus
		reason      string
		devices     int
	}{
		{"gpu hotter", gpuPath, [2]float64{55, 60}, [2]float64{56, 71}, model.DriftTempRose, "", 2},
		{"both steady", gpuPath, [2]float64{55, 60}, [2]float64{57, 63}, model.DriftTempSteady, "", 2},
		{"exactly 5 degrees hotter", gpuPath, [2]float64{55, 60}, [2]float64{55, 65}, model.DriftTempRose, "", 2},
		{"just under 5 degrees hotter", gpuPath, [2]float64{55, 60}, [2]float64{55, 64.99}, model.DriftTempSteady, "", 2},
		{"rnic hotter, gpu unread", gpuPath, [2]float64{55, 0}, [2]float64{62, 0}, model.DriftTempRose, "", 1},
		{"rnic steady, gpu unread", gpuPath, [2]float64{55, 0}, [2]float64{56, 0}, model.DriftTempUnknown, "gpu0: no_sensor", 1},
		{"sensor appeared later", gpuPath, [2]float64{0, 60}, [2]float64{70, 61}, model.DriftTempUnknown, "rnic0: no_reference_reading", 1},
		{"sensor went away", gpuPath, [2]float64{55, 60}, [2]float64{0, 61}, model.DriftTempUnknown, "rnic0: no_recent_reading", 1},
		{"nothing read", gpuPath, [2]float64{}, [2]float64{}, model.DriftTempUnknown, "rnic0: no_sensor", 0},
		{"memory path, rnic steady", memPath, [2]float64{55, 0}, [2]float64{56, 0}, model.DriftTempSteady, "", 1},
		{"memory path, rnic hotter", memPath, [2]float64{55, 0}, [2]float64{61, 0}, model.DriftTempRose, "", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSeries(t)
			s.p = c.p
			s.tr = New(testTh, []PathIdent{c.p}, nil, File{})
			for i := 0; i < 20; i++ {
				r := temps(c.ref[0], c.ref[1])
				if i >= 7 {
					r = temps(c.recent[0], c.recent[1])
					r.BandwidthBPS = baseBW * 0.9
				}
				s.observe(&r, Software{})
				s.now = s.now.Add(24 * time.Hour)
			}
			got := s.finding(model.DriftBandwidth).Temperature
			if got.Status != c.want || got.Reason != c.reason || len(got.Devices) != c.devices {
				t.Fatalf("temperature = %+v, want %s (%q) over %d devices", got, c.want, c.reason, c.devices)
			}
		})
	}
}

// A device has a reference temperature when it was read on most of the 7
// reference days (4), and a recent one when it was read on 3 of the recent
// days. Fewer readings are no temperature: the device is unknown, never
// steady.
func TestTemperatureNeedsEnoughReadings(t *testing.T) {
	run := func(refRead, recentRead int) model.DriftTemperature {
		s := newSeries(t)
		s.p = memPath
		s.tr = New(testTh, []PathIdent{memPath}, nil, File{})
		for i := 0; i < 15; i++ {
			r := temps(0, 0)
			switch {
			case i < 7 && i < refRead:
				r = temps(55, 0)
			case i >= 8 && i-8 < recentRead:
				r = temps(56, 0)
			}
			if i >= 7 {
				r.BandwidthBPS = baseBW * 0.9
			}
			s.observe(&r, Software{})
			s.now = s.now.Add(24 * time.Hour)
		}
		return s.finding(model.DriftBandwidth).Temperature
	}
	if got := run(4, 7); got.Status != model.DriftTempSteady {
		t.Errorf("read on 4 of 7 reference days: %+v, want steady", got)
	}
	if got := run(3, 7); got.Status != model.DriftTempUnknown || got.Reason != "rnic0: no_reference_reading" {
		t.Errorf("read on 3 of 7 reference days: %+v, want unknown (no_reference_reading)", got)
	}
	if got := run(7, 3); got.Status != model.DriftTempSteady {
		t.Errorf("read on 3 recent days: %+v, want steady", got)
	}
	if got := run(7, 2); got.Status != model.DriftTempUnknown || got.Reason != "rnic0: no_recent_reading" {
		t.Errorf("read on 2 recent days: %+v, want unknown (no_recent_reading)", got)
	}
}

// A kernel, hostping or firmware change is recorded once, on the day it is
// first seen, and named by the report section whose findings rest on a
// reference that is older. A value that was not read is not a change, and a
// change from before the reference is not named.
func TestSoftwareChanges(t *testing.T) {
	s := newSeries(t)
	sw := Software{Kernel: "6.8.0-41-generic", Hostping: "0.2.5", Firmware: map[string]string{"0000:17:00.0": "28.39.1002", "0000:99:00.0": "1.0"}}
	run := func(level float64, sw Software) {
		s.observe(reading(level), sw)
		s.now = s.now.Add(24 * time.Hour)
	}
	run(1, sw)
	// A change during the reference days counts; the day before them would
	// not (see below).
	sw.Kernel = "6.8.0-45-generic"
	for i := 0; i < 14; i++ {
		run(1, sw)
	}
	run(1, Software{}) // unread: not a change
	if ev := s.tr.File().Events; len(ev) != 1 || ev[0].Component != "kernel" {
		t.Fatalf("events before the update: %+v", ev)
	}
	if len(s.res.Summary.SoftwareChanges) != 0 {
		t.Fatalf("a report without a finding names software changes: %+v", s.res.Summary.SoftwareChanges)
	}
	next := Software{Kernel: "6.8.0-48-generic\n", Hostping: "0.2.6", Firmware: map[string]string{"0000:17:00.0": "28.41.1000"}}
	for i := 0; i < 10; i++ {
		run(0.92, next)
	}
	want := []model.DriftSoftwareChange{
		{On: "2026-03-02", Component: "kernel", From: "6.8.0-41-generic", To: "6.8.0-45-generic"},
		{On: "2026-03-17", Component: "kernel", From: "6.8.0-45-generic", To: "6.8.0-48-generic"},
		{On: "2026-03-17", Component: "hostping", From: "0.2.5", To: "0.2.6"},
		{On: "2026-03-17", Component: "firmware", From: "28.39.1002", To: "28.41.1000", Devices: 1},
	}
	if got := s.res.Summary.SoftwareChanges; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("software changes = %+v, want %+v", got, want)
	}
	f := s.finding(model.DriftBandwidth)
	if f.Shape != model.DriftShapeStep || f.StepOn != "2026-03-17" {
		t.Fatalf("shape %s on %s, want the step on the day of the change", f.Shape, f.StepOn)
	}
	if fw := s.tr.File().Software.Firmware; len(fw) != 1 {
		t.Fatalf("firmware of an RNIC the topology does not have was kept: %v", fw)
	}
	line := strings.Join(s.logs, "\n")
	for _, sub := range []string{
		"hostping changed from 0.2.5 to 0.2.6",
		"firmware of 0000:17:00.0 changed from 28.39.1002 to 28.41.1000",
		"drifted: rnic0->gpu0 bandwidth 184.00 Gb/s against its reference 200.00 Gb/s (-8.0%, threshold 5.0%; reference 2026-03-01..2026-03-07, recent ",
		"4 of 7 recent days worse by half the threshold), shape step on 2026-03-17, temperature unknown (rnic0: no_sensor); 2026-03-02 kernel 6.8.0-41-generic -> 6.8.0-45-generic; 2026-03-17 kernel 6.8.0-45-generic -> 6.8.0-48-generic; 2026-03-17 hostping 0.2.5 -> 0.2.6; 2026-03-17 firmware 28.39.1002 -> 28.41.1000 (1 RNICs)",
	} {
		if !strings.Contains(line, sub) {
			t.Errorf("log %q lacks %q", line, sub)
		}
	}
	// A change that was seen before the reference began is not this
	// finding's.
	if got := s.tr.softwareSince(dayOf(start) + 2); len(got) != 3 || got[0].On != "2026-03-17" {
		t.Fatalf("changes since the day after the first: %+v", got)
	}
	if got := s.tr.softwareSince(dayOf(start) + 1); len(got) != 4 {
		t.Fatalf("changes since the day of the first: %+v", got)
	}
}

// The history keeps the last 4 changes of each component, so that many
// changes of one do not push out the last change of another, and a version
// string is cut to what the file holds.
func TestSoftwareChangesAreKeptPerComponent(t *testing.T) {
	rnics := []string{"0000:17:00.0", "0000:18:00.0"}
	tr := New(testTh, []PathIdent{gpuPath}, rnics, File{})
	now := start
	tr.Observe(now, nil, Software{Kernel: "6.8.0-1", Firmware: map[string]string{rnics[0]: "fw-0", rnics[1]: "fw-0"}})
	now = now.Add(24 * time.Hour)
	tr.Observe(now, nil, Software{Kernel: "6.8.0-2-" + strings.Repeat("é", 100)})
	for i := 1; i <= 10; i++ {
		now = now.Add(24 * time.Hour)
		tr.Observe(now, nil, Software{Firmware: map[string]string{rnics[0]: fmt.Sprintf("fw-%d", i), rnics[1]: fmt.Sprintf("fw-%d", i)}})
	}
	per := map[string][]Event{}
	for _, e := range tr.File().Events {
		per[e.Component+" "+e.Device] = append(per[e.Component+" "+e.Device], e)
	}
	if len(per["kernel "]) != 1 {
		t.Fatalf("kernel changes kept: %+v, want the one there was, after 20 firmware changes", per["kernel "])
	}
	for _, k := range rnics {
		ev := per["firmware "+k]
		if len(ev) != 4 || ev[0].To != "fw-7" || ev[3].To != "fw-10" {
			t.Fatalf("firmware changes of %s kept: %+v, want the last 4", k, ev)
		}
	}
	if k := tr.File().Software.Kernel; len(k) > 64 || !strings.HasSuffix(k, "é") || !strings.HasPrefix(k, "6.8.0-2-") {
		t.Fatalf("kernel %q is %d bytes", k, len(k))
	}
	if _, err := Parse(mustMarshal(t, tr.File())); err != nil {
		t.Fatalf("the tracker wrote a history it would reject: %v", err)
	}
}

func mustMarshal(t *testing.T, f File) []byte {
	t.Helper()
	b, err := Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A report names at most 16 software changes, the newest, and counts the
// rest.
func TestSoftwareChangesInAReportAreBounded(t *testing.T) {
	var rnics []string
	for i := 0; i < 20; i++ {
		rnics = append(rnics, fmt.Sprintf("0000:%02x:00.0", i))
	}
	s := newSeries(t)
	s.tr = New(testTh, []PathIdent{gpuPath}, rnics, File{})
	for d := 0; d < 30; d++ {
		fw := map[string]string{}
		for i, k := range rnics {
			fw[k] = "fw-1"
			if d >= 10+i%10 { // every RNIC on a day and to a version of its own
				fw[k] = fmt.Sprintf("fw-2-%d", i)
			}
		}
		level := 1.0
		if d >= 20 {
			level = 0.9
		}
		s.observe(reading(level), Software{Firmware: fw})
		s.now = s.now.Add(24 * time.Hour)
	}
	sum := s.res.Summary
	if len(sum.SoftwareChanges) != 16 || sum.SoftwareChangesOmitted != 4 {
		t.Fatalf("%d software changes, %d omitted: want 16 of 20", len(sum.SoftwareChanges), sum.SoftwareChangesOmitted)
	}
	if first, last := sum.SoftwareChanges[0].On, sum.SoftwareChanges[15].On; first != "2026-03-13" || last != "2026-03-20" {
		t.Fatalf("changes from %s to %s, want the newest", first, last)
	}
}

// Readings taken with other probe settings are not comparable. A path whose
// settings changed starts a new history instead of being judged against a
// reference taken with the old ones, and the log says so.
func TestProbeSettingsChangeStartsOver(t *testing.T) {
	probe := Probe{SmallMsgBytes: 1, LargeMsgBytes: 262144, Iters: 512}
	ident := func(p Probe) []PathIdent {
		g, m := gpuPath, memPath
		g.Probe, m.Probe = p.For(model.EndpointGPU), p.For(model.EndpointMemory)
		return []PathIdent{g, m}
	}
	both := func(level float64) []Sample {
		return []Sample{{RNIC: "rnic0", Endpoint: "gpu0", Reading: *reading(level)}, {RNIC: "rnic0", Endpoint: "mem0", Reading: *reading(level)}}
	}
	tr := New(testTh, ident(probe), nil, File{})
	now := start
	for i := 0; i < 12; i++ {
		tr.Observe(now, both(1), Software{})
		now = now.Add(24 * time.Hour)
	}
	if h := tr.File().Paths[gpuPath.Key]; h.Probe != "small_msg=1 large_msg=262144 iters=512" || h.Ref == nil {
		t.Fatalf("GPU path history: probe %q, reference %v", h.Probe, h.Ref)
	}
	if h := tr.File().Paths[memPath.Key]; h.Probe != "small_msg=1 large_msg=262144 iters=512 mem_warmup=0s" {
		t.Fatalf("memory path history: probe %q", h.Probe)
	}

	// The memory warm-up changes what a memory path reads, and nothing for a
	// GPU path.
	warm := probe
	warm.MemWarmup = 200 * time.Millisecond
	tr = New(testTh, ident(warm), nil, reload(t, tr.File()))
	res := tr.Observe(now, both(0.8), Software{})
	if res.Paths[0].Status != model.DriftStable || res.Paths[1].Reason != model.DriftCollectingReference {
		t.Fatalf("after --mem-warmup: GPU path %s (%s), memory path %s (%s): want only the memory path started over",
			res.Paths[0].Status, res.Paths[0].Reason, res.Paths[1].Status, res.Paths[1].Reason)
	}
	if len(res.Logs) != 1 || res.Logs[0] != "1 paths start a new history: their readings were taken with other probe settings (small_msg=1 large_msg=262144 iters=512 mem_warmup=0s -> small_msg=1 large_msg=262144 iters=512 mem_warmup=200ms) and are not comparable" {
		t.Fatalf("logs = %q", res.Logs)
	}

	// Each of the other settings starts every path over.
	for name, edit := range map[string]func(*Probe){
		"--small-msg": func(p *Probe) { p.SmallMsgBytes = 4096 },
		"--large-msg": func(p *Probe) { p.LargeMsgBytes = 65536 },
		"--iters":     func(p *Probe) { p.Iters = 64 },
	} {
		changed := warm
		edit(&changed)
		next := New(testTh, ident(changed), nil, reload(t, tr.File()))
		// Readings 20% away from the old reference: with it, a finding.
		var last Result
		at := now.Add(24 * time.Hour)
		for i := 0; i < 12; i++ {
			last = next.Observe(at, both(0.8), Software{})
			at = at.Add(24 * time.Hour)
		}
		if last.Summary.Drifted != 0 || last.Summary.Stable != 2 {
			t.Errorf("%s: %+v, want both paths stable against a reference of their own", name, last.Summary)
		}
		if ref := next.File().Paths[gpuPath.Key].Ref; ref == nil || ref.BandwidthBPS != baseBW*0.8 || ref.From != dayOf(now)+1 {
			t.Errorf("%s: reference %+v, want one taken with the new settings", name, ref)
		}
	}
}

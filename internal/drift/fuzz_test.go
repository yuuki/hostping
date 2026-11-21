package drift

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"
)

// FuzzParse: the drift history is read from disk at every daemon start, and
// a file that is damaged, foreign or hand-edited must neither crash the
// daemon nor be taken as a history the tracker then misbehaves on. Whatever
// Parse accepts must
//
//   - encode and parse again to the same content,
//   - carry a tracker through runs under a clock that is behind it, level
//     with it and ahead of it without a panic,
//   - stay within the bounds of the format, and
//   - be written back as a file Parse accepts.
func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"version":1,"paths":{}}`))
	f.Add([]byte(`{"version":1,"paths":null}`))
	f.Add([]byte(`{"version":2,"paths":{}}`))
	f.Add([]byte(`{"version":1,"paths":{"k":null}}`))
	f.Add([]byte(`{"version":1,"paths":{"k":{"probe":""}}}`))
	f.Add([]byte(`{"version":1,"saved_at":"2026-03-21T00:00:00Z","last_day":20530,"clock_behind_since":20529,"software":{"kernel":"6.8.0","hostping":"0.2.6","firmware":{"a":"1.0"}},"events":[{"day":20500,"component":"kernel","from":"6.7","to":"6.8.0"},{"day":20501,"component":"firmware","device":"a","from":"0.9","to":"1.0"}],"paths":{"a->mem0of2/local_mem":{"probe":"small_msg=1 large_msg=262144 iters=512 mem_warmup=0s","absent":3,"reference":{"from":20500,"to":20506,"bw":2e11,"p50":1400,"p99":1600,"t_rnic":55},"days":[{"day":20520,"n":288,"bw":1.9e11,"p50":1400,"p99":1600,"t_rnic":61.5},{"day":20521,"n":1,"bw":1.8e11,"p50":1500,"p99":1700}],"open":{"day":20530,"seen":40,"stride":4,"samples":[{"bw":1.8e11,"p50":1500,"p99":1700,"t_ep":70}]},"drifted":{"bandwidth":{"since":20525,"threshold":0.05},"latency_p99":{"since":20526,"threshold":0.31}}}}}`))
	f.Add([]byte(`{"version":1,"last_day":4194304,"paths":{"k":{"probe":"","open":{"day":4194304,"seen":1073741824,"stride":1048576,"samples":[{"bw":1e18,"p50":1e18,"p99":1e18,"t_rnic":-100,"t_ep":1000}]}}}}`))
	f.Add([]byte(`{"version":1,"paths":{"k":{"probe":"","days":[{"day":5,"n":1,"bw":1e999,"p50":1,"p99":1}]}}}`))
	f.Add([]byte(`{"version":1,"paths":{"k":{"probe":"","days":[{"day":5,"n":1,"bw":-1,"p50":1,"p99":1}]}}}`))
	f.Add([]byte(`{"version":1,"paths":{"k":{"probe":"","days":[{"day":5,"n":1,"bw":1e-320,"p50":1,"p99":1}]}}}`))
	// The extremes a history may hold: the smallest reference against the
	// largest days, with a verdict at the largest threshold.
	f.Add([]byte(`{"version":1,"last_day":20,"paths":{"k":{"probe":"","reference":{"from":1,"to":7,"bw":1e18,"p50":1,"p99":1},"days":[{"day":8,"n":1,"bw":1,"p50":1e18,"p99":1e18},{"day":9,"n":1,"bw":1,"p50":1e18,"p99":1e18},{"day":10,"n":1,"bw":1,"p50":1e18,"p99":1e18},{"day":11,"n":1,"bw":1e18,"p50":1,"p99":1},{"day":12,"n":1,"bw":1,"p50":1e18,"p99":1e18},{"day":13,"n":1,"bw":1e18,"p50":1,"p99":1},{"day":14,"n":1,"bw":1,"p50":1e18,"p99":1e18},{"day":15,"n":1,"bw":1e18,"p50":1,"p99":1}],"drifted":{"latency_p50":{"since":9,"threshold":1e19}}}}}`))
	if b, err := Marshal(fuzzSeedHistory()); err == nil {
		f.Add(b)
		f.Add(b[:len(b)/2])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Parse(data)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse error %v does not wrap ErrInvalid", err)
			}
			return
		}
		first, err := Marshal(file)
		if err != nil {
			t.Fatalf("an accepted history does not encode: %v", err)
		}
		again, err := Parse(first)
		if err != nil {
			t.Fatalf("an accepted history is rejected once re-encoded: %v\n%s", err, first)
		}
		if second, _ := Marshal(again); !bytes.Equal(first, second) {
			t.Fatalf("re-encoding changed the history:\n%s\n%s", first, second)
		}

		// Track up to three of its paths and one it has never seen.
		keys := make([]string, 0, len(file.Paths))
		for k := range file.Paths {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		paths := []PathIdent{{RNIC: "rnicN", Endpoint: "gpuN", Key: "new/path", EndpointSensor: true}}
		for i, k := range keys {
			if i == 3 {
				break
			}
			// The first keeps the probe settings the file has for it, the
			// others come with settings of their own and may start over.
			probe := "small_msg=1 large_msg=262144 iters=512"
			if i == 0 {
				probe = file.Paths[k].Probe
			}
			paths = append(paths, PathIdent{RNIC: "rnic" + string(rune('0'+i)), Endpoint: "mem0", Key: k, Probe: probe})
		}
		var samples []Sample
		temp := 60.0
		for _, p := range paths {
			samples = append(samples, Sample{RNIC: p.RNIC, Endpoint: p.Endpoint, Reading: Reading{BandwidthBPS: 1.5e11, LatP50Ns: 1500, LatP99Ns: 1900, TempRNIC: &temp}})
		}
		newest := file.newestDay()
		tr := New(0.05, paths, []string{"a"}, file)
		at := func(day int) time.Time { return time.Unix(int64(day)*86400+3600, 0) }
		clock := []time.Time{time.Unix(-5, 0), at(0), at(newest / 2), at(newest - 1), at(newest - 1 + healDays)}
		for i := 0; i < 14; i++ {
			clock = append(clock, at(newest+i))
		}
		clock = append(clock, at(newest+200), at(maxDay), time.Unix(1<<62, 0), at(newest))
		for _, now := range clock {
			res := tr.Observe(now, samples, Software{Kernel: "k" + now.Format("0102"), Hostping: "h" + now.Format("01"), Firmware: map[string]string{"a": "fw" + now.Format("02")}})
			if len(res.Paths) != len(paths) {
				t.Fatalf("result names %d paths, want %d", len(res.Paths), len(paths))
			}
			if n := res.Summary.Stable + res.Summary.Drifted + res.Summary.Unknown; n != len(paths) {
				t.Fatalf("summary counts %d paths, want %d", n, len(paths))
			}
			// Whatever the history held, the report section is one JSON can
			// carry: a change or threshold that is not a finite number would
			// lose the whole report.
			if _, err := json.Marshal(res.Summary); err != nil {
				t.Fatalf("the drift section does not encode: %v\n%+v", err, res.Summary)
			}
			if _, err := json.Marshal(res.Paths); err != nil {
				t.Fatalf("the path states do not encode: %v\n%+v", err, res.Paths)
			}
			if len(res.Summary.Findings) > maxFindings || len(res.Summary.SoftwareChanges) > maxChanges {
				t.Fatalf("%d findings, %d software changes: out of bounds", len(res.Summary.Findings), len(res.Summary.SoftwareChanges))
			}
		}
		out := tr.File()
		// The kernel, hostping and the one RNIC the tracker was given, on top
		// of whatever events the file came with.
		if len(out.Paths) > MaxPaths || len(out.Events) > len(file.Events)+3*maxEventsPer {
			t.Fatalf("%d paths, %d events: out of bounds", len(out.Paths), len(out.Events))
		}
		for k, h := range out.Paths {
			if len(h.Days) > windowDays || (h.Open != nil && len(h.Open.Samples) > openCap) {
				t.Fatalf("path %q: %d days, open %+v: out of bounds", k, len(h.Days), h.Open)
			}
		}
		b, err := Marshal(out)
		if err != nil {
			t.Fatalf("the history does not encode after tracking: %v", err)
		}
		if _, err := Parse(b); err != nil {
			t.Fatalf("the tracker wrote a history Parse rejects: %v\n%s", err, b)
		}
	})
}

// fuzzSeedHistory is a history a tracker wrote over a few weeks.
func fuzzSeedHistory() File {
	tr := New(0.05, []PathIdent{gpuPath, memPath}, []string{"0000:17:00.0"}, File{})
	now := start
	temp := 55.0
	for i := 0; i < 25; i++ {
		level := 1.0
		if i > 15 {
			level = 0.9
		}
		r := Reading{BandwidthBPS: baseBW * level, LatP50Ns: baseP50, LatP99Ns: baseP99, TempRNIC: &temp}
		tr.Observe(now, []Sample{{RNIC: "rnic0", Endpoint: "gpu0", Reading: r}, {RNIC: "rnic0", Endpoint: "mem0", Reading: r}},
			Software{Kernel: "6.8.0-45", Hostping: "0.2.6", Firmware: map[string]string{"0000:17:00.0": "28.39.1002"}})
		now = now.Add(24 * time.Hour)
	}
	return tr.File()
}

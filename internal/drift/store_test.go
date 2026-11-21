package drift

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/model"
)

// history is a small history with every part of the file in it: a
// reference, closed days, an open day, a verdict, software and an event.
func history(t *testing.T) File {
	t.Helper()
	s := newSeries(t)
	tC, tE := 55.0, 61.5
	for i := 0; i < 20; i++ {
		level := 1.0
		if i >= 12 {
			level = 0.9
		}
		sw := Software{Kernel: "6.8.0-45-generic", Hostping: "0.2.6", Firmware: map[string]string{"0000:17:00.0": "28.39.1002"}}
		if i >= 12 {
			sw.Kernel = "6.8.0-48-generic"
		}
		s.observe(&Reading{BandwidthBPS: baseBW * level, LatP50Ns: baseP50, LatP99Ns: baseP99, TempRNIC: &tC, TempEndpoint: &tE}, sw)
		s.now = s.now.Add(24 * time.Hour)
	}
	f := s.tr.File()
	h := f.Paths[gpuPath.Key]
	if h.Ref == nil || len(h.Days) == 0 || h.Open == nil || len(h.Drifted) == 0 || len(f.Events) == 0 {
		t.Fatalf("the sample history lacks a part: %+v", h)
	}
	return f
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if _, status, why := s.Load(); status != LoadFresh || why == "" {
		t.Fatalf("empty directory: status %v (%q), want LoadFresh", status, why)
	}
	want := history(t)
	now := time.Date(2026, 3, 21, 9, 0, 0, 0, time.FixedZone("JST", 9*3600))
	if err := s.Save(want, now); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("history mode %v, want 0600", st.Mode().Perm())
	}
	// One line, with the path keys spelled as they are.
	raw, _ := os.ReadFile(s.Path())
	if n := strings.Count(string(raw), "\n"); n != 1 || raw[len(raw)-1] != '\n' || !strings.Contains(string(raw), `"`+gpuPath.Key+`":{`) {
		t.Fatalf("history is %d lines, or its keys are escaped:\n%s", n, raw)
	}
	got, status, why := NewStore(dir).Load()
	if status != LoadRestored || why != "" {
		t.Fatalf("status %v (%q), want LoadRestored", status, why)
	}
	if got.Version != Version || !got.SavedAt.Equal(now) || got.SavedAt.Location() != time.UTC {
		t.Fatalf("header = version %d, saved_at %v", got.Version, got.SavedAt)
	}
	// What was read continues as what was written would have.
	a, _ := Marshal(want)
	got.SavedAt = time.Time{}
	b, _ := Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("round trip changed the history:\n%s\n%s", a, b)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != FileName {
		t.Fatalf("directory holds %v, want only %s (no temporary file left)", ents, FileName)
	}
}

// A file that is not a usable history is set aside, never overwritten in
// place and never read as an empty history that silently replaces it.
func TestStoreSetsUnusableFileAside(t *testing.T) {
	good, _ := Marshal(history(t))
	day := func(d int, bw string) string {
		return `{"day": ` + strconv.Itoa(d) + `, "n": 1, "bw": ` + bw + `, "p50": 1, "p99": 1}`
	}
	path := func(body string) string {
		return `{"version": 1, "last_day": 9, "paths": {"k": {"probe": "", ` + body + `}}}`
	}
	const ref = `"reference": {"from": 1, "to": 7, "bw": 1, "p50": 1, "p99": 1}`
	cases := map[string]string{
		"truncated":      string(good[:len(good)/2]),
		"empty":          "",
		"not json":       "hello\n",
		"another tool's": `{"version": 1, "paths": {}, "owner": "something else"}`,
		"version 0":      `{"version": 0, "paths": {}}`,
		"no version":     `{"paths": {}}`,
		"trailing data":  string(good) + "{}",
		"null path":      `{"version": 1, "paths": {"k": null}}`,
		// A field of an earlier shape of this file is another file.
		"field of another shape": `{"version": 1, "paths": {"k": {"first_seen": 1, "last_seen": 9}}}`,
		"negative last day":      `{"version": 1, "last_day": -1, "paths": {}}`,
		"negative absent":        path(`"absent": -1`),
		"negative day":           path(`"days": [` + day(-1, "1") + `]`),
		"days unordered":         path(`"days": [` + day(5, "1") + `, ` + day(4, "1") + `]`),
		"a day twice":            path(`"days": [` + day(5, "1") + `, ` + day(5, "1") + `]`),
		"zero bandwidth":         path(`"days": [` + day(5, "0") + `]`),
		"huge number":            path(`"days": [` + day(5, "1e999") + `]`),
		// A value no probe reads: a change against it would not be a number
		// a report can carry.
		"denormal bandwidth":       path(`"days": [` + day(5, "1e-320") + `]`),
		"bandwidth under 1 bit/s":  path(`"days": [` + day(5, "0.999") + `]`),
		"bandwidth over 1e18":      path(`"days": [` + day(5, "1.01e18") + `]`),
		"reference under 1":        path(`"reference": {"from": 1, "to": 7, "bw": 1e-320, "p50": 1, "p99": 1}`),
		"reference ends first":     path(`"reference": {"from": 7, "to": 1, "bw": 1, "p50": 1, "p99": 1}`),
		"verdict, no ref":          path(`"drifted": {"bandwidth": {"since": 5, "threshold": 0.05}}`),
		"verdict of another shape": path(ref + `, "drifted": {"bandwidth": 5}`),
		"unknown signal":           path(ref + `, "drifted": {"jitter": {"since": 5, "threshold": 0.05}}`),
		"verdict, no threshold":    path(ref + `, "drifted": {"bandwidth": {"since": 5}}`),
		"negative threshold":       path(ref + `, "drifted": {"bandwidth": {"since": 5, "threshold": -0.05}}`),
		"threshold over 1e19":      path(ref + `, "drifted": {"bandwidth": {"since": 5, "threshold": 1e20}}`),
		"stride not 2^n":           path(`"open": {"day": 9, "seen": 3, "stride": 3, "samples": [{"bw": 1, "p50": 1, "p99": 1}]}`),
		"open, no sample":          path(`"open": {"day": 9, "seen": 1, "stride": 1, "samples": []}`),
		"open day is a closed day": path(`"days": [` + day(9, "1") + `], "open": {"day": 9, "seen": 1, "stride": 1, "samples": [{"bw": 1, "p50": 1, "p99": 1}]}`),
		"open day before the last": path(`"days": [` + day(9, "1") + `], "open": {"day": 8, "seen": 1, "stride": 1, "samples": [{"bw": 1, "p50": 1, "p99": 1}]}`),
		"31 days":                  path(`"days": [` + manyDays(31) + `]`),
		"too many events":          `{"version": 1, "paths": {}, "events": [` + manyEvents(4*(2048+2)+1) + `]}`,
		"long version":             `{"version": 1, "paths": {}, "software": {"kernel": "` + strings.Repeat("k", 65) + `"}}`,
		"long probe":               `{"version": 1, "paths": {"k": {"probe": "` + strings.Repeat("p", 257) + `"}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := NewStore(dir)
			if err := os.WriteFile(s.Path(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			f, status, why := s.Load()
			if status != LoadSetAside || why == "" || len(f.Paths) != 0 {
				t.Fatalf("status %v (%q), %d paths: want LoadSetAside and an empty history", status, why, len(f.Paths))
			}
			if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the unusable file is still in place: %v", err)
			}
			kept, err := os.ReadFile(s.RejectedPath())
			if err != nil || string(kept) != content {
				t.Fatalf("set-aside file = %q (%v), want the original content", kept, err)
			}
			// The new history is written next to it.
			if err := s.Save(File{}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, status, _ := s.Load(); status != LoadRestored {
				t.Fatalf("after the first save: status %v", status)
			}
		})
	}
}

func manyDays(n int) string {
	var days []string
	for i := 0; i < n; i++ {
		days = append(days, `{"day": `+strconv.Itoa(100+i)+`, "n": 1, "bw": 1, "p50": 1, "p99": 1}`)
	}
	return strings.Join(days, ", ")
}

func manyEvents(n int) string {
	var events []string
	for i := 0; i < n; i++ {
		events = append(events, `{"day": 5, "component": "kernel", "from": "a", "to": "b"}`)
	}
	return strings.Join(events, ", ")
}

// The edges of what a history may hold are themselves accepted: the bounds
// are the documented ones, not one short of them.
func TestParseAcceptsTheBounds(t *testing.T) {
	path := func(body string) string {
		return `{"version": 1, "last_day": 9, "paths": {"k": {"probe": "", ` + body + `}}}`
	}
	for name, content := range map[string]string{
		"1 bit/s and 1 ns":             path(`"days": [{"day": 5, "n": 1, "bw": 1, "p50": 1, "p99": 1}]`),
		"1e18":                         path(`"days": [{"day": 5, "n": 1, "bw": 1e18, "p50": 1e18, "p99": 1e18}]`),
		"30 days":                      path(`"days": [` + manyDays(30) + `]`),
		"open day after the last":      path(`"days": [{"day": 8, "n": 1, "bw": 1, "p50": 1, "p99": 1}], "open": {"day": 9, "seen": 1, "stride": 1, "samples": [{"bw": 1, "p50": 1, "p99": 1}]}`),
		"reference of one day":         path(`"reference": {"from": 7, "to": 7, "bw": 1, "p50": 1, "p99": 1}`),
		"threshold 1e19":               path(`"reference": {"from": 1, "to": 7, "bw": 1, "p50": 1, "p99": 1}, "drifted": {"latency_p99": {"since": 0, "threshold": 1e19}}`),
		"clock behind since":           `{"version": 1, "last_day": 9, "clock_behind_since": 4, "paths": {}}`,
		"every event the bound allows": `{"version": 1, "paths": {}, "events": [` + manyEvents(4*(2048+2)) + `]}`,
		"version of 64 bytes":          `{"version": 1, "paths": {}, "software": {"kernel": "` + strings.Repeat("k", 64) + `", "hostping": "0.2.6"}}`,
	} {
		if _, err := Parse([]byte(content)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Only one set-aside file is kept, so a file that keeps getting damaged
// cannot fill the directory.
func TestStoreKeepsOneSetAsideFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for _, content := range []string{"first", "second"} {
		if err := os.WriteFile(s.Path(), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, status, _ := s.Load(); status != LoadSetAside {
			t.Fatalf("status %v", status)
		}
	}
	kept, _ := os.ReadFile(s.RejectedPath())
	ents, _ := os.ReadDir(dir)
	if string(kept) != "second" || len(ents) != 1 {
		t.Fatalf("kept %q among %d files, want the latest only", kept, len(ents))
	}
}

// A history written by a newer build is left exactly as it is, and tracking
// stays off: this build would otherwise rewrite it without what it does not
// know, on a downgrade that may be rolled forward again.
func TestStoreLeavesNewerVersionAlone(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	content := `{"version": 2, "paths": {}, "new_in_v2": [1, 2, 3]}`
	if err := os.WriteFile(s.Path(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, status, why := s.Load()
	if status != LoadBlocked || !strings.Contains(why, "schema version 2") || !strings.Contains(why, "newer build") {
		t.Fatalf("status %v (%q), want LoadBlocked naming the version", status, why)
	}
	if got, _ := os.ReadFile(s.Path()); string(got) != content {
		t.Fatalf("the newer file was changed: %q", got)
	}
	if _, err := os.Stat(s.RejectedPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the newer file was set aside")
	}
}

// A history that cannot be read is not replaced either.
func TestStoreBlocksOnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	// A directory in the file's place cannot be read as a file.
	if err := os.Mkdir(s.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, status, why := s.Load(); status != LoadBlocked || !strings.HasPrefix(why, "unreadable: ") {
		t.Fatalf("status %v (%q), want LoadBlocked (unreadable)", status, why)
	}
	if st, err := os.Stat(s.Path()); err != nil || !st.IsDir() {
		t.Fatalf("the unreadable entry was touched: %v", err)
	}
}

// An unusable file that cannot be set aside blocks tracking: saving over it
// would lose it.
func TestStoreBlocksWhenSetAsideFails(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := os.WriteFile(s.Path(), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory where the set-aside file goes: the rename fails.
	if err := os.MkdirAll(filepath.Join(s.RejectedPath(), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, status, why := s.Load(); status != LoadBlocked || !strings.Contains(why, "could not be set aside") {
		t.Fatalf("status %v (%q), want LoadBlocked", status, why)
	}
	if got, _ := os.ReadFile(s.Path()); string(got) != "damaged" {
		t.Fatalf("the file was changed: %q", got)
	}
}

// A file far larger than any history is not read into memory.
func TestStoreBoundsWhatItReads(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	fh, err := os.Create(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.Truncate(maxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	if _, status, why := s.Load(); status != LoadSetAside || !strings.Contains(why, "larger than") {
		t.Fatalf("status %v (%q), want LoadSetAside (too large)", status, why)
	}
}

// A failed save leaves the previous history as it was.
func TestStoreSaveFailureKeepsPreviousFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(history(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.Path())
	bad := history(t)
	nan := 0.0
	nan /= nan
	bad.Paths[gpuPath.Key].Days[0].BandwidthBPS = nan // JSON cannot hold it
	if err := s.Save(bad, time.Now()); err == nil {
		t.Fatal("a history JSON cannot hold was saved")
	}
	after, _ := os.ReadFile(s.Path())
	ents, _ := os.ReadDir(dir)
	if string(after) != string(before) || len(ents) != 1 {
		t.Fatalf("a failed save changed the file or left %d entries behind", len(ents))
	}
}

func TestParseErrorsWrapErrInvalid(t *testing.T) {
	for _, in := range []string{"", "{", `{"version": 3}`, `{"version": 1, "extra": 1}`} {
		if _, err := Parse([]byte(in)); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %v, want ErrInvalid", in, err)
		}
	}
	f, err := Parse([]byte(`{"version": 1, "paths": null}`))
	if err != nil || len(f.Paths) != 0 {
		t.Fatalf("an empty history: %v", err)
	}
	// A history without paths still tracks.
	res := New(testTh, []PathIdent{gpuPath}, nil, f).Observe(start, nil, Software{})
	if len(res.Paths) != 1 || res.Paths[0].Reason != model.DriftCollectingReference {
		t.Fatalf("result = %+v", res.Paths)
	}
}

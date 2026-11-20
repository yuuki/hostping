package drift

import (
	"errors"
	"os"
	"path/filepath"
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
		sw := Software{Kernel: "6.8.0-45-generic", Firmware: map[string]string{"0000:17:00.0": "28.39.1002"}}
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
	cases := map[string]string{
		"truncated":       string(good[:len(good)/2]),
		"empty":           "",
		"not json":        "hello\n",
		"another tool's":  `{"version": 1, "paths": {}, "owner": "something else"}`,
		"version 0":       `{"version": 0, "paths": {}}`,
		"no version":      `{"paths": {}}`,
		"trailing data":   string(good) + "{}",
		"negative day":    `{"version": 1, "paths": {"k": {"first_seen": -1, "last_seen": 0}}}`,
		"days unordered":  `{"version": 1, "paths": {"k": {"first_seen": 1, "last_seen": 9, "days": [{"day": 5, "n": 1, "bw": 1, "p50": 1, "p99": 1}, {"day": 4, "n": 1, "bw": 1, "p50": 1, "p99": 1}]}}}`,
		"zero bandwidth":  `{"version": 1, "paths": {"k": {"first_seen": 1, "last_seen": 9, "days": [{"day": 5, "n": 1, "bw": 0, "p50": 1, "p99": 1}]}}}`,
		"huge number":     `{"version": 1, "paths": {"k": {"first_seen": 1, "last_seen": 9, "days": [{"day": 5, "n": 1, "bw": 1e999, "p50": 1, "p99": 1}]}}}`,
		"null path":       `{"version": 1, "paths": {"k": null}}`,
		"verdict, no ref": `{"version": 1, "paths": {"k": {"first_seen": 1, "last_seen": 9, "drifted": {"bandwidth": 5}}}}`,
		"unknown signal":  `{"version": 1, "paths": {"k": {"first_seen": 1, "last_seen": 9, "reference": {"from": 1, "to": 7, "bw": 1, "p50": 1, "p99": 1, "bw_spread": 0, "p50_spread": 0, "p99_spread": 0}, "drifted": {"jitter": 5}}}}`,
		"stride not 2^n":  `{"version": 1, "paths": {"k": {"first_seen": 1, "last_seen": 9, "open": {"day": 9, "seen": 3, "stride": 3, "samples": [{"bw": 1, "p50": 1, "p99": 1}]}}}}`,
		"open, no sample": `{"version": 1, "paths": {"k": {"first_seen": 1, "last_seen": 9, "open": {"day": 9, "seen": 1, "stride": 1, "samples": []}}}}`,
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

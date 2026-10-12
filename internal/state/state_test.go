package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuuki/hostping/internal/topology"
)

const maxAge = 15 * time.Minute

func open(t *testing.T, dir, boot string) *Store {
	t.Helper()
	s, err := Open(dir, boot, Fingerprint(topology.Fig2()))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sample(now time.Time) File {
	return File{
		Gray:          map[string]int{"mem.0": 2, "upi.0": 1},
		SlowMR:        map[string]int{"gpu3": 1},
		Latency:       map[string]int{"rnic0->gpu3": 1},
		GrayUpdatedAt: now.Add(-time.Minute),
		LastIdleEnd:   now.Add(-time.Minute),
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "boot-a")
	now := time.Now()
	if err := s.Save(sample(now), now); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v, want 0600", st.Mode().Perm())
	}
	f, ok, why := open(t, dir, "boot-a").Load(now, maxAge)
	if !ok {
		t.Fatalf("not restored: %s", why)
	}
	if f.Version != Version || f.BootID != "boot-a" || f.Topology != Fingerprint(topology.Fig2()) {
		t.Fatalf("header not stamped: %+v", f)
	}
	if len(f.Gray) != 2 || f.Gray["mem.0"] != 2 || f.Gray["upi.0"] != 1 {
		t.Fatalf("gray = %v", f.Gray)
	}
	if !f.LastIdleEnd.Equal(now.Add(-time.Minute)) || !f.GrayUpdatedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("times = %v %v", f.LastIdleEnd, f.GrayUpdatedAt)
	}
	if len(f.SlowMR) != 1 || f.SlowMR["gpu3"] != 1 {
		t.Fatalf("slow_mr = %v", f.SlowMR)
	}
	if len(f.Latency) != 1 || f.Latency["rnic0->gpu3"] != 1 {
		t.Fatalf("latency = %v", f.Latency)
	}
}

// slow_mr and latency are optional: a file written before they existed, or
// with no streak to keep, restores with empty streaks under the same schema
// version.
func TestRoundTripWithoutSlowMR(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "boot-a")
	now := time.Now()
	f := sample(now)
	f.SlowMR = nil
	f.Latency = nil
	if err := s.Save(f, now); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "slow_mr") || strings.Contains(string(b), `"latency"`) {
		t.Fatalf("an empty slow_mr or latency was written: %s", b)
	}
	got, ok, why := open(t, dir, "boot-a").Load(now, maxAge)
	if !ok {
		t.Fatalf("a file without slow_mr not restored: %s", why)
	}
	if got.SlowMR == nil || len(got.SlowMR) != 0 || got.Latency == nil || len(got.Latency) != 0 || got.Gray["mem.0"] != 2 {
		t.Fatalf("slow_mr = %#v latency = %#v gray = %v", got.SlowMR, got.Latency, got.Gray)
	}
}

func TestLoadRejects(t *testing.T) {
	now := time.Now()
	write := func(t *testing.T, dir string, edit func(map[string]any)) {
		t.Helper()
		s := open(t, dir, "boot-a")
		if err := s.Save(sample(now), now); err != nil {
			t.Fatal(err)
		}
		if edit == nil {
			return
		}
		b, _ := os.ReadFile(s.Path())
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		b, _ = json.Marshal(m)
		if err := os.WriteFile(s.Path(), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		store func(t *testing.T, dir string) *Store
		now   time.Time
		want  string
	}{
		{name: "missing", setup: func(*testing.T, string) {}, want: "no state file"},
		{name: "corrupt", setup: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"version":1,"gray":`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "corrupt"},
		{name: "version", setup: func(t *testing.T, dir string) {
			write(t, dir, func(m map[string]any) { m["version"] = Version + 1 })
		}, want: "schema version"},
		{name: "topology", setup: func(t *testing.T, dir string) { write(t, dir, nil) },
			store: func(t *testing.T, dir string) *Store {
				g := topology.Fig2()
				g.Links = g.Links[1:]
				s, err := Open(dir, "boot-a", Fingerprint(g))
				if err != nil {
					t.Fatal(err)
				}
				return s
			}, want: "topology changed"},
		{name: "boot", setup: func(t *testing.T, dir string) { write(t, dir, nil) },
			store: func(t *testing.T, dir string) *Store { return open(t, dir, "boot-b") }, want: "rebooted"},
		{name: "age", setup: func(t *testing.T, dir string) { write(t, dir, nil) },
			now: now.Add(maxAge), want: "more than --flap-window x --idle-interval"},
		{name: "no run", setup: func(t *testing.T, dir string) {
			s := open(t, dir, "boot-a")
			if err := s.Save(File{Gray: map[string]int{"mem.0": 1}}, now); err != nil {
				t.Fatal(err)
			}
		}, want: "records no run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			s := open(t, dir, "boot-a")
			if tc.store != nil {
				s = tc.store(t, dir)
			}
			at := now
			if !tc.now.IsZero() {
				at = tc.now
			}
			if f, ok, why := s.Load(at, maxAge); ok || !strings.Contains(why, tc.want) {
				t.Fatalf("Load = %+v ok=%v why=%q, want rejection containing %q", f, ok, why, tc.want)
			}
		})
	}
}

// The age limit is inclusive and measured from the last recorded run, not
// from when the file was written: a clean shutdown long after the last run
// must not make old streaks look fresh.
func TestLoadAgeFromLastRun(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "boot-a")
	now := time.Now()
	f := sample(now)
	f.LastIdleEnd = now.Add(-maxAge)
	f.GrayUpdatedAt = now.Add(-maxAge - time.Minute)
	if err := s.Save(f, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, why := s.Load(now, maxAge); !ok {
		t.Fatalf("a run exactly maxAge old must be restored: %s", why)
	}
	if _, ok, _ := s.Load(now.Add(time.Second), maxAge); ok {
		t.Fatal("a run older than maxAge must not be restored")
	}
}

// A failed save leaves the previous file intact and no temporary file
// behind.
func TestSaveFailureLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir, "boot-a")
	now := time.Now()
	// The rename target is a non-empty directory: the rename fails after the
	// temporary file was written and synced.
	if err := os.Mkdir(s.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Path(), "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(sample(now), now); err == nil {
		t.Fatal("Save over a non-empty directory must fail")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != FileName {
			t.Fatalf("leftover %s after a failed save", e.Name())
		}
	}

	// An earlier good file survives a save that fails before the rename.
	dir2 := t.TempDir()
	s2 := open(t, dir2, "boot-a")
	if err := s2.Save(sample(now), now); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s2.Path())
	if err := os.Chmod(dir2, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir2, 0o700) }()
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	if err := s2.Save(File{Gray: map[string]int{}, LastIdleEnd: now}, now); err == nil {
		t.Fatal("Save into a read-only directory must fail")
	}
	after, _ := os.ReadFile(s2.Path())
	if string(before) != string(after) {
		t.Fatal("a failed save changed the previous file")
	}
}

func TestOpenRejectsUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if _, err := Open(dir, "boot-a", "fp"); err == nil {
		t.Fatal("Open on a read-only directory must fail")
	}
	if _, err := Open(filepath.Join(dir, "sub"), "boot-a", "fp"); err == nil {
		t.Fatal("Open must fail when the directory cannot be created")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("Open left %d entries behind", len(ents))
	}
}

func TestOpenCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := Open(dir, "boot-a", "fp"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("created mode %v, want 0700", st.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("the writability probe left %d entries", len(ents))
	}
}

func TestFingerprint(t *testing.T) {
	g := topology.Fig2()
	fp := Fingerprint(g)
	if fp != Fingerprint(topology.Fig2()) {
		t.Fatal("fingerprint is not stable")
	}
	// Re-read numbers do not invalidate the counters.
	c := topology.Fig2()
	for i := range c.Links {
		c.Links[i].CapacityBPS++
		c.Links[i].NegotiatedBPS++
	}
	for i := range c.RNICs {
		c.RNICs[i].LineRate++
		c.RNICs[i].Fabric = "other"
	}
	// Order does not matter.
	c.Links[0], c.Links[1] = c.Links[1], c.Links[0]
	if Fingerprint(c) != fp {
		t.Fatal("capacities, rails or order changed the fingerprint")
	}
	// A path that now crosses different links does.
	p := topology.Fig2()
	p.Paths[0].LinkIDs = append([]string(nil), p.Paths[0].LinkIDs[1:]...)
	if Fingerprint(p) == fp {
		t.Fatal("a changed path kept the fingerprint")
	}
	r := topology.Fig2()
	r.Links[0].ID += "x"
	if Fingerprint(r) == fp {
		t.Fatal("a renamed link kept the fingerprint")
	}
}

// The Fig.2 host has no sub-NUMA clustering: modeling packages must
// leave its fingerprint, and so a saved state file, as it was. The hash is
// the one hostping computed before packages were modeled.
func TestFingerprintFig2Golden(t *testing.T) {
	const want = "3d874f8a1730c4c58ef5ddbd6f776d2f03c518a483686993c2c93d6202e91035"
	if got := Fingerprint(topology.Fig2()); got != want {
		t.Fatalf("Fig2 fingerprint %s, want %s", got, want)
	}
	// The SNC variant is a different graph: its state is not restored onto
	// Fig2's and vice versa.
	if Fingerprint(topology.Fig2SNC()) == want {
		t.Fatal("fig2-snc shares Fig2's fingerprint")
	}
}

func TestReadBootID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "boot_id")
	if err := os.WriteFile(p, []byte("1234-abcd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := readBootID(p)
	if err != nil || id != "1234-abcd" {
		t.Fatalf("readBootID = %q, %v", id, err)
	}
	if _, err := readBootID(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing boot ID must be an error")
	}
	if err := os.WriteFile(p, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readBootID(p); err == nil {
		t.Fatal("an empty boot ID must be an error")
	}
}
